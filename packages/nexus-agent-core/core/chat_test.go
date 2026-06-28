package core

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
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

// TestGatewayModels parses the VK-scoped /v1/models list, sends the VK as a bearer,
// hits GET /v1/models, and skips blank ids — the model-derivation source.
func TestGatewayModels(t *testing.T) {
	var gotAuth, gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotMethod = r.Header.Get("Authorization"), r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-4o","object":"model"},{"id":""},{"id":"claude-sonnet-4-6"}]}`)
	}))
	defer srv.Close()
	c := NewClient(Env{Name: "local", AIGatewayBaseURL: srv.URL}, fixedTokenSource{}, srv.Client())

	models, err := c.GatewayModels(context.Background(), "nvk_sys")
	if err != nil {
		t.Fatalf("GatewayModels: %v", err)
	}
	if len(models) != 2 || models[0] != "gpt-4o" || models[1] != "claude-sonnet-4-6" {
		t.Fatalf("models = %v, want [gpt-4o claude-sonnet-4-6] (blank id skipped)", models)
	}
	if gotAuth != "Bearer nvk_sys" {
		t.Errorf("Authorization = %q, want the VK bearer", gotAuth)
	}
	if gotMethod != http.MethodGet || gotPath != "/v1/models" {
		t.Errorf("request = %s %s, want GET /v1/models", gotMethod, gotPath)
	}
}

func TestGatewayModels_RejectsEmptyVK(t *testing.T) {
	c := NewClient(Env{Name: "local", AIGatewayBaseURL: "http://unused"}, fixedTokenSource{}, http.DefaultClient)
	if _, err := c.GatewayModels(context.Background(), "  "); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty VK should be unauthorized, got %v", err)
	}
}

func TestGatewayModels_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad key","code":"AUTH_INVALID_KEY"}}`)
	}))
	defer srv.Close()
	c := NewClient(Env{Name: "local", AIGatewayBaseURL: srv.URL}, fixedTokenSource{}, srv.Client())
	if _, err := c.GatewayModels(context.Background(), "bad"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("401 should map to unauthorized, got %v", err)
	}
}

// TestGatewayModels_MalformedBody: a 200 with a non-JSON body is a transport error
// (so the caller fails open), not a panic or a silent empty list.
func TestGatewayModels_MalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `not json at all`)
	}))
	defer srv.Close()
	c := NewClient(Env{Name: "local", AIGatewayBaseURL: srv.URL}, fixedTokenSource{}, srv.Client())
	_, err := c.GatewayModels(context.Background(), "nvk_sys")
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("a malformed 200 body must be a transport error, got %v", err)
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

func TestScanChatToolSSEAccumulatesToolCalls(t *testing.T) {
	// Two content deltas, then a tool call streamed across three frames (name in
	// the first, arguments split across the next two), then a finish + usage.
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Let me "}}]}`,
		`data: {"choices":[{"delta":{"content":"check."}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"observe_cost","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"groupBy\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"provider\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17}}`,
		`data: [DONE]`,
	}, "\n")

	var streamed string
	res, err := scanChatToolSSE(strings.NewReader(body), func(s string) { streamed += s }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if streamed != "Let me check." || res.Content != "Let me check." {
		t.Fatalf("content deltas should stream + accumulate, got streamed=%q content=%q", streamed, res.Content)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("one tool call expected, got %d", len(res.ToolCalls))
	}
	tc := res.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "observe_cost" || tc.Function.Arguments != `{"groupBy":"provider"}` {
		t.Fatalf("tool call must accumulate id+name+arguments, got %+v", tc)
	}
	if res.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason should be tool_calls, got %q", res.FinishReason)
	}
	if res.Usage == nil || res.Usage.TotalTokens != 17 {
		t.Fatalf("usage should be captured, got %+v", res.Usage)
	}
}

func TestScanChatToolSSEMultipleCallsOrdered(t *testing.T) {
	// Two parallel tool calls, frames arriving out of index order, must come back
	// sorted by index with each argument string fully accumulated.
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"observe_nodes","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"observe_alerts","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n")
	res, err := scanChatToolSSE(strings.NewReader(body), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) != 2 || res.ToolCalls[0].ID != "a" || res.ToolCalls[1].ID != "b" {
		t.Fatalf("tool calls must be returned sorted by index, got %+v", res.ToolCalls)
	}
}

func TestScanChatToolSSEReasoningChannel(t *testing.T) {
	// The reasoning/thinking channel streams to onReasoning (not onDelta) and is NOT
	// folded into res.Content — it is display-only. reasoning_content is preferred;
	// a frame that only carries the legacy `reasoning` alias still streams.
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"weigh "}}]}`,
		`data: {"choices":[{"delta":{"reasoning":"options. "}}]}`,
		`data: {"choices":[{"delta":{"content":"Answer."}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n")
	var content, reasoning string
	res, err := scanChatToolSSE(strings.NewReader(body),
		func(s string) { content += s },
		func(s string) { reasoning += s })
	if err != nil {
		t.Fatal(err)
	}
	if reasoning != "weigh options. " {
		t.Fatalf("reasoning deltas must stream to onReasoning, got %q", reasoning)
	}
	if content != "Answer." || res.Content != "Answer." {
		t.Fatalf("reasoning must not leak into content, got streamed=%q content=%q", content, res.Content)
	}
}

func TestScanChatToolSSEReasoningPrefersContentField(t *testing.T) {
	// When a single frame carries BOTH reasoning_content and the legacy reasoning
	// alias, only reasoning_content is emitted (no double-count).
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"primary","reasoning":"alias"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n")
	var reasoning string
	if _, err := scanChatToolSSE(strings.NewReader(body), nil, func(s string) { reasoning += s }); err != nil {
		t.Fatal(err)
	}
	if reasoning != "primary" {
		t.Fatalf("reasoning_content should win over the alias, got %q", reasoning)
	}
}

func TestScanChatToolSSEPlainTextStop(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n")
	res, err := scanChatToolSSE(strings.NewReader(body), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "hi" || len(res.ToolCalls) != 0 || res.FinishReason != "stop" {
		t.Fatalf("plain stop turn shape wrong: %+v", res)
	}
}

func TestChatToolStream_RejectsEmptyVK(t *testing.T) {
	c := NewClient(Env{Name: "local"}, fixedTokenSource{}, http.DefaultClient)
	_, err := c.ChatToolStream(context.Background(), "  ", ChatRequest{}, nil, nil)
	if err == nil || !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty VK must be unauthorized, got %v", err)
	}
}

func TestChatToolStream_SendsToolsAndParsesToolCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer nvk_live" {
			t.Errorf("VK bearer missing, got %q", r.Header.Get("Authorization"))
		}
		var req ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Tools) != 1 || req.Tools[0].Function.Name != "observe_cost" || req.ToolChoice != "auto" {
			t.Errorf("tools[] not forwarded with tool_choice=auto: %+v choice=%q", req.Tools, req.ToolChoice)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"observe_cost\",\"arguments\":\"{}\"}}]}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n")
	}))
	defer srv.Close()
	c := NewClient(Env{Name: "local", AIGatewayBaseURL: srv.URL}, fixedTokenSource{}, srv.Client())

	res, err := c.ChatToolStream(context.Background(), "nvk_live", ChatRequest{
		Model:      "gpt-4o",
		Messages:   []ChatMessage{{Role: "user", Content: "cost?"}},
		Tools:      []ChatTool{{Type: "function", Function: ChatToolFunction{Name: "observe_cost"}}},
		ToolChoice: "auto",
	}, nil, nil)
	if err != nil {
		t.Fatalf("ChatToolStream: %v", err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Function.Name != "observe_cost" || res.FinishReason != "tool_calls" {
		t.Fatalf("tool call not parsed end-to-end: %+v", res)
	}
}

func TestChatToolStream_UpstreamError(t *testing.T) {
	// A 403 (the design §7 PII-block caveat) must surface as a classified API error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"PII detected"}}`)
	}))
	defer srv.Close()
	c := NewClient(Env{Name: "local", AIGatewayBaseURL: srv.URL}, fixedTokenSource{}, srv.Client())
	_, err := c.ChatToolStream(context.Background(), "nvk", ChatRequest{Model: "m"}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "PII detected") {
		t.Fatalf("403 must surface the upstream message, got %v", err)
	}
}

func TestChatToolStream_TransportError(t *testing.T) {
	c := NewClient(Env{Name: "local", AIGatewayBaseURL: "http://127.0.0.1:0"}, fixedTokenSource{}, http.DefaultClient)
	_, err := c.ChatToolStream(context.Background(), "nvk", ChatRequest{Model: "m"}, nil, nil)
	if err == nil || !errors.Is(err, ErrTransport) {
		t.Fatalf("an unroutable gateway must be a transport error, got %v", err)
	}
}
