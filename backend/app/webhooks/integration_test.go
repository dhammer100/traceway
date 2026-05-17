//go:build !pgch

package webhooks

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/tracewayapp/traceway/backend/app/db"
	"github.com/tracewayapp/traceway/backend/app/middleware"
	"github.com/tracewayapp/traceway/backend/app/models"
	"github.com/tracewayapp/traceway/backend/app/repositories"
)

// TestPVE_EndToEnd_WritesToLogRecords spins up the full handler → pipeline →
// repository path on an in-memory SQLite (when the harness has bootstrapped
// db.Init) and asserts the record lands in log_records with the expected
// fields. Skipped without the bootstrap — the unit tests cover the contract;
// this one only fires under an integration harness.
func TestPVE_EndToEnd_WritesToLogRecords(t *testing.T) {
	if db.TelemetryDB == nil {
		t.Skip("requires db.Init() bootstrap — run via cmd.Run() integration harness")
	}

	t.Cleanup(func() { ResetForTest() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	Start(ctx, "16", "65536")

	projectId := uuid.New()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/webhooks/pve", func(c *gin.Context) {
		c.Set(middleware.ProjectIdContextKey, projectId)
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		rec, err := ParsePVE(body, projectId, c.ClientIP(), time.Now().UTC())
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !Get().Enqueue(rec) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "queue full"})
			return
		}
		c.Status(http.StatusAccepted)
	})

	body := []byte(`{"severity":"error","message":"Backup of VM 104 failed","fields":{"hostname":"pve01","type":"vzdump","vmid":104}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/pve", bytes.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status: got %d, want 202", w.Code)
	}

	// Wait up to 3 s for the batcher to flush.
	deadline := time.Now().Add(3 * time.Second)
	var found []models.LogRecord
	for time.Now().Before(deadline) {
		recs, _, err := repositories.LogRecordRepository.Search(ctx, repositories.LogSearchParams{
			ProjectId: projectId,
			FromDate:  time.Now().UTC().Add(-time.Minute),
			ToDate:    time.Now().UTC().Add(time.Minute),
			PageSize:  10,
		})
		if err == nil && len(recs) > 0 {
			found = recs
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if len(found) != 1 {
		t.Fatalf("expected exactly 1 record in log_records, got %d", len(found))
	}
	if found[0].SeverityText != "ERROR" {
		t.Errorf("severity_text: got %q, want ERROR", found[0].SeverityText)
	}
	if found[0].ServiceName != "vzdump" {
		t.Errorf("service_name: got %q, want vzdump", found[0].ServiceName)
	}
	if found[0].LogAttributes["pve.vmid"] != "104" {
		t.Errorf("pve.vmid: got %q", found[0].LogAttributes["pve.vmid"])
	}
}
