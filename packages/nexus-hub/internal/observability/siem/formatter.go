package siem

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Formatter converts a batch of audit Events into a byte payload for delivery
// to an external SIEM. Implementations are stateless and safe for concurrent use.
type Formatter interface {
	// ContentType returns the MIME type for the formatted payload
	// (e.g. "application/json" or "text/plain").
	ContentType() string
	// FormatBatch serialises events into a single byte payload.
	FormatBatch(events []Event) ([]byte, error)
}

// NewFormatter returns the Formatter for the given format string.
// Accepted values: "json", "cef", "syslog". Empty string defaults to JSON.
func NewFormatter(format string) Formatter {
	switch format {
	case "cef":
		return &CEFFormatter{}
	case "syslog":
		return &SyslogFormatter{}
	default:
		return &JSONFormatter{}
	}
}

// JSONFormatter serialises a batch as a JSON array.
type JSONFormatter struct{}

// ContentType returns "application/json".
func (f *JSONFormatter) ContentType() string { return "application/json" }

// FormatBatch marshals events into a JSON array.
func (f *JSONFormatter) FormatBatch(events []Event) ([]byte, error) {
	data, err := json.Marshal(events)
	if err != nil {
		return nil, fmt.Errorf("siem/json: marshal: %w", err)
	}
	return data, nil
}

// CEFFormatter serialises a batch as newline-separated CEF (Common Event
// Format) lines, one line per event.
type CEFFormatter struct{}

// ContentType returns "text/plain".
func (f *CEFFormatter) ContentType() string { return "text/plain" }

// FormatBatch produces one CEF line per event, separated by newlines.
func (f *CEFFormatter) FormatBatch(events []Event) ([]byte, error) {
	var sb strings.Builder
	for i, evt := range events {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(cefLine(evt))
	}
	return []byte(sb.String()), nil
}

// cefLine formats a single event as a CEF line.
func cefLine(evt Event) string {
	eventType := strField(evt, "eventType")
	severity := cefSeverity(eventType)

	// Build extensions — include only non-empty fields.
	var ext strings.Builder
	appendCEFExt(&ext, "src", strField(evt, "sourceIp"))
	appendCEFExt(&ext, "dst", strField(evt, "targetHost"))
	appendCEFExt(&ext, "act", strField(evt, "action"))
	appendCEFExt(&ext, "suser", strField(evt, "actorLabel"))
	appendCEFExt(&ext, "msg", strField(evt, "hookReason"))
	appendCEFExt(&ext, "rt", strField(evt, "timestamp"))
	appendCEFExt(&ext, "externalId", strField(evt, "id"))

	return fmt.Sprintf("CEF:0|NexusGateway|ControlPlane|1.0|%s|%s|%d|%s",
		cefEscape(eventType),
		cefEscape(eventType),
		severity,
		ext.String(),
	)
}

// appendCEFExt appends a "key=value " pair if value is non-empty.
func appendCEFExt(sb *strings.Builder, key, value string) {
	if value == "" {
		return
	}
	if sb.Len() > 0 {
		sb.WriteByte(' ')
	}
	sb.WriteString(key)
	sb.WriteByte('=')
	sb.WriteString(cefEscapeValue(value))
}

// sanitizeControl neutralises characters that would let an attacker-controlled
// field forge a second SIEM record: CR and LF are rendered as the literal
// two-character sequences \r and \n, and every other control character (NUL,
// ESC, TAB, DEL, …) is dropped. It runs AFTER the format-specific backslash
// escaping so the backslashes it introduces are interpreted by the SIEM as
// escape sequences, not re-doubled. Without this an actor label such as
// "x@y\nCEF:0|…forged…" (the email on an unauthenticated admin.login.failed)
// would inject a forged line into the security audit stream.
func sanitizeControl(s string) string {
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1 // drop remaining control characters
		}
		return r
	}, s)
}

// cefEscape escapes pipes and backslashes in CEF header fields, then strips
// control characters so a header field cannot break the line.
func cefEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `|`, `\|`)
	return sanitizeControl(s)
}

// cefEscapeValue escapes backslashes and equals signs in CEF extension values,
// then strips control characters (CR/LF injection guard).
func cefEscapeValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `=`, `\=`)
	return sanitizeControl(s)
}

// SyslogFormatter serialises a batch as newline-separated RFC-5424 syslog
// lines, one line per event.
type SyslogFormatter struct{}

// ContentType returns "text/plain".
func (f *SyslogFormatter) ContentType() string { return "text/plain" }

// FormatBatch produces one syslog line per event, separated by newlines.
func (f *SyslogFormatter) FormatBatch(events []Event) ([]byte, error) {
	var sb strings.Builder
	for i, evt := range events {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(syslogLine(evt))
	}
	return []byte(sb.String()), nil
}

// syslogLine formats a single event as an RFC-5424 syslog line.
func syslogLine(evt Event) string {
	eventType := strField(evt, "eventType")
	source := strField(evt, "source")
	actor := strField(evt, "actorLabel")
	timestamp := strField(evt, "timestamp")
	if timestamp == "" {
		timestamp = "-"
	}

	// Build message from hookReason or a generic default.
	message := strField(evt, "hookReason")
	if message == "" {
		message = eventType
	}

	// Priority = facility * 8 + severity.
	// facility = local0 (16)
	const facility = 16
	sev := syslogSeverity(eventType)
	pri := facility*8 + sev

	// Structured data
	sd := fmt.Sprintf(`[nexus@0 eventType="%s" source="%s" actor="%s"]`,
		syslogEscape(eventType),
		syslogEscape(source),
		syslogEscape(actor),
	)

	// RFC-5424: <PRI>VERSION TIMESTAMP HOSTNAME APP-NAME PROCID MSGID SD MSG.
	// timestamp, eventType (MSGID) and message are inserted outside any SD-PARAM
	// quoting, so they are control-sanitised here to prevent a newline in any of
	// them from forging a new syslog line. eventType keeps its raw form for the
	// severity classification above.
	return fmt.Sprintf("<%d>1 %s nexus-gateway control-plane - %s %s %s",
		pri,
		sanitizeControl(timestamp),
		sanitizeControl(eventType),
		sd,
		sanitizeControl(message),
	)
}

// syslogEscape escapes double-quotes and backslashes inside SD-PARAM values,
// then strips control characters (CR/LF injection guard).
func syslogEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return sanitizeControl(s)
}

// strField safely extracts a string value from an Event map.
// Returns "" if the key is absent or the value is not a string.
func strField(evt Event, key string) string {
	v, ok := evt[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
