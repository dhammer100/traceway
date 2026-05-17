package webhookcontrollers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/tracewayapp/traceway/backend/app/middleware"
	"github.com/tracewayapp/traceway/backend/app/webhooks"
)

func setupRouter(t *testing.T) (*gin.Engine, uuid.UUID) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(func() {
		webhooks.ResetForTest()
	})
	webhooks.Start(ctx, "16", "65536")

	projectId := uuid.New()
	r := gin.New()
	r.POST("/webhooks/pve", func(c *gin.Context) {
		c.Set(middleware.ProjectIdContextKey, projectId)
		PVEController.Receive(c)
	})
	return r, projectId
}

func TestPVE_Receive_Accepts202(t *testing.T) {
	r, _ := setupRouter(t)

	body := []byte(`{"severity":"warning","message":"Backup of VM 104 failed","fields":{"hostname":"pve01","type":"vzdump","vmid":104}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/pve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status: got %d, want 202; body=%s", w.Code, w.Body.String())
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if webhooks.Get().Received() == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected received counter to reach 1, got %d", webhooks.Get().Received())
}
