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
