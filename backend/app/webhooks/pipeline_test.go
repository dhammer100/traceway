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
