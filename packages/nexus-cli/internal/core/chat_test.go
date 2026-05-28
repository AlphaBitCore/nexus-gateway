package core

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// sampleSSE is a real-shape OpenAI chat SSE body (two content deltas, a
// finish-reason frame, then a usage-only frame, then [DONE]). The extra
// "obfuscation" field mirrors what the live gateway emits and must be ignored.
const sampleSSE = `data: {"choices":[{"delta":{"content":"Hello"},"finish_reason":null}],"usage":null,"obfuscation":"x"}

data: {"choices":[{"delta":{"content":" world"},"finish_reason":null}],"usage":null}

data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":null}

data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":8,"total_tokens":19,"prompt_tokens_details":{"cached_tokens":4}}}

data: [DONE]

`

func TestChatStream_DeltasAndUsage(t *testing.T) {
	var gotAuth, gotAccept string
	var gotBody ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sampleSSE)
	}))
	defer srv.Close()

	c := NewClient(Env{Name: "local", AIGatewayBaseURL: srv.URL}, fixedTokenSource{}, srv.Client())
	var sb strings.Builder
	temp := 0.2
	usage, err := c.ChatStream(context.Background(), "nvk_secret",
		ChatRequest{Model: "gpt-4o-mini", Messages: []ChatMessage{{Role: "user", Content: "hi"}}, MaxTokens: 30, Temperature: &temp},
		func(d string) { sb.WriteString(d) })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if sb.String() != "Hello world" {
		t.Fatalf("assembled content = %q, want %q", sb.String(), "Hello world")
	}
	if usage == nil || usage.TotalTokens != 19 || usage.PromptTokensDetails.CachedTokens != 4 {
		t.Fatalf("usage wrong: %+v", usage)
	}
	// VK is the credential (not the admin token); streaming + usage forced on.
	if gotAuth != "Bearer nvk_secret" {
		t.Fatalf("auth header = %q, want VK bearer", gotAuth)
	}
	if gotAccept != "text/event-stream" {
		t.Fatalf("accept = %q", gotAccept)
	}
	if !gotBody.Stream || gotBody.StreamOptions == nil || !gotBody.StreamOptions.IncludeUsage {
		t.Fatalf("ChatStream must force stream+include_usage: %+v", gotBody)
	}
	if gotBody.Temperature == nil || *gotBody.Temperature != 0.2 {
		t.Fatalf("temperature should pass through to the request body: %+v", gotBody.Temperature)
	}
}

func TestChatStream_RejectsEmptyVK(t *testing.T) {
	c := NewClient(Env{Name: "local", AIGatewayBaseURL: "http://unused"}, fixedTokenSource{}, http.DefaultClient)
	_, err := c.ChatStream(context.Background(), "  ", ChatRequest{Model: "m"}, nil)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty VK should be unauthorized, got %v", err)
	}
}

func TestChatStream_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"virtual key invalid","code":"AUTH_INVALID_KEY","type":"proxy_error"}}`)
	}))
	defer srv.Close()
	c := NewClient(Env{Name: "local", AIGatewayBaseURL: srv.URL}, fixedTokenSource{}, srv.Client())
	_, err := c.ChatStream(context.Background(), "bad", ChatRequest{Model: "m"}, func(string) {})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("401 should map to unauthorized, got %v", err)
	}
	if !strings.Contains(err.Error(), "virtual key invalid") {
		t.Fatalf("error should carry upstream message: %v", err)
	}
}

func TestScanChatSSE_SkipsMalformedFrames(t *testing.T) {
	body := "data: not-json\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n"
	var sb strings.Builder
	usage, err := scanChatSSE(strings.NewReader(body), func(d string) { sb.WriteString(d) })
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if sb.String() != "ok" {
		t.Fatalf("malformed frame should be skipped, got %q", sb.String())
	}
	if usage != nil {
		t.Fatalf("no usage frame → nil usage, got %+v", usage)
	}
}

func TestClient_SimulatorForward(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/ai-gateway-simulator/forward" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body SimulatorForwardRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.VK != "nvk_x" || body.Path != "/v1/chat/completions" {
			t.Errorf("body not forwarded: %+v", body)
		}
		// Admin auth is attached by the typed client, not the VK.
		if r.Header.Get("Authorization") != "Bearer T" {
			t.Errorf("admin auth missing: %s", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],"usage":{"total_tokens":17}}`)
	}))
	defer srv.Close()
	c := NewClient(Env{Name: "local", CPBaseURL: srv.URL},
		fixedTokenSource{header: "Authorization", value: "Bearer T"}, srv.Client())

	raw, err := c.SimulatorForward(context.Background(), SimulatorForwardRequest{
		Path: "/v1/chat/completions", Method: "POST", VK: "nvk_x",
		Body: json.RawMessage(`{"model":"gpt-4o-mini"}`),
	})
	if err != nil {
		t.Fatalf("SimulatorForward: %v", err)
	}
	if !strings.Contains(string(raw), `"total_tokens":17`) {
		t.Fatalf("raw response not returned: %s", raw)
	}
}

// chatErrReader fails every Read so scanChatSSE's scanner.Err() path runs.
type chatErrReader struct{}

func (chatErrReader) Read([]byte) (int, error) { return 0, errors.New("read boom") }

func TestScanChatSSE_ReaderError(t *testing.T) {
	_, err := scanChatSSE(chatErrReader{}, nil)
	if err == nil || !errors.Is(err, ErrTransport) || !strings.Contains(err.Error(), "read boom") {
		t.Fatalf("reader error should surface as transport error, got %v", err)
	}
}

func TestChatStream_TransportError(t *testing.T) {
	// An unroutable base URL makes httpc.Do fail → classified transport error.
	c := NewClient(Env{Name: "local", AIGatewayBaseURL: "http://127.0.0.1:0"}, fixedTokenSource{}, http.DefaultClient)
	_, err := c.ChatStream(context.Background(), "nvk", ChatRequest{Model: "m"}, nil)
	if err == nil || !errors.Is(err, ErrTransport) {
		t.Fatalf("dial failure should be transport error, got %v", err)
	}
}

// TestClient_NewMethodsErrorPaths drives every new admin-authed method against a
// server that 500s, asserting each propagates a classified error (not nil).
func TestClient_NewMethodsErrorPaths(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer done()
	if _, err := c.CacheROI(context.Background(), nil); err == nil {
		t.Error("CacheROI should error on 500")
	}
	if _, err := c.RoutingFallbacks(context.Background(), nil); err == nil {
		t.Error("RoutingFallbacks should error on 500")
	}
	if _, err := c.LatencyPhases(context.Background(), "provider", nil); err == nil {
		t.Error("LatencyPhases should error on 500")
	}
	if _, err := c.SimulatorForward(context.Background(), SimulatorForwardRequest{Path: "/v1/models", Method: "GET", VK: "x"}); err == nil {
		t.Error("SimulatorForward should error on 500")
	}
}

func TestSparklineResult_Totals(t *testing.T) {
	// No top-level summary → sum the series buckets (the live sparkline shape).
	r := &SparklineResult{Series: []SparklineBucket{
		{Values: map[string]float64{MetricRequestCount: 20, MetricStatus5xxCount: 1}},
		{Values: map[string]float64{MetricRequestCount: 22, MetricStatus5xxCount: 1}},
	}}
	tot := r.Totals()
	if tot[MetricRequestCount] != 42 || tot[MetricStatus5xxCount] != 2 {
		t.Fatalf("series should be summed: %+v", tot)
	}
	// A populated summary is returned as-is (not double-counted).
	r2 := &SparklineResult{Summary: map[string]float64{MetricRequestCount: 7}}
	if got := r2.Totals(); got[MetricRequestCount] != 7 {
		t.Fatalf("summary should be used directly: %+v", got)
	}
}

func TestClient_SLOMethods(t *testing.T) {
	c, done := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/admin/analytics/cache-roi":
			_, _ = io.WriteString(w, `{"periodDays":8,"totalEstimatedCostUsd":3.96,"totalCacheNetSavingsUsd":2.11,"gatewayCacheHitCount":22,"requestsWithCacheHit":210}`)
		case "/api/admin/analytics/routing/fallbacks":
			_, _ = io.WriteString(w, `{"data":[{"group":"passthrough-fallback","groupLabel":"Passthrough","requestCount":516}]}`)
		case "/api/admin/analytics/latency-phases":
			if r.URL.Query().Get("groupBy") != "provider" {
				t.Errorf("groupBy not set: %s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"window":{"start":"a","end":"b"},"rows":[{"groupKey":"openai","groupLabel":"openai","requestCount":173,"totalP95Ms":90008,"upstreamTtfbP95Ms":13567}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer done()

	roi, err := c.CacheROI(context.Background(), nil)
	if err != nil || roi.RequestsWithCacheHit != 210 || roi.TotalCacheNetSavingsUSD != 2.11 {
		t.Fatalf("CacheROI wrong: %+v err=%v", roi, err)
	}
	fb, err := c.RoutingFallbacks(context.Background(), nil)
	if err != nil || len(fb.Data) != 1 || fb.Data[0].RequestCount != 516 {
		t.Fatalf("RoutingFallbacks wrong: %+v err=%v", fb, err)
	}
	lp, err := c.LatencyPhases(context.Background(), "provider", url.Values{"start": {"x"}})
	if err != nil || len(lp.Rows) != 1 || lp.Rows[0].TotalP95Ms != 90008 || lp.Rows[0].UpstreamTTFBP95Ms != 13567 {
		t.Fatalf("LatencyPhases wrong: %+v err=%v", lp, err)
	}
}
