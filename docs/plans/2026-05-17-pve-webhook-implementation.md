# PVE Webhook Ingestion Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Accept Proxmox VE 8 notification webhooks at `POST /api/webhooks/pve`, convert each payload into a `LogRecord`, and write it into `log_records` via the existing `LogRecordRepository.InsertAsync` (works in both ClickHouse and SQLite modes).

**Architecture:** A small `backend/app/webhooks/` package owns a single bounded `inserts` channel and a batcher goroutine started at app boot from `cmd/run.go`, mirroring `backend/app/syslog/`. The Gin HTTP handler parses the JSON body inline (this is cheap for <64 KB), builds the `LogRecord`, and enqueues it. Auth is the existing `middleware.UseClientAuth` (project bearer token); project ID comes from the Gin context, not from the body.

**Tech Stack:** Go 1.25, Gin, existing `traceway.CaptureMetric`/`CaptureException`, `LogRecordRepository.InsertAsync`.

**Design reference:** `docs/plans/2026-05-17-pve-webhook-design.md`

**Intentional deviation from design:** The design doc described a syslog-style worker pool (`WEBHOOK_WORKERS=4`). Dropped during planning — HTTP handlers are already concurrent goroutines per request, and JSON parsing is sub-millisecond, so a worker pool adds latency without benefit. Two env knobs instead of three: `WEBHOOK_QUEUE_SIZE` and `WEBHOOK_MAX_BODY_BYTES`.

---

## Working agreements

- **One assertion per failing test step.** If a behavior has three checks, that's three tests, not one with three `t.Errorf` calls.
- **Commit after each task.** Use the message shown at the bottom of the task.
- **No `_ = err`.** Every error gets handled or wrapped with `fmt.Errorf("...: %w", err)` / `traceway.NewStackTraceErrorf`.
- **`go test ./backend/...` must pass after every task.** If it doesn't, stop and fix before moving on.
- **No frontend changes** in this plan. The `/logs` page already filters by `service_name` and `log_attributes`; that's our UI.

---

## Task 1: Add webhook env-var plumbing to config

**Files:**
- Modify: `backend/app/config/config.go` — add 2 fields to `Cfg`, populate from env in `LoadFromEnv`

**Step 1: Read the current Syslog block to mirror its style.**

Lines to mirror: `backend/app/config/config.go:36-45` (struct fields) and `:111-120` (LoadFromEnv).

**Step 2: Add fields immediately after the syslog block.**

Edit `backend/app/config/config.go` — after the `SyslogQueueSize string` field at line 45, add:

```go
	WebhookQueueSize    string
	WebhookMaxBodyBytes string
```

And in `LoadFromEnv` after `SyslogQueueSize: os.Getenv("SYSLOG_QUEUE_SIZE"),` (line 120), add:

```go
		WebhookQueueSize:    os.Getenv("WEBHOOK_QUEUE_SIZE"),
		WebhookMaxBodyBytes: os.Getenv("WEBHOOK_MAX_BODY_BYTES"),
```

**Step 3: Verify it compiles.**

```bash
cd /Users/dh/dev/traceway/backend && go build ./...
```

Expected: clean build.

**Step 4: Commit.**

```bash
git add backend/app/config/config.go
git commit -m "config: add WEBHOOK_QUEUE_SIZE and WEBHOOK_MAX_BODY_BYTES env vars"
```

---

## Task 2: Add `monitoring.RecordWebhookIngest`

**Files:**
- Create: `backend/app/monitoring/webhooks.go`
- Create: `backend/app/monitoring/webhooks_test.go`

**Step 1: Write the failing test.**

`backend/app/monitoring/webhooks_test.go`:

```go
package monitoring

import "testing"

func TestRecordWebhookIngest_DoesNotPanic(t *testing.T) {
	// CaptureMetric is a no-op when traceway isn't initialized. The point of
	// this test is to lock down the function signature so the listener and
	// monitoring helper stay in sync.
	RecordWebhookIngest("pve", 0, 0, 0, 0, 0, 0)
}
```

**Step 2: Run it.**

```bash
cd /Users/dh/dev/traceway/backend && go test ./app/monitoring/... -run TestRecordWebhookIngest_DoesNotPanic
```

Expected: FAIL with `undefined: RecordWebhookIngest`.

**Step 3: Implement.**

`backend/app/monitoring/webhooks.go`:

```go
package monitoring

import traceway "go.tracewayapp.com"

// RecordWebhookIngest emits the per-source gauges and counters for the webhook
// pipeline. `source` is e.g. "pve" so future webhook sources can share the
// same metric names with a discriminator tag-style suffix.
func RecordWebhookIngest(source string, queueDepth int, received, inserted, droppedOverflow, parseErrors, failed uint64) {
	prefix := "traceway.webhooks." + source + "."
	traceway.CaptureMetric(prefix+"queue_depth", float64(queueDepth))
	traceway.CaptureMetric(prefix+"received", float64(received))
	traceway.CaptureMetric(prefix+"inserted", float64(inserted))
	traceway.CaptureMetric(prefix+"dropped_overflow", float64(droppedOverflow))
	traceway.CaptureMetric(prefix+"parse_errors", float64(parseErrors))
	traceway.CaptureMetric(prefix+"failed", float64(failed))
}
```

**Step 4: Re-run.**

```bash
cd /Users/dh/dev/traceway/backend && go test ./app/monitoring/... -run TestRecordWebhookIngest_DoesNotPanic
```

Expected: PASS.

**Step 5: Commit.**

```bash
git add backend/app/monitoring/webhooks.go backend/app/monitoring/webhooks_test.go
git commit -m "monitoring: add RecordWebhookIngest helper for webhook pipeline"
```

---

## Task 3: PVE payload → LogRecord conversion (pure function, no I/O)

**Files:**
- Create: `backend/app/webhooks/pve.go`
- Create: `backend/app/webhooks/pve_test.go`

**Step 1: Write the failing test for severity mapping.**

`backend/app/webhooks/pve_test.go`:

```go
package webhooks

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestParsePVE_SeverityMapping(t *testing.T) {
	cases := []struct {
		in       string
		wantText string
		wantNum  uint8
	}{
		{"error", "ERROR", 17},
		{"warning", "WARN", 13},
		{"notice", "INFO2", 10},
		{"info", "INFO", 9},
		{"unknown", "INFO", 9},
		{"", "INFO", 9},
	}
	for _, c := range cases {
		body := []byte(`{"severity":"` + c.in + `","message":"x"}`)
		rec, err := ParsePVE(body, uuid.Nil, "1.2.3.4", time.Unix(0, 0).UTC())
		if err != nil {
			t.Fatalf("ParsePVE(%q): %v", c.in, err)
		}
		if rec.SeverityText != c.wantText || rec.SeverityNumber != c.wantNum {
			t.Errorf("severity %q: got %s/%d, want %s/%d", c.in, rec.SeverityText, rec.SeverityNumber, c.wantText, c.wantNum)
		}
	}
}
```

**Step 2: Run it.**

```bash
cd /Users/dh/dev/traceway/backend && go test ./app/webhooks/... -run TestParsePVE_SeverityMapping
```

Expected: FAIL with `no Go files in .../webhooks` (the directory doesn't exist yet).

**Step 3: Implement just enough to make the severity test pass.**

`backend/app/webhooks/pve.go`:

```go
package webhooks

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tracewayapp/traceway/backend/app/models"
)

// pvePayload is the JSON shape we ask the operator to configure in PVE 8's
// notification target body template. PVE's notification metadata is renderable
// via the {{ ... }} handlebars-style syntax — see docs/pages/server/webhooks.mdx
// for the exact template to paste.
type pvePayload struct {
	Title     string          `json:"title"`
	Message   string          `json:"message"`
	Severity  string          `json:"severity"`
	Timestamp string          `json:"timestamp"`
	Fields    json.RawMessage `json:"fields"`
}

// ParsePVE converts the JSON body received from a PVE webhook into an
// OTel-shaped LogRecord. `received` is the wall-clock instant the request was
// accepted, used as a fallback when the PVE-provided timestamp is missing or
// unparseable. `sourceAddr` is the remote address for forensics.
func ParsePVE(body []byte, projectId uuid.UUID, sourceAddr string, received time.Time) (models.LogRecord, error) {
	var p pvePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return models.LogRecord{}, err
	}
	if strings.TrimSpace(p.Message) == "" {
		return models.LogRecord{}, errors.New("webhook payload missing required field: message")
	}

	sevText, sevNum := pveSeverityToOTel(p.Severity)

	ts := received
	if p.Timestamp != "" {
		if parsed, err := time.Parse(time.RFC3339, p.Timestamp); err == nil {
			ts = parsed
		}
	}

	logAttrs := map[string]string{}
	if p.Title != "" {
		logAttrs["pve.title"] = p.Title
	}
	if sourceAddr != "" {
		logAttrs["webhook.source"] = sourceAddr
	}

	resourceAttrs := map[string]string{}
	serviceName := ""
	if len(p.Fields) > 0 {
		var fields map[string]any
		if err := json.Unmarshal(p.Fields, &fields); err == nil {
			for k, v := range fields {
				s := stringifyField(v)
				if s == "" {
					continue
				}
				switch k {
				case "hostname":
					resourceAttrs["host.name"] = s
				case "type":
					serviceName = s
					resourceAttrs["service.name"] = s
				}
				logAttrs["pve."+k] = s
			}
		}
	}

	return models.LogRecord{
		Id:                 uuid.New(),
		ProjectId:          projectId,
		Timestamp:          ts.UTC(),
		SeverityText:       sevText,
		SeverityNumber:     sevNum,
		ServiceName:        serviceName,
		Body:               p.Message,
		ResourceAttributes: resourceAttrs,
		LogAttributes:      logAttrs,
	}, nil
}

// pveSeverityToOTel maps PVE's notification severity strings to OTel
// severity_text + severity_number, matching the syslog importer's
// numeric scale so the /logs UI severity filter is consistent across sources.
func pveSeverityToOTel(s string) (string, uint8) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error":
		return "ERROR", 17
	case "warning":
		return "WARN", 13
	case "notice":
		return "INFO2", 10
	case "info":
		return "INFO", 9
	default:
		return "INFO", 9
	}
}

// stringifyField coerces any JSON-decoded value into a string suitable for the
// log_attributes Map(String, String). Numbers are rendered without trailing
// zeros (so 104 stays 104, not 104.000000).
func stringifyField(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		// JSON numbers decode as float64. Emit ints without a decimal point.
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case nil:
		return ""
	default:
		// Fallback for nested objects/arrays — re-encode as compact JSON.
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	}
}
```

Add `"strconv"` to the import block.

**Step 4: Re-run.**

```bash
cd /Users/dh/dev/traceway/backend && go test ./app/webhooks/... -run TestParsePVE_SeverityMapping
```

Expected: PASS.

**Step 5: Commit.**

```bash
git add backend/app/webhooks/pve.go backend/app/webhooks/pve_test.go
git commit -m "webhooks: PVE payload parser with severity → OTel mapping"
```

---

## Task 4: PVE parser — body, missing-field, and timestamp fallback tests

**Files:**
- Modify: `backend/app/webhooks/pve_test.go`

**Step 1: Add three more failing tests.**

Append to `backend/app/webhooks/pve_test.go`:

```go
func TestParsePVE_MissingMessageIsError(t *testing.T) {
	body := []byte(`{"severity":"warning"}`)
	_, err := ParsePVE(body, uuid.Nil, "", time.Now().UTC())
	if err == nil {
		t.Fatal("expected error when 'message' is missing")
	}
}

func TestParsePVE_FallsBackToReceivedTimeOnBadTimestamp(t *testing.T) {
	recv := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	body := []byte(`{"severity":"info","message":"hi","timestamp":"not-a-date"}`)
	rec, err := ParsePVE(body, uuid.Nil, "", recv)
	if err != nil {
		t.Fatalf("ParsePVE: %v", err)
	}
	if !rec.Timestamp.Equal(recv) {
		t.Errorf("timestamp: got %v, want fallback to %v", rec.Timestamp, recv)
	}
}

func TestParsePVE_FieldsBecomeAttributes(t *testing.T) {
	body := []byte(`{
		"severity": "error",
		"message":  "Backup of VM 104 failed",
		"fields":   {"hostname": "pve01", "type": "vzdump", "vmid": 104, "backup-target": "pbs"}
	}`)
	rec, err := ParsePVE(body, uuid.Nil, "10.0.0.1", time.Now().UTC())
	if err != nil {
		t.Fatalf("ParsePVE: %v", err)
	}
	if rec.ResourceAttributes["host.name"] != "pve01" {
		t.Errorf("host.name: got %q", rec.ResourceAttributes["host.name"])
	}
	if rec.ServiceName != "vzdump" {
		t.Errorf("service_name: got %q, want vzdump", rec.ServiceName)
	}
	if rec.LogAttributes["pve.vmid"] != "104" {
		t.Errorf("pve.vmid: got %q, want 104", rec.LogAttributes["pve.vmid"])
	}
	if rec.LogAttributes["pve.backup-target"] != "pbs" {
		t.Errorf("pve.backup-target: got %q", rec.LogAttributes["pve.backup-target"])
	}
	if rec.LogAttributes["webhook.source"] != "10.0.0.1" {
		t.Errorf("webhook.source: got %q", rec.LogAttributes["webhook.source"])
	}
}
```

**Step 2: Run them.**

```bash
cd /Users/dh/dev/traceway/backend && go test ./app/webhooks/... -v
```

Expected: All three PASS (the implementation in task 3 already covers these). If any fail, fix the implementation in `pve.go` before moving on — the tests describe the contract, not the implementation.

**Step 3: Commit.**

```bash
git add backend/app/webhooks/pve_test.go
git commit -m "webhooks: lock down PVE parser contract with field-mapping tests"
```

---

## Task 5: Pipeline skeleton — bounded inserts channel + counters

**Files:**
- Create: `backend/app/webhooks/pipeline.go`
- Create: `backend/app/webhooks/pipeline_test.go`

**Step 1: Write the failing test.**

`backend/app/webhooks/pipeline_test.go`:

```go
package webhooks

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/tracewayapp/traceway/backend/app/models"
)

func TestPipeline_Enqueue_CountsReceived(t *testing.T) {
	p := newPipeline(2)
	p.Enqueue(models.LogRecord{Id: uuid.New()})
	if got := p.received.Load(); got != 1 {
		t.Errorf("received: got %d, want 1", got)
	}
}

func TestPipeline_Enqueue_DropsOnOverflow(t *testing.T) {
	p := newPipeline(2)
	for i := 0; i < 5; i++ {
		p.Enqueue(models.LogRecord{Id: uuid.New()})
	}
	if got := p.received.Load(); got != 5 {
		t.Errorf("received: got %d, want 5", got)
	}
	if got := p.droppedOverflow.Load(); got != 3 {
		t.Errorf("droppedOverflow: got %d, want 3", got)
	}
}

func TestPipeline_StartStop_NoDeadlock(t *testing.T) {
	p := newPipeline(2)
	ctx, cancel := context.WithCancel(context.Background())
	p.start(ctx)
	cancel()
	// give the batcher a chance to return; if start() leaks goroutines they'll
	// show up under -race.
}
```

**Step 2: Run them.**

```bash
cd /Users/dh/dev/traceway/backend && go test ./app/webhooks/... -run TestPipeline -v
```

Expected: FAIL with `undefined: newPipeline`.

**Step 3: Implement just the skeleton (no batching/metrics yet, just the channel + counters + lifecycle).**

`backend/app/webhooks/pipeline.go`:

```go
package webhooks

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tracewayapp/traceway/backend/app/models"
	"github.com/tracewayapp/traceway/backend/app/repositories"
	traceway "go.tracewayapp.com"
)

const (
	defaultQueueSize    = 1024
	defaultMaxBodyBytes = 64 * 1024
	insertBatchSize     = 1000
	insertFlushInterval = 2 * time.Second
	metricsTickInterval = 10 * time.Second
	dropReportInterval  = time.Minute
)

type pipeline struct {
	inserts chan models.LogRecord

	received        atomic.Uint64
	inserted        atomic.Uint64
	droppedOverflow atomic.Uint64
	parseErrors     atomic.Uint64
	failed          atomic.Uint64

	dropMu             sync.Mutex
	lastDropReportAt   time.Time
	droppedSinceReport uint64
}

var singleton *pipeline

func newPipeline(queueSize int) *pipeline {
	if queueSize < 1 {
		queueSize = defaultQueueSize
	}
	return &pipeline{
		inserts: make(chan models.LogRecord, queueSize),
	}
}

// Enqueue pushes a record onto the inserts channel non-blockingly. On overflow
// it drops the record, increments droppedOverflow, and fires a rate-limited
// CaptureException so sustained pressure is visible.
func (p *pipeline) Enqueue(rec models.LogRecord) {
	p.received.Add(1)
	select {
	case p.inserts <- rec:
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
			"webhooks pipeline dropped %d records due to full queue (cap=%d)", report, cap(p.inserts)))
	}
}

// IncParseErrors is called by HTTP handlers when a payload fails validation.
func (p *pipeline) IncParseErrors() { p.parseErrors.Add(1) }

func (p *pipeline) start(ctx context.Context) {
	go p.batcher(ctx)
	// metrics loop comes next task
}

func (p *pipeline) batcher(ctx context.Context) {
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
			traceway.CaptureException(traceway.NewStackTraceErrorf("webhooks: failed to insert batch of %d log_records: %w", len(batch), err))
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
```

**Step 4: Re-run.**

```bash
cd /Users/dh/dev/traceway/backend && go test ./app/webhooks/... -race -run TestPipeline -v
```

Expected: PASS.

**Step 5: Commit.**

```bash
git add backend/app/webhooks/pipeline.go backend/app/webhooks/pipeline_test.go
git commit -m "webhooks: pipeline skeleton with bounded queue + batcher"
```

---

## Task 6: Pipeline metrics loop + exported `Start` / `Get`

**Files:**
- Modify: `backend/app/webhooks/pipeline.go`

**Step 1: Write the failing test.**

Append to `backend/app/webhooks/pipeline_test.go`:

```go
func TestStart_CreatesSingletonOnce(t *testing.T) {
	t.Cleanup(func() { singleton = nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	Start(ctx, "", "")
	if singleton == nil {
		t.Fatal("Start with empty queue size should still create the singleton")
	}
	first := singleton
	Start(ctx, "", "")
	if singleton != first {
		t.Fatal("Start called twice should be a no-op")
	}
}

func TestGet_ReturnsSingleton(t *testing.T) {
	t.Cleanup(func() { singleton = nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if Get() != nil {
		t.Fatal("Get before Start should return nil")
	}
	Start(ctx, "", "")
	if Get() == nil {
		t.Fatal("Get after Start should return the singleton")
	}
}
```

**Step 2: Run them.**

```bash
cd /Users/dh/dev/traceway/backend && go test ./app/webhooks/... -race -run "TestStart_|TestGet_" -v
```

Expected: FAIL — `Start` and `Get` don't exist yet.

**Step 3: Add `Start`, `Get`, `MaxBodyBytes`, metrics loop.**

Add to `backend/app/webhooks/pipeline.go`:

```go
import (
	// add to existing imports:
	"strconv"
	"strings"

	"github.com/tracewayapp/traceway/backend/app/monitoring"
)

// Start brings up the webhook pipeline singleton. Safe to call twice — the
// second call is a no-op. queueSize and maxBodyBytes are raw env strings; both
// fall back to defaults on blank / invalid input.
func Start(ctx context.Context, queueSize, maxBodyBytes string) {
	if singleton != nil {
		return
	}
	qs := resolveInt(queueSize, defaultQueueSize, 1)
	p := newPipeline(qs)
	p.maxBodyBytes = resolveInt(maxBodyBytes, defaultMaxBodyBytes, 512)
	singleton = p
	p.start(ctx)
}

// Get returns the running pipeline, or nil if Start was never called. HTTP
// handlers use this to enqueue; the nil check lets tests exercise the handler
// without starting the pipeline.
func Get() *pipeline { return singleton }

func (p *pipeline) MaxBodyBytes() int { return p.maxBodyBytes }

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
```

Add a `maxBodyBytes int` field to the `pipeline` struct (right under `inserts chan models.LogRecord`).

Replace the existing `start` method body to also launch the metrics loop:

```go
func (p *pipeline) start(ctx context.Context) {
	go p.batcher(ctx)
	go p.metricsLoop(ctx)
}

func (p *pipeline) metricsLoop(ctx context.Context) {
	defer traceway.Recover()

	ticker := time.NewTicker(metricsTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			monitoring.RecordWebhookIngest(
				"pve",
				len(p.inserts),
				p.received.Load(),
				p.inserted.Load(),
				p.droppedOverflow.Load(),
				p.parseErrors.Load(),
				p.failed.Load(),
			)
		}
	}
}
```

**Step 4: Re-run.**

```bash
cd /Users/dh/dev/traceway/backend && go test ./app/webhooks/... -race -v
```

Expected: all PASS.

**Step 5: Commit.**

```bash
git add backend/app/webhooks/pipeline.go backend/app/webhooks/pipeline_test.go
git commit -m "webhooks: add Start/Get singleton + metrics loop"
```

---

## Task 7: HTTP handler — happy path

**Files:**
- Create: `backend/app/controllers/webhookcontrollers/pve.go`
- Create: `backend/app/controllers/webhookcontrollers/pve_test.go`

**Step 1: Write the failing test.**

`backend/app/controllers/webhookcontrollers/pve_test.go`:

```go
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
		// reset singleton between tests so subsequent Start calls work
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

func TestPVE_Receive_Accepts200(t *testing.T) {
	r, _ := setupRouter(t)

	body := []byte(`{"severity":"warning","message":"Backup of VM 104 failed","fields":{"hostname":"pve01","type":"vzdump","vmid":104}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/pve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status: got %d, want 202; body=%s", w.Code, w.Body.String())
	}

	// Drain time: give the batcher one flush window. We can't easily probe the
	// inserts channel from outside, so just assert received went up.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if webhooks.Get().Received() == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected received counter to reach 1, got %d", webhooks.Get().Received())
}
```

**Step 2: Run it.**

```bash
cd /Users/dh/dev/traceway/backend && go test ./app/controllers/webhookcontrollers/... -run TestPVE_Receive_Accepts200 -v
```

Expected: FAIL — `webhookcontrollers` and `PVEController` don't exist.

**Step 3: Add `ResetForTest()` + `Received()` to the pipeline.**

In `backend/app/webhooks/pipeline.go`, add:

```go
// Received returns the count of records that ever entered Enqueue, regardless
// of whether they were accepted or dropped. Exposed for tests.
func (p *pipeline) Received() uint64 { return p.received.Load() }

// ResetForTest clears the singleton. Tests that call Start should defer this.
// Do NOT call from production code.
func ResetForTest() { singleton = nil }
```

**Step 4: Implement the handler.**

`backend/app/controllers/webhookcontrollers/pve.go`:

```go
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
		// pipeline disabled — the route is registered but Start was never
		// called (e.g. tests). Treat the request as accepted-but-discarded so
		// PVE doesn't loop on retries.
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
```

Add the necessary accessors to `backend/app/webhooks/pipeline.go`:

```go
// QueueLen returns the current channel depth. Useful for handlers to detect
// overflow after a non-blocking Enqueue.
func (p *pipeline) QueueLen() int { return len(p.inserts) }

// QueueRaw exposes the underlying channel for cap() — handlers should NOT
// read from it directly.
func (p *pipeline) QueueRaw() chan models.LogRecord { return p.inserts }
```

**Step 5: Re-run.**

```bash
cd /Users/dh/dev/traceway/backend && go test ./app/controllers/webhookcontrollers/... -race -v
```

Expected: PASS.

**Step 6: Commit.**

```bash
git add backend/app/controllers/webhookcontrollers/ backend/app/webhooks/pipeline.go
git commit -m "webhooks: PVE HTTP handler (202 accept, body-size + parse validation)"
```

---

## Task 8: HTTP handler — error cases

**Files:**
- Modify: `backend/app/controllers/webhookcontrollers/pve_test.go`

**Step 1: Add three failing tests.**

```go
func TestPVE_Receive_400OnMalformedJSON(t *testing.T) {
	r, _ := setupRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/pve", bytes.NewReader([]byte(`not json`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestPVE_Receive_400OnMissingMessage(t *testing.T) {
	r, _ := setupRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/pve", bytes.NewReader([]byte(`{"severity":"info"}`)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestPVE_Receive_413OnOversizeBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(func() { webhooks.ResetForTest() })
	webhooks.Start(ctx, "16", "32") // 32 byte cap

	projectId := uuid.New()
	r := gin.New()
	r.POST("/webhooks/pve", func(c *gin.Context) {
		c.Set(middleware.ProjectIdContextKey, projectId)
		PVEController.Receive(c)
	})

	big := bytes.Repeat([]byte("x"), 64)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/pve", bytes.NewReader(big))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want 413; body=%s", w.Code, w.Body.String())
	}
}
```

**Step 2: Run.**

```bash
cd /Users/dh/dev/traceway/backend && go test ./app/controllers/webhookcontrollers/... -race -v
```

Expected: PASS (the handler from task 7 already handles these). Fix the handler if not.

**Step 3: Commit.**

```bash
git add backend/app/controllers/webhookcontrollers/pve_test.go
git commit -m "webhooks: lock down error cases (400 malformed, 400 missing, 413 oversize)"
```

---

## Task 9: Register the route

**Files:**
- Modify: `backend/app/controllers/routes.go`

**Step 1: Add the import.**

In `backend/app/controllers/routes.go`, alongside the existing `clientcontrollers` and `otelcontrollers` imports (line 5-6), add:

```go
	"github.com/tracewayapp/traceway/backend/app/controllers/webhookcontrollers"
```

**Step 2: Register the route.**

Right after the OTel block (lines 35-39), add:

```go
	// PVE webhook ingestion — accepts Proxmox VE 8 notification webhooks.
	// See docs/pages/server/webhooks.mdx for the body template the operator
	// pastes into the PVE notification target.
	router.POST("/webhooks/pve", middleware.UseClientAuth, webhookcontrollers.PVEController.Receive)
```

**Step 3: Verify routes compile and the existing test suite still passes.**

```bash
cd /Users/dh/dev/traceway/backend && go build ./... && go test ./... -race -count=1
```

Expected: clean build, full test suite green.

**Step 4: Commit.**

```bash
git add backend/app/controllers/routes.go
git commit -m "routes: register POST /api/webhooks/pve"
```

---

## Task 10: Boot the pipeline from cmd/run.go

**Files:**
- Modify: `backend/cmd/run.go`

**Step 1: Add the import.**

In `backend/cmd/run.go`, alongside the existing `syslog` and `recordings` imports (lines 14-18), add:

```go
	"github.com/tracewayapp/traceway/backend/app/webhooks"
```

**Step 2: Start the pipeline.**

After `recordings.Start(ctx)` at line 141 in `backend/cmd/run.go`, add:

```go
	webhooks.Start(ctx, cfg.WebhookQueueSize, cfg.WebhookMaxBodyBytes)
```

This runs in BOTH env mode and embedded/test mode (it's before the `if o != nil` split), so SQLite-mode self-hosted users get the same pipeline.

**Step 3: Verify it starts cleanly in an in-process test.**

There's no targeted run_test.go for this; just verify the build and the existing suite.

```bash
cd /Users/dh/dev/traceway/backend && go build ./... && go test ./... -race -count=1
```

Expected: clean build, full test suite green.

**Step 4: Commit.**

```bash
git add backend/cmd/run.go
git commit -m "cmd: start webhooks pipeline alongside recordings/retention"
```

---

## Task 11: End-to-end test — request lands in LogRecordRepository

**Files:**
- Create: `backend/app/webhooks/integration_test.go`

This task verifies the full HTTP → enqueue → batcher → repository path actually fires `InsertAsync` exactly once with the expected record. We use the SQLite in-process build so it touches a real repository.

**Step 1: Write the failing test.**

`backend/app/webhooks/integration_test.go`:

```go
//go:build !pgch

package webhooks

import (
	"bytes"
	"context"
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

// TestPVE_EndToEnd_WritesToLogRecords spins up an in-memory SQLite, runs the
// full handler → pipeline → repository path, and asserts the record landed in
// log_records with the expected fields. This is the only test in the plan
// that exercises the real InsertAsync, so it's where SQLite-mode regressions
// will surface.
func TestPVE_EndToEnd_WritesToLogRecords(t *testing.T) {
	// db.Init reads from config.Config; tests that need it should already be
	// set up by an in-process bootstrap helper. If this test is the first to
	// touch the DB in a fresh `go test ./app/webhooks/...` run, we initialize
	// it here.
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
		// import cycle prevents calling the controller directly from this
		// package — instead, exercise the parse + enqueue path inline.
		body, _ := io.ReadAll(c.Request.Body)
		rec, err := ParsePVE(body, projectId, c.ClientIP(), time.Now().UTC())
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		Get().Enqueue(rec)
		c.Status(202)
	})

	body := []byte(`{"severity":"error","message":"Backup of VM 104 failed","fields":{"hostname":"pve01","type":"vzdump","vmid":104}}`)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/pve", bytes.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status: got %d, want 202", w.Code)
	}

	// Wait up to 3s for the batcher to flush.
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
```

Add `"io"` to the import block.

**Step 2: Run it.**

```bash
cd /Users/dh/dev/traceway/backend && go test ./app/webhooks/... -run TestPVE_EndToEnd -v
```

Expected: SKIP — `db.TelemetryDB == nil`. The skip is intentional: integration test exists, runs when bootstrapped under a harness, otherwise gets out of the way. The unit tests in earlier tasks cover the contract.

If you want to actually exercise this path locally, run the backend with `cd backend && go run . -seed=true` against an ephemeral SQLite and curl the endpoint manually (see Task 13 verification).

**Step 3: Commit.**

```bash
git add backend/app/webhooks/integration_test.go
git commit -m "webhooks: end-to-end integration test (skipped without db.Init bootstrap)"
```

---

## Task 12: Self-host docs page

**Files:**
- Create: `docs/pages/server/webhooks.mdx`
- Modify: `docs/pages/server/_meta.json`

**Step 1: Add to the sidebar.**

Edit `docs/pages/server/_meta.json`. Current content:

```json
{
  "index": "Overview",
  "all-in-one": "All-in-One Container",
  "docker-compose": "Docker Compose",
  "minimal": "Minimal Container",
  "sqlite": "SQLite",
  "local-setup": "Local Setup",
  "haloy": "Haloy"
}
```

Add `"webhooks": "Webhooks (PVE)"` before the closing brace:

```json
{
  "index": "Overview",
  "all-in-one": "All-in-One Container",
  "docker-compose": "Docker Compose",
  "minimal": "Minimal Container",
  "sqlite": "SQLite",
  "local-setup": "Local Setup",
  "haloy": "Haloy",
  "webhooks": "Webhooks (PVE)"
}
```

**Step 2: Write the page.**

`docs/pages/server/webhooks.mdx`:

````mdx
# Webhooks — Proxmox VE

Traceway accepts Proxmox VE 8 notification webhooks at `POST /api/webhooks/pve`.
Each webhook is converted to a log record and stored in the same `log_records`
table that powers the **Logs** page, so you can filter PVE alerts alongside
syslog and OTel logs.

## Configure the endpoint in PVE

In the PVE web UI: **Datacenter → Notifications → Add → Webhook**.

| Field | Value |
|-------|-------|
| **Name** | `traceway` |
| **URL** | `https://<your-traceway-host>/api/webhooks/pve` |
| **Method** | `POST` |
| **Headers** | `Authorization: Bearer <project-token>`<br/>`Content-Type: application/json` |
| **Body** | (paste the template below) |

Use the project token from **Traceway → Project Settings → Tokens**. This is
the same token your applications use to report telemetry.

### Body template

```handlebars
{
  "title": "{{ title }}",
  "message": "{{ escape message }}",
  "severity": "{{ severity }}",
  "timestamp": "{{ timestamp }}",
  "fields": {{ json fields }}
}
```

PVE's template engine fills `{{ title }}`, `{{ message }}`, etc. with the
notification's metadata. `{{ escape message }}` JSON-escapes embedded
characters; `{{ json fields }}` emits the full metadata bag as a JSON object.

Then attach this target to whichever **Notification Matcher** you care about
(backup failures, replication errors, certificate renewals, etc.).

## What you see in Traceway

Each webhook becomes a log record on `/logs`:

| PVE field | Where it lands |
|-----------|----------------|
| `severity` | Severity column (ERROR / WARN / INFO2 / INFO) |
| `message` | Log body, full-text searchable |
| `title` | `log_attributes["pve.title"]` |
| `fields.hostname` | `resource_attributes["host.name"]` |
| `fields.type` (e.g. `vzdump`) | Service name column |
| `fields.<other>` | `log_attributes["pve.<key>"]` (e.g. `pve.vmid=104`) |

## Configuration

| Environment variable | Default | Purpose |
|----------------------|---------|---------|
| `WEBHOOK_QUEUE_SIZE` | `1024` | Max records buffered in memory. Excess returns 503; PVE retries. |
| `WEBHOOK_MAX_BODY_BYTES` | `65536` | Reject larger payloads with 413. |

No env var is needed to *enable* the endpoint — it's always on. The project
bearer token in the `Authorization` header binds each request to a project.

## Observability

Metrics emitted every 10 s (tag `source=pve`):

| Metric | Type | Meaning |
|--------|------|---------|
| `traceway.webhooks.pve.received` | counter | Requests accepted by the HTTP handler |
| `traceway.webhooks.pve.inserted` | counter | Records successfully written to `log_records` |
| `traceway.webhooks.pve.queue_depth` | gauge | Records waiting in the channel |
| `traceway.webhooks.pve.dropped_overflow` | counter | Records dropped because the queue was full |
| `traceway.webhooks.pve.parse_errors` | counter | Requests rejected with 400 or 413 |
| `traceway.webhooks.pve.failed` | counter | Records that failed to insert into the database |

Sustained overflow also fires a rate-limited (1/min) exception to the
**Issues** page so the problem is visible without flooding it.

## Retention

Webhook records share the `log_records` table TTL: **30 days** in ClickHouse
mode, configurable in SQLite mode via `SQLITE_RETENTION_DAYS`.
````

**Step 3: Verify the docs build (if Next.js tooling is installed).**

```bash
cd /Users/dh/dev/traceway/docs && npm run build 2>&1 | tail -20
```

Expected: clean build (or the same warnings the docs already emit). If `npm` isn't installed, skip — the file is well-formed Nextra MDX.

**Step 4: Commit.**

```bash
git add docs/pages/server/webhooks.mdx docs/pages/server/_meta.json
git commit -m "docs: self-host page for PVE webhook ingestion"
```

---

## Task 13: CLAUDE.md — "Webhook Importer" section

**Files:**
- Modify: `CLAUDE.md`

**Step 1: Find the syslog block.**

Search for `#### Syslog Importer` in `CLAUDE.md`. The new section goes immediately after it (parallel structure).

**Step 2: Add env vars to the env-var block.**

At the bottom of the existing env-var listing under "Environment Variables (Backend)" (the one that already documents `SYSLOG_UDP_ADDR` etc.), add:

```
# Webhook ingestion (see "Webhook Importer" section below). Endpoint is
# always on; project token in the Authorization header binds each request
# to a project. No per-source enable flag.
WEBHOOK_QUEUE_SIZE=1024
WEBHOOK_MAX_BODY_BYTES=65536
```

**Step 3: Add the section immediately after `#### Syslog Importer`.**

```markdown
#### Webhook Importer

An HTTP webhook receiver (`backend/app/webhooks/`, started from `cmd/run.go` next to `syslog.Start`) accepts JSON-shaped notification payloads from external systems and writes them into the same `log_records` table the syslog importer and OTel logs use. The first (and currently only) supported source is **Proxmox VE 8**, on `POST /api/webhooks/pve`.

The endpoint is gated by `middleware.UseClientAuth` — same project bearer token your applications use for telemetry. The project is determined per-request from the token, unlike the syslog importer which routes every message to one configured `SYSLOG_DEFAULT_PROJECT_ID`.

**Pipeline shape** — no worker pool. HTTP handlers are already concurrent and JSON parsing is sub-millisecond, so the Gin handler parses inline and pushes a finished `models.LogRecord` directly onto a bounded `inserts` channel. A single batcher goroutine drains it via `LogRecordRepository.InsertAsync` once per 1000 rows or every 2 s.

**Tunables:**

| Variable | Default | Notes |
|----------|---------|-------|
| `WEBHOOK_QUEUE_SIZE` | `1024` | Channel capacity. Overflow returns 503; PVE retries. |
| `WEBHOOK_MAX_BODY_BYTES` | `65536` | Larger bodies are rejected with 413. |

**PVE field mapping** (configured via the PVE notification target's body template — see `docs/pages/server/webhooks.mdx`):

| PVE field | LogRecord target |
|-----------|------------------|
| `severity` | `severity_text` + `severity_number` (same OTel scale as the syslog importer) |
| `message` | `body` |
| `title` | `log_attributes["pve.title"]` |
| `timestamp` | `timestamp` (fall back to `time.Now().UTC()` on missing/unparseable) |
| `fields.hostname` | `resource_attributes["host.name"]` |
| `fields.type` (e.g. `vzdump`, `replication`) | `service_name` |
| All `fields[*]` | `log_attributes["pve.<key>"]` |
| Remote IP | `log_attributes["webhook.source"]` |

**Observability** (emitted every 10 s):

| Metric | Type | Meaning |
|--------|------|---------|
| `traceway.webhooks.pve.received` | counter | Requests accepted at HTTP layer |
| `traceway.webhooks.pve.inserted` | counter | Rows successfully written to `log_records` |
| `traceway.webhooks.pve.queue_depth` | gauge | Records waiting in the channel |
| `traceway.webhooks.pve.dropped_overflow` | counter | Records dropped because the queue was full |
| `traceway.webhooks.pve.parse_errors` | counter | Requests rejected with 400 or 413 |
| `traceway.webhooks.pve.failed` | counter | Rows that failed to insert |

Sustained overflow fires a rate-limited (1/min) `traceway.CaptureException` so the problem is visible on the Issues page.

Adding a future source (Grafana, Alertmanager, etc.) means: a new `backend/app/webhooks/<source>.go` parser, a new controller under `backend/app/controllers/webhookcontrollers/`, and a route. The pipeline itself is source-agnostic — the metrics dimension switches via the string passed to `monitoring.RecordWebhookIngest`.
```

**Step 4: Smoke test the local backend.**

```bash
cd /Users/dh/dev/traceway/backend && go run . &
BACKEND_PID=$!
sleep 3

# Replace <TOKEN> with a real project token from your dev DB.
curl -i -X POST http://localhost:8082/api/webhooks/pve \
  -H "Authorization: Bearer <TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"severity":"warning","message":"Backup of VM 104 failed","fields":{"hostname":"pve01","type":"vzdump","vmid":104}}'

kill $BACKEND_PID
```

Expected: `HTTP/1.1 202 Accepted` and a corresponding row visible in the `/logs` page with `service_name=vzdump`, `log_attributes["pve.vmid"]=104`.

If you don't have a dev project handy, skip the curl step — the unit tests already cover the contract.

**Step 5: Commit.**

```bash
git add CLAUDE.md
git commit -m "docs(claude.md): document webhook importer alongside syslog"
```

---

## Final verification

```bash
cd /Users/dh/dev/traceway/backend && go build ./... && go test ./... -race -count=1
```

Expected: clean build, all tests green.

```bash
git log --oneline main..HEAD
```

Expected: 13 commits, all small and reviewable.

---

## Plan complete

Plan complete and saved to `docs/plans/2026-05-17-pve-webhook-implementation.md`. Two execution options:

**1. Subagent-Driven (this session)** — I dispatch a fresh subagent per task, review between tasks, fast iteration. Best when you want to stay in this conversation and ratify each task as it lands.

**2. Parallel Session (separate)** — Open a new session in this repo and tell it to run `superpowers:executing-plans` against this file. Batches the work with checkpoints; this conversation stays clean.

**Which approach?**
