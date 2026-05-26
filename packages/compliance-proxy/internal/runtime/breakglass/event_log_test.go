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
