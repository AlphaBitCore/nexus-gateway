package siem

import (
	"github.com/goccy/go-json"
	"strings"
	"testing"
)

func sampleEvent() Event {
	return Event{
		"id":         "evt-001",
		"eventType":  "proxy.request",
		"sourceIp":   "10.0.0.1",
		"targetHost": "api.example.com",
		"action":     "allow",
		"actorLabel": "alice",
		"hookReason": "passed compliance check",
		"timestamp":  "2026-04-15T10:00:00Z",
		"source":     "vk",
	}
}

// TestJSONFormatter verifies content type and valid JSON array output.
func TestJSONFormatter(t *testing.T) {
	f := &JSONFormatter{}

	if ct := f.ContentType(); ct != "application/json" {
		t.Errorf("ContentType() = %q, want %q", ct, "application/json")
	}

	events := []Event{sampleEvent()}
	data, err := f.FormatBatch(events)
	if err != nil {
		t.Fatalf("FormatBatch() error: %v", err)
	}

	var result []map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("output is not valid JSON array: %v", err)
	}
	if len(result) != 1 {
		t.Errorf("expected 1 event in array, got %d", len(result))
	}
}

// TestCEFFormatter verifies CEF:0 prefix, event type presence, and src= extension.
func TestCEFFormatter(t *testing.T) {
	f := &CEFFormatter{}

	if ct := f.ContentType(); ct != "text/plain" {
		t.Errorf("ContentType() = %q, want %q", ct, "text/plain")
	}

	events := []Event{sampleEvent()}
	data, err := f.FormatBatch(events)
	if err != nil {
		t.Fatalf("FormatBatch() error: %v", err)
	}

	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, "CEF:0") {
		t.Errorf("output should start with CEF:0, got: %s", line)
	}
	if !strings.Contains(line, "proxy.request") {
		t.Errorf("output should contain event type 'proxy.request', got: %s", line)
	}
	if !strings.Contains(line, "src=10.0.0.1") {
		t.Errorf("output should contain src=10.0.0.1, got: %s", line)
	}
}

// TestSyslogFormatter verifies the output contains nexus-gateway and the event type.
func TestSyslogFormatter(t *testing.T) {
	f := &SyslogFormatter{}

	if ct := f.ContentType(); ct != "text/plain" {
		t.Errorf("ContentType() = %q, want %q", ct, "text/plain")
	}

	events := []Event{sampleEvent()}
	data, err := f.FormatBatch(events)
	if err != nil {
		t.Fatalf("FormatBatch() error: %v", err)
	}

	line := strings.TrimSpace(string(data))
	if !strings.Contains(line, "nexus-gateway") {
		t.Errorf("output should contain 'nexus-gateway', got: %s", line)
	}
	if !strings.Contains(line, "proxy.request") {
		t.Errorf("output should contain event type 'proxy.request', got: %s", line)
	}
}

// TestFormatters_RejectControlCharInjection is the regression test: an
// attacker-controlled field (e.g. the unauthenticated email on admin.login.failed,
// surfaced as actorLabel) must not be able to forge a second SIEM record by
// embedding CR/LF, nor smuggle other control bytes into the audit stream.
func TestFormatters_RejectControlCharInjection(t *testing.T) {
	evil := Event{
		"id":         "evt-evil",
		"eventType":  "auth.login_failure",
		"sourceIp":   "10.0.0.9",
		"actorLabel": "x@y\nCEF:0|NexusGateway|ControlPlane|1.0|forged|forged|10|",
		"hookReason": "denied\n<34>1 2026-01-01 host forged - - - injected",
		"timestamp":  "2026-06-06T00:00:00Z",
		"source":     "control-plane\x1b[31m",
	}

	for _, tc := range []struct {
		name string
		f    Formatter
	}{
		{"cef", &CEFFormatter{}},
		{"syslog", &SyslogFormatter{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.f.FormatBatch([]Event{evil})
			if err != nil {
				t.Fatalf("FormatBatch() error: %v", err)
			}
			s := string(out)
			// A single-event batch must be exactly one line — any raw newline
			// means the attacker forged a second record.
			if strings.ContainsAny(s, "\n\r") {
				t.Fatalf("output contains a raw CR/LF (record-forging): %q", s)
			}
			// The CR/LF must survive as the visible literal escape, not vanish
			// silently — so an operator can see the value contained a newline.
			if !strings.Contains(s, `\n`) {
				t.Errorf("expected the newline to be rendered as the literal \\n escape, got: %q", s)
			}
			// Other control bytes (NUL, ESC) must be dropped entirely.
			if strings.ContainsRune(s, '\x00') || strings.ContainsRune(s, '\x1b') {
				t.Errorf("output still contains a raw control byte: %q", s)
			}
		})
	}

	// Multi-event batches still use the newline as the inter-record separator:
	// two clean events → exactly one separating newline.
	for _, tc := range []struct {
		name string
		f    Formatter
	}{
		{"cef", &CEFFormatter{}},
		{"syslog", &SyslogFormatter{}},
	} {
		t.Run(tc.name+"_separator_intact", func(t *testing.T) {
			out, err := tc.f.FormatBatch([]Event{sampleEvent(), sampleEvent()})
			if err != nil {
				t.Fatalf("FormatBatch() error: %v", err)
			}
			if got := strings.Count(string(out), "\n"); got != 1 {
				t.Errorf("two-event batch should have exactly 1 separator newline, got %d", got)
			}
		})
	}
}

// TestNewFormatter verifies the factory returns non-nil for all known formats and "".
func TestNewFormatter(t *testing.T) {
	cases := []string{"json", "cef", "syslog", ""}
	for _, format := range cases {
		f := NewFormatter(format)
		if f == nil {
			t.Errorf("NewFormatter(%q) returned nil", format)
		}
	}
}
