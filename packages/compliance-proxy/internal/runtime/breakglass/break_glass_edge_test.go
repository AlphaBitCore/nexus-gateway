package breakglass

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/handler"
)

// TestNewBreakGlassState_EmptyDataDirDisablesSurface pins the
// "no data dir configured" contract: NewBreakGlassState("") returns a
// nil State, and the PUT handler turns that nil into a 503 — the
// break-glass surface is fully off, never half-configured with a
// dangling event log or pending buffer in the working directory.
func TestNewBreakGlassState_EmptyDataDirDisablesSurface(t *testing.T) {
	bg := NewBreakGlassState("", &fakeReporter{}, &fakeVersionSource{})
	if bg != nil {
		t.Fatalf("empty dataDir must yield a nil (disabled) State; got %+v", bg)
	}
	h := HandleBreakGlassPut(handler.RuntimeDeps{}, "killswitch", bg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/config/killswitch",
		strings.NewReader(`{"state":{"engaged":true}}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("PUT on disabled surface = %d, want 503", rec.Code)
	}
}

// TestWritePending_MarshalErrLeavesNoPendingFile covers writePending's
// json.Marshal error branch: a syntactically invalid RawMessage state
// must surface a "pending: marshal" error and leave NO pending buffer
// on disk — a corrupt buffer would poison every later replay attempt.
func TestWritePending_MarshalErrLeavesNoPendingFile(t *testing.T) {
	dir := t.TempDir()
	bg := NewBreakGlassState(dir, &fakeReporter{}, &fakeVersionSource{})
	err := bg.writePending(pendingBreakGlass{
		ConfigKey: "killswitch", KeyVersion: 1,
		State: json.RawMessage(`{"engaged":`), // truncated JSON
	})
	if err == nil {
		t.Fatal("writePending must reject an unmarshalable state")
	}
	if !strings.Contains(err.Error(), "pending: marshal") {
		t.Errorf("error not wrapped with 'pending: marshal': %v", err)
	}
	if _, statErr := os.Stat(bg.pendingPath()); !os.IsNotExist(statErr) {
		t.Errorf("failed marshal must not create a pending file; stat err = %v", statErr)
	}
}

// TestWritePending_RenameErrWhenTargetBlocked covers writePending's
// os.Rename error branch: when the pending path is occupied by a
// directory the atomic temp→final rename fails and the caller gets a
// "pending: rename" wrapped error instead of a silently dropped buffer.
func TestWritePending_RenameErrWhenTargetBlocked(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, pendingBreakGlassFileName), 0o750); err != nil {
		t.Fatal(err)
	}
	bg := NewBreakGlassState(dir, &fakeReporter{}, &fakeVersionSource{})
	err := bg.writePending(pendingBreakGlass{
		ConfigKey: "killswitch", KeyVersion: 1,
		State: json.RawMessage(`{"engaged":false}`),
	})
	if err == nil {
		t.Fatal("writePending must error when the pending path cannot be renamed into place")
	}
	if !strings.Contains(err.Error(), "pending: rename") {
		t.Errorf("error not wrapped with 'pending: rename': %v", err)
	}
}

// TestEventLog_PathPointsAtAppendTarget pins Path() to the file Append
// actually writes: external log shippers and the runbook both resolve
// the JSONL location via Path(), so a divergence between the two would
// silently ship an empty file while audit lines pile up elsewhere.
func TestEventLog_PathPointsAtAppendTarget(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)
	want := filepath.Join(dir, breakGlassEventLogFileName)
	if got := log.Path(); got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}
	if err := log.Append(BreakGlassEvent{
		ConfigKey: "killswitch", KeyVersion: 2,
		State: json.RawMessage(`{"engaged":true}`),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	data, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatalf("appended event must be readable at Path(): %v", err)
	}
	if !strings.Contains(string(data), `"config_key":"killswitch"`) {
		t.Errorf("file at Path() missing the appended event line: %q", data)
	}
}

// TestEventLog_AppendMarshalErrWritesNothing covers Append's
// json.Marshal error branch: an unmarshalable event (truncated
// RawMessage state) must return an "event log: marshal" error and
// write no file at all — a partial or garbage line would corrupt the
// audit log of record for emergency overrides.
func TestEventLog_AppendMarshalErrWritesNothing(t *testing.T) {
	dir := t.TempDir()
	log := NewEventLog(dir)
	err := log.Append(BreakGlassEvent{
		ConfigKey: "killswitch",
		State:     json.RawMessage(`{"engaged":`), // truncated JSON
	})
	if err == nil {
		t.Fatal("Append must reject an unmarshalable event")
	}
	if !strings.Contains(err.Error(), "event log: marshal") {
		t.Errorf("error not wrapped with 'event log: marshal': %v", err)
	}
	if _, statErr := os.Stat(log.Path()); !os.IsNotExist(statErr) {
		t.Errorf("failed marshal must not create the log file; stat err = %v", statErr)
	}
}
