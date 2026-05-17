package monitoring

import "testing"

func TestRecordWebhookIngest_DoesNotPanic(t *testing.T) {
	// CaptureMetric is a no-op when traceway isn't initialized. The point of
	// this test is to lock down the function signature so the listener and
	// monitoring helper stay in sync.
	RecordWebhookIngest("pve", 0, 0, 0, 0, 0, 0)
}
