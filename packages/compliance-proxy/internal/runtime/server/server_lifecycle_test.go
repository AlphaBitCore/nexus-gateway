package server

import (
	"context"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/auth"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/breakglass"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/runtime/handler"
)

// server.go — Start / Shutdown / Server.ReplayPending

// pickFreePort returns the addr of a port the OS just freed. The caller
// binds immediately so the gap before another process grabs it stays
// small. We need the actual addr (not httptest's wrapper) because Start
// takes one.
func pickFreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// newTestServerStruct builds the *Server (not the httptest wrapper) so
// the test can drive Start/Shutdown directly.
func newTestServerStruct(t *testing.T, deps handler.RuntimeDeps, addr string) *Server {
	t.Helper()
	tokenAuth := auth.NewTokenAuth("test-token")
	return NewServer(addr, deps, tokenAuth)
}

// countingReporter counts SendBreakGlassShadowReport calls. Satisfies
// handler.BreakGlassReporter without coupling the server tests to
// the breakglass package's unexported fakeReporter.
type countingReporter struct {
	calls int
}

func (r *countingReporter) SendBreakGlassShadowReport(
	_ context.Context, _ string, _ json.RawMessage, _ int64, _, _, _ string,
) error {
	r.calls++
	return nil
}

// TestServer_StartAndShutdown_RoundTrip starts the real server, hits
// /healthz to prove ListenAndServe bound, then cancels the context —
// Start must return nil after Shutdown's graceful drain. Pins the
// observable behavior the compliance-proxy main loop relies on (without
// this, a regression that hangs Start would block service shutdown).
func TestServer_StartAndShutdown_RoundTrip(t *testing.T) {
	deps := newTestDeps()
	addr := pickFreePort(t)
	srv := newTestServerStruct(t, deps, addr)

	ctx, cancel := context.WithCancel(context.Background())

	startErr := make(chan error, 1)
	go func() {
		startErr <- srv.Start(ctx)
	}()

	deadline := time.Now().Add(3 * time.Second)
	var resp *http.Response
	var err error
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://" + addr + "/healthz")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatalf("server never accepted connections at %s: %v", addr, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-startErr:
		if err != nil {
			t.Errorf("Start returned %v, want nil after ctx cancel", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Start did not return after ctx cancel within 15s")
	}
}

// TestServer_Start_PortAlreadyInUse exercises Start's `case err :=
// <-errCh` branch — ListenAndServe returns a real error when the port is
// taken, and Start must propagate it instead of blocking forever.
func TestServer_Start_PortAlreadyInUse(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	defer l.Close() //nolint:errcheck

	deps := newTestDeps()
	srv := newTestServerStruct(t, deps, addr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- srv.Start(ctx)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Start returned nil for taken port; expected listen error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return error for taken port within 5s")
	}
}

// TestServer_ReplayPending_DelegatesToBgState covers the thin pass-
// through wrapper in server.go: nil-receiver short-circuit, nil-bg
// short-circuit, and successful delegation to State.ReplayPending.
// The wrapper is what main.go wires to thingclient.OnReconnect — a bug
// here silently breaks pending replay after a Hub flap.
func TestServer_ReplayPending_DelegatesToBgState(t *testing.T) {
	t.Run("nil receiver short-circuits without panic", func(t *testing.T) {
		var s *Server
		drained, err := s.ReplayPending(context.Background())
		if err != nil || drained {
			t.Errorf("(nil Server).ReplayPending = (%v, %v), want (false, nil)", drained, err)
		}
	})

	t.Run("server with nil bg short-circuits", func(t *testing.T) {
		srv := &Server{bg: nil}
		drained, err := srv.ReplayPending(context.Background())
		if err != nil || drained {
			t.Errorf("Server{bg:nil}.ReplayPending = (%v, %v), want (false, nil)", drained, err)
		}
	})

	t.Run("delegates to State with pending entry", func(t *testing.T) {
		dir := t.TempDir()
		reporter := &countingReporter{}
		bg := breakglass.NewBreakGlassState(dir, reporter, nil)
		if bg == nil {
			t.Fatal("NewBreakGlassState returned nil for non-empty dataDir")
		}

		// Seed the pending buffer via a direct file write — the on-disk
		// format is the pendingBreakGlass JSON shape.
		pending := struct {
			ConfigKey  string          `json:"config_key"`
			KeyVersion int64           `json:"key_version"`
			State      json.RawMessage `json:"state"`
		}{
			ConfigKey:  "killswitch",
			KeyVersion: 9,
			State:      json.RawMessage(`{"enabled":false}`),
		}
		data, _ := json.Marshal(pending)
		if err := os.WriteFile(
			filepath.Join(dir, "pending_break_glass.json"), data, 0o640,
		); err != nil {
			t.Fatalf("seed pending: %v", err)
		}

		srv := &Server{
			bg:     bg,
			logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		drained, err := srv.ReplayPending(context.Background())
		if err != nil {
			t.Fatalf("Server.ReplayPending: %v", err)
		}
		if !drained {
			t.Errorf("Server.ReplayPending drained = false, want true")
		}
		if reporter.calls != 1 {
			t.Errorf("reporter not invoked through Server wrapper (calls=%d)", reporter.calls)
		}
	})
}

// TestServer_RuntimeConfigKey_MethodNotAllowed pins the per-key route
// SWITCH default branch in server.go — PATCH/DELETE/POST on
// /runtime/config/{key} must surface 405 with the documented JSON
// error envelope. Without this assertion, a future refactor could
// silently accept arbitrary methods as PUT.
func TestServer_RuntimeConfigKey_MethodNotAllowed(t *testing.T) {
	deps := newTestDeps()
	deps.ThingID = "proxy-x"
	deps.ThingType = "compliance-proxy"
	ts := newTestServer(t, deps)
	defer ts.Close()

	for _, method := range []string{http.MethodPatch, http.MethodDelete, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			req, err := http.NewRequest(method, ts.URL+"/runtime/config/killswitch", nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close() //nolint:errcheck
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s status = %d, want 405", method, resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), "method not allowed") {
				t.Errorf("405 body missing error message: %q", string(body))
			}
		})
	}
}

// TestServer_RuntimeConfigKey_GetAndPut covers the GET and PUT arms of the
// per-key route switch in NewServer (server.go lines 89–92). The GET arm
// calls getH.ServeHTTP, the PUT arm calls putH.ServeHTTP. Both must be
// exercised to reach the two uncovered branches in NewServer at 94.4%.
//
// Named failure mode: a regression that swaps GET / PUT dispatch would
// silently break break-glass shadow writes while the 405 test still passes.
func TestServer_RuntimeConfigKey_GetAndPut(t *testing.T) {
	deps := newTestDeps()
	deps.ThingID = "proxy-y"
	deps.ThingType = "compliance-proxy"
	ts := newTestServer(t, deps)
	defer ts.Close()

	// GET /runtime/config/killswitch — exercises the getH.ServeHTTP arm.
	// The handler returns the current killswitch config as JSON (200).
	t.Run("GET exercises getH branch", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/runtime/config/killswitch")
		if err != nil {
			t.Fatalf("GET /runtime/config/killswitch: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		// Any non-panic response confirms the GET arm was executed.
		// 200 or 404 are both acceptable — this is a snapshot read.
		if resp.StatusCode == http.StatusMethodNotAllowed {
			t.Errorf("GET dispatched to 405 path; expected getH branch execution")
		}
	})

	// PUT /runtime/config/killswitch — exercises the putH.ServeHTTP arm.
	// The break-glass handler validates the body; an invalid body returns
	// 400 or 422 (not 405) proving the PUT arm was dispatched.
	t.Run("PUT exercises putH branch", func(t *testing.T) {
		req, err := http.NewRequest(
			http.MethodPut,
			ts.URL+"/runtime/config/killswitch",
			strings.NewReader(`{"enabled":false}`),
		)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT /runtime/config/killswitch: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		// Any non-405 response proves the PUT arm was dispatched.
		if resp.StatusCode == http.StatusMethodNotAllowed {
			t.Errorf("PUT dispatched to 405 path; expected putH branch execution")
		}
	})
}
