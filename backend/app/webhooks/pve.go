package webhooks

import (
	"encoding/json"
	"errors"
	"strconv"
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
