package webhooks

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tracewayapp/traceway/backend/app/models"
	"github.com/tracewayapp/traceway/backend/app/monitoring"
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
	inserts      chan models.LogRecord
	maxBodyBytes int

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

// Enqueue pushes a record onto the inserts channel non-blockingly. Returns
// true if the record was accepted, false if the channel was full (drop). On
// drop it increments droppedOverflow and fires a rate-limited
// CaptureException so sustained pressure is visible.
func (p *pipeline) Enqueue(rec models.LogRecord) bool {
	p.received.Add(1)
	select {
	case p.inserts <- rec:
		return true
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
	return false
}

// IncParseErrors is called by HTTP handlers when a payload fails validation.
func (p *pipeline) IncParseErrors() { p.parseErrors.Add(1) }

// Start brings up the webhook pipeline singleton. Safe to call twice — the
// second call is a no-op. queueSize and maxBodyBytes are raw env strings; both
// fall back to defaults on blank / invalid input.
func Start(ctx context.Context, queueSize, maxBodyBytes string) {
	if singleton != nil {
		return
	}
	qs := resolveInt(queueSize, defaultQueueSize, 1)
	p := newPipeline(qs)
	p.maxBodyBytes = resolveInt(maxBodyBytes, defaultMaxBodyBytes, 16)
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

// Received returns the count of records that ever entered Enqueue, regardless
// of whether they were accepted or dropped. Exposed for tests.
func (p *pipeline) Received() uint64 { return p.received.Load() }

// ResetForTest clears the singleton. Tests that call Start should defer this.
// Do NOT call from production code.
func ResetForTest() { singleton = nil }

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
