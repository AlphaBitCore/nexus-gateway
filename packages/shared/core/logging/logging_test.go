package logging

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewLogger_FileTee(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "svc.log")

	logger, err := NewLogger(Config{
		Level:  "info",
		Format: "json",
		File:   logPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	logger.Info("hello", slog.String("k", "v"))

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"msg":"hello"`)) && !bytes.Contains(data, []byte(`hello`)) {
		t.Fatalf("log file missing message: %s", data)
	}
}

func TestNewLogger_StackOnError(t *testing.T) {
	var buf bytes.Buffer
	// Build handler manually like NewLogger but with buffer only for deterministic test.
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	base := slog.NewJSONHandler(&buf, opts)
	h := &errorStackHandler{inner: base}
	logger := slog.New(h)

	logger.Error("boom", slog.String("error", "x"))

	out := buf.String()
	if !strings.Contains(out, `"stack"`) {
		t.Fatalf("expected stack field in log line: %s", out)
	}
	if !strings.Contains(out, "boom") {
		t.Fatalf("expected message in output: %s", out)
	}
}

// TestErrorStackHandler_NoStackOptOut asserts that an Error-level record
// carrying `noStack=true` skips the goroutine stack injection. Used by
// scheduled data-state checks (e.g. audit_freshness_check) that surface
// at ERROR for diag-event routing but aren't programming errors.
func TestErrorStackHandler_NoStackOptOut(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(&errorStackHandler{inner: base})

	logger.Error("data-state", slog.String("event", "x"), slog.Bool("noStack", true))

	out := buf.String()
	if strings.Contains(out, `"stack"`) {
		t.Fatalf("expected NO stack field with noStack=true: %s", out)
	}
	if !strings.Contains(out, `"data-state"`) {
		t.Fatalf("expected message preserved: %s", out)
	}
	if !strings.Contains(out, `"noStack":true`) {
		t.Fatalf("expected noStack attr preserved on the record: %s", out)
	}

	// Sanity: noStack=false must NOT suppress the stack.
	buf.Reset()
	logger.Error("still-stack", slog.Bool("noStack", false))
	if !strings.Contains(buf.String(), `"stack"`) {
		t.Fatalf("noStack=false should not suppress stack: %s", buf.String())
	}

	// Sanity: a non-bool attribute named "noStack" must NOT suppress.
	buf.Reset()
	logger.Error("still-stack-2", slog.String("noStack", "true"))
	if !strings.Contains(buf.String(), `"stack"`) {
		t.Fatalf("noStack as string should not suppress stack: %s", buf.String())
	}
}

func TestNewLogger_LOGFileEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "from-env.log")
	t.Setenv("LOG_FILE", logPath)
	t.Cleanup(func() { _ = os.Unsetenv("LOG_FILE") })

	logger, err := NewLogger(Config{Level: "info", Format: "json", File: "ignored.log"})
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("from-env")

	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("expected env log file: %v", err)
	}
}

func TestTruthyEnv(t *testing.T) {
	if !truthyEnv("TRUE") || !truthyEnv("1") || !truthyEnv("yes") {
		t.Fatal("expected true")
	}
	if truthyEnv("false") || truthyEnv("0") || truthyEnv("no") {
		t.Fatal("expected false")
	}
}

// TestSetLevel_ToggleVisibility asserts SetLevel(...) flips the
// effective filter live: a Debug record suppressed at Info becomes
// visible after SetLevel("debug") on the SAME logger instance, and
// goes silent again after SetLevel("info"). This is the load-bearing
// guarantee for the log_level shadow key — handler chain is built
// once at NewLogger and the level swap must take effect without
// rebuilding it.
func TestSetLevel_ToggleVisibility(t *testing.T) {
	prev := CurrentLevel()
	t.Cleanup(func() { currentLevel.Set(prev) })

	// Construct a logger that writes to a buffer instead of stdout so we
	// can assert on the record set deterministically. We mirror what
	// NewLogger does at the handler level so &currentLevel is the
	// leveler.
	var buf bytes.Buffer
	opts := &slog.HandlerOptions{Level: &currentLevel}
	logger := slog.New(slog.NewJSONHandler(&buf, opts))

	// Start at Info; a Debug record must be suppressed.
	SetLevel("info")
	logger.Debug("hidden-at-info")
	if strings.Contains(buf.String(), "hidden-at-info") {
		t.Fatalf("Debug record leaked at Info level: %s", buf.String())
	}

	// Flip to Debug; the SAME logger must now emit the next Debug.
	SetLevel("debug")
	buf.Reset()
	logger.Debug("visible-at-debug")
	if !strings.Contains(buf.String(), "visible-at-debug") {
		t.Fatalf("Debug record dropped at Debug level: %s", buf.String())
	}

	// Flip back to Info; Debug records suppressed again on the same
	// handler instance — proves the leveler is consulted per record,
	// not cached.
	SetLevel("info")
	buf.Reset()
	logger.Debug("hidden-again")
	if strings.Contains(buf.String(), "hidden-again") {
		t.Fatalf("Debug record leaked after SetLevel back to Info: %s", buf.String())
	}
}

// TestSetLevel_UnknownDegradesToInfo asserts a misspelled shadow
// payload falls back to Info instead of breaking logging. Matches the
// ParseLevel contract used everywhere else.
func TestSetLevel_UnknownDegradesToInfo(t *testing.T) {
	prev := CurrentLevel()
	t.Cleanup(func() { currentLevel.Set(prev) })

	SetLevel("not-a-level")
	if got := CurrentLevel(); got != slog.LevelInfo {
		t.Fatalf("CurrentLevel after bad input = %v, want Info", got)
	}
}

// TestSetLevel_AcceptsTrace asserts the custom TRACE level
// round-trips through SetLevel.
func TestSetLevel_AcceptsTrace(t *testing.T) {
	prev := CurrentLevel()
	t.Cleanup(func() { currentLevel.Set(prev) })

	applied := SetLevel("trace")
	if applied != LevelTrace {
		t.Fatalf("SetLevel(trace) returned %v, want LevelTrace (%v)", applied, LevelTrace)
	}
	if got := CurrentLevel(); got != LevelTrace {
		t.Fatalf("CurrentLevel = %v, want LevelTrace", got)
	}
}

// TestParseLevel_AllBranches covers every alias including warn/warning/error
// so a misspelled shadow payload (or YAML config) never silently disables a
// production log level.
func TestParseLevel_AllBranches(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"trace", LevelTrace},
		{"TRACE", LevelTrace},
		{"debug", slog.LevelDebug},
		{"Debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"WARN", slog.LevelWarn},
		{"error", slog.LevelError},
		{"ERROR", slog.LevelError},
		{"garbage", slog.LevelInfo},
	}
	for _, c := range cases {
		if got := ParseLevel(c.in); got != c.want {
			t.Fatalf("ParseLevel(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestNewLogger_TextFormat asserts cfg.Format="text" selects the text handler
// (key=value style) instead of JSON; verifying observable output shape, not
// just construction success.
func TestNewLogger_TextFormat(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "text.log")

	logger, err := NewLogger(Config{Level: "info", Format: "text", File: logPath})
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("text-fmt-marker", slog.String("k", "v"))

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	// Text handler writes key=value, not JSON braces.
	if !strings.Contains(out, "msg=text-fmt-marker") {
		t.Fatalf("text handler expected key=value `msg=text-fmt-marker`, got: %s", out)
	}
	if strings.Contains(out, `"msg":"text-fmt-marker"`) {
		t.Fatalf("text handler unexpectedly emitted JSON: %s", out)
	}
	if !strings.Contains(out, "k=v") {
		t.Fatalf("text handler missing attribute k=v: %s", out)
	}
}

// TestNewLogger_StackOnErrorWiresWrapper covers the StackOnError cfg branch
// end-to-end through NewLogger (not just via direct errorStackHandler
// instantiation) so the wrapper actually fires on Error records.
func TestNewLogger_StackOnErrorWiresWrapper(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "stack.log")

	logger, err := NewLogger(Config{
		Level:        "info",
		Format:       "json",
		File:         logPath,
		StackOnError: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	logger.Error("err-with-stack")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"stack"`) {
		t.Fatalf("NewLogger(StackOnError=true) did not wrap with errorStackHandler: %s", data)
	}
}

// TestNewLogger_LOGStackOnErrorEnvOverrides asserts the LOG_STACK_ON_ERROR
// env var overrides cfg.StackOnError, matching the documented contract.
func TestNewLogger_LOGStackOnErrorEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "stack-env.log")
	t.Setenv("LOG_STACK_ON_ERROR", "true")

	logger, err := NewLogger(Config{
		Level:        "info",
		Format:       "json",
		File:         logPath,
		StackOnError: false, // cfg says off; env must flip on
	})
	if err != nil {
		t.Fatal(err)
	}
	logger.Error("boom-from-env")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"stack"`) {
		t.Fatalf("LOG_STACK_ON_ERROR=true did not enable stack wrapper: %s", data)
	}
}

// TestNewLogger_LOGStackOnErrorEnvFalsy covers the LOG_STACK_ON_ERROR=false
// branch: env present but truthyEnv returns false. Asserts the wrapper is
// disabled even though the env var is set.
func TestNewLogger_LOGStackOnErrorEnvFalsy(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "stack-env-off.log")
	t.Setenv("LOG_STACK_ON_ERROR", "false")

	logger, err := NewLogger(Config{
		Level:        "info",
		Format:       "json",
		File:         logPath,
		StackOnError: true, // cfg on, but env "false" must override to off
	})
	if err != nil {
		t.Fatal(err)
	}
	logger.Error("no-stack-here")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"stack"`) {
		t.Fatalf("LOG_STACK_ON_ERROR=false should disable wrapper: %s", data)
	}
}

// TestNewLogger_MkdirAllError surfaces the parent-directory creation error
// path: target log path lives under a regular file masquerading as a dir, so
// MkdirAll fails and NewLogger returns the error.
func TestNewLogger_MkdirAllError(t *testing.T) {
	dir := t.TempDir()
	// Create a regular file at the path that would be the parent dir of the
	// log file — MkdirAll then fails because the path exists but is not a
	// directory.
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(blocker, "child", "svc.log")

	_, err := NewLogger(Config{Level: "info", Format: "json", File: logPath})
	if err == nil {
		t.Fatalf("expected error when parent dir cannot be created, got nil")
	}
}

// TestNewLogger_OpenFileError surfaces the OpenFile error path: target path
// is itself an existing directory, so OpenFile(O_WRONLY) fails.
func TestNewLogger_OpenFileError(t *testing.T) {
	dir := t.TempDir()
	// Pass a directory path as the log file — MkdirAll on its parent
	// succeeds (parent exists), but OpenFile of a directory in write mode
	// fails.
	_, err := NewLogger(Config{Level: "info", Format: "json", File: dir})
	if err == nil {
		t.Fatalf("expected OpenFile error for directory-as-file path, got nil")
	}
}

// TestNewLogger_TraceLevelNameInOutput asserts the ReplaceAttr branch that
// rewrites slog.LevelKey to "TRACE" when the record level == LevelTrace.
// We route output through a tee'd file (NewLogger always writes stdout, plus
// File when set), then parse the file for the level token.
func TestNewLogger_TraceLevelNameInOutput(t *testing.T) {
	prev := CurrentLevel()
	t.Cleanup(func() { currentLevel.Set(prev) })

	dir := t.TempDir()
	logPath := filepath.Join(dir, "trace.log")

	logger, err := NewLogger(Config{Level: "trace", Format: "json", File: logPath})
	if err != nil {
		t.Fatal(err)
	}
	logger.Log(context.Background(), LevelTrace, "trace-line")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	if !strings.Contains(out, `"level":"TRACE"`) {
		t.Fatalf("expected level=TRACE in output, got: %s", out)
	}
	if !strings.Contains(out, "trace-line") {
		t.Fatalf("expected message in trace output: %s", out)
	}
}

// TestErrorStackHandler_WithAttrsAndGroup covers the wrapper passthroughs:
// attaching attrs / groups through the wrapper must (a) keep the
// errorStackHandler in the chain so Error records still get a "stack" attr
// and (b) propagate the original attr/group through the inner handler.
func TestErrorStackHandler_WithAttrsAndGroup(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	h := slog.Handler(&errorStackHandler{inner: base})

	withAttrs := h.WithAttrs([]slog.Attr{slog.String("svc", "control-plane")})
	if _, ok := withAttrs.(*errorStackHandler); !ok {
		t.Fatalf("WithAttrs must preserve errorStackHandler wrapper, got %T", withAttrs)
	}
	withGroup := withAttrs.WithGroup("req")
	if _, ok := withGroup.(*errorStackHandler); !ok {
		t.Fatalf("WithGroup must preserve errorStackHandler wrapper, got %T", withGroup)
	}

	logger := slog.New(withGroup)
	logger.Error("boom", slog.String("reason", "x"))

	out := buf.String()
	if !strings.Contains(out, `"svc":"control-plane"`) {
		t.Fatalf("WithAttrs attr lost: %s", out)
	}
	if !strings.Contains(out, `"req":{`) {
		t.Fatalf("WithGroup grouping lost: %s", out)
	}
	if !strings.Contains(out, `"stack"`) {
		t.Fatalf("stack attr missing after WithAttrs/WithGroup chain: %s", out)
	}
}

// TestErrorStackHandler_Enabled asserts Enabled delegates to inner with the
// inner's level filter respected.
func TestErrorStackHandler_Enabled(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	h := &errorStackHandler{inner: base}

	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatalf("Enabled(Info) at base=Warn should be false")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Fatalf("Enabled(Error) at base=Warn should be true")
	}
}

// TestErrorStackHandler_NoStackBelowError asserts Warn/Info records do NOT
// get a "stack" attribute — the wrapper only fires at LevelError+.
func TestErrorStackHandler_NoStackBelowError(t *testing.T) {
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	h := &errorStackHandler{inner: base}
	logger := slog.New(h)

	logger.Warn("just-a-warning")
	if strings.Contains(buf.String(), `"stack"`) {
		t.Fatalf("Warn record should not carry stack attr: %s", buf.String())
	}
}

// captureHandler is an io.Writer-backed slog handler used to assert exact log
// attributes from HTTPRequestLogger.
func newBufLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// TestHTTPRequestLogger_200 asserts the middleware logs method/path/status=200
// plus duration + remoteAddr for a successful request.
func TestHTTPRequestLogger_200(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufLogger(&buf)

	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := httptest.NewServer(HTTPRequestLogger(logger)(inner))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if !called {
		t.Fatal("inner handler was not invoked")
	}
	out := buf.String()
	if !strings.Contains(out, `"msg":"http request"`) {
		t.Fatalf("missing msg: %s", out)
	}
	if !strings.Contains(out, `"method":"GET"`) {
		t.Fatalf("missing method: %s", out)
	}
	if !strings.Contains(out, `"path":"/healthz"`) {
		t.Fatalf("missing path: %s", out)
	}
	if !strings.Contains(out, `"status":200`) {
		t.Fatalf("missing status: %s", out)
	}
	if !strings.Contains(out, `"duration"`) {
		t.Fatalf("missing duration: %s", out)
	}
	if !strings.Contains(out, `"remoteAddr"`) {
		t.Fatalf("missing remoteAddr: %s", out)
	}
}

// TestHTTPRequestLogger_CapturesNon200 asserts WriteHeader propagates the
// non-200 status code into the logged "status" field.
func TestHTTPRequestLogger_CapturesNon200(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufLogger(&buf)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	srv := httptest.NewServer(HTTPRequestLogger(logger)(inner))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/teapot")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("inner status not propagated to client: got %d", resp.StatusCode)
	}
	if !strings.Contains(buf.String(), `"status":418`) {
		t.Fatalf("status 418 not in log line: %s", buf.String())
	}
}

// TestHTTPRequestLogger_DefaultStatus asserts that when the inner handler
// never calls WriteHeader (status stays at the constructor default), the log
// line records 200 — the documented default contract.
func TestHTTPRequestLogger_DefaultStatus(t *testing.T) {
	var buf bytes.Buffer
	logger := newBufLogger(&buf)

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("body-without-explicit-header"))
	})
	srv := httptest.NewServer(HTTPRequestLogger(logger)(inner))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/default")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if !strings.Contains(buf.String(), `"status":200`) {
		t.Fatalf("default status 200 missing: %s", buf.String())
	}
}

// flushRecorder is a httptest.ResponseRecorder that records Flush() calls so
// we can assert statusCapture.Flush forwards to the underlying writer.
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed int
}

func (f *flushRecorder) Flush() { f.flushed++ }

// TestStatusCapture_FlushForwards asserts statusCapture.Flush invokes the
// underlying ResponseWriter's Flush when it implements http.Flusher.
func TestStatusCapture_FlushForwards(t *testing.T) {
	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	sc := &statusCapture{ResponseWriter: rec, status: http.StatusOK}

	// statusCapture must itself satisfy http.Flusher.
	var _ http.Flusher = sc
	sc.Flush()

	if rec.flushed != 1 {
		t.Fatalf("expected underlying Flush() to be called once, got %d", rec.flushed)
	}
}

// TestStatusCapture_FlushNoFlusher asserts Flush is a safe no-op when the
// underlying ResponseWriter does NOT implement http.Flusher (defensive
// branch).
func TestStatusCapture_FlushNoFlusher(t *testing.T) {
	// nonFlusherRW intentionally omits Flush; only the bare ResponseWriter
	// surface is implemented.
	sc := &statusCapture{ResponseWriter: &nonFlusherRW{header: http.Header{}}, status: http.StatusOK}
	sc.Flush() // must not panic
}

type nonFlusherRW struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (n *nonFlusherRW) Header() http.Header         { return n.header }
func (n *nonFlusherRW) Write(b []byte) (int, error) { return n.body.Write(b) }
func (n *nonFlusherRW) WriteHeader(code int)        { n.status = code }

// TestStatusCapture_Unwrap asserts Unwrap returns the inner ResponseWriter
// so http.ResponseController can reach it.
func TestStatusCapture_Unwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	sc := &statusCapture{ResponseWriter: rec, status: http.StatusOK}
	if got := sc.Unwrap(); got != rec {
		t.Fatalf("Unwrap = %v, want %v", got, rec)
	}
}

// TestTrimmedStack_FiltersInfraFrames asserts trimmedStack removes
// runtime/debug.Stack, log/slog., and shared/logging frames from the captured
// stack, leaving the business call site as the first non-header line.
func TestTrimmedStack_FiltersInfraFrames(t *testing.T) {
	s := trimmedStack()
	if s == "" {
		t.Fatal("trimmedStack returned empty")
	}
	if strings.Contains(s, "runtime/debug.Stack") {
		t.Fatalf("debug.Stack frame not filtered: %s", s)
	}
	if strings.Contains(s, "log/slog.") {
		t.Fatalf("log/slog frame not filtered: %s", s)
	}
	// Header (goroutine N [running]:) must survive — it's lines[0].
	if !strings.HasPrefix(s, "goroutine ") {
		t.Fatalf("goroutine header lost: %s", s)
	}
}
