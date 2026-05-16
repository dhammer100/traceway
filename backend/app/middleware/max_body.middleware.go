package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	// MaxJSONBodyBytes is the cap for dashboard / control-plane JSON endpoints.
	// /api/report and /api/otel/v1/* have their own larger caps (UseGzip and the
	// otel codec) since they ingest telemetry payloads.
	MaxJSONBodyBytes = 1 << 20 // 1 MB

	// MaxMultipartBytes is the cap for /api/sourcemaps/upload. Matches the
	// per-file cap enforced in the controller.
	MaxMultipartBytes = 50 << 20 // 50 MB
)

// MaxBody caps request bodies on routes that don't ingest telemetry. Without
// this, any authenticated user can submit arbitrarily large JSON and exhaust
// memory. /api/report and /api/otel/* manage their own caps because they need
// different limits.
func MaxBody(c *gin.Context) {
	path := c.Request.URL.Path

	switch {
	case path == "/api/report":
		// UseGzip handles the cap for this route.
	case strings.HasPrefix(path, "/api/otel/v1/"):
		// otelcontrollers.codec wraps the body in io.LimitReader.
	case path == "/api/sourcemaps/upload":
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxMultipartBytes)
	default:
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxJSONBodyBytes)
	}

	c.Next()
}
