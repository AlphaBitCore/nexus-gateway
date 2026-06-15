package selfdispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/initiator"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// recordingRT is a base round-tripper that records the request and returns a canned
// response, so the "delegates to base" branch is observable without a network.
type recordingRT struct {
	seen *http.Request
	resp *http.Response
}

func (r *recordingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.seen = req
	if r.resp != nil {
		return r.resp, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
}

// staticTokenSource is a minimal core.TokenSource for the interop test.
type staticTokenSource struct{ authz string }

func (s staticTokenSource) Credential(context.Context) (string, string, error) {
	if s.authz == "" {
		return "", "", errors.New("no credential")
	}
	return "Authorization", s.authz, nil
}

// cpRouterEcho builds an echo router that echoes back, as JSON, what an in-process
// admin call observed: the unforgeable initiator channel (R3 audit stamp), the
// caller IP that EntryFor would record (c.RealIP()), the forwarded body, the request
// id, and any x-nexus-run-token credential header (so the workflow consumer's
// credential injection is verifiable).
func cpRouterEcho(t *testing.T) *echo.Echo {
	t.Helper()
	e := echo.New()
	handler := func(c echo.Context) error {
		body, _ := io.ReadAll(c.Request().Body)
		return c.JSON(http.StatusOK, map[string]any{
			"via":   initiator.From(c.Request().Context()),
			"ip":    c.RealIP(),
			"body":  string(body),
			"reqid": c.Request().Header.Get("X-Nexus-Request-Id"),
		})
	}
	e.GET("/api/admin/ping", handler)
	e.POST("/api/admin/ping", handler)
	// A handler that returns a non-2xx so status propagation is covered.
	e.GET("/api/admin/boom", func(c echo.Context) error {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "nope"})
	})
	// A handler that returns 204 with no body — exercises the empty-2xx contract that
	// core.Client.do relies on (a self-call DELETE tool lands here).
	e.DELETE("/api/admin/gone", func(c echo.Context) error {
		return c.NoContent(http.StatusNoContent)
	})
	return e
}

func doRoundTrip(t *testing.T, tr http.RoundTripper, method, url, body string) (*http.Response, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, rdr)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp, decoded
}

// TestRecorder_IsFlusher pins the contract that the in-process recorder satisfies
// http.Flusher (echo's Response.Flush probes for it; without this an admin handler
// that flushes would panic) and that Flush is a safe no-op for the buffered recorder.
func TestRecorder_IsFlusher(t *testing.T) {
	var f http.Flusher = &recorder{}
	f.Flush() // must not panic
}

// TestDispatchesCPHostInProcess is the R3 core for the assistant consumer: a request
// to the CP host is dispatched straight into the router (no network), carries the
// unforgeable assistant initiator stamp, and records the originating user's IP — not
// the loopback — for the audit actor.
func TestDispatchesCPHostInProcess(t *testing.T) {
	e := cpRouterEcho(t)
	tr := New(Config{
		Handler: e, CPBaseURL: "http://localhost:9999",
		Initiator: initiator.ViaAssistant, SourceIP: "203.0.113.5", RequestID: "turn-req-abc123",
		Base: &recordingRT{},
	})

	resp, decoded := doRoundTrip(t, tr, http.MethodPost, "http://localhost:9999/api/admin/ping", `{"k":"v"}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	if decoded["via"] != initiator.ViaAssistant {
		t.Errorf("in-process call via = %v; want %q (unforgeable AI-initiated stamp)", decoded["via"], initiator.ViaAssistant)
	}
	if decoded["ip"] != "203.0.113.5" {
		t.Errorf("audit RealIP = %v; want the originating user IP 203.0.113.5", decoded["ip"])
	}
	if decoded["body"] != `{"k":"v"}` {
		t.Errorf("forwarded body = %v; want the request body intact", decoded["body"])
	}
	if decoded["reqid"] != "turn-req-abc123" {
		t.Errorf("self-call X-Nexus-Request-Id = %v; want the originating turn id (audit correlation)", decoded["reqid"])
	}
}

// TestPropagatesNon2xx ensures a CP handler's non-2xx status + body round-trip back
// to core.Client unchanged (so the agent relays a 403 verbatim).
func TestPropagatesNon2xx(t *testing.T) {
	e := cpRouterEcho(t)
	tr := New(Config{Handler: e, CPBaseURL: "http://localhost:9999", Initiator: initiator.ViaAssistant})

	resp, decoded := doRoundTrip(t, tr, http.MethodGet, "http://localhost:9999/api/admin/boom", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d; want 403", resp.StatusCode)
	}
	if decoded["error"] != "nope" {
		t.Errorf("body = %v; want the handler's error body", decoded)
	}
}

// TestDelegatesNonCPHost verifies inference / external hosts skip the in-process path
// entirely and go over base (the real network in production) — and that the
// initiator stamp does not leak onto an external request.
func TestDelegatesNonCPHost(t *testing.T) {
	e := cpRouterEcho(t)
	base := &recordingRT{}
	tr := New(Config{
		Handler: e, CPBaseURL: "http://localhost:9999",
		Initiator: initiator.ViaAssistant, SourceIP: "203.0.113.5",
		Base: base,
	})

	_, _ = doRoundTrip(t, tr, http.MethodPost, "http://gateway.example.com/v1/chat/completions", `{"x":1}`)

	if base.seen == nil {
		t.Fatal("a non-CP host must delegate to base (network), not dispatch in-process")
	}
	if base.seen.URL.Host != "gateway.example.com" {
		t.Errorf("base saw host %q; want gateway.example.com", base.seen.URL.Host)
	}
	if initiator.From(base.seen.Context()) != "" {
		t.Error("external call must not carry the in-process initiator context stamp")
	}
}

// TestNilHandlerAllToBase covers the test/pool-less fallback: with no dispatcher,
// every request (even the CP host) goes over base.
func TestNilHandlerAllToBase(t *testing.T) {
	base := &recordingRT{}
	tr := New(Config{CPBaseURL: "http://localhost:9999", Base: base})
	_, _ = doRoundTrip(t, tr, http.MethodGet, "http://localhost:9999/api/admin/ping", "")
	if base.seen == nil {
		t.Fatal("nil handler must delegate everything to base")
	}
}

// TestDefaultBaseTransportWhenNil pins that a nil Base resolves to the widened-TLS
// default (so consumers passing no base still get the inference-safe transport).
func TestDefaultBaseTransportWhenNil(t *testing.T) {
	tr := New(Config{Handler: cpRouterEcho(t), CPBaseURL: "http://localhost:9999"})
	if tr.base == nil {
		t.Fatal("nil Base must resolve to DefaultBaseTransport, not stay nil")
	}
}

// TestDoesNotMutateCallerRequest pins the RoundTripper contract: the caller's
// *http.Request must not be mutated (the transport clones), or a shared request
// would be corrupted.
func TestDoesNotMutateCallerRequest(t *testing.T) {
	e := cpRouterEcho(t)
	tr := New(Config{
		Handler: e, CPBaseURL: "http://localhost:9999",
		Initiator: initiator.ViaAssistant, SourceIP: "203.0.113.5",
	})

	req, _ := http.NewRequest(http.MethodGet, "http://localhost:9999/api/admin/ping", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()
	if got := req.Header.Get("X-Forwarded-For"); got != "" {
		t.Errorf("caller request mutated: X-Forwarded-For = %q, want empty (transport must clone)", got)
	}
	if initiator.From(req.Context()) != "" {
		t.Error("caller request context mutated with the initiator value (transport must clone)")
	}
}

// TestEmptyBodyInteropThroughClient pins the 204/empty-2xx contract end-to-end
// through a real core.Client (the consumer): a self-call DELETE tool hits a
// 204-no-body handler, and core.Client.AdminRequest must surface status 204 with an
// empty body and no spurious decode error.
func TestEmptyBodyInteropThroughClient(t *testing.T) {
	e := cpRouterEcho(t)
	tr := New(Config{Handler: e, CPBaseURL: "http://localhost:9999", Initiator: initiator.ViaAssistant, SourceIP: "203.0.113.5"})
	client := core.NewClient(
		core.Env{CPBaseURL: "http://localhost:9999"},
		staticTokenSource{authz: "Bearer test-token"},
		&http.Client{Transport: tr},
	)
	raw, status, err := client.AdminRequest(context.Background(), http.MethodDelete, "/api/admin/gone", nil, nil)
	if err != nil {
		t.Fatalf("AdminRequest on a 204 handler returned an error: %v", err)
	}
	if status != http.StatusNoContent {
		t.Fatalf("status = %d; want 204", status)
	}
	if len(raw) != 0 {
		t.Errorf("body = %q; want empty for a 204", raw)
	}
}
