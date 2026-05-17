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

func TestParsePVE_RecordsRawTimestampOnParseFailure(t *testing.T) {
	body := []byte(`{"severity":"info","message":"hi","timestamp":"not-a-date"}`)
	rec, err := ParsePVE(body, uuid.Nil, "", time.Now().UTC())
	if err != nil {
		t.Fatalf("ParsePVE: %v", err)
	}
	if got := rec.LogAttributes["pve.timestamp.raw"]; got != "not-a-date" {
		t.Errorf("pve.timestamp.raw: got %q, want %q", got, "not-a-date")
	}
}

func TestParsePVE_AcceptsRFC3339Nano(t *testing.T) {
	want := time.Date(2026, 5, 17, 10, 30, 45, 123456789, time.UTC)
	body := []byte(`{"severity":"info","message":"hi","timestamp":"` + want.Format(time.RFC3339Nano) + `"}`)
	rec, err := ParsePVE(body, uuid.Nil, "", time.Now().UTC())
	if err != nil {
		t.Fatalf("ParsePVE: %v", err)
	}
	if !rec.Timestamp.Equal(want) {
		t.Errorf("timestamp: got %v, want %v", rec.Timestamp, want)
	}
	if _, ok := rec.LogAttributes["pve.timestamp.raw"]; ok {
		t.Error("pve.timestamp.raw should not be set when timestamp parses cleanly")
	}
}

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
