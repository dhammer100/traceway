package syslog

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/tracewayapp/traceway/backend/app/cache"
	"github.com/tracewayapp/traceway/backend/app/config"
	"github.com/tracewayapp/traceway/backend/app/models"
	"github.com/tracewayapp/traceway/backend/app/monitoring"
	"github.com/tracewayapp/traceway/backend/app/repositories"
	traceway "go.tracewayapp.com"
)

const (
	defaultWorkers      = 8
	defaultQueueSize    = 4096
	defaultMaxMsgBytes  = 64 * 1024
	insertBatchSize     = 1000
	insertFlushInterval = 2 * time.Second
	metricsTickInterval = 10 * time.Second
	dropReportInterval  = time.Minute
	udpReadBuffer       = 4 * 1024 * 1024
)

// defaultTrustedCIDRs covers the source ranges the user opted into:
// RFC1918, loopback, Tailscale CGNAT, IPv6 loopback, and IPv6 ULA.
var defaultTrustedCIDRs = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"127.0.0.0/8",
	"100.64.0.0/10",
	"::1/128",
	"fc00::/7",
}

// Tailnet is the subset of *tsnet.Server we depend on. Pass nil for stdlib
// listeners (tests / embedded mode); pass a *tsnet.Server in env mode so TCP
// and TLS bind to the tailnet only.
type Tailnet interface {
	Listen(network, addr string) (net.Listener, error)
	ListenTLS(network, addr string) (net.Listener, error)
}

type ingestJob struct {
	raw    []byte
	src    string
	recvAt time.Time
}

type pool struct {
	workers   int
	maxMsg    int
	projectId uuid.UUID
	trusted   []*net.IPNet
	allowAll  bool

	queue   chan ingestJob
	inserts chan models.LogRecord

	received        atomic.Uint64
	droppedOverflow atomic.Uint64
	droppedAuth     atomic.Uint64
	parseErrors     atomic.Uint64
	inserted        atomic.Uint64
	failed          atomic.Uint64

	dropMu             sync.Mutex
	lastDropReportAt   time.Time
	droppedSinceReport uint64
}

var singleton *pool

// Start brings up whichever of UDP / TCP / TLS listeners are configured via
// env. It is a no-op when no listener addr is set, or when the default project
// id is missing or invalid.
func Start(ctx context.Context, tailnet Tailnet) {
	if singleton != nil {
		return
	}

	cfg := config.Config
	udpAddr := strings.TrimSpace(cfg.SyslogUDPAddr)
	tcpAddr := strings.TrimSpace(cfg.SyslogTCPAddr)
	tlsAddr := strings.TrimSpace(cfg.SyslogTLSAddr)
	if udpAddr == "" && tcpAddr == "" && tlsAddr == "" {
		return
	}

	projectId, ok := resolveDefaultProject(cfg.SyslogDefaultProjectId)
	if !ok {
		return
	}

	trusted, allowAll, err := parseTrustedCIDRs(cfg.SyslogTrustedCIDRs)
	if err != nil {
		config.Logf("syslog: invalid SYSLOG_TRUSTED_CIDRS (%v); listeners disabled", err)
		return
	}

	p := &pool{
		workers:   resolveInt(cfg.SyslogWorkers, defaultWorkers, 1),
		maxMsg:    resolveInt(cfg.SyslogMaxMsgBytes, defaultMaxMsgBytes, 512),
		projectId: projectId,
		trusted:   trusted,
		allowAll:  allowAll,
		queue:     make(chan ingestJob, resolveInt(cfg.SyslogQueueSize, defaultQueueSize, 1)),
		inserts:   make(chan models.LogRecord, insertBatchSize),
	}
	singleton = p
	p.start(ctx)

	if udpAddr != "" {
		if err := p.startUDP(ctx, udpAddr); err != nil {
			config.Logf("syslog: failed to start UDP listener on %s: %v", udpAddr, err)
		} else {
			config.Logf("syslog: listening on UDP %s (trusted-cidrs=%s)", udpAddr, describeCIDRs(trusted, allowAll))
		}
	}

	if tcpAddr != "" {
		if err := p.startTCP(ctx, tcpAddr, tailnet); err != nil {
			config.Logf("syslog: failed to start TCP listener on %s: %v", tcpAddr, err)
		} else {
			via := "tailnet"
			if tailnet == nil {
				via = "stdlib"
			}
			config.Logf("syslog: listening on TCP %s (%s)", tcpAddr, via)
		}
	}

	if tlsAddr != "" {
		if err := p.startTLS(ctx, tlsAddr, tailnet, cfg.SyslogTLSCert, cfg.SyslogTLSKey); err != nil {
			config.Logf("syslog: failed to start TLS listener on %s: %v", tlsAddr, err)
		} else {
			via := "tailnet"
			if tailnet == nil {
				via = "stdlib"
			}
			config.Logf("syslog: listening on TLS %s (%s)", tlsAddr, via)
		}
	}
}

func resolveDefaultProject(raw string) (uuid.UUID, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		config.Logln("syslog: SYSLOG_DEFAULT_PROJECT_ID is unset; listeners disabled")
		return uuid.Nil, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		config.Logf("syslog: SYSLOG_DEFAULT_PROJECT_ID is not a valid UUID (%v); listeners disabled", err)
		return uuid.Nil, false
	}
	if cache.ProjectCache.GetById(id) == nil {
		config.Logf("syslog: SYSLOG_DEFAULT_PROJECT_ID %s not found in project cache; listeners disabled", id)
		return uuid.Nil, false
	}
	return id, true
}

func parseTrustedCIDRs(raw string) ([]*net.IPNet, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "*" {
		return nil, true, nil
	}
	src := defaultTrustedCIDRs
	if raw != "" {
		src = strings.Split(raw, ",")
	}
	nets := make([]*net.IPNet, 0, len(src))
	for _, s := range src {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, false, err
		}
		nets = append(nets, n)
	}
	return nets, false, nil
}

func describeCIDRs(nets []*net.IPNet, allowAll bool) string {
	if allowAll {
		return "*"
	}
	parts := make([]string, len(nets))
	for i, n := range nets {
		parts[i] = n.String()
	}
	return strings.Join(parts, ",")
}

func resolveInt(raw string, def, min int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < min {
		return def
	}
	return v
}

// ----------------------------------------------------------------------------
// Pool lifecycle (workers + batcher + metrics)
// ----------------------------------------------------------------------------

func (p *pool) start(ctx context.Context) {
	for i := 0; i < p.workers; i++ {
		go p.worker(ctx)
	}
	go p.batcher(ctx)
	go p.metricsLoop(ctx)
}

func (p *pool) enqueue(j ingestJob) {
	p.received.Add(1)
	select {
	case p.queue <- j:
		return
	default:
	}

	p.droppedOverflow.Add(1)

	var report uint64
	p.dropMu.Lock()
	p.droppedSinceReport++
	if time.Since(p.lastDropReportAt) >= dropReportInterval {
		report = p.droppedSinceReport
		p.droppedSinceReport = 0
		p.lastDropReportAt = time.Now()
	}
	p.dropMu.Unlock()

	if report > 0 {
		traceway.CaptureException(traceway.NewStackTraceErrorf(
			"syslog listener dropped %d messages due to full queue (cap=%d)", report, cap(p.queue)))
	}
}

func (p *pool) worker(ctx context.Context) {
	defer traceway.Recover()

	for {
		select {
		case <-ctx.Done():
			return
		case j, ok := <-p.queue:
			if !ok {
				return
			}
			p.process(ctx, j)
		}
	}
}

func (p *pool) process(ctx context.Context, j ingestJob) {
	msg, err := Parse(j.raw)
	if err != nil {
		p.parseErrors.Add(1)
		return
	}
	rec := ToLogRecord(msg, p.projectId, j.src, j.recvAt)
	select {
	case p.inserts <- rec:
	case <-ctx.Done():
	}
}

func (p *pool) batcher(ctx context.Context) {
	defer traceway.Recover()

	batch := make([]models.LogRecord, 0, insertBatchSize)
	timer := time.NewTimer(insertFlushInterval)
	if !timer.Stop() {
		<-timer.C
	}
	timerActive := false

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := repositories.LogRecordRepository.InsertAsync(ctx, batch); err != nil {
			p.failed.Add(uint64(len(batch)))
			traceway.CaptureException(traceway.NewStackTraceErrorf("syslog: failed to insert batch of %d log_records: %w", len(batch), err))
		} else {
			p.inserted.Add(uint64(len(batch)))
		}
		batch = batch[:0]
		if timerActive {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timerActive = false
		}
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case rec, ok := <-p.inserts:
			if !ok {
				flush()
				return
			}
			batch = append(batch, rec)
			if !timerActive {
				timer.Reset(insertFlushInterval)
				timerActive = true
			}
			if len(batch) >= insertBatchSize {
				flush()
			}
		case <-timer.C:
			timerActive = false
			flush()
		}
	}
}

func (p *pool) metricsLoop(ctx context.Context) {
	defer traceway.Recover()

	ticker := time.NewTicker(metricsTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			monitoring.RecordSyslogIngest(
				len(p.queue),
				p.received.Load(),
				p.droppedOverflow.Load(),
				p.droppedAuth.Load(),
				p.parseErrors.Load(),
				p.inserted.Load(),
				p.failed.Load(),
			)
		}
	}
}

// ----------------------------------------------------------------------------
// IP allowlist
// ----------------------------------------------------------------------------

func (p *pool) trusts(ip net.IP) bool {
	if p.allowAll || ip == nil {
		return p.allowAll
	}
	for _, n := range p.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------------------
// UDP
// ----------------------------------------------------------------------------

func (p *pool) startUDP(ctx context.Context, addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	// Best-effort: many kernels silently cap this. The default rmem_max on
	// Linux is typically 200KB which drops packets under bursty syslog load.
	if udp, ok := pc.(*net.UDPConn); ok {
		_ = udp.SetReadBuffer(udpReadBuffer)
	}
	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()
	go p.readUDP(ctx, pc)
	return nil
}

func (p *pool) readUDP(ctx context.Context, pc net.PacketConn) {
	defer traceway.Recover()

	buf := make([]byte, p.maxMsg)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if n == 0 {
			continue
		}
		udpSrc, _ := src.(*net.UDPAddr)
		if udpSrc == nil || !p.trusts(udpSrc.IP) {
			p.droppedAuth.Add(1)
			continue
		}
		raw := make([]byte, n)
		copy(raw, buf[:n])
		p.enqueue(ingestJob{
			raw:    trimTrailingNewline(raw),
			src:    udpSrc.String(),
			recvAt: time.Now().UTC(),
		})
	}
}

// ----------------------------------------------------------------------------
// TCP
// ----------------------------------------------------------------------------

func (p *pool) startTCP(ctx context.Context, addr string, tailnet Tailnet) error {
	ln, err := listenStream(addr, tailnet)
	if err != nil {
		return err
	}
	go p.serveStream(ctx, ln)
	return nil
}

func (p *pool) startTLS(ctx context.Context, addr string, tailnet Tailnet, certPath, keyPath string) error {
	if tailnet != nil {
		ln, err := tailnet.ListenTLS("tcp", addr)
		if err != nil {
			return err
		}
		go p.serveStream(ctx, ln)
		return nil
	}
	// Stdlib fallback (embedded / tests): require explicit cert+key.
	if certPath == "" || keyPath == "" {
		return errors.New("SYSLOG_TLS_CERT and SYSLOG_TLS_KEY required when running without tailnet")
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return err
	}
	ln, err := tls.Listen("tcp", addr, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		return err
	}
	go p.serveStream(ctx, ln)
	return nil
}

func listenStream(addr string, tailnet Tailnet) (net.Listener, error) {
	if tailnet != nil {
		return tailnet.Listen("tcp", addr)
	}
	return net.Listen("tcp", addr)
}

func (p *pool) serveStream(ctx context.Context, ln net.Listener) {
	defer traceway.Recover()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if !p.trustsConn(conn) {
			p.droppedAuth.Add(1)
			_ = conn.Close()
			continue
		}
		go p.handleStream(ctx, conn)
	}
}

func (p *pool) trustsConn(conn net.Conn) bool {
	remote := conn.RemoteAddr()
	if remote == nil {
		return false
	}
	host, _, err := net.SplitHostPort(remote.String())
	if err != nil {
		return false
	}
	return p.trusts(net.ParseIP(host))
}

// handleStream demultiplexes between octet-counting (RFC 6587 §3.4.1) and
// non-transparent / newline-delimited framing by peeking the first byte: a
// digit means an octet count is incoming, anything else means newline-framed.
func (p *pool) handleStream(ctx context.Context, conn net.Conn) {
	defer traceway.Recover()
	defer conn.Close()

	src := conn.RemoteAddr().String()
	br := bufio.NewReaderSize(conn, 4*p.maxMsg)

	first, err := br.Peek(1)
	if err != nil {
		return
	}
	if first[0] >= '0' && first[0] <= '9' {
		p.readOctetCounted(ctx, br, src)
		return
	}
	p.readNewlineDelimited(ctx, br, src)
}

func (p *pool) readOctetCounted(ctx context.Context, br *bufio.Reader, src string) {
	for {
		if ctx.Err() != nil {
			return
		}
		lenStr, err := br.ReadString(' ')
		if err != nil {
			return
		}
		lenStr = strings.TrimSpace(lenStr)
		if lenStr == "" {
			return
		}
		n, err := strconv.Atoi(lenStr)
		if err != nil || n <= 0 {
			return
		}
		if n > p.maxMsg {
			// Bigger than we'll accept — consume and drop to keep stream sync.
			if _, err := io.CopyN(io.Discard, br, int64(n)); err != nil {
				return
			}
			p.droppedOverflow.Add(1)
			continue
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(br, buf); err != nil {
			return
		}
		p.enqueue(ingestJob{
			raw:    buf,
			src:    src,
			recvAt: time.Now().UTC(),
		})
	}
}

func (p *pool) readNewlineDelimited(ctx context.Context, br *bufio.Reader, src string) {
	scanner := bufio.NewScanner(br)
	scanner.Buffer(make([]byte, 4096), p.maxMsg)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		raw := make([]byte, len(line))
		copy(raw, line)
		p.enqueue(ingestJob{
			raw:    raw,
			src:    src,
			recvAt: time.Now().UTC(),
		})
	}
}

func trimTrailingNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
