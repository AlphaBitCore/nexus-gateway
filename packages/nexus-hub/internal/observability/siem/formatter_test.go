package siem

import (
	"encoding/json"
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
