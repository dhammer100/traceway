package middleware

import (
	"compress/gzip"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

// MaxReportBodyBytes caps the on-the-wire request body for /api/report and the
// decompressed body when the request arrives gzipped. Project tokens ship to
// browsers via the JS SDK, so they are effectively public — without this cap a
// tiny gzip bomb can OOM the server.
const (
	MaxReportBodyBytes        = 32 << 20  // 32 MB raw request body
	MaxReportDecompressedBytes = 256 << 20 // 256 MB after gzip decompression
)

type limitedReadCloser struct {
	io.Reader
	io.Closer
}

func UseGzip(c *gin.Context) {
	// Cap the raw body even when the client did NOT announce gzip — the
	// pagehide / keepalive code path in the SDK can't `await` the async
	// CompressionStream, so those requests arrive as plain JSON.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxReportBodyBytes)

	if c.GetHeader("Content-Encoding") != "gzip" {
		c.Next()
		return
	}

	gzReader, err := gzip.NewReader(c.Request.Body)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "Invalid gzip"})
		return
	}
	// Cap the decompressed stream so a malicious gzip bomb can't blow up memory
	// after passing the raw-body check.
	limited := io.LimitReader(gzReader, MaxReportDecompressedBytes)
	c.Request.Body = &limitedReadCloser{Reader: limited, Closer: gzReader}
	c.Next()
}
