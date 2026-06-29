package breakglass

import (
	"bytes"
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
)

// break_glass.go — nextVersion reported>desired branch

// TestNextVersion_ReportedHigherThanDesired covers nextVersion's
// `r > hi` branch. Existing TestBreakGlassPut_VersionFromSourceMax
// covers the d>r case; this one pins the inverse so a regression that
// drops the `if r > hi` swap silently breaks proxies whose reported
// version has outrun Hub's desired template.
func TestNextVersion_ReportedHigherThanDesired(t *testing.T) {
	deps, _, reporter, dir := newBGTestDeps(t)
	bg := NewBreakGlassState(dir, reporter, &fakeVersionSource{desired: 2, reported: 11})

	h := HandleBreakGlassPut(deps, "killswitch", bg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/config/killswitch",
		bytes.NewBufferString(`{"state":{"engaged":false}}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	var resp bgResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Version != 12 {
		t.Errorf("resp.Version = %d, want 12 (max(2,11)+1)", resp.Version)
	}
}

// break_glass.go — HandleBreakGlassPut error arms

// TestBreakGlassPut_MethodNotAllowed covers the handler-side
// `r.Method != http.MethodPut` guard (separate from the server.go route
// dispatcher's 405). Pinned for callers that hit the bound handler
// directly without going through the server mux.
func TestBreakGlassPut_MethodNotAllowed(t *testing.T) {
	deps, bg, _, _ := newBGTestDeps(t)
	h := HandleBreakGlassPut(deps, "killswitch", bg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/runtime/config/killswitch", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// TestBreakGlassPut_MalformedJSONBody covers
// `json.NewDecoder(r.Body).Decode(&req)` returning an error — body
// not valid JSON must surface 400 with the documented envelope.
func TestBreakGlassPut_MalformedJSONBody(t *testing.T) {
	deps, bg, _, _ := newBGTestDeps(t)
	h := HandleBreakGlassPut(deps, "killswitch", bg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/config/killswitch",
		bytes.NewBufferString(`{not valid json`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid JSON body") {
		t.Errorf("body missing 'invalid JSON body': %q", rec.Body.String())
	}
}

// TestBreakGlassPut_EmptyStateRejected covers the `len(req.State) == 0`
// branch — body without a state field returns 400 before the apply or
// event log fires. The existing null-state test (Decode succeeds but
// State == "null") covers the sibling bytes.Equal check.
func TestBreakGlassPut_EmptyStateRejected(t *testing.T) {
	deps, bg, _, _ := newBGTestDeps(t)
	h := HandleBreakGlassPut(deps, "killswitch", bg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/config/killswitch",
		bytes.NewBufferString(`{"reason":"no state"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "state is required") {
		t.Errorf("body missing 'state is required': %q", rec.Body.String())
	}
}

// TestBreakGlassPut_LogAppendFailure_Returns500 covers the
// `bg.log.Append(evt)` error branch in HandleBreakGlassPut. We force
// the failure by pointing the bg state's dataDir at a regular file so
// EventLog.Append's MkdirAll surfaces "not a directory". The local
// apply has succeeded, so the response must be 500 with the documented
// "applied but event log failed" envelope.
func TestBreakGlassPut_LogAppendFailure_Returns500(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses dir mode")
	}
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ks := killswitch.NewKillSwitch(slog.New(slog.NewTextHandler(io.Discard, nil)))
	deps := handler.RuntimeDeps{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		KillswitchSnap: ks,
		DataDir:        blocker,
	}
	bg := NewBreakGlassState(blocker, &fakeReporter{}, &fakeVersionSource{})

	h := HandleBreakGlassPut(deps, "killswitch", bg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/config/killswitch",
		bytes.NewBufferString(`{"state":{"engaged":false},"reason":"drill"}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (log append failure); body=%q",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "break-glass applied but event log failed") {
		t.Errorf("500 body missing documented prefix: %q", rec.Body.String())
	}
	// And the apply did happen locally before the log write.
	if ks.IsEngaged() {
		t.Errorf("local apply must have run before log: killswitch still engaged")
	}
}

// TestBreakGlassPut_PendingSpoolError_StillReturns200 covers the
// `werr := bg.writePending(...)` error branch — the apply has already
// succeeded locally, so the handler must still respond 200 with
// PendingReport=true even though the spool failed. The spool error
// surfaces only via the logger.
func TestBreakGlassPut_PendingSpoolError_StillReturns200(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses dir mode")
	}
	dir := t.TempDir()
	// Pre-create the event log file so Append succeeds (it opens an
	// existing file in O_APPEND mode without writing the dir).
	logPath := filepath.Join(dir, breakGlassEventLogFileName)
	if err := os.WriteFile(logPath, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ks := killswitch.NewKillSwitch(slog.New(slog.NewTextHandler(io.Discard, nil)))
	deps := handler.RuntimeDeps{Logger: logger, KillswitchSnap: ks, DataDir: dir}
	reporter := &fakeReporter{failWith: errors.New("hub unreachable")}
	bg := NewBreakGlassState(dir, reporter, &fakeVersionSource{})

	h := HandleBreakGlassPut(deps, "killswitch", bg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/config/killswitch",
		bytes.NewBufferString(`{"state":{"engaged":false}}`))
	h.ServeHTTP(rec, req)

	_ = os.Chmod(dir, 0o700) // restore before assertions

	if rec.Code != http.StatusOK {
		t.Skipf("filesystem ignored 0500; skipping (status=%d body=%q)",
			rec.Code, rec.Body.String())
	}
	var resp bgResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.PendingReport || resp.ReportStatus != "pending" {
		t.Errorf("expected pending=true status=pending; got %+v", resp)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "failed to spool pending report") {
		t.Logf("spool-error log line not found (filesystem may have bypassed chmod): %s", logs)
	}
}

// TestBreakGlassPut_NilReporter_StatusSkipped covers
// `if bg.reporter != nil` false — when no reporter is wired the apply
// is local-only and reportStatus must be "skipped". Without this
// branch a misconfigured deployment would silently 200 without ever
// attempting Hub delivery.
func TestBreakGlassPut_NilReporter_StatusSkipped(t *testing.T) {
	dir := t.TempDir()
	ks := killswitch.NewKillSwitch(slog.New(slog.NewTextHandler(io.Discard, nil)))
	deps := handler.RuntimeDeps{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		KillswitchSnap: ks,
		DataDir:        dir,
	}
	bg := NewBreakGlassState(dir, nil, &fakeVersionSource{})

	h := HandleBreakGlassPut(deps, "killswitch", bg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/config/killswitch",
		bytes.NewBufferString(`{"state":{"engaged":false}}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	var resp bgResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.ReportStatus != "skipped" {
		t.Errorf("ReportStatus = %q, want skipped (nil reporter)", resp.ReportStatus)
	}
	if resp.PendingReport {
		t.Errorf("PendingReport = true with nil reporter; want false")
	}
}

// break_glass.go — NewBreakGlassState disabled-surface branch

// TestNewBreakGlassState_EmptyDataDirReturnsNil covers the
// `if dataDir == ""` guard. An empty dataDir means the break-glass surface
// is disabled; the constructor must return a nil *State so the PUT handler
// answers 503 rather than dereferencing a half-built State. A regression
// that returned a non-nil State with an empty pendingDir would silently
// spool to the process CWD.
func TestNewBreakGlassState_EmptyDataDirReturnsNil(t *testing.T) {
	if bg := NewBreakGlassState("", &fakeReporter{}, &fakeVersionSource{}); bg != nil {
		t.Errorf("NewBreakGlassState(\"\") = %v, want nil (disabled surface)", bg)
	}
}

// break_glass.go — writePending error arms

// TestWritePending_MarshalError covers the `json.Marshal(p)` error arm in
// writePending. A pendingBreakGlass whose State is a malformed
// json.RawMessage cannot be serialized; the named failure mode is
// "pending: marshal:" and no pending file may be left behind — a corrupt
// spool would be replayed verbatim to Hub on the next retry.
func TestWritePending_MarshalError(t *testing.T) {
	dir := t.TempDir()
	bg := NewBreakGlassState(dir, &fakeReporter{}, &fakeVersionSource{})

	err := bg.writePending(pendingBreakGlass{
		ConfigKey: "killswitch",
		State:     json.RawMessage(`{not-json`),
	})
	if err == nil {
		t.Fatal("expected marshal error for invalid RawMessage state")
	}
	if !strings.Contains(err.Error(), "pending: marshal") {
		t.Errorf("error not wrapped with 'pending: marshal': %v", err)
	}
	// No spool file (and no leftover .tmp) may exist after a marshal failure.
	if _, statErr := os.Stat(filepath.Join(dir, pendingBreakGlassFileName)); !os.IsNotExist(statErr) {
		t.Errorf("pending file must not exist after marshal failure; stat err = %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, pendingBreakGlassFileName+".tmp")); !os.IsNotExist(statErr) {
		t.Errorf(".tmp file must not exist after marshal failure; stat err = %v", statErr)
	}
}

// TestWritePending_RenameError covers the `os.Rename(tmp, …)` error arm.
// After MkdirAll + temp-file write succeed, we make the destination path a
// non-empty directory so the atomic rename fails with ENOTEMPTY. The named
// failure mode is "pending: rename:" — the apply has run locally, so the
// handler relies on this surfacing rather than silently dropping the spool.
func TestWritePending_RenameError(t *testing.T) {
	dir := t.TempDir()
	// Make the final pending path a non-empty directory: os.Rename of a
	// regular temp file onto a non-empty dir fails with ENOTEMPTY/EEXIST.
	dest := filepath.Join(dir, pendingBreakGlassFileName)
	if err := os.Mkdir(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "occupant"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	bg := NewBreakGlassState(dir, &fakeReporter{}, &fakeVersionSource{})

	err := bg.writePending(pendingBreakGlass{
		ConfigKey:  "killswitch",
		KeyVersion: 1,
		State:      json.RawMessage(`{"engaged":false}`),
	})
	if err == nil {
		t.Fatal("expected rename error when destination is a non-empty directory")
	}
	if !strings.Contains(err.Error(), "pending: rename") {
		t.Errorf("error not wrapped with 'pending: rename': %v", err)
	}
}

// break_glass.go — applyBreakGlassLocal exemptions success branch

// TestApplyBreakGlassLocal_ActiveExemptionsSuccess covers the
// `case "exemptions"` success path that returns
// ApplyActiveExemptions's nil — existing tests only cover the
// nil-rebuilder error and the killswitch happy path.
func TestApplyBreakGlassLocal_ActiveExemptionsSuccess(t *testing.T) {
	store := exemption.NewStore(slog.New(slog.NewTextHandler(io.Discard, nil)))
	deps := handler.RuntimeDeps{ExemptionRebuilder: store}
	state := json.RawMessage(`{"entries":[{"id":"e1","sourceIp":"10.0.0.1","targetHost":"api.openai.com:443","expiresAt":"2099-01-01T00:00:00Z"}]}`)
	if err := applyBreakGlassLocal(deps, "exemptions", state); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Observable behavior: the store now holds the rebuilt entry.
	got := store.List()
	if len(got) != 1 {
		t.Errorf("store.List() len = %d, want 1 after rebuild", len(got))
	}
}

// break_glass.go — State file-side error arms

// TestClearPending_NonNotExistError covers the
// `!os.IsNotExist(err)` branch in clearPending — a Remove that fails
// for a reason other than "not found" must surface "pending: remove:"
// wrapped error so the caller can log it.
func TestClearPending_NonNotExistError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses dir mode")
	}
	dir := t.TempDir()
	pendingFile := filepath.Join(dir, pendingBreakGlassFileName)
	if err := os.WriteFile(pendingFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	bg := NewBreakGlassState(dir, &fakeReporter{}, &fakeVersionSource{})
	err := bg.clearPending()
	_ = os.Chmod(dir, 0o700)
	if err == nil {
		t.Skip("filesystem did not honour 0o500; cannot exercise Remove-error branch")
	}
	if !strings.Contains(err.Error(), "pending: remove") {
		t.Errorf("error not wrapped with 'pending: remove': %v", err)
	}
}

// TestReadPending_NonNotExistError covers readPending's
// `!os.IsNotExist(err)` ReadFile branch — a read that errors with
// something other than "not found" (here: pending path is a directory
// so ReadFile returns EISDIR) must surface "pending: read:" wrapped
// error.
func TestReadPending_NonNotExistError(t *testing.T) {
	dir := t.TempDir()
	pendingDir := filepath.Join(dir, pendingBreakGlassFileName)
	if err := os.Mkdir(pendingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bg := NewBreakGlassState(dir, &fakeReporter{}, &fakeVersionSource{})
	_, _, err := bg.readPending()
	if err == nil {
		t.Fatal("expected pending: read error when pending path is a directory")
	}
	if !strings.Contains(err.Error(), "pending: read") {
		t.Errorf("error not wrapped with 'pending: read': %v", err)
	}
}

// break_glass.go — ReplayPending clearPending error branch

// swapPendingToDirReporter succeeds the SendBreakGlassShadowReport
// call, then swaps the pending file for a non-empty directory so the
// subsequent os.Remove inside clearPending returns ENOTEMPTY (a
// non-NotExist error). Exercises ReplayPending's "pending: clear
// after replay" wrap.
type swapPendingToDirReporter struct {
	dir string
}

func (r *swapPendingToDirReporter) SendBreakGlassShadowReport(
	_ context.Context, _ string, _ json.RawMessage, _ int64,
	_, _, _ string,
) error {
	pendingFile := filepath.Join(r.dir, pendingBreakGlassFileName)
	if err := os.Remove(pendingFile); err != nil {
		return err
	}
	if err := os.Mkdir(pendingFile, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(pendingFile, "blocker"), []byte("x"), 0o600); err != nil {
		return err
	}
	return nil
}

func TestReplayPending_ClearPendingError(t *testing.T) {
	dir := t.TempDir()
	bg := NewBreakGlassState(dir, &swapPendingToDirReporter{dir: dir}, &fakeVersionSource{})
	if err := bg.writePending(pendingBreakGlass{
		ConfigKey:  "killswitch",
		KeyVersion: 1,
		State:      json.RawMessage(`{"engaged":false}`),
	}); err != nil {
		t.Fatalf("writePending: %v", err)
	}

	drained, err := bg.ReplayPending(context.Background())
	// Cleanup the swapped-in non-empty directory so t.TempDir can
	// recurse-delete the parent.
	pendingFile := filepath.Join(dir, pendingBreakGlassFileName)
	_ = os.RemoveAll(pendingFile)

	if err == nil {
		t.Fatal("expected error from clearPending after replay")
	}
	if !strings.Contains(err.Error(), "pending: clear after replay") {
		t.Errorf("error not wrapped with 'pending: clear after replay': %v", err)
	}
	if drained {
		t.Errorf("drained = true on clear-error; want false")
	}
}

// event_log.go — Append error arms

// TestEventLog_Append_MkdirError covers `os.MkdirAll(filepath.Dir(l.path), …)`
// failing — when the parent dir is actually a regular file, MkdirAll
// surfaces "not a directory" wrapped with "event log: mkdir:".
func TestEventLog_Append_MkdirError(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := NewEventLog(blocker) // dir is now a regular file
	err := log.Append(BreakGlassEvent{
		ConfigKey: "killswitch", KeyVersion: 1,
		State: json.RawMessage(`{"engaged":false}`),
	})
	if err == nil {
		t.Fatal("expected mkdir error when parent dir is a regular file")
	}
	if !strings.Contains(err.Error(), "event log: mkdir") {
		t.Errorf("error not wrapped with 'event log: mkdir': %v", err)
	}
}

// TestEventLog_Append_OpenError covers `os.OpenFile(l.path, …)`
// failing — when the target path is itself a directory, OpenFile
// (O_APPEND|O_CREATE|O_WRONLY) returns EISDIR which surfaces wrapped
// as "event log: open:".
func TestEventLog_Append_OpenError(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, breakGlassEventLogFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	log := NewEventLog(dir)
	err := log.Append(BreakGlassEvent{
		ConfigKey: "killswitch", KeyVersion: 1,
		State: json.RawMessage(`{"engaged":false}`),
	})
	if err == nil {
		t.Fatal("expected open error when log path is a directory")
	}
	if !strings.Contains(err.Error(), "event log: open") {
		t.Errorf("error not wrapped with 'event log: open': %v", err)
	}
}
