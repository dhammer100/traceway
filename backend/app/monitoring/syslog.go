package monitoring

import traceway "go.tracewayapp.com"

func RecordSyslogIngest(queueDepth int, received, droppedOverflow, droppedAuth, parseErrors, inserted, failed uint64) {
	traceway.CaptureMetric("traceway.syslog.queue_depth", float64(queueDepth))
	traceway.CaptureMetric("traceway.syslog.received", float64(received))
	traceway.CaptureMetric("traceway.syslog.dropped_overflow", float64(droppedOverflow))
	traceway.CaptureMetric("traceway.syslog.dropped_auth", float64(droppedAuth))
	traceway.CaptureMetric("traceway.syslog.parse_errors", float64(parseErrors))
	traceway.CaptureMetric("traceway.syslog.inserted", float64(inserted))
	traceway.CaptureMetric("traceway.syslog.failed", float64(failed))
}
