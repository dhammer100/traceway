package syslog

import (
	"testing"
	"time"
)

func TestParse_RFC5424_Full(t *testing.T) {
	line := []byte(`<165>1 2023-10-11T22:14:15.003Z mymachine.example.com evntslog 1234 ID47 [exampleSDID@32473 iut="3" eventSource="App" eventID="1011"] BOMAn application event log entry...`)
	m, err := Parse(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Facility != 20 || m.Severity != 5 {
		t.Errorf("facility/severity: got %d/%d, want 20/5", m.Facility, m.Severity)
	}
	if m.Version != 1 {
		t.Errorf("version: got %d, want 1", m.Version)
	}
	want, _ := time.Parse(time.RFC3339Nano, "2023-10-11T22:14:15.003Z")
	if !m.Timestamp.Equal(want) {
		t.Errorf("timestamp: got %v, want %v", m.Timestamp, want)
	}
	if m.Hostname != "mymachine.example.com" {
		t.Errorf("hostname: got %q", m.Hostname)
	}
	if m.AppName != "evntslog" {
		t.Errorf("appname: got %q", m.AppName)
	}
	if m.ProcID != "1234" {
		t.Errorf("procid: got %q", m.ProcID)
	}
	if m.MsgID != "ID47" {
		t.Errorf("msgid: got %q", m.MsgID)
	}
	sd := m.StructuredData["exampleSDID@32473"]
	if sd == nil || sd["iut"] != "3" || sd["eventSource"] != "App" || sd["eventID"] != "1011" {
		t.Errorf("structured-data: got %+v", m.StructuredData)
	}
	if m.Message != "BOMAn application event log entry..." {
		t.Errorf("body: got %q", m.Message)
	}
}

func TestParse_RFC5424_BOMStripped(t *testing.T) {
	// The BOM is the bytes EF BB BF immediately before the body text.
	line := append([]byte(`<13>1 2023-10-11T22:14:15Z host app - - - `), 0xEF, 0xBB, 0xBF)
	line = append(line, []byte("hello")...)
	m, err := Parse(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Message != "hello" {
		t.Errorf("body: got %q want %q", m.Message, "hello")
	}
}

func TestParse_RFC5424_NilValues(t *testing.T) {
	line := []byte(`<14>1 - - - - - - just a message`)
	m, err := Parse(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !m.Timestamp.IsZero() {
		t.Errorf("expected zero timestamp, got %v", m.Timestamp)
	}
	if m.Hostname != "" || m.AppName != "" || m.ProcID != "" || m.MsgID != "" {
		t.Errorf("expected all nil fields empty, got host=%q app=%q proc=%q msgid=%q",
			m.Hostname, m.AppName, m.ProcID, m.MsgID)
	}
	if m.Message != "just a message" {
		t.Errorf("body: got %q", m.Message)
	}
}

func TestParse_RFC5424_StructuredDataEscapes(t *testing.T) {
	line := []byte(`<14>1 - - - - - [ex@1 a="hello \"world\"" b="brack\]et"] body`)
	m, err := Parse(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sd := m.StructuredData["ex@1"]
	if sd["a"] != `hello "world"` {
		t.Errorf("escape \\\": got %q", sd["a"])
	}
	if sd["b"] != `brack]et` {
		t.Errorf("escape \\]: got %q", sd["b"])
	}
}

func TestParse_RFC5424_MultipleSDElements(t *testing.T) {
	line := []byte(`<14>1 - - - - - [a@1 k="v"][b@2 j="w"] msg`)
	m, err := Parse(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.StructuredData["a@1"]["k"] != "v" {
		t.Errorf("a@1.k: %+v", m.StructuredData)
	}
	if m.StructuredData["b@2"]["j"] != "w" {
		t.Errorf("b@2.j: %+v", m.StructuredData)
	}
}

func TestParse_RFC3164_WithPID(t *testing.T) {
	line := []byte(`<34>Oct 11 22:14:15 mymachine sshd[1234]: Failed password for root from 1.2.3.4`)
	m, err := Parse(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Facility != 4 || m.Severity != 2 {
		t.Errorf("facility/severity: got %d/%d, want 4/2", m.Facility, m.Severity)
	}
	if m.Hostname != "mymachine" {
		t.Errorf("hostname: got %q", m.Hostname)
	}
	if m.AppName != "sshd" {
		t.Errorf("appname: got %q", m.AppName)
	}
	if m.ProcID != "1234" {
		t.Errorf("procid: got %q", m.ProcID)
	}
	if m.Message != "Failed password for root from 1.2.3.4" {
		t.Errorf("body: got %q", m.Message)
	}
}

func TestParse_RFC3164_NoPID(t *testing.T) {
	line := []byte(`<13>Oct 11 22:14:15 myhost myapp: hello world`)
	m, err := Parse(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.AppName != "myapp" || m.ProcID != "" || m.Message != "hello world" {
		t.Errorf("got app=%q proc=%q body=%q", m.AppName, m.ProcID, m.Message)
	}
}

func TestParse_RFC3164_SingleDigitDay(t *testing.T) {
	line := []byte(`<13>Oct  1 22:14:15 myhost myapp: msg`)
	m, err := Parse(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Timestamp.IsZero() {
		t.Errorf("expected parsed timestamp, got zero")
	}
	if m.Hostname != "myhost" || m.AppName != "myapp" || m.Message != "msg" {
		t.Errorf("got host=%q app=%q body=%q", m.Hostname, m.AppName, m.Message)
	}
}

func TestParse_Errors(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"missing PRI", "no bracket"},
		{"bad PRI digits", "<abc>1 - - - - - - body"},
		{"PRI too long", "<99999>1 - - - - - - body"},
		{"PRI > 191", "<200>1 - - - - - - body"},
		{"5424 truncated", "<13>1 2023-10-11T22:14:15Z host"},
		{"5424 bad timestamp", "<13>1 notatime host app - - - body"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse([]byte(c.in)); err == nil {
				t.Errorf("expected error for input %q", c.in)
			}
		})
	}
}

func TestSeverityToOTel_All(t *testing.T) {
	cases := []struct {
		in     int
		want   uint8
		wantTx string
	}{
		{0, 21, "FATAL"},
		{3, 17, "ERROR"},
		{4, 13, "WARN"},
		{6, 9, "INFO"},
		{7, 5, "DEBUG"},
	}
	for _, c := range cases {
		gotTx, gotNum := severityToOTel(c.in)
		if gotNum != c.want || gotTx != c.wantTx {
			t.Errorf("severityToOTel(%d) = %s/%d, want %s/%d", c.in, gotTx, gotNum, c.wantTx, c.want)
		}
	}
}
