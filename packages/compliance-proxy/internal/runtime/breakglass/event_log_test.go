package breakglass

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestEventLog_AppendCreatesFileAndLine(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)

	err := log.Append(BreakGlassEvent{
		ConfigKey:    "killswitch",
		KeyVersion:   3,
		State:        json.RawMessage(`{"enabled":false}`),
		Reason:       "test",
		SourceIP:     "10.0.0.1",
		ActorTokenID: "tok",
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	path := filepath.Join(dir, breakGlassEventLogFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Errorf("file does not end in newline: %q", data)
	}

	var evt BreakGlassEvent
	if err := json.Unmarshal([]byte(strings.TrimRight(string(data), "\n")), &evt); err != nil {
		t.Fatalf("unmarshal line: %v", err)
	}
	if evt.ConfigKey != "killswitch" {
		t.Errorf("ConfigKey = %q", evt.ConfigKey)
	}
	if evt.At.IsZero() {
		t.Errorf("At should have been auto-populated")
	}
}

func TestEventLog_AppendIsAtomicPerLine(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)

	// Fire many goroutines to verify the mutex keeps lines atomic.
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			_ = log.Append(BreakGlassEvent{
				ConfigKey:  "killswitch",
				KeyVersion: int64(i),
				State:      json.RawMessage(`{"enabled":false}`),
			})
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(filepath.Join(dir, breakGlassEventLogFileName))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("expected %d lines, got %d", n, len(lines))
	}
	for _, line := range lines {
		var evt BreakGlassEvent
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Errorf("line not valid JSON: %q (err=%v)", line, err)
		}
	}
}

// TestEventLog_PathReturnsJSONLPath pins Path() to the documented
// <dataDir>/break_glass_events.jsonl location. The handler logs this path
// on every append, and tooling (fluentd/jq) tails it; a regression that
// returned the bare dir or the wrong filename would point operators at an
// empty file and silently lose the audit trail.
func TestEventLog_PathReturnsJSONLPath(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)
	want := filepath.Join(dir, breakGlassEventLogFileName)
	if got := log.Path(); got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

// TestEventLog_Append_MarshalError covers the `json.Marshal(evt)` error arm.
// A BreakGlassEvent whose State is a malformed json.RawMessage (bytes that
// are not valid JSON) cannot be re-serialized into the log line; the named
// failure mode is "event log: marshal:" and NOTHING must be written to disk.
// This is the contract that protects the audit log from recording a half
// event that downstream jq/fluentd consumers cannot parse.
func TestEventLog_Append_MarshalError(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)

	err := log.Append(BreakGlassEvent{
		ConfigKey: "killswitch",
		// json.RawMessage is marshaled verbatim; invalid JSON here makes
		// the whole-event Marshal fail.
		State: json.RawMessage(`{not-json`),
	})
	if err == nil {
		t.Fatal("expected marshal error for invalid RawMessage state")
	}
	if !strings.Contains(err.Error(), "event log: marshal") {
		t.Errorf("error not wrapped with 'event log: marshal': %v", err)
	}
	// Observable consequence: no file created, no partial line written.
	if _, statErr := os.Stat(filepath.Join(dir, breakGlassEventLogFileName)); !os.IsNotExist(statErr) {
		t.Errorf("log file must not exist after a marshal failure; stat err = %v", statErr)
	}
}

func TestEventLog_AppendAutoPopulatesTime(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)

	if err := log.Append(BreakGlassEvent{ConfigKey: "killswitch"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, breakGlassEventLogFileName))
	var evt BreakGlassEvent
	if err := json.Unmarshal([]byte(strings.TrimRight(string(data), "\n")), &evt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if evt.At.IsZero() {
		t.Errorf("At was not auto-populated")
	}
}
