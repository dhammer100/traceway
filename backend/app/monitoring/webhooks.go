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
