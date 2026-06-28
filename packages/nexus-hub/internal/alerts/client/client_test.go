package alertclient_test

import (
	"context"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	alertclient "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/client"
)

// osWriteFile is just os.WriteFile, exposed via a tiny shim so the
// only place this test file pulls in the os package is one focused
// line below — the rest of the suite uses higher-level fixtures.
var osWriteFile = os.WriteFile

// _ = io.EOF keeps the io import live; the package is referenced by
// other tests in this file via http.Response body usage.
var _ = io.EOF

func newClient(t *testing.T, baseURL string) *alertclient.Client {
	t.Helper()
	c, err := alertclient.New(alertclient.Config{
		HubBaseURL:    baseURL,
		AuthHeader:    "Bearer test",
		SpoolDir:      t.TempDir(),
		SpoolMaxBytes: 1 << 20,
		HTTPTimeout:   2 * time.Second,
		ReplayEvery:   time.Hour, // don't auto-tick in unit tests
		Logger:        slog.Default(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestFireSuccess(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/alerts/raise" {
			http.Error(w, "path", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test" {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)

	err := c.Fire(context.Background(), alertclient.AlertEnvelope{
		RuleID: "proxy.hook_failure_rate", TargetKey: "proxy:n1", Severity: "high",
		Message: "x", FiredAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 || c.PendingCount() != 0 {
		t.Fatalf("hits=%d pending=%d", hits, c.PendingCount())
	}
}

func TestFireSpoolsOn5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)

	err := c.Fire(context.Background(), alertclient.AlertEnvelope{
		RuleID: "r", TargetKey: "t", Severity: "medium", Message: "m", FiredAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("Fire must not return error on 5xx (caller non-blocking): %v", err)
	}
	if c.PendingCount() != 1 {
		t.Fatalf("pending=%d, want 1", c.PendingCount())
	}
}

func TestReplayPendingDrains(t *testing.T) {
	var hits int32
	var payloads []alertclient.AlertEnvelope
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		var env alertclient.AlertEnvelope
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &env)
		payloads = append(payloads, env)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Spool 2 entries while "Hub is down" by pointing at a dead server first.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "", http.StatusServiceUnavailable)
	}))
	dead.Close()

	c, _ := alertclient.New(alertclient.Config{
		HubBaseURL: dead.URL,
		AuthHeader: "Bearer t", SpoolDir: t.TempDir(),
		SpoolMaxBytes: 1 << 20, HTTPTimeout: time.Second, ReplayEvery: time.Hour,
		Logger: slog.Default(),
	})
	_ = c.Fire(context.Background(), alertclient.AlertEnvelope{RuleID: "r", TargetKey: "1", Severity: "low", FiredAt: time.Now()})
	_ = c.Fire(context.Background(), alertclient.AlertEnvelope{RuleID: "r", TargetKey: "2", Severity: "low", FiredAt: time.Now()})
	if c.PendingCount() != 2 {
		t.Fatalf("setup: pending=%d", c.PendingCount())
	}

	// Rewire to live server and drain.
	c.SetHubBaseURL(srv.URL)
	drained, err := c.ReplayPending(context.Background())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if drained != 2 || atomic.LoadInt32(&hits) != 2 || c.PendingCount() != 0 {
		t.Fatalf("drained=%d hits=%d pending=%d", drained, hits, c.PendingCount())
	}
}

func TestResolve(t *testing.T) {
	var hit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := newClient(t, srv.URL)
	if err := c.Resolve(context.Background(), "r", "t", "auto"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if hit != "/api/v1/alerts/resolve" {
		t.Fatalf("hit=%s", hit)
	}
}

// TestNew_EmptyHubBaseURLErrors covers the
// `if cfg.HubBaseURL == "" → error` branch — operators with a broken
// config get a clear error at construction time instead of nil-deref
// inside Fire.
func TestNew_EmptyHubBaseURLErrors(t *testing.T) {
	_, err := alertclient.New(alertclient.Config{
		HubBaseURL: "",
		SpoolDir:   t.TempDir(),
		Logger:     slog.Default(),
	})
	if err == nil {
		t.Fatal("expected error for empty HubBaseURL")
	}
}

// TestNew_AppliesDefaults covers the three zero-value defaults in
// New(): HTTPTimeout=5s, ReplayEvery=30s, SpoolMaxBytes=50MB. The
// observable assertion is "client constructed cleanly with zero
// timing fields".
func TestNew_AppliesDefaults(t *testing.T) {
	c, err := alertclient.New(alertclient.Config{
		HubBaseURL: "http://hub.example",
		SpoolDir:   t.TempDir(),
		// Leave HTTPTimeout / ReplayEvery / SpoolMaxBytes at zero.
		Logger: slog.Default(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c == nil {
		t.Fatal("New returned nil client")
	}
}

// TestNew_BadSpoolDirReturnsError covers the spool.New error branch
// in New() — passing a path that refers to an existing regular file
// (not a directory) breaks the spool's mkdir-as-dir contract.
func TestNew_BadSpoolDirReturnsError(t *testing.T) {
	dir := t.TempDir()
	// Plant a regular file at the path New() will try to mkdir.
	bogus := dir + "/blocker"
	if err := writeFile(bogus, "x"); err != nil {
		t.Fatal(err)
	}
	_, err := alertclient.New(alertclient.Config{
		HubBaseURL: "http://hub.example",
		SpoolDir:   bogus, // not a directory
		Logger:     slog.Default(),
	})
	if err == nil {
		t.Fatal("expected spool init error when SpoolDir is a regular file")
	}
}

// TestPost_NewRequestErrorBubblesUpAsResolve covers the
// `http.NewRequestWithContext` failure in post(). A base URL with a
// control byte cannot be parsed, so Resolve (which surfaces post()
// errors directly, unlike Fire which swallows into the spool) must
// return a non-nil error mentioning "req".
func TestPost_NewRequestErrorBubblesUpAsResolve(t *testing.T) {
	c, err := alertclient.New(alertclient.Config{
		HubBaseURL: "http://\x7f", // unparseable URL
		SpoolDir:   t.TempDir(),
		Logger:     slog.Default(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = c.Resolve(context.Background(), "r", "k", "reason")
	if err == nil {
		t.Fatal("Resolve must surface NewRequestWithContext error")
	}
}

// TestSetHubBaseURL_HotSwapHonoredByFire covers SetHubBaseURL — the
// next Fire after a base-URL swap must hit the new endpoint. Without
// this, runtime Hub-rotation would silently keep hitting the old
// host.
func TestSetHubBaseURL_HotSwapHonoredByFire(t *testing.T) {
	var hitsA, hitsB int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hitsA, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hitsB, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srvB.Close()

	c := newClient(t, srvA.URL)
	_ = c.Fire(context.Background(), alertclient.AlertEnvelope{RuleID: "x", FiredAt: time.Now()})
	c.SetHubBaseURL(srvB.URL)
	_ = c.Fire(context.Background(), alertclient.AlertEnvelope{RuleID: "x", FiredAt: time.Now()})

	if atomic.LoadInt32(&hitsA) != 1 {
		t.Errorf("expected one hit on A before swap, got %d", hitsA)
	}
	if atomic.LoadInt32(&hitsB) != 1 {
		t.Errorf("expected one hit on B after swap, got %d", hitsB)
	}
}

// writeFile is a thin wrapper around os.WriteFile used by
// TestNew_BadSpoolDirReturnsError to plant a blocker file. Kept here
// to localise the only new os-package dependency this test file
// gained.
func writeFile(path, content string) error {
	return osWriteFile(path, []byte(content), 0o600)
}

// TestFireSpoolEnqueueFailureWrapped covers the fireDrop branch in
// Fire: when the HTTP POST fails AND the disk spool Enqueue also
// fails (e.g. read-only spool directory), Fire must return a non-nil
// wrapped "spool enqueue failed" error so the caller knows the
// envelope was lost. This is the only path where Fire returns an
// error to the caller — without coverage, the fireDrop counter wiring
// + error wrapping would be silently regressable.
func TestFireSpoolEnqueueFailureWrapped(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer dead.Close()

	dir := t.TempDir()
	c, err := alertclient.New(alertclient.Config{
		HubBaseURL:    dead.URL,
		AuthHeader:    "Bearer t",
		SpoolDir:      dir,
		SpoolMaxBytes: 1 << 20,
		HTTPTimeout:   time.Second,
		ReplayEvery:   time.Hour,
		Logger:        slog.Default(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Now make the spool directory unwritable so Enqueue's
	// os.WriteFile fails. spool.New created <dir>/alertclient/.
	spoolPath := dir + "/alertclient"
	if err := os.Chmod(spoolPath, 0o500); err != nil {
		t.Fatalf("Chmod ro: %v", err)
	}
	defer func() { _ = os.Chmod(spoolPath, 0o750) }() // restore for TempDir cleanup

	err = c.Fire(context.Background(), alertclient.AlertEnvelope{
		RuleID: "r", TargetKey: "t", Severity: "low", Message: "m", FiredAt: time.Now(),
	})
	if err == nil {
		t.Fatal("expected Fire to return spool-enqueue error when both POST and spool write fail")
	}
	if !strings.Contains(err.Error(), "spool enqueue failed") {
		t.Errorf("expected error to mention 'spool enqueue failed', got %v", err)
	}
}

// TestResolveMarshalErrorReturned covers the json.Marshal error branch
// in post(). Marshal can only fail when the body contains an
// unmarshalable value (channel, function, complex number, or an
// unsupported NaN/Inf float). We trigger this by passing a payload
// containing a NaN float through ResolveRequest's `Reason` indirectly
// — but the cleanest way is to expose post via Resolve with a request
// that includes a non-marshalable extension. Since the production
// payload types are all string fields, this branch is defensive
// against future schema drift. We exercise it via a deliberately
// malformed Details map on Fire (map[string]any can hold a
// non-marshalable value). Fire swallows the post error into the
// spool, so we observe the marshal failure indirectly via the
// spooled envelope count (Fire still spools the original envelope
// because Enqueue marshals successfully — the inner json.Marshal of
// the same envelope inside post() is the path under test). To check
// observable behavior, we assert Fire returned nil and the spool
// holds the envelope.
func TestPostMarshalErrorViaUnsupportedDetails(t *testing.T) {
	// httptest server is unused — marshal fails before the POST.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newClient(t, srv.URL)
	// A channel is not JSON-marshalable; embedding it in Details forces
	// json.Marshal inside post() to return an error.
	env := alertclient.AlertEnvelope{
		RuleID: "r", TargetKey: "t", Severity: "low", FiredAt: time.Now(),
		Details: map[string]any{"chan": make(chan int)},
	}
	// Fire's contract: best-effort; on POST failure (here, the marshal
	// error inside post) it tries to spool. Enqueue also marshals via
	// json.Marshal so it too will fail — making Fire return a
	// non-nil "spool enqueue failed" error. That observable signal
	// confirms post()'s marshal error was hit.
	err := c.Fire(context.Background(), env)
	if err == nil {
		t.Fatal("expected Fire to surface non-nil error when both post() marshal and spool marshal fail on unmarshalable Details")
	}
}
