package breakglass

import (
	"fmt"
	"github.com/goccy/go-json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// breakGlassEventLogFileName is the JSONL file that records every break-glass
// PUT. Each line is a self-contained BreakGlassEvent JSON object and is
// fsync'd before the handler returns to the caller — this is the audit log
// of record for local emergency overrides. Hub consumes a copy of the event
// via the shadow_report_break_glass envelope; the JSONL here is the
// survive-restart tail the proxy replays into its pending-buffer when Hub
// delivery failed.
const breakGlassEventLogFileName = "break_glass_events.jsonl"

// BreakGlassEvent is one line of the durable break-glass log. Fields are
// snake_case so external tooling (jq, fluentd) can consume the file without
// translation. Time is always UTC RFC3339Nano so ordering survives restart.
//
// The event is appended BEFORE the Hub shadow_report is attempted (spec
// §5.2), so the log is an authoritative record of what was applied locally
// even if Hub delivery fails. Delivery status (ok / pending / skipped) lives
// on the HTTP response and in pending_break_glass.json, not here.
type BreakGlassEvent struct {
	At           time.Time       `json:"at"`
	ConfigKey    string          `json:"config_key"`
	KeyVersion   int64           `json:"key_version"`
	State        json.RawMessage `json:"state"`
	Reason       string          `json:"reason,omitempty"`
	SourceIP     string          `json:"source_ip,omitempty"`
	ActorTokenID string          `json:"actor_token_id,omitempty"`
}

// EventLog appends BreakGlassEvent lines to a JSONL file and fsyncs each
// write before returning. The mutex serializes concurrent appenders so the
// file stays line-aligned. The directory is created lazily on the first
// write.
type EventLog struct {
	mu   sync.Mutex
	path string
}

// NewEventLog returns an EventLog that writes to <dataDir>/break_glass_events.jsonl.
// dataDir must be a writable directory owned by the proxy — callers should
// have pre-created it at startup; NewEventLog does not create it.
func NewEventLog(dataDir string) *EventLog {
	return &EventLog{path: filepath.Join(dataDir, breakGlassEventLogFileName)}
}

// Path returns the full path of the underlying JSONL file. Exposed for tests
// and for the break-glass handler so it can log the path on every append.
func (l *EventLog) Path() string { return l.path }

// Append serializes evt, writes it as a single line, and fsyncs the file.
// Returns an error only if the write or fsync failed — the caller should
// treat an error as "local apply succeeded but durable event log failed"
// and return 500 so operators see the failure immediately.
func (l *EventLog) Append(evt BreakGlassEvent) error {
	if evt.At.IsZero() {
		evt.At = time.Now().UTC()
	}
	line, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("event log: marshal: %w", err)
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	// Ensure the directory exists. MkdirAll is a no-op when it already does.
	if err := os.MkdirAll(filepath.Dir(l.path), 0o750); err != nil {
		return fmt.Errorf("event log: mkdir: %w", err)
	}

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("event log: open: %w", err)
	}
	defer f.Close() //nolint:errcheck

	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("event log: write: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("event log: fsync: %w", err)
	}
	return nil
}
