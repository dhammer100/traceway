package syslog

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func mustParseCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", s, err)
	}
	return n
}

func TestParseTrustedCIDRs_Defaults(t *testing.T) {
	nets, allowAll, err := parseTrustedCIDRs("")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if allowAll {
		t.Fatal("default should not be allow-all")
	}
	if len(nets) != len(defaultTrustedCIDRs) {
		t.Errorf("expected %d default CIDRs, got %d", len(defaultTrustedCIDRs), len(nets))
	}
}

func TestParseTrustedCIDRs_Wildcard(t *testing.T) {
	_, allowAll, err := parseTrustedCIDRs("*")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !allowAll {
		t.Fatal("'*' should set allowAll")
	}
}

func TestParseTrustedCIDRs_Invalid(t *testing.T) {
	if _, _, err := parseTrustedCIDRs("not-a-cidr"); err == nil {
		t.Fatal("expected error for invalid CIDR")
	}
}

func TestPool_TrustsIP(t *testing.T) {
	p := &pool{
		trusted: []*net.IPNet{
			mustParseCIDR(t, "192.168.0.0/16"),
			mustParseCIDR(t, "10.0.0.0/8"),
		},
	}
	cases := []struct {
		ip   string
		want bool
	}{
		{"192.168.1.5", true},
		{"10.0.0.1", true},
		{"172.16.0.1", false},
		{"8.8.8.8", false},
		{"100.64.0.1", false},
	}
	for _, c := range cases {
		got := p.trusts(net.ParseIP(c.ip))
		if got != c.want {
			t.Errorf("trusts(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

func TestPool_TrustsIP_AllowAll(t *testing.T) {
	p := &pool{allowAll: true}
	if !p.trusts(net.ParseIP("8.8.8.8")) {
		t.Fatal("allowAll should accept public IP")
	}
}

func TestPool_Enqueue_DropsOnOverflow(t *testing.T) {
	p := &pool{queue: make(chan ingestJob, 2)}
	for i := 0; i < 5; i++ {
		p.enqueue(ingestJob{raw: []byte("x")})
	}
	if got := p.received.Load(); got != 5 {
		t.Errorf("received: got %d, want 5", got)
	}
	if got := p.droppedOverflow.Load(); got != 3 {
		t.Errorf("droppedOverflow: got %d, want 3", got)
	}
	if got := len(p.queue); got != 2 {
		t.Errorf("queue len: got %d, want 2", got)
	}
}

// TestReadOctetCounted_FramedCorrectly verifies that RFC 6587 §3.4.1
// octet-counted framing pulls exactly the right number of bytes per message.
func TestReadOctetCounted_FramedCorrectly(t *testing.T) {
	p := &pool{
		queue:  make(chan ingestJob, 10),
		maxMsg: 1024,
	}
	stream := strings.NewReader("5 hello7 goodbye")
	p.readOctetCounted(context.Background(), bufio.NewReader(stream), "test")
	close(p.queue)

	var got []string
	for j := range p.queue {
		got = append(got, string(j.raw))
	}
	if len(got) != 2 || got[0] != "hello" || got[1] != "goodbye" {
		t.Errorf("frames: %q", got)
	}
}

func TestReadOctetCounted_OverlongDropped(t *testing.T) {
	p := &pool{
		queue:  make(chan ingestJob, 10),
		maxMsg: 4,
	}
	stream := strings.NewReader("10 toolongmsg2 ok")
	p.readOctetCounted(context.Background(), bufio.NewReader(stream), "test")
	close(p.queue)

	var got []string
	for j := range p.queue {
		got = append(got, string(j.raw))
	}
	if len(got) != 1 || got[0] != "ok" {
		t.Errorf("expected only 'ok' to make it through, got %q", got)
	}
	if p.droppedOverflow.Load() != 1 {
		t.Errorf("expected 1 overflow drop, got %d", p.droppedOverflow.Load())
	}
}

func TestReadNewlineDelimited(t *testing.T) {
	p := &pool{
		queue:  make(chan ingestJob, 10),
		maxMsg: 1024,
	}
	stream := bytes.NewBufferString("<13>1 - - - - - - one\n<13>1 - - - - - - two\n")
	p.readNewlineDelimited(context.Background(), bufio.NewReader(stream), "test")
	close(p.queue)

	count := 0
	for range p.queue {
		count++
	}
	if count != 2 {
		t.Errorf("expected 2 framed messages, got %d", count)
	}
}

func TestTrimTrailingNewline(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"abc\n", "abc"},
		{"abc\r\n", "abc"},
		{"abc", "abc"},
		{"\n\n", ""},
	}
	for _, c := range cases {
		got := string(trimTrailingNewline([]byte(c.in)))
		if got != c.want {
			t.Errorf("trim(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveInt(t *testing.T) {
	cases := []struct {
		raw      string
		def, min int
		want     int
	}{
		{"", 10, 1, 10},
		{"5", 10, 1, 5},
		{"0", 10, 1, 10},   // below min -> default
		{"abc", 10, 1, 10}, // not a number -> default
		{"3", 10, 1, 3},
	}
	for _, c := range cases {
		got := resolveInt(c.raw, c.def, c.min)
		if got != c.want {
			t.Errorf("resolveInt(%q, %d, %d) = %d, want %d", c.raw, c.def, c.min, got, c.want)
		}
	}
}

// quick sanity for ToLogRecord — ensure required fields are set.
func TestToLogRecord_Defaults(t *testing.T) {
	m := &Message{Severity: 3, Facility: 4, Message: "hi"}
	now := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	rec := ToLogRecord(m, uuid.Nil, "1.2.3.4:514", now)
	if !rec.Timestamp.Equal(now) {
		t.Errorf("timestamp: got %v, want %v (received-time fallback)", rec.Timestamp, now)
	}
	if rec.SeverityText != "ERROR" || rec.SeverityNumber != 17 {
		t.Errorf("severity: got %s/%d", rec.SeverityText, rec.SeverityNumber)
	}
	if rec.LogAttributes["syslog.facility.name"] != "auth" {
		t.Errorf("facility name: got %q", rec.LogAttributes["syslog.facility.name"])
	}
	if rec.LogAttributes["syslog.source"] != "1.2.3.4:514" {
		t.Errorf("source: got %q", rec.LogAttributes["syslog.source"])
	}
}
