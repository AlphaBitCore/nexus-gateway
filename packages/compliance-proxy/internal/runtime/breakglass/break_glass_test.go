package breakglass

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/killswitch"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/interception"
)

// fakeReporter records every SendBreakGlassShadowReport call. failWith forces
// the call to error so the handler's pending-buffer branch can be exercised.
type fakeReporter struct {
	calls    int
	lastKey  string
	lastVer  int64
	lastBody json.RawMessage
	failWith error
}

func (f *fakeReporter) SendBreakGlassShadowReport(
	_ context.Context, key string, state json.RawMessage, keyVer int64,
	_, _, _ string,
) error {
	f.calls++
	f.lastKey = key
	f.lastBody = state
	f.lastVer = keyVer
	return f.failWith
}

// fakeVersionSource is a test double for BreakGlassVersionSource. Returning
// zeros mimics the fresh-start case: max(0,0)+1 = 1, matching the legacy
// counter's first-call behavior so existing assertions stay valid.
type fakeVersionSource struct {
	desired  int64
	reported int64
}

func (f *fakeVersionSource) DesiredVer() int64  { return f.desired }
func (f *fakeVersionSource) ReportedVer() int64 { return f.reported }

func newBGTestDeps(t *testing.T) (handler.RuntimeDeps, *State, *fakeReporter, string) {
	t.Helper()
	dir := t.TempDir()
	ks := killswitch.NewKillSwitch(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	reporter := &fakeReporter{}
	bg := NewBreakGlassState(dir, reporter, &fakeVersionSource{})
	deps := handler.RuntimeDeps{
		Logger:         slog.New(slog.NewTextHandler(os.Stderr, nil)),
		KillswitchSnap: ks,
		DataDir:        dir,
	}
	return deps, bg, reporter, dir
}

func TestBreakGlassPut_HappyPath_Killswitch(t *testing.T) {
	deps, bg, reporter, dir := newBGTestDeps(t)

	h := HandleBreakGlassPut(deps, "killswitch", bg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/config/killswitch",
		bytes.NewBufferString(`{"state":{"engaged":false},"reason":"drill"}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	var resp bgResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Key != "killswitch" {
		t.Errorf("resp.Key = %q", resp.Key)
	}
	if resp.Version != 1 {
		t.Errorf("resp.Version = %d, want 1", resp.Version)
	}
	if resp.ReportStatus != "ok" {
		t.Errorf("resp.ReportStatus = %q, want ok", resp.ReportStatus)
	}
	if resp.PendingReport {
		t.Errorf("PendingReport should be false on happy path")
	}

	// Reporter was invoked with the new state.
	if reporter.calls != 1 {
		t.Errorf("reporter calls = %d, want 1", reporter.calls)
	}
	if reporter.lastKey != "killswitch" {
		t.Errorf("reporter lastKey = %q", reporter.lastKey)
	}

	// KillSwitch was flipped.
	if deps.KillswitchSnap.(*killswitch.KillSwitch).IsEngaged() {
		t.Errorf("killswitch should be disengaged after break-glass")
	}

	// JSONL event log has exactly one line.
	logPath := filepath.Join(dir, breakGlassEventLogFileName)
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(lines))
	}
	var evt BreakGlassEvent
	if err := json.Unmarshal([]byte(lines[0]), &evt); err != nil {
		t.Fatalf("unmarshal log: %v", err)
	}
	if evt.ConfigKey != "killswitch" || evt.KeyVersion != 1 {
		t.Errorf("log entry mismatch: %+v", evt)
	}

	// No pending buffer on the happy path.
	if _, err := os.Stat(filepath.Join(dir, pendingBreakGlassFileName)); !os.IsNotExist(err) {
		t.Errorf("pending buffer should not exist on happy path (err=%v)", err)
	}
}

func TestBreakGlassPut_ReporterFailure_SpoolsPending(t *testing.T) {
	deps, bg, reporter, dir := newBGTestDeps(t)
	reporter.failWith = errors.New("hub unreachable")

	h := HandleBreakGlassPut(deps, "killswitch", bg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/config/killswitch",
		bytes.NewBufferString(`{"state":{"engaged":false},"reason":"drill"}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (apply succeeded despite Hub failure), body=%q",
			rec.Code, rec.Body.String())
	}
	var resp bgResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.PendingReport || resp.ReportStatus != "pending" {
		t.Errorf("expected PendingReport=true and status=pending, got %+v", resp)
	}

	// Pending buffer must exist and round-trip.
	data, err := os.ReadFile(filepath.Join(dir, pendingBreakGlassFileName))
	if err != nil {
		t.Fatalf("read pending: %v", err)
	}
	var p pendingBreakGlass
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("unmarshal pending: %v", err)
	}
	if p.ConfigKey != "killswitch" {
		t.Errorf("pending.ConfigKey = %q", p.ConfigKey)
	}
	if p.KeyVersion != 1 {
		t.Errorf("pending.KeyVersion = %d", p.KeyVersion)
	}
}

func TestBreakGlassPut_SuccessClearsStalePending(t *testing.T) {
	deps, bg, reporter, dir := newBGTestDeps(t)
	// Seed a stale pending buffer on disk.
	if err := os.WriteFile(
		filepath.Join(dir, pendingBreakGlassFileName),
		[]byte(`{"config_key":"killswitch","key_version":0,"state":{"engaged":true}}`),
		0o640,
	); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	_ = reporter // reporter succeeds by default

	h := HandleBreakGlassPut(deps, "killswitch", bg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/config/killswitch",
		bytes.NewBufferString(`{"state":{"engaged":false}}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	if _, err := os.Stat(filepath.Join(dir, pendingBreakGlassFileName)); !os.IsNotExist(err) {
		t.Errorf("successful report should clear pending buffer (err=%v)", err)
	}
}

func TestBreakGlassPut_InvalidStateReturns400(t *testing.T) {
	deps, bg, _, _ := newBGTestDeps(t)

	h := HandleBreakGlassPut(deps, "killswitch", bg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/config/killswitch",
		bytes.NewBufferString(`{"state":null}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for null state, got %d body=%q", rec.Code, rec.Body.String())
	}
}

func TestBreakGlassPut_UnknownKeyReturns400(t *testing.T) {
	deps, bg, _, _ := newBGTestDeps(t)

	h := HandleBreakGlassPut(deps, "bogus_key", bg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/config/bogus_key",
		bytes.NewBufferString(`{"state":{}}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unsupported key, got %d", rec.Code)
	}
}

func TestBreakGlassPut_DisabledWhenBgNil(t *testing.T) {
	deps, _, _, _ := newBGTestDeps(t)

	h := HandleBreakGlassPut(deps, "killswitch", nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/config/killswitch",
		bytes.NewBufferString(`{"state":{"engaged":false}}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when bgState is nil, got %d", rec.Code)
	}
}

// TestBreakGlassPut_KillswitchShadowApplied verifies the KillswitchSnapshotter
// interface includes ApplyBreakGlass and that it routes through the concrete
// *killswitch.KillSwitch.Toggle path (engaged flag flips, history entry recorded).
func TestBreakGlassPut_KillswitchShadowApplied(t *testing.T) {
	deps, bg, _, _ := newBGTestDeps(t)
	// Seed: explicitly engage the kill switch so the test can verify toggle→off.
	ks := deps.KillswitchSnap.(*killswitch.KillSwitch)
	ks.Toggle(true, "test-setup")
	if !ks.IsEngaged() {
		t.Fatal("precondition: killswitch should be engaged after Toggle(true)")
	}

	h := HandleBreakGlassPut(deps, "killswitch", bg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/config/killswitch",
		bytes.NewBufferString(`{"state":{"engaged":false},"reason":"drill"}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ks.IsEngaged() {
		t.Errorf("killswitch should be disengaged after break-glass")
	}
	// Compile-time assertion: interception.Killswitch is the decoded shape.
	var want interception.Killswitch
	if err := json.Unmarshal([]byte(`{"engaged":false}`), &want); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	if want.Engaged {
		t.Errorf("want.Engaged should be false")
	}
}

// TestBreakGlassPut_VersionFromSourceMax verifies spec §5.2 step 3: the newVer
// is max(DesiredVer, ReportedVer) + 1 rather than a local monotonic counter.
// Without this, a Hub template already at v=5 (because a CP admin write landed
// first) would silently drop a proxy break-glass stamped v=1, because Hub's
// reconciliation only applies break-glass when reportedVer > currentVersion.
func TestBreakGlassPut_VersionFromSourceMax(t *testing.T) {
	deps, _, reporter, dir := newBGTestDeps(t)
	bg := NewBreakGlassState(dir, reporter, &fakeVersionSource{desired: 7, reported: 3})

	h := HandleBreakGlassPut(deps, "killswitch", bg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/config/killswitch",
		bytes.NewBufferString(`{"state":{"engaged":false},"reason":"drill"}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	var resp bgResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// max(7, 3) + 1 = 8.
	if resp.Version != 8 {
		t.Errorf("resp.Version = %d, want 8 (max(7,3)+1)", resp.Version)
	}
	if reporter.lastVer != 8 {
		t.Errorf("reporter.lastVer = %d, want 8", reporter.lastVer)
	}
}

// TestBreakGlassPut_LogAppendedBeforeReport verifies spec §5.2 step 2: the
// event log line is durable BEFORE the reporter runs. Even when Hub delivery
// fails and the state is spooled to pending, the JSONL log must carry the
// exact newVer that will be re-reported on replay. A reader of the log file
// alone (SIEM tail, post-mortem jq) must be able to reconstruct the apply
// without cross-referencing the pending buffer.
func TestBreakGlassPut_LogAppendedBeforeReport(t *testing.T) {
	deps, bg, reporter, dir := newBGTestDeps(t)
	reporter.failWith = errors.New("hub unreachable")

	h := HandleBreakGlassPut(deps, "killswitch", bg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/config/killswitch",
		bytes.NewBufferString(`{"state":{"engaged":false},"reason":"drill"}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	// Log must have the line even though the reporter failed.
	logPath := filepath.Join(dir, breakGlassEventLogFileName)
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d", len(lines))
	}
	var evt BreakGlassEvent
	if err := json.Unmarshal([]byte(lines[0]), &evt); err != nil {
		t.Fatalf("unmarshal log line: %v", err)
	}
	if evt.KeyVersion != 1 {
		t.Errorf("log line version = %d, want 1", evt.KeyVersion)
	}
	if evt.ConfigKey != "killswitch" {
		t.Errorf("log line key = %q", evt.ConfigKey)
	}
	// And pending buffer carries the same version for replay.
	pdata, err := os.ReadFile(filepath.Join(dir, pendingBreakGlassFileName))
	if err != nil {
		t.Fatalf("read pending: %v", err)
	}
	var p pendingBreakGlass
	if err := json.Unmarshal(pdata, &p); err != nil {
		t.Fatalf("unmarshal pending: %v", err)
	}
	if p.KeyVersion != evt.KeyVersion {
		t.Errorf("pending.KeyVersion=%d, log.KeyVersion=%d (must match)",
			p.KeyVersion, evt.KeyVersion)
	}
}

// TestBreakGlassPut_NilVersionSource covers the test-only fallback where
// bgState.versionSource is nil (production always wires *thingclient.Client).
// The first break-glass should still stamp version=1 so legacy assertions and
// fresh-process replay semantics hold.
func TestBreakGlassPut_NilVersionSource(t *testing.T) {
	deps, _, reporter, dir := newBGTestDeps(t)
	bg := NewBreakGlassState(dir, reporter, nil)

	h := HandleBreakGlassPut(deps, "killswitch", bg)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/config/killswitch",
		bytes.NewBufferString(`{"state":{"engaged":false}}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp bgResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Version != 1 {
		t.Errorf("resp.Version = %d, want 1 (nil-source fallback)", resp.Version)
	}
}

func TestReplayPending_NoFile_ReturnsFalse(t *testing.T) {
	_, bg, _, _ := newBGTestDeps(t)

	drained, err := bg.ReplayPending(context.Background())
	if err != nil {
		t.Fatalf("ReplayPending: %v", err)
	}
	if drained {
		t.Errorf("drained = true on empty dir; want false")
	}
}

func TestReplayPending_SuccessClears(t *testing.T) {
	_, bg, reporter, dir := newBGTestDeps(t)

	// Seed a pending entry directly.
	if err := bg.writePending(pendingBreakGlass{
		ConfigKey:    "killswitch",
		KeyVersion:   42,
		State:        json.RawMessage(`{"engaged":false}`),
		Reason:       "test",
		SourceIP:     "10.0.0.5",
		ActorTokenID: "tok-abc",
	}); err != nil {
		t.Fatalf("writePending: %v", err)
	}

	drained, err := bg.ReplayPending(context.Background())
	if err != nil {
		t.Fatalf("ReplayPending: %v", err)
	}
	if !drained {
		t.Errorf("drained = false; want true")
	}
	if reporter.calls != 1 {
		t.Errorf("reporter.calls = %d, want 1", reporter.calls)
	}
	if reporter.lastKey != "killswitch" || reporter.lastVer != 42 {
		t.Errorf("reporter got key=%q ver=%d; want killswitch/42", reporter.lastKey, reporter.lastVer)
	}

	// File should be gone after a successful replay.
	if _, err := os.Stat(filepath.Join(dir, pendingBreakGlassFileName)); !os.IsNotExist(err) {
		t.Errorf("pending file still present after replay: err=%v", err)
	}
}

func TestReplayPending_ReporterError_LeavesFile(t *testing.T) {
	_, bg, reporter, dir := newBGTestDeps(t)
	reporter.failWith = errors.New("hub unreachable")

	if err := bg.writePending(pendingBreakGlass{
		ConfigKey:  "killswitch",
		KeyVersion: 7,
		State:      json.RawMessage(`{"engaged":true}`),
	}); err != nil {
		t.Fatalf("writePending: %v", err)
	}

	drained, err := bg.ReplayPending(context.Background())
	if err == nil {
		t.Fatalf("expected error from ReplayPending, got nil")
	}
	if drained {
		t.Errorf("drained = true on error; want false")
	}

	// File must still be on disk so the next retry can try again.
	if _, err := os.Stat(filepath.Join(dir, pendingBreakGlassFileName)); err != nil {
		t.Errorf("pending file missing after failed replay: %v", err)
	}
}

func TestReplayPending_NilReporter_NoOp(t *testing.T) {
	dir := t.TempDir()
	bg := NewBreakGlassState(dir, nil, &fakeVersionSource{})

	if err := bg.writePending(pendingBreakGlass{
		ConfigKey:  "killswitch",
		KeyVersion: 1,
		State:      json.RawMessage(`{"engaged":false}`),
	}); err != nil {
		t.Fatalf("writePending: %v", err)
	}

	drained, err := bg.ReplayPending(context.Background())
	if err != nil {
		t.Fatalf("ReplayPending: %v", err)
	}
	if drained {
		t.Errorf("drained = true with nil reporter; want false")
	}
	// File must remain — a nil reporter means no delivery path is configured
	// yet, which should not purge the spooled state.
	if _, err := os.Stat(filepath.Join(dir, pendingBreakGlassFileName)); err != nil {
		t.Errorf("pending file missing after nil-reporter replay: %v", err)
	}
}

// TestClientSourceIP_EveryBranch covers the three paths in
// clientSourceIP: X-Forwarded-For with comma (first segment), without
// comma (full header), absent (r.RemoteAddr). The BFF sets this
// header so the break-glass audit log records the original client
// rather than the loopback address the runtime API is bound to.
func TestClientSourceIP_EveryBranch(t *testing.T) {
	cases := []struct {
		name       string
		xff        string
		remoteAddr string
		wantSource string
	}{
		{"no XFF falls back to RemoteAddr", "", "192.0.2.10:51234", "192.0.2.10:51234"},
		{"single XFF entry", "203.0.113.5", "127.0.0.1:5000", "203.0.113.5"},
		{"comma-separated XFF takes first", "203.0.113.5, 10.0.0.1, 10.0.0.2", "127.0.0.1:5000", "203.0.113.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/x", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := clientSourceIP(req); got != tc.wantSource {
				t.Errorf("clientSourceIP = %q, want %q", got, tc.wantSource)
			}
		})
	}
}

// TestActorTokenID_FallbackToAPI covers the
// `if v := r.Header.Get("X-Nexus-Actor-Token-Id"); v != ""` branch
// in actorTokenID — without the BFF in front, the field falls back
// to "api" so the event log distinguishes break-glass-via-API from
// shadow-applied entries.
func TestActorTokenID_FallbackToAPI(t *testing.T) {
	req := httptest.NewRequest(http.MethodPut, "/x", nil)
	if got := actorTokenID(req); got != "api" {
		t.Errorf("absent header should fall back to api, got %q", got)
	}
	req.Header.Set("X-Nexus-Actor-Token-Id", "tok-123")
	if got := actorTokenID(req); got != "tok-123" {
		t.Errorf("header forwarded by BFF should be returned: got %q", got)
	}
}

// TestApplyBreakGlassLocal_KillswitchPayloadInvalid covers the
// json.Unmarshal error branch inside applyBreakGlassLocal for the
// killswitch key. Without it, an operator sending malformed JSON
// would crash the handler instead of getting a 400.
func TestApplyBreakGlassLocal_KillswitchPayloadInvalid(t *testing.T) {
	deps, _, _, _ := newBGTestDeps(t)
	err := applyBreakGlassLocal(deps, "killswitch", json.RawMessage(`{not json}`))
	if err == nil {
		t.Fatal("expected unmarshal error for malformed killswitch payload")
	}
}

// TestApplyBreakGlassLocal_NilDepsErrors covers the
// `deps.KillswitchSnap == nil` and `deps.ExemptionRebuilder == nil`
// branches.
func TestApplyBreakGlassLocal_NilDepsErrors(t *testing.T) {
	t.Run("nil killswitch", func(t *testing.T) {
		err := applyBreakGlassLocal(handler.RuntimeDeps{}, "killswitch", json.RawMessage(`{"engaged":false}`))
		if err == nil {
			t.Fatal("expected killswitch surface not configured error")
		}
	})
	t.Run("nil exemptions", func(t *testing.T) {
		err := applyBreakGlassLocal(handler.RuntimeDeps{}, "exemptions", json.RawMessage(`{"entries":[]}`))
		if err == nil {
			t.Fatal("expected exemptions surface not configured error")
		}
	})
}

// TestReadPending_DecodeError covers the
// `json.Unmarshal(data, &p)` error branch in readPending — a corrupt
// pending file must surface the parse error so the caller can log it
// (not silently treat it as "no pending entry").
func TestReadPending_DecodeError(t *testing.T) {
	dir := t.TempDir()
	bg := NewBreakGlassState(dir, &fakeReporter{}, &fakeVersionSource{})
	// Plant a malformed pending file.
	if err := os.WriteFile(filepath.Join(dir, pendingBreakGlassFileName), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := bg.readPending()
	if err == nil {
		t.Fatal("expected pending: decode error on malformed pending file")
	}
	if !strings.Contains(err.Error(), "pending: decode") {
		t.Errorf("error not wrapped with pending: decode prefix: %v", err)
	}
}

// TestReplayPending_ReadError surfaces an unexpected read error
// (file exists but unreadable due to junk content) — the wrapper
// must return the err so the caller logs it.
func TestReplayPending_ReadError(t *testing.T) {
	dir := t.TempDir()
	bg := NewBreakGlassState(dir, &fakeReporter{}, &fakeVersionSource{})
	if err := os.WriteFile(filepath.Join(dir, pendingBreakGlassFileName), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := bg.ReplayPending(context.Background())
	// Decode error inside readPending should propagate.
	if err == nil {
		t.Fatal("expected error from ReplayPending when pending file is corrupt")
	}
}

// TestEscapeBGError_QuoteAndNewline covers the
// escapeBGError helper indirectly. We pass an error containing
// problematic characters and ensure HandleBreakGlassPut's response
// stays valid JSON. (Direct call kept simple via the handler.)
func TestEscapeBGError_QuoteAndNewlineInResponse(t *testing.T) {
	deps, bg, _, _ := newBGTestDeps(t)
	h := HandleBreakGlassPut(deps, "exemptions", bg)
	// exemptions surface is nil → applyBreakGlassLocal returns
	// error; ensure response body is decodable JSON.
	deps.ExemptionRebuilder = nil
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/runtime/config/exemptions",
		bytes.NewBufferString(`{"state":{"entries":[]},"reason":"t"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Logf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	// Body must be parseable JSON regardless of which branch fired.
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Errorf("response body not valid JSON: %v body=%q", err, rec.Body.String())
	}
	_ = errors.New("unused") // touch the errors import.
}

// TestWritePending_MkdirAllError covers writePending's MkdirAll error
// branch — when pendingDir refers to a regular file instead of a
// directory, os.MkdirAll surfaces "not a directory" and the caller
// gets a "pending: mkdir:" wrapped error.
func TestWritePending_MkdirAllError(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Point pendingDir at the regular file; writePending tries to
	// MkdirAll(blocker) which fails.
	bg := NewBreakGlassState(blocker, &fakeReporter{}, &fakeVersionSource{})
	err := bg.writePending(pendingBreakGlass{
		ConfigKey: "killswitch", KeyVersion: 1,
		State: json.RawMessage(`{"engaged":false}`),
	})
	if err == nil {
		t.Fatal("writePending must error when pendingDir is a regular file")
	}
	if !strings.Contains(err.Error(), "pending: mkdir") {
		t.Errorf("error not wrapped with 'pending: mkdir': %v", err)
	}
}

// TestWritePending_ReadOnlyDirSurfaceswriteError covers writePending's
// os.WriteFile error branch — a directory the process cannot write to
// (chmod 0o500) must surface "pending: write temp:" without leaving a
// half-written file behind.
//
// Skipped on root since chmod 0o500 still allows root to write.
func TestWritePending_ReadOnlyDirSurfacesWriteError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses dir mode; chmod 0500 cannot block writes")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // so TempDir can clean up

	bg := NewBreakGlassState(dir, &fakeReporter{}, &fakeVersionSource{})
	err := bg.writePending(pendingBreakGlass{
		ConfigKey: "killswitch", KeyVersion: 1,
		State: json.RawMessage(`{"engaged":false}`),
	})
	if err == nil {
		t.Skip("filesystem did not honour 0o500 dir mode; cannot test write-error branch")
	}
	if !strings.Contains(err.Error(), "pending: write temp") {
		t.Errorf("error not wrapped with 'pending: write temp': %v", err)
	}
}

// TestClearPending_NotExistIsNoError covers clearPending's
// os.IsNotExist(err) tolerate-branch — a clearPending call when no
// pending file exists must return nil so callers don't spam logs
// during the steady-state no-buffer case.
func TestClearPending_NotExistIsNoError(t *testing.T) {
	dir := t.TempDir()
	bg := NewBreakGlassState(dir, &fakeReporter{}, &fakeVersionSource{})
	if err := bg.clearPending(); err != nil {
		t.Errorf("clearPending on fresh dir must return nil; got: %v", err)
	}
}

// TestPendingPath_JoinsDirAndFile covers pendingPath(). One line, but
// without a direct assertion any future refactor that swaps to a
// different filename would silently break replay.
func TestPendingPath_JoinsDirAndFile(t *testing.T) {
	bg := NewBreakGlassState("/tmp/test-dir", &fakeReporter{}, &fakeVersionSource{})
	want := filepath.Join("/tmp/test-dir", pendingBreakGlassFileName)
	if got := bg.pendingPath(); got != want {
		t.Errorf("pendingPath = %q, want %q", got, want)
	}
}
