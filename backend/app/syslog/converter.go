package syslog

import (
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/tracewayapp/traceway/backend/app/models"
)

// ToLogRecord converts a parsed syslog Message into an OTel-shaped LogRecord
// ready for insertion into log_records. The projectId comes from the listener
// (default project for v1) and sourceAddr is the remote network address the
// message arrived from, stored as a log attribute for forensics.
func ToLogRecord(m *Message, projectId uuid.UUID, sourceAddr string, received time.Time) models.LogRecord {
	// RFC 3164 timestamps carry no timezone or year, so devices in different
	// timezones land in the wrong part of the timeline. Use receive time for
	// 3164; keep the device-reported value as an attribute for diagnostics.
	// RFC 5424 timestamps are timezone-qualified and trustworthy.
	ts := received
	useDeviceTime := m.Version >= 1 && !m.Timestamp.IsZero()
	if useDeviceTime {
		ts = m.Timestamp
	}

	sevText, sevNum := severityToOTel(m.Severity)

	resourceAttrs := map[string]string{}
	if m.Hostname != "" {
		resourceAttrs["host.name"] = m.Hostname
	}

	serviceName := m.AppName
	if serviceName != "" {
		resourceAttrs["service.name"] = serviceName
	}

	logAttrs := map[string]string{
		"syslog.facility":      strconv.Itoa(m.Facility),
		"syslog.facility.name": facilityName(m.Facility),
		"syslog.severity":      strconv.Itoa(m.Severity),
		"syslog.severity.name": syslogSeverityName(m.Severity),
	}
	if m.Version > 0 {
		logAttrs["syslog.version"] = strconv.Itoa(m.Version)
	}
	if m.ProcID != "" {
		logAttrs["process.pid"] = m.ProcID
	}
	if m.MsgID != "" {
		logAttrs["syslog.msgid"] = m.MsgID
	}
	if sourceAddr != "" {
		logAttrs["syslog.source"] = sourceAddr
	}
	if !m.Timestamp.IsZero() && !useDeviceTime {
		logAttrs["syslog.device.timestamp"] = m.Timestamp.Format(time.RFC3339)
	}

	for sdID, params := range m.StructuredData {
		for k, v := range params {
			logAttrs["syslog.sd."+sdID+"."+k] = v
		}
	}

	return models.LogRecord{
		Id:                 uuid.New(),
		ProjectId:          projectId,
		Timestamp:          ts.UTC(),
		SeverityText:       sevText,
		SeverityNumber:     sevNum,
		ServiceName:        serviceName,
		Body:               m.Message,
		ResourceAttributes: resourceAttrs,
		LogAttributes:      logAttrs,
	}
}

// severityToOTel maps the syslog 0..7 severity onto the closest OTel
// severity_number / severity_text. We intentionally collapse 0/1 onto FATAL
// rather than spreading them across 21–24, because nothing downstream
// distinguishes the sub-levels.
func severityToOTel(s int) (string, uint8) {
	switch s {
	case 0: // Emergency
		return "FATAL", 21
	case 1: // Alert
		return "FATAL2", 22
	case 2: // Critical
		return "FATAL3", 23
	case 3: // Error
		return "ERROR", 17
	case 4: // Warning
		return "WARN", 13
	case 5: // Notice
		return "INFO2", 10
	case 6: // Informational
		return "INFO", 9
	case 7: // Debug
		return "DEBUG", 5
	default:
		return "INFO", 9
	}
}

func syslogSeverityName(s int) string {
	switch s {
	case 0:
		return "emerg"
	case 1:
		return "alert"
	case 2:
		return "crit"
	case 3:
		return "err"
	case 4:
		return "warning"
	case 5:
		return "notice"
	case 6:
		return "info"
	case 7:
		return "debug"
	default:
		return "info"
	}
}

func facilityName(f int) string {
	names := []string{
		"kern", "user", "mail", "daemon", "auth", "syslog", "lpr", "news",
		"uucp", "cron", "authpriv", "ftp", "ntp", "audit", "alert", "cron2",
		"local0", "local1", "local2", "local3", "local4", "local5", "local6", "local7",
	}
	if f >= 0 && f < len(names) {
		return names[f]
	}
	return ""
}
