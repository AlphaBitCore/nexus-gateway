package intercept

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	agentcompliance "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/compliance"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/shadow"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// newTestPipeline builds an AgentPipeline with a single enabled interception
// domain backed by the real openai-compat adapter, but no hook configs. This
// keeps the hook pipeline inactive so ProcessRequest goes through the
// "HasHooks == false" fast-path and we can assert purely on detector output.
func newTestPipeline(t *testing.T, hostPattern string) *agentcompliance.AgentPipeline {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := agentcompliance.NewAgentPipeline(logger)
	snap := &shadow.ConfigSnapshot{
		HookConfigs: nil,
		InterceptionDomains: []shadow.InterceptionDomainDTO{
			{
				ID:                "dom-openai",
				Name:              "openai",
				HostPattern:       hostPattern,
				HostMatchType:     "EXACT",
				AdapterID:         "openai-compat",
				Enabled:           true,
				Priority:          100,
				DefaultPathAction: "PROCESS",
				OnAdapterError:    "FAIL_OPEN",
				NetworkZone:       "PUBLIC",
			},
		},
	}
	p.ApplySnapshot(snap)
	return p
}

func TestHandler_ProcessRequest_PopulatesDetectorSignals(t *testing.T) {
	p := newTestPipeline(t, "api.openai.com")
	h := NewHandler(p, slog.New(slog.NewTextHandler(io.Discard, nil)))

	headers := http.Header{}
	headers.Set("Authorization", "Bearer sk-ant-test")
	headers.Set("Content-Type", "application/json")
	body := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)

	res := h.ProcessRequest(
		context.Background(),
		"api.openai.com",
		http.MethodPost,
		"/v1/chat/completions",
		headers,
		body,
	)

	if res.Action != traffic.Process {
		t.Fatalf("Action = %v, want Process", res.Action)
	}
	if res.Provider != "openai" {
		t.Fatalf("Provider = %q, want openai", res.Provider)
	}
	if res.Model != "gpt-4o-mini" {
		t.Fatalf("Model = %q, want gpt-4o-mini", res.Model)
	}
	if res.ApiKeyClass != "sk-ant-" {
		t.Fatalf("ApiKeyClass = %q, want sk-ant-", res.ApiKeyClass)
	}
	want := traffic.ApiKeyFingerprint("sk-ant-test")
	if res.ApiKeyFingerprint != want {
		t.Fatalf("ApiKeyFingerprint = %q, want %q", res.ApiKeyFingerprint, want)
	}
}

func TestHandler_ProcessRequest_EmptyHeadersYieldsEmptyKeyFields(t *testing.T) {
	p := newTestPipeline(t, "api.openai.com")
	h := NewHandler(p, slog.New(slog.NewTextHandler(io.Discard, nil)))

	body := []byte(`{"model":"gpt-4o-mini","messages":[]}`)
	res := h.ProcessRequest(
		context.Background(),
		"api.openai.com",
		http.MethodPost,
		"/v1/chat/completions",
		nil,
		body,
	)

	if res.Provider != "openai" {
		t.Fatalf("Provider = %q, want openai (from adapter match)", res.Provider)
	}
	if res.Model != "gpt-4o-mini" {
		t.Fatalf("Model = %q, want gpt-4o-mini", res.Model)
	}
	if res.ApiKeyClass != "" {
		t.Fatalf("ApiKeyClass = %q, want empty", res.ApiKeyClass)
	}
	if res.ApiKeyFingerprint != "" {
		t.Fatalf("ApiKeyFingerprint = %q, want empty", res.ApiKeyFingerprint)
	}
}

func TestHandler_ProcessRequest_UnmatchedHostPassthrough(t *testing.T) {
	p := newTestPipeline(t, "api.openai.com")
	h := NewHandler(p, slog.New(slog.NewTextHandler(io.Discard, nil)))

	headers := http.Header{}
	headers.Set("Authorization", "Bearer sk-test")
	res := h.ProcessRequest(
		context.Background(),
		"example.com",
		http.MethodGet,
		"/",
		headers,
		nil,
	)

	if res.Action != traffic.Passthrough {
		t.Fatalf("Action = %v, want Passthrough for unknown host", res.Action)
	}
	if res.Provider != "" || res.Model != "" || res.ApiKeyClass != "" || res.ApiKeyFingerprint != "" {
		t.Fatalf("expected empty detector fields on passthrough, got %+v", res)
	}
}

func TestBuildSyntheticRequest_PopulatesFields(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-proj-abc")

	req := buildSyntheticRequest("api.openai.com", "POST", "/v1/chat/completions?stream=true", h)

	if req.Method != http.MethodPost {
		t.Fatalf("Method = %q, want POST", req.Method)
	}
	if req.Host != "api.openai.com" {
		t.Fatalf("Host = %q, want api.openai.com", req.Host)
	}
	if req.URL.Scheme != "https" {
		t.Fatalf("URL.Scheme = %q, want https", req.URL.Scheme)
	}
	if req.URL.Host != "api.openai.com" {
		t.Fatalf("URL.Host = %q, want api.openai.com", req.URL.Host)
	}
	if req.URL.Path != "/v1/chat/completions" {
		t.Fatalf("URL.Path = %q, want /v1/chat/completions", req.URL.Path)
	}
	if req.URL.RawQuery != "stream=true" {
		t.Fatalf("URL.RawQuery = %q, want stream=true", req.URL.RawQuery)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-proj-abc" {
		t.Fatalf("Authorization header = %q, want Bearer sk-proj-abc", got)
	}
}

func TestBuildSyntheticRequest_EmptyMethodDefaultsToPost(t *testing.T) {
	req := buildSyntheticRequest("api.openai.com", "", "/", nil)
	if req.Method != http.MethodPost {
		t.Fatalf("Method = %q, want POST default", req.Method)
	}
}
