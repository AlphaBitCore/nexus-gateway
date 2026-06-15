package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// appWithVK extends newTestApp with an AI-Gateway URL + a stored VK secret so
// the VK-authed chat/simulate commands resolve their credential.
func appWithVK(srv *httptest.Server) *App {
	a := newTestApp(srv, false)
	a.Env.AIGatewayBaseURL = srv.URL
	a.Env.LastModel = "gpt-4o-mini"
	_ = a.Store.Set("local", core.SecretVKSecret, "nvk_stored")
	return a
}

func TestChatCmd_StreamAndJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer nvk_stored" {
			t.Errorf("unexpected chat request: %s auth=%s", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello world\"}}]}\n\n"+
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":8,\"total_tokens\":19}}\n\n"+
			"data: [DONE]\n")
	}))
	defer srv.Close()

	out, err := runCLI(t, appWithVK(srv), "chat", "hello", "--model", "gpt-4o-mini")
	if err != nil || !strings.Contains(out, "Hello world") || !strings.Contains(out, "tokens 19") {
		t.Fatalf("chat table wrong: %q err=%v", out, err)
	}
	out, err = runCLI(t, appWithVK(srv), "chat", "hello", "-o", "json")
	if err != nil || !strings.Contains(out, `"content": "Hello world"`) || !strings.Contains(out, `"total_tokens": 19`) {
		t.Fatalf("chat json wrong: %q err=%v", out, err)
	}
}

func TestChatCmd_UsageErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	// empty prompt → usage exit 2
	if _, err := runCLI(t, appWithVK(srv), "chat"); exitCode(err) != 2 {
		t.Fatalf("empty prompt should be exit 2, got %d", exitCode(err))
	}
	// no VK secret + no --vk → usage exit 2
	a := newTestApp(srv, false)
	a.Env.AIGatewayBaseURL = srv.URL
	a.Env.LastModel = "m"
	if _, err := runCLI(t, a, "chat", "hi"); exitCode(err) != 2 {
		t.Fatalf("missing VK should be exit 2, got %d", exitCode(err))
	}
	// no model + no --model → usage exit 2
	a2 := newTestApp(srv, false)
	a2.Env.AIGatewayBaseURL = srv.URL
	_ = a2.Store.Set("local", core.SecretVKSecret, "nvk_x")
	if _, err := runCLI(t, a2, "chat", "hi"); exitCode(err) != 2 {
		t.Fatalf("missing model should be exit 2, got %d", exitCode(err))
	}
}

func TestSimulateCmd_ForwardTableAndJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/ai-gateway-simulator/forward" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],"usage":{"total_tokens":17}}`)
	}))
	defer srv.Close()
	out, err := runCLI(t, appWithVK(srv), "simulate", "--model", "gpt-4o-mini", "--prompt", "ping")
	if err != nil || !strings.Contains(out, "total_tokens") || !strings.Contains(out, "hi") {
		t.Fatalf("simulate table wrong: %q err=%v", out, err)
	}
	out, err = runCLI(t, appWithVK(srv), "simulate", "-o", "json")
	if err != nil || !strings.Contains(out, `"total_tokens":17`) {
		t.Fatalf("simulate json wrong: %q err=%v", out, err)
	}
}

func TestSLOCmd_TableAndJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/admin/analytics/latency-phases":
			_, _ = io.WriteString(w, `{"window":{"start":"a","end":"b"},"rows":[{"groupKey":"openai","groupLabel":"OpenAI","requestCount":173,"totalP50Ms":1245,"totalP95Ms":90008,"upstreamTtfbP95Ms":13567}]}`)
		case "/api/admin/analytics/routing/fallbacks":
			_, _ = io.WriteString(w, `{"data":[{"group":"passthrough-fallback","groupLabel":"Passthrough","requestCount":516}]}`)
		case "/api/admin/analytics/sparkline":
			_, _ = io.WriteString(w, `{"granularity":"1h","series":[{"bucketStart":"2026-05-28T00:00:00Z","values":{"request_count":100,"status_5xx_count":2}}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	out, err := runCLI(t, newTestApp(srv, false), "slo")
	if err != nil || !strings.Contains(out, "OpenAI") || !strings.Contains(out, "availability: 98.00%") || !strings.Contains(out, "Passthrough") {
		t.Fatalf("slo table wrong: %q err=%v", out, err)
	}
	out, err = runCLI(t, newTestApp(srv, false), "slo", "-o", "json")
	if err != nil || !strings.Contains(out, `"availabilityPct": 98`) || !strings.Contains(out, `"totalP95Ms": 90008`) {
		t.Fatalf("slo json wrong: %q err=%v", out, err)
	}
}
