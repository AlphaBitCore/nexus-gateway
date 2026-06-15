package audit

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNDJSONWriter_Write(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	w, err := NewNDJSONWriter(dir, "inst-1", 10, 100, logger)
	if err != nil {
		t.Fatalf("NewNDJSONWriter: %v", err)
	}
	defer w.Close() //nolint:errcheck

	event := AuditEvent{
		ID:                  "evt-001",
		TransactionID:       "tx-001",
		ConnectionID:        "conn-001",
		TrafficSource:       "COMPLIANCE_PROXY",
		IngressType:         "CONNECT",
		BumpStatus:          "BUMP_SUCCESS",
		SourceIP:            "10.0.0.1",
		TargetHost:          "api.openai.com",
		Method:              "POST",
		Path:                "/v1/chat/completions",
		RequestHookDecision: "ALLOW",
		LatencyMs:           42,
		Timestamp:           time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC),
	}

	if err := w.Write(event); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Read the file and verify it is valid JSON.
	entries, err := os.ReadDir(filepath.Join(dir, "inst-1"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}

	data, err := os.ReadFile(filepath.Join(dir, "inst-1", entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 JSON line, got %d", len(lines))
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &parsed); err != nil {
		t.Fatalf("invalid JSON line: %v", err)
	}

	if parsed["transactionId"] != "tx-001" {
		t.Errorf("expected transactionId=tx-001, got %v", parsed["transactionId"])
	}
	if parsed["targetHost"] != "api.openai.com" {
		t.Errorf("expected targetHost=api.openai.com, got %v", parsed["targetHost"])
	}
}

func TestNDJSONWriter_Rotation(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// maxFileSizeMB=1: three ~0.6 MB events (padded TargetHost) force the
	// writer to roll over into a second spool file.
	w, err := NewNDJSONWriter(dir, "inst-rotate", 1, 100, logger)
	if err != nil {
		t.Fatalf("NewNDJSONWriter: %v", err)
	}
	defer w.Close() //nolint:errcheck

	event := AuditEvent{
		ID:                  "evt-rot",
		TransactionID:       "tx-rot",
		ConnectionID:        "conn-rot",
		TrafficSource:       "COMPLIANCE_PROXY",
		IngressType:         "CONNECT",
		BumpStatus:          "BUMP_SUCCESS",
		SourceIP:            "10.0.0.1",
		TargetHost:          strings.Repeat("a", 600*1024),
		RequestHookDecision: "ALLOW",
		LatencyMs:           10,
		Timestamp:           time.Now().UTC(),
	}

	for i := range 3 {
		if err := w.Write(event); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(dir, "inst-rotate"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	if len(entries) < 2 {
		t.Fatalf("expected at least 2 files after rotation, got %d", len(entries))
	}
}

func TestNDJSONWriter_QuotaExceeded(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// 1 MB total quota. The quota gate is a pre-write check, so a single
	// oversized event (>1 MB once marshalled) lands, and the next write is
	// refused because the spool now exceeds the quota — driven through the
	// public API rather than poking internals.
	w, err := NewNDJSONWriter(dir, "inst-quota", 2, 1, logger)
	if err != nil {
		t.Fatalf("NewNDJSONWriter: %v", err)
	}
	defer w.Close() //nolint:errcheck

	event := AuditEvent{
		ID:                  "evt-quota",
		TransactionID:       "tx-quota",
		ConnectionID:        "conn-quota",
		TrafficSource:       "COMPLIANCE_PROXY",
		IngressType:         "CONNECT",
		BumpStatus:          "BUMP_SUCCESS",
		SourceIP:            "10.0.0.1",
		TargetHost:          strings.Repeat("a", 1100*1024), // pushes the line past the 1 MB quota
		RequestHookDecision: "ALLOW",
		LatencyMs:           5,
		Timestamp:           time.Now().UTC(),
	}

	if err := w.Write(event); err != nil {
		t.Fatalf("first (oversized) write should land: %v", err)
	}
	quotaErr := w.Write(event)
	if quotaErr == nil {
		t.Fatal("expected quota exceeded error on the second write")
	}
	if !strings.Contains(quotaErr.Error(), "exceeds quota") {
		t.Fatalf("expected quota error, got: %v", quotaErr)
	}
}

func TestNDJSONWriter_InstanceIsolation(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	w1, err := NewNDJSONWriter(dir, "instance-a", 10, 100, logger)
	if err != nil {
		t.Fatalf("NewNDJSONWriter instance-a: %v", err)
	}
	defer w1.Close() //nolint:errcheck

	w2, err := NewNDJSONWriter(dir, "instance-b", 10, 100, logger)
	if err != nil {
		t.Fatalf("NewNDJSONWriter instance-b: %v", err)
	}
	defer w2.Close() //nolint:errcheck

	event := AuditEvent{
		ID:                  "evt-iso",
		TransactionID:       "tx-iso",
		ConnectionID:        "conn-iso",
		TrafficSource:       "COMPLIANCE_PROXY",
		IngressType:         "CONNECT",
		BumpStatus:          "BUMP_SUCCESS",
		SourceIP:            "10.0.0.1",
		TargetHost:          "example.com",
		RequestHookDecision: "ALLOW",
		LatencyMs:           1,
		Timestamp:           time.Now().UTC(),
	}

	if err := w1.Write(event); err != nil {
		t.Fatalf("w1.Write: %v", err)
	}
	if err := w2.Write(event); err != nil {
		t.Fatalf("w2.Write: %v", err)
	}

	// Each instance directory should have its own file.
	entriesA, err := os.ReadDir(filepath.Join(dir, "instance-a"))
	if err != nil {
		t.Fatalf("ReadDir instance-a: %v", err)
	}
	entriesB, err := os.ReadDir(filepath.Join(dir, "instance-b"))
	if err != nil {
		t.Fatalf("ReadDir instance-b: %v", err)
	}

	if len(entriesA) != 1 {
		t.Errorf("instance-a: expected 1 file, got %d", len(entriesA))
	}
	if len(entriesB) != 1 {
		t.Errorf("instance-b: expected 1 file, got %d", len(entriesB))
	}

	// Verify the directories are different paths.
	pathA := filepath.Join(dir, "instance-a", entriesA[0].Name())
	pathB := filepath.Join(dir, "instance-b", entriesB[0].Name())
	if pathA == pathB {
		t.Error("instance-a and instance-b should write to different file paths")
	}
}
