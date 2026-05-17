package cmd

import (
	"github.com/tracewayapp/traceway/backend/app/cache"
	"github.com/tracewayapp/traceway/backend/app/chdb"
	"github.com/tracewayapp/traceway/backend/app/config"
	"github.com/tracewayapp/traceway/backend/app/controllers"
	"github.com/tracewayapp/traceway/backend/app/db"
	"github.com/tracewayapp/traceway/backend/app/middleware"
	"github.com/tracewayapp/traceway/backend/app/migrations"
	"github.com/tracewayapp/traceway/backend/app/models"
	"github.com/tracewayapp/traceway/backend/app/monitoring"
	"github.com/tracewayapp/traceway/backend/app/notifications"
	"github.com/tracewayapp/traceway/backend/app/recordings"
	"github.com/tracewayapp/traceway/backend/app/retention"
	"github.com/tracewayapp/traceway/backend/app/services"
	"github.com/tracewayapp/traceway/backend/app/storage"
	"github.com/tracewayapp/traceway/backend/app/syslog"
	"github.com/tracewayapp/traceway/backend/static"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	traceway "go.tracewayapp.com"
	tracewaygin "go.tracewayapp.com/tracewaygin"
	"tailscale.com/tsnet"
)

var PostStartupHooks []func(ctx context.Context)

func Run(opts ...Option) {
	var cfg *config.Cfg
	var o *options

	if len(opts) == 0 {
		godotenv.Load()
		cfg = config.LoadFromEnv()
	} else {
		o = &options{sqlitePath: ":memory:"}
		for _, opt := range opts {
			opt(o)
		}
		port := o.port
		if port == 0 {
			port = 8082
		}
		jwtSecret := o.jwtSecret
		if jwtSecret == "" {
			generated, err := generateEphemeralSecret()
			if err != nil {
				panic(fmt.Errorf("failed to generate ephemeral JWT secret: %w", err))
			}
			jwtSecret = generated
		}
		cfg = &config.Cfg{
			JWTSecret:   jwtSecret,
			DBType:      "sqlite",
			SQLitePath:  o.sqlitePath,
			StorageType: "local",
			StoragePath: "./storage",
			APIOnly:     "false",
			Ports:       fmt.Sprintf("%d", port),
		}
		if o.serverURL == "" {
			o.serverURL = fmt.Sprintf("http://localhost:%d", port)
		}
		cfg.AppBaseURL = o.serverURL
		cfg.MonitoringTracewayURL = o.monitoringTracewayURL
		gin.SetMode(gin.ReleaseMode)
		if o.disableLogging {
			config.LoggingEnabled = false
		}
	}
	config.Init(cfg)

	if err := services.InitJWT(); err != nil {
		panic(fmt.Errorf("failed to initialize JWT: %w", err))
	}

	err := db.Init()
	if err != nil {
		panic(fmt.Errorf("error connecting to database: %w", err))
	}

	err = chdb.Init()
	if err != nil {
		panic(fmt.Errorf("error connecting to chdb: %w", err))
	}

	models.Init(db.Driver)

	if err := storage.Init(); err != nil {
		panic(fmt.Errorf("failed to initialize storage: %w", err))
	}

	err = migrations.Run(cfg.DBType)
	if err != nil {
		panic(fmt.Errorf("migrations run failed: %w", err))
	}

	if o != nil {
		if err := seed(o); err != nil {
			panic(fmt.Errorf("seeding failed: %w", err))
		}
	}

	ctx := context.Background()
	if err := cache.ProjectCache.Init(ctx); err != nil {
		panic(fmt.Errorf("projects cache could not be initialized: %w", err))
	}
	cache.InitSourceMapCache(200, 500*1024*1024)

	middleware.InitUseClientAuth()
	middleware.InitUseAppAuth()
	middleware.InitRequireWriteAccess()
	middleware.InitRequireProjectAccess()
	middleware.InitRequireAdminAccess()
	middleware.InitUseSourceMapAuth()

	services.InitEmail()
	services.InitTurnstile()
	services.InitOAuth()

	for _, hook := range PostStartupHooks {
		hook(ctx)
	}

	notifications.StartEvaluator(ctx)
	retention.Start(ctx)
	recordings.Start(ctx)

	var router *gin.Engine
	if o != nil && o.disableLogging {
		router = gin.New()
		router.Use(gin.Recovery())
	} else {
		router = gin.Default()
	}

	if monitoringTracewayUrl := cfg.MonitoringTracewayURL; monitoringTracewayUrl != "" {
		// NOTE: deliberately NOT including RecordingBody / RecordingHeader.
		// Bodies on error contain plaintext passwords (/api/login) and reset
		// tokens (/api/password-reset/:token). Headers contain Authorization
		// bearer tokens (JWTs + project tokens) that the upstream Traceway
		// has no need to know. URL + query is enough debug context for now.
		router.Use(tracewaygin.New(
			monitoringTracewayUrl,
			tracewaygin.WithOnErrorRecording(tracewaygin.RecordingQuery|tracewaygin.RecordingUrl),
		))
		monitoring.StartClickHouseReporter(ctx)
	}

	router.GET("/health", func(c *gin.Context) {
		c.String(200, "OK")
	})

	apiRouterGroup := router.Group("/api", middleware.MaxBody)
	controllers.RegisterControllers(apiRouterGroup)

	router.GET("/version", func(ctx *gin.Context) {
		ctx.JSON(200, gin.H{"version": "0.0.1"})
	})

	apiOnly := cfg.APIOnly == "true"

	if apiOnly {
		router.NoRoute(func(c *gin.Context) {
			c.JSON(404, gin.H{"error": "Not found"})
		})
	} else {
		staticFS, err := static.GetStaticFS()
		if err != nil {
			config.Logf("Warning: Could not load static files: %v", err)
			staticFS = nil
		}

		if staticFS != nil {
			router.StaticFS("/assets", http.FS(mustSubFS(staticFS, "assets")))
			router.StaticFS("/_app", http.FS(mustSubFS(staticFS, "_app")))
			router.GET("/favicon.ico", serveStaticFile(staticFS, "favicon.ico"))
			router.GET("/robots.txt", serveStaticFile(staticFS, "robots.txt"))
		}

		router.NoRoute(createSPAHandler(staticFS))
	}

	// Embedded/test mode (Run was called with options): keep stdlib HTTP so
	// tests don't have to join a tailnet. Env mode (no options) is tailnet-only.
	if o != nil {
		ports := cfg.Ports
		if ports == "" {
			ports = "80,8082"
		}
		portsList := strings.Split(ports, ",")
		if len(portsList) == 0 {
			panic(fmt.Errorf("ports option is invalid - no ports found"))
		}

		if len(portsList) > 1 {
			for i := 1; i < len(portsList); i++ {
				if len(portsList[i]) == 0 {
					continue
				}
				go func(p string) {
					defer traceway.Recover()
					config.Logln("Starting server on :" + p)
					if err := router.Run(":" + p); err != nil {
						panic(fmt.Errorf("Error starting server on port %s: %v", p, err))
					}
				}(portsList[i])
			}
		}

		notifySystemd()
		if err := router.Run(":" + portsList[0]); err != nil {
			panic(fmt.Errorf("Error starting server on port %s: %v", portsList[0], err))
		}
		return
	}

	if cfg.TSNetHostname == "" {
		panic(fmt.Errorf("TSNET_HOSTNAME is required (this build only listens on the tailnet)"))
	}
	if cfg.Ports != "" {
		config.Logln("PORTS is set but ignored — this build listens only on the tailnet")
	}

	srv := newTSNetServer(cfg)
	defer srv.Close()

	if _, err := srv.Up(ctx); err != nil {
		panic(fmt.Errorf("failed to bring up tsnet node %q: %w", cfg.TSNetHostname, err))
	}

	syslog.Start(ctx, srv)

	listenAddr := cfg.TSNetListenAddr
	useTLS := cfg.TSNetHTTPS == "true"
	if listenAddr == "" {
		if useTLS {
			listenAddr = ":443"
		} else {
			listenAddr = ":80"
		}
	}

	var ln net.Listener
	var lnErr error
	if useTLS {
		ln, lnErr = srv.ListenTLS("tcp", listenAddr)
	} else {
		ln, lnErr = srv.Listen("tcp", listenAddr)
	}
	if lnErr != nil {
		panic(fmt.Errorf("failed to listen on tailnet %s: %w", listenAddr, lnErr))
	}

	config.Logf("Listening on tailnet %s as %q (tls=%v)", listenAddr, cfg.TSNetHostname, useTLS)

	notifySystemd()
	server := &http.Server{Handler: router}
	if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
		panic(fmt.Errorf("tailnet http server failed: %w", err))
	}
}

func newTSNetServer(cfg *config.Cfg) *tsnet.Server {
	dir := cfg.TSNetDir
	if dir == "" {
		dir = filepath.Join(".", "tsnet-state")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		panic(fmt.Errorf("failed to create tsnet state dir %s: %w", dir, err))
	}

	srv := &tsnet.Server{
		Hostname:  cfg.TSNetHostname,
		AuthKey:   cfg.TSNetAuthKey,
		Dir:       dir,
		Ephemeral: false,
	}
	if cfg.TSNetLogf == "" || cfg.TSNetLogf == "quiet" {
		srv.Logf = func(string, ...any) {}
	}
	return srv
}

func generateEphemeralSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func notifySystemd() {
	sent, err := daemon.SdNotify(false, daemon.SdNotifyReady)
	if err != nil {
		config.Logf("Failed to notify systemd: %v", err)
	} else if sent {
		config.Logln("Notified systemd that service is ready")
	}

	go func() {
		defer traceway.Recover()

		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			daemon.SdNotify(false, daemon.SdNotifyWatchdog)
		}
	}()
}

func mustSubFS(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		return emptyFS{}
	}
	return sub
}

type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) {
	return nil, fs.ErrNotExist
}

func serveStaticFile(staticFS fs.FS, filename string) gin.HandlerFunc {
	return func(c *gin.Context) {
		data, err := fs.ReadFile(staticFS, filename)
		if err != nil {
			c.Status(404)
			return
		}
		contentType := "application/octet-stream"
		if strings.HasSuffix(filename, ".ico") {
			contentType = "image/x-icon"
		} else if strings.HasSuffix(filename, ".txt") {
			contentType = "text/plain"
		}
		c.Data(200, contentType, data)
	}
}

func createSPAHandler(staticFS fs.FS) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		accept := c.GetHeader("Accept")

		if strings.HasPrefix(path, "/api") {
			c.JSON(404, gin.H{"error": "Not found"})
			return
		}

		if strings.Contains(accept, "application/json") &&
			!strings.Contains(accept, "text/html") &&
			!strings.Contains(accept, "*/*") {
			c.JSON(404, gin.H{"error": "Not found"})
			return
		}

		if staticFS == nil {
			c.JSON(404, gin.H{"error": "Not found"})
			return
		}

		cleanPath := strings.TrimPrefix(path, "/")
		// Defense-in-depth — embed.FS already rejects "../" via fs.ValidPath,
		// but document the assumption so a swap to os.DirFS doesn't silently
		// open a path-traversal hole.
		if cleanPath != "" && !strings.Contains(cleanPath, "..") && !strings.ContainsRune(cleanPath, 0) {
			if data, err := fs.ReadFile(staticFS, cleanPath); err == nil {
				contentType := detectContentType(cleanPath)
				c.Data(200, contentType, data)
				return
			}
		}

		indexData, err := fs.ReadFile(staticFS, "index.html")
		if err != nil {
			c.JSON(404, gin.H{"error": "Not found"})
			return
		}
		c.Data(200, "text/html; charset=utf-8", indexData)
	}
}

func detectContentType(filename string) string {
	switch {
	case strings.HasSuffix(filename, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(filename, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(filename, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(filename, ".json"):
		return "application/json"
	case strings.HasSuffix(filename, ".png"):
		return "image/png"
	case strings.HasSuffix(filename, ".jpg"), strings.HasSuffix(filename, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(filename, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(filename, ".ico"):
		return "image/x-icon"
	case strings.HasSuffix(filename, ".woff"):
		return "font/woff"
	case strings.HasSuffix(filename, ".woff2"):
		return "font/woff2"
	default:
		return "application/octet-stream"
	}
}
