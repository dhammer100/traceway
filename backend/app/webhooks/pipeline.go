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
	// metrics loop comes in Task 6
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
