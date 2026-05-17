package syslog

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// Message is a parsed syslog message from either RFC 3164 or RFC 5424. Empty
// fields are normal — the BSD format carries far less metadata than 5424.
type Message struct {
	Facility       int
	Severity       int
	Timestamp      time.Time
	Hostname       string
	AppName        string
	ProcID         string
	MsgID          string
	StructuredData map[string]map[string]string
	Message        string
	// Version is 1 for RFC 5424. RFC 3164 has no version field, so we use 0.
	Version int
}

var (
	errEmpty          = errors.New("syslog: empty message")
	errMissingPRI     = errors.New("syslog: missing PRI")
	errBadPRI         = errors.New("syslog: bad PRI")
	errTruncatedRFC5  = errors.New("syslog: truncated RFC 5424 message")
	errBadTimestamp   = errors.New("syslog: bad timestamp")
	errBadStructured  = errors.New("syslog: bad structured-data")
)

// Parse decodes a single syslog line. It autodetects RFC 5424 vs RFC 3164 by
// looking for the version digit between the PRI and the first space.
func Parse(line []byte) (*Message, error) {
	if len(line) == 0 {
		return nil, errEmpty
	}

	pri, rest, err := parsePRI(line)
	if err != nil {
		return nil, err
	}
	if len(rest) == 0 {
		return nil, errTruncatedRFC5
	}

	m := &Message{
		Facility: pri >> 3,
		Severity: pri & 0x07,
	}

	// RFC 5424 starts with a version digit immediately after the PRI, e.g.
	// "<34>1 2003-10-11T22:14:15.003Z ...". RFC 3164 starts with a month
	// abbreviation like "Oct".
	if rest[0] >= '0' && rest[0] <= '9' {
		if err := parseRFC5424(rest, m); err != nil {
			return nil, err
		}
		return m, nil
	}

	parseRFC3164(rest, m)
	return m, nil
}

// parsePRI consumes "<NNN>" and returns the priority value (facility*8 + severity).
func parsePRI(line []byte) (int, []byte, error) {
	if line[0] != '<' {
		return 0, nil, errMissingPRI
	}
	end := -1
	for i := 1; i < len(line) && i < 5; i++ {
		if line[i] == '>' {
			end = i
			break
		}
	}
	if end < 2 {
		return 0, nil, errBadPRI
	}
	pri, err := strconv.Atoi(string(line[1:end]))
	if err != nil || pri < 0 || pri > 191 {
		return 0, nil, errBadPRI
	}
	return pri, line[end+1:], nil
}

// parseRFC5424 fills m with VERSION TIMESTAMP HOSTNAME APP-NAME PROCID MSGID
// STRUCTURED-DATA MSG, all space-separated and with "-" meaning NILVALUE.
func parseRFC5424(b []byte, m *Message) error {
	version, rest, ok := takeField(b)
	if !ok {
		return errTruncatedRFC5
	}
	v, err := strconv.Atoi(version)
	if err != nil {
		return errTruncatedRFC5
	}
	m.Version = v

	ts, rest, ok := takeField(rest)
	if !ok {
		return errTruncatedRFC5
	}
	if ts != "-" {
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return errBadTimestamp
		}
		m.Timestamp = t
	}

	host, rest, ok := takeField(rest)
	if !ok {
		return errTruncatedRFC5
	}
	m.Hostname = nilOrValue(host)

	app, rest, ok := takeField(rest)
	if !ok {
		return errTruncatedRFC5
	}
	m.AppName = nilOrValue(app)

	proc, rest, ok := takeField(rest)
	if !ok {
		return errTruncatedRFC5
	}
	m.ProcID = nilOrValue(proc)

	msgid, rest, ok := takeField(rest)
	if !ok {
		return errTruncatedRFC5
	}
	m.MsgID = nilOrValue(msgid)

	sd, rest, err := takeStructuredData(rest)
	if err != nil {
		return err
	}
	m.StructuredData = sd

	// What's left, after an optional separating space, is the free-form MSG.
	// RFC 5424 §6.4 says the MSG part may start with a UTF-8 BOM; strip it.
	if len(rest) > 0 && rest[0] == ' ' {
		rest = rest[1:]
	}
	rest = stripBOM(rest)
	m.Message = string(rest)
	return nil
}

// parseRFC3164 fills m with what it can extract from the BSD format. The
// format is loose — devices in the wild routinely omit hostname or break the
// timestamp — so we never fail; whatever we can't parse becomes part of MSG.
func parseRFC3164(b []byte, m *Message) {
	// Timestamp: "Mmm dd hh:mm:ss" is exactly 15 chars; "Mmm  d hh:mm:ss"
	// (single-digit day) is also 15 chars with a double space. Need at least
	// 16 chars including the trailing space.
	if len(b) >= 16 && b[15] == ' ' {
		layouts := []string{"Jan _2 15:04:05", "Jan 2 15:04:05"}
		for _, layout := range layouts {
			if t, err := time.Parse(layout, string(b[:15])); err == nil {
				// RFC 3164 has no year — assume current year.
				t = t.AddDate(time.Now().Year(), 0, 0)
				m.Timestamp = t
				b = b[16:]
				break
			}
		}
	}

	// Hostname: up to next space.
	if sp := indexByte(b, ' '); sp > 0 {
		m.Hostname = string(b[:sp])
		b = b[sp+1:]
	}

	// TAG up to first non-alnum (often ':' or '['). Optional [PID]. Then ':'
	// and space.
	tagEnd := 0
	for tagEnd < len(b) && tagEnd < 32 && isAlnum(b[tagEnd]) {
		tagEnd++
	}
	if tagEnd > 0 {
		m.AppName = string(b[:tagEnd])
		b = b[tagEnd:]
		if len(b) > 0 && b[0] == '[' {
			if close := indexByte(b, ']'); close > 0 {
				m.ProcID = string(b[1:close])
				b = b[close+1:]
			}
		}
		if len(b) > 0 && b[0] == ':' {
			b = b[1:]
		}
		if len(b) > 0 && b[0] == ' ' {
			b = b[1:]
		}
	}

	m.Message = string(b)
}

func takeField(b []byte) (string, []byte, bool) {
	if len(b) == 0 {
		return "", b, false
	}
	sp := indexByte(b, ' ')
	if sp < 0 {
		return string(b), nil, true
	}
	return string(b[:sp]), b[sp+1:], true
}

// takeStructuredData parses either "-" (NILVALUE) or one-or-more "[SD-ID
// PARAM="VAL" ...]" elements concatenated together.
func takeStructuredData(b []byte) (map[string]map[string]string, []byte, error) {
	if len(b) == 0 {
		return nil, b, errTruncatedRFC5
	}
	if b[0] == '-' {
		if len(b) > 1 && b[1] != ' ' {
			return nil, nil, errBadStructured
		}
		return nil, b[1:], nil
	}
	if b[0] != '[' {
		return nil, nil, errBadStructured
	}

	sd := map[string]map[string]string{}
	for len(b) > 0 && b[0] == '[' {
		end, params, name, err := parseSDElement(b)
		if err != nil {
			return nil, nil, err
		}
		sd[name] = params
		b = b[end:]
	}
	return sd, b, nil
}

func parseSDElement(b []byte) (int, map[string]string, string, error) {
	// b[0] == '['; walk to find SD-ID (up to space or ']') then params.
	i := 1
	nameStart := i
	for i < len(b) && b[i] != ' ' && b[i] != ']' {
		i++
	}
	if i >= len(b) {
		return 0, nil, "", errBadStructured
	}
	name := string(b[nameStart:i])
	params := map[string]string{}

	for {
		if i >= len(b) {
			return 0, nil, "", errBadStructured
		}
		if b[i] == ']' {
			return i + 1, params, name, nil
		}
		if b[i] == ' ' {
			i++
			continue
		}
		// PARAM-NAME = until '='
		keyStart := i
		for i < len(b) && b[i] != '=' && b[i] != ']' && b[i] != ' ' {
			i++
		}
		if i >= len(b) || b[i] != '=' {
			return 0, nil, "", errBadStructured
		}
		key := string(b[keyStart:i])
		i++
		if i >= len(b) || b[i] != '"' {
			return 0, nil, "", errBadStructured
		}
		i++
		var val strings.Builder
		for i < len(b) {
			c := b[i]
			if c == '\\' && i+1 < len(b) {
				next := b[i+1]
				if next == '"' || next == '\\' || next == ']' {
					val.WriteByte(next)
					i += 2
					continue
				}
			}
			if c == '"' {
				i++
				break
			}
			val.WriteByte(c)
			i++
		}
		params[key] = val.String()
	}
}

func nilOrValue(s string) string {
	if s == "-" {
		return ""
	}
	return s
}

func stripBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

func isAlnum(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == '-' || c == '.' || c == '/'
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
