package tlsbump

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	compliance "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/pipeline"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters/api/openai"
)

// Finding C-18: the bumped path used to run the full normalize decode on every
// request and response whether or not any hook was bound to consume it. Its only
// consumer is HookInput.Normalized, and the audit row does not carry it — the
// emitter reads AuditInfo.RequestNormalized / ResponseNormalized, which this path
// never stamps. So with no hooks the decode was computed and discarded.
//
// Before this change nothing in the package wired a normalize registry or asserted
// on HookInput.Normalized at all, so both halves below are new coverage: that the
// decode still happens when a hook needs it, and that it no longer happens when
// nothing does.

// countingAdapter observes whether runtimeNormalize ran, by counting the adapter
// extraction it performs. Embedding the interface keeps this to the one method under
// observation and delegates the other eight to the real adapter, so the request still
// travels the production decode path rather than a stub of it.
type countingAdapter struct {
	traffic.Adapter
	extractRequests  *atomic.Int64
	extractResponses *atomic.Int64
}

func (a countingAdapter) ExtractRequest(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	a.extractRequests.Add(1)
	return a.Adapter.ExtractRequest(ctx, body, path)
}

func (a countingAdapter) ExtractResponse(ctx context.Context, body []byte, path string) (traffic.NormalizedContent, error) {
	if a.extractResponses != nil {
		a.extractResponses.Add(1)
	}
	return a.Adapter.ExtractResponse(ctx, body, path)
}

// capturingReqHook records the HookInput it was handed, so a test can assert on what
// the hook actually SAW rather than on whether the pipeline merely ran.
type capturingReqHook struct {
	seen *core.HookInput
}

func (h *capturingReqHook) Execute(_ context.Context, in *core.HookInput) (*core.HookResult, error) {
	*h.seen = *in
	return &core.HookResult{HookID: "h-cap", HookName: "capturing-hook", Decision: core.Approve}, nil
}

func (h *capturingReqHook) SupportsEndpoint(core.EndpointType) bool { return true }
func (h *capturingReqHook) SupportsModality(core.Modality) bool     { return true }

func capturingResolver(t *testing.T, stage string, seen *core.HookInput) *compliance.PolicyResolver {
	t.Helper()
	reg := core.NewHookRegistry()
	reg.Register("capturing", func(_ *core.HookConfig) (core.Hook, error) {
		return &capturingReqHook{seen: seen}, nil
	})
	return compliance.NewPolicyResolver([]core.HookConfig{{
		ID: "h-cap", ImplementationID: "capturing", Name: "capturing-hook",
		Stage: stage, Enabled: true, FailBehavior: "fail-open",
		ApplicableIngress: []string{"ALL"},
	}}, reg, discardSlog())
}

const c18RequestBody = `{"model":"gpt-4o","messages":[{"role":"user","content":"normalize me please"}]}`

func c18Options(t *testing.T, resolver *compliance.PolicyResolver, reqCount, respCount *atomic.Int64, writer *recordingAuditWriter) *bumpOptions {
	t.Helper()
	areg := traffic.NewAdapterRegistry("test")
	if err := areg.Register("openai-compat", func() traffic.Adapter {
		return countingAdapter{Adapter: &openai.Adapter{}, extractRequests: reqCount, extractResponses: respCount}
	}); err != nil {
		t.Fatalf("adapter register: %v", err)
	}
	return &bumpOptions{
		policyResolver:  resolver,
		auditEmitter:    compliance.NewAuditEmitter(writer, discardSlog()),
		domainEngine:    adapterDomainEngine(t, "openai-compat"),
		adapterRegistry: areg,
		// normalizeRegistry deliberately nil: runtimeNormalize then takes its
		// documented adapter-extraction fallback, which is the path the counter
		// observes. The gating under test is upstream of which path it takes.
	}
}

// TestForwardHandler_NormalizeRunsWhenRequestHookBound is the equivalence half. The
// decode is now deferred until after BuildPipeline, so if that wiring were wrong the
// hook would receive a nil Normalized, silently fall back to flat-text projection, and
// still approve — no error anywhere, just a PII pipeline scanning less than it should.
// Asserted on what the hook received, for that reason.
func TestForwardHandler_NormalizeRunsWhenRequestHookBound(t *testing.T) {
	var seen core.HookInput
	var extracts atomic.Int64
	writer := &recordingAuditWriter{}
	rt := &bodyCapturingRoundTripper{makeResp: jsonUpstream}

	bo := c18Options(t, capturingResolver(t, "request", &seen), &extracts, nil, writer)
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	req := httptest.NewRequest(http.MethodPost, "https://api.example.com/v1/chat/completions", strings.NewReader(c18RequestBody))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got := extracts.Load(); got != 1 {
		t.Fatalf("adapter extractions = %d, want 1: a bound request hook must still get a normalized payload", got)
	}
	if seen.Normalized == nil {
		t.Fatal("the request hook received a nil Normalized. The deferred normalize did not reach it, " +
			"so hooks would silently scan the flat-text fallback instead of structured messages — " +
			"no error is raised on that path, which is why this is asserted on the hook's own input.")
	}
	if !strings.Contains(strings.Join(seen.TextSegments(), " "), "normalize me please") {
		t.Fatalf("hook saw segments %q, want the request content", seen.TextSegments())
	}
}

// TestForwardHandler_NormalizeSkippedWhenNoRequestHook is the optimization half, and it
// also pins what must NOT be skipped alongside it. DetectRequestMeta feeds the audit
// row's provider and model and stays unconditional; only the decode whose sole consumer
// is the (absent) hook is skipped. Without the second assertion this test would still
// pass if the whole adapter block had been skipped, which would silently blank the audit
// row's provider and model.
func TestForwardHandler_NormalizeSkippedWhenNoRequestHook(t *testing.T) {
	var extracts atomic.Int64
	writer := &recordingAuditWriter{}
	rt := &bodyCapturingRoundTripper{makeResp: jsonUpstream}

	// emptyResolver has no hooks at all, so BuildPipeline returns a nil pipeline for
	// every stage while compliance stays enabled and the audit row is still emitted.
	bo := c18Options(t, emptyResolver(t), &extracts, nil, writer)
	h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

	req := httptest.NewRequest(http.MethodPost, "https://api.example.com/v1/chat/completions", strings.NewReader(c18RequestBody))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got := extracts.Load(); got != 0 {
		t.Fatalf("adapter extractions = %d, want 0: with no hook bound the normalized payload "+
			"has no consumer and must not be computed", got)
	}
	if got := rt.calls(); got != 1 {
		t.Fatalf("upstream forwards = %d, want 1: skipping the decode must not change delivery", got)
	}
	events := writer.snapshot()
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	if events[0].Model != "gpt-4o" {
		t.Fatalf("audit row Model = %q, want %q. DetectRequestMeta must stay unconditional — "+
			"it feeds the audit row, unlike the normalized payload.", events[0].Model, "gpt-4o")
	}
}

// TestForwardHandler_ResponseNormalizeGating is the response-phase half. It is a
// separate call site with a separate consumer (respInput.Normalized) and its own
// unconditional neighbour (DetectResponseUsage, which reaches the audit row), so the
// request-phase tests above do not cover it.
func TestForwardHandler_ResponseNormalizeGating(t *testing.T) {
	for _, tc := range []struct {
		name         string
		resolver     func(*testing.T, *core.HookInput) *compliance.PolicyResolver
		wantExtracts int64
		wantSeen     bool
	}{
		{
			name: "response hook bound: decode still runs and reaches the hook",
			resolver: func(t *testing.T, seen *core.HookInput) *compliance.PolicyResolver {
				return capturingResolver(t, "response", seen)
			},
			wantExtracts: 1,
			wantSeen:     true,
		},
		{
			name:         "no response hook: decode has no consumer and is skipped",
			resolver:     func(t *testing.T, _ *core.HookInput) *compliance.PolicyResolver { return emptyResolver(t) },
			wantExtracts: 0,
			wantSeen:     false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen core.HookInput
			var respExtracts atomic.Int64
			writer := &recordingAuditWriter{}
			rt := &bodyCapturingRoundTripper{makeResp: openAIJSONUpstream}

			bo := c18Options(t, tc.resolver(t, &seen), nil, &respExtracts, writer)
			h := buildForwardHandler(context.Background(), "api.example.com:443", &UpstreamTransport{transport: rt}, discardSlog(), bo)

			req := httptest.NewRequest(http.MethodPost, "https://api.example.com/v1/chat/completions", strings.NewReader(c18RequestBody))
			req.Header.Set("Content-Type", "application/json")
			h.ServeHTTP(httptest.NewRecorder(), req)

			if got := respExtracts.Load(); got != tc.wantExtracts {
				t.Fatalf("response adapter extractions = %d, want %d", got, tc.wantExtracts)
			}
			if gotSeen := seen.Normalized != nil; gotSeen != tc.wantSeen {
				t.Fatalf("hook saw Normalized = %v, want %v", gotSeen, tc.wantSeen)
			}
			// Delivery and the audit row are unaffected either way: skipping a decode
			// nobody reads must not change what the client or the auditor sees.
			if len(writer.snapshot()) != 1 {
				t.Fatalf("audit events = %d, want 1", len(writer.snapshot()))
			}
		})
	}
}

// openAIJSONUpstream returns an OpenAI-shaped completion so the response-side adapter
// extraction has real content to walk, rather than the opaque body jsonUpstream returns.
func openAIJSONUpstream() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"c-1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"normalized answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)),
	}
}
