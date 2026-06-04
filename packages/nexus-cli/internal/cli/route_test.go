package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

func TestRouteExplain_TableAndJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/routing-rules/simulate" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body core.RoutingSimulateRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.ModelID != "gpt-4o-mini" {
			t.Errorf("model not forwarded: %+v", body)
		}
		_, _ = io.WriteString(w, `{"substituted":true,"ruleName":"prefer-anthropic","targets":[{"providerName":"Anthropic","modelCode":"claude-sonnet-4-6"}],"recoveryTargets":[{"providerName":"OpenAI","modelCode":"gpt-4o"}],"warnings":["no stage-1 rule matched"]}`)
	}))
	defer srv.Close()

	out, err := runCLI(t, newTestApp(srv, false), "route", "explain", "--model", "gpt-4o-mini")
	if err != nil || !strings.Contains(out, "Anthropic") || !strings.Contains(out, "prefer-anthropic") || !strings.Contains(out, "no stage-1 rule matched") {
		t.Fatalf("route explain table wrong: %q err=%v", out, err)
	}
	if !strings.Contains(out, "recovery:") || !strings.Contains(out, "OpenAI") {
		t.Fatalf("route explain should show recovery chain: %q", out)
	}
	out, err = runCLI(t, newTestApp(srv, false), "route", "explain", "--model", "gpt-4o-mini", "-o", "json")
	if err != nil || !strings.Contains(out, `"substituted": true`) {
		t.Fatalf("route explain json wrong: %q err=%v", out, err)
	}
}

func TestRouteExplain_NoSubstitutionAndNoTargets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"substituted":false,"targets":[],"warnings":["no stage-1 rule matched — request would be rejected"]}`)
	}))
	defer srv.Close()
	out, err := runCLI(t, newTestApp(srv, false), "route", "explain", "--model", "gpt-4o-mini")
	if err != nil || !strings.Contains(out, "substituted: no") || !strings.Contains(out, "rejected by the router") {
		t.Fatalf("no-target route wrong: %q err=%v", out, err)
	}
}

func TestRouteExplain_EndpointForwarded(t *testing.T) {
	var gotBody core.RoutingSimulateRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"substituted":false,"targets":[],"warnings":[]}`)
	}))
	defer srv.Close()
	// --endpoint must forward as EndpointType (previously unasserted at the wire).
	if _, err := runCLI(t, newTestApp(srv, false), "route", "explain", "--model", "m", "--endpoint", "responses"); err != nil {
		t.Fatalf("route explain --endpoint: err=%v", err)
	}
	if gotBody.EndpointType != "responses" {
		t.Fatalf("--endpoint must forward EndpointType, got %q", gotBody.EndpointType)
	}
	// The flag defaults to "chat" when omitted.
	gotBody = core.RoutingSimulateRequest{}
	if _, err := runCLI(t, newTestApp(srv, false), "route", "explain", "--model", "m"); err != nil {
		t.Fatalf("route explain default endpoint: err=%v", err)
	}
	if gotBody.EndpointType != "chat" {
		t.Fatalf("default endpoint must be chat, got %q", gotBody.EndpointType)
	}
}

func TestRouteExplain_UsageErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	if _, err := runCLI(t, newTestApp(srv, false), "route", "explain"); exitCode(err) != 2 {
		t.Fatalf("missing --model should be exit 2, got %d", exitCode(err))
	}
	// gateway error propagates
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer srv2.Close()
	if _, err := runCLI(t, newTestApp(srv2, false), "route", "explain", "--model", "m"); err == nil {
		t.Fatal("route explain should propagate the gateway error")
	}
}
