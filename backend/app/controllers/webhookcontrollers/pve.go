package webhookcontrollers

import (
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tracewayapp/traceway/backend/app/middleware"
	"github.com/tracewayapp/traceway/backend/app/webhooks"
	traceway "go.tracewayapp.com"
)

type pveController struct{}

var PVEController = pveController{}

// Receive accepts a PVE 8 webhook notification, parses it, and enqueues a
// LogRecord onto the webhooks pipeline. It returns 202 when the record is
// queued, 400 on malformed input, 413 on oversize body, and 503 when the
// pipeline queue is full (PVE will retry).
func (pveController) Receive(c *gin.Context) {
	p := webhooks.Get()
	if p == nil {
		// Pipeline disabled — route is registered but Start was never called
		// (e.g. tests). Treat as accepted-but-discarded so PVE doesn't loop.
		c.Status(http.StatusServiceUnavailable)
		return
	}

	projectId, err := middleware.GetProjectId(c)
	if err != nil {
		c.AbortWithError(http.StatusInternalServerError, traceway.NewStackTraceErrorf("UseClientAuth middleware must be applied: %w", err))
		return
	}

	max := int64(p.MaxBodyBytes())
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, max+1))
	if err != nil {
		p.IncParseErrors()
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}
	if int64(len(body)) > max {
		p.IncParseErrors()
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "body too large"})
		return
	}

	rec, err := webhooks.ParsePVE(body, projectId, c.ClientIP(), time.Now().UTC())
	if err != nil {
		p.IncParseErrors()
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Non-blocking enqueue. Overflow returns 503 so PVE retries.
	prev := p.QueueLen()
	p.Enqueue(rec)
	if p.QueueLen() == prev && prev == cap(p.QueueRaw()) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "webhook pipeline queue full"})
		return
	}

	c.Status(http.StatusAccepted)
}
