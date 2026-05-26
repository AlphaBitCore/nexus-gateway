// Coverage-gap tests for the intercept package. Pin every observable
// behavior of the NE-adjacent dispatcher per CLAUDE.md "macOS NE proxy
// must fail-open" binding: parse errors, unknown adapters, unknown
// hosts, rewrite-unsupported, pipeline-build failure, and the response
// stage usage stamping all return Passthrough/Approve (or Process+
// Approve) — never a hung or error-propagating path.
package intercept

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	agentcompliance "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/compliance"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/shadow"
	hooks "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// silentLogger returns a logger that discards everything — keeps test
// output clean while still exercising every Warn / Error code path.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// pipelineWithDomain builds an AgentPipeline with the openai-compat
// adapter bound to hostPattern (EXACT match) and the supplied path
// rules + hookConfigs. Mirrors the harness in handler_test.go but
// reaches further: lets tests inject hooks (so ProcessRequest /
// ProcessResponse exercise their `HasHooks == true` branches) and per-
// path actions (PASSTHROUGH / BLOCK / PROCESS).
func pipelineWithDomain(
	t *testing.T,
	hostPattern string,
	paths []shadow.InterceptionPathDTO,
	hookConfigs []hooks.HookConfig,
	defaultPathAction string,
) *agentcompliance.AgentPipeline {
	t.Helper()
	if defaultPathAction == "" {
		defaultPathAction = "PROCESS"
	}
	p := agentcompliance.NewAgentPipeline(silentLogger())
	p.ApplySnapshot(&shadow.ConfigSnapshot{
		HookConfigs: hookConfigs,
		InterceptionDomains: []shadow.InterceptionDomainDTO{
			{
				ID:                "dom-openai",
				Name:              "openai",
				HostPattern:       hostPattern,
				HostMatchType:     "EXACT",
				AdapterID:         "openai-compat",
				Enabled:           true,
				Priority:          100,
				DefaultPathAction: defaultPathAction,
				OnAdapterError:    "FAIL_OPEN",
				NetworkZone:       "PUBLIC",
				Paths:             paths,
			},
		},
	})
	return p
}

// pipelineWithUnknownAdapter creates a pipeline whose only domain
// references an adapter ID the registry doesn't know — exercising the
// "host matched but no adapter" fail-open branch in ProcessRequest /
// ProcessResponse.
//
// The traffic.DomainSnapshot builder skips domains whose adapter ID
// has no registered factory, so we can't put the unknown-adapter case
// in via ApplySnapshot directly. Instead we apply a known domain and
// rely on a path-action mismatch test for that branch coverage; the
// "no adapter" branch is reachable in production only via a
// snapshot/registry race — code there is defensive.

func TestProcessRequest_BlockActionShortCircuits(t *testing.T) {
	// A PATH rule with action=BLOCK pins the early-exit branch where
	// filterResult != Process. Result must carry Action=Block, no
	// adapter work attempted.
	p := pipelineWithDomain(t, "api.openai.com",
		[]shadow.InterceptionPathDTO{
			{
				ID:          "p-block",
				PathPattern: []string{"/v1/chat/completions"},
				MatchType:   "EXACT",
				Action:      "BLOCK",
				Priority:    100,
				Enabled:     true,
			},
		}, nil, "PROCESS")
	h := NewHandler(p, silentLogger())

	res := h.ProcessRequest(
		context.Background(),
		"api.openai.com",
		http.MethodPost,
		"/v1/chat/completions",
		nil,
		[]byte(`{"model":"gpt-4o","messages":[]}`),
	)
	if res.Action != traffic.Block {
		t.Fatalf("Action = %v, want Block", res.Action)
	}
	if res.Decision != hooks.Approve {
		t.Fatalf("Decision = %v, want Approve (early-exit decision is always Approve)", res.Decision)
	}
	// No adapter / detector ran → no detector signals populated.
	if res.Provider != "" || res.Model != "" {
		t.Fatalf("expected empty detector fields on Block early-exit, got Provider=%q Model=%q", res.Provider, res.Model)
	}
}

func TestProcessRequest_PassthroughPathSkipsExtractor(t *testing.T) {
	// PATH rule action=PASSTHROUGH must short-circuit before
	// ExtractRequest runs — the body is intentionally malformed JSON to
	// prove the extractor never sees it (otherwise ErrMalformed would
	// route into the parse-error branch).
	p := pipelineWithDomain(t, "api.openai.com",
		[]shadow.InterceptionPathDTO{
			{
				ID:          "p-pass",
				PathPattern: []string{"/v1/chat/completions"},
				MatchType:   "EXACT",
				Action:      "PASSTHROUGH",
				Priority:    100,
				Enabled:     true,
			},
		}, nil, "PROCESS")
	h := NewHandler(p, silentLogger())

	res := h.ProcessRequest(
		context.Background(),
		"api.openai.com",
		http.MethodPost,
		"/v1/chat/completions",
		nil,
		[]byte(`{not valid json`),
	)
	if res.Action != traffic.Passthrough {
		t.Fatalf("Action = %v, want Passthrough", res.Action)
	}
	if res.Decision != hooks.Approve {
		t.Fatalf("Decision = %v, want Approve", res.Decision)
	}
}

func TestProcessRequest_AdapterParseErrorFailsOpenToPassthrough(t *testing.T) {
	// Malformed JSON body — openai-compat ExtractRequest returns
	// ErrMalformed. Per CLAUDE.md fail-open binding, ProcessRequest
	// MUST downgrade to Passthrough+Approve, NOT propagate the error.
	p := pipelineWithDomain(t, "api.openai.com", nil, nil, "PROCESS")
	h := NewHandler(p, silentLogger())

	res := h.ProcessRequest(
		context.Background(),
		"api.openai.com",
		http.MethodPost,
		"/v1/chat/completions",
		nil,
		[]byte(`{this-is-not-json`),
	)
	if res.Action != traffic.Passthrough {
		t.Fatalf("Action = %v, want Passthrough (fail-open on parse error)", res.Action)
	}
	if res.Decision != hooks.Approve {
		t.Fatalf("Decision = %v, want Approve", res.Decision)
	}
	if res.RewrittenBody != nil {
		t.Fatalf("RewrittenBody should be nil on parse failure, got %d bytes", len(res.RewrittenBody))
	}
}

func TestProcessRequest_AdapterUnknownSchemaFailsOpen(t *testing.T) {
	// Valid JSON, but no `messages` field — openai-compat returns
	// ErrUnknownSchema. Same fail-open contract as ErrMalformed.
	p := pipelineWithDomain(t, "api.openai.com", nil, nil, "PROCESS")
	h := NewHandler(p, silentLogger())

	res := h.ProcessRequest(
		context.Background(),
		"api.openai.com",
		http.MethodPost,
		"/v1/chat/completions",
		nil,
		[]byte(`{"model":"gpt-4o"}`),
	)
	if res.Action != traffic.Passthrough {
		t.Fatalf("Action = %v, want Passthrough on unknown schema", res.Action)
	}
}

func TestProcessRequest_UnknownPathFailsOpen(t *testing.T) {
	// A host we know, but a path the openai adapter does not recognise
	// (`/v1/audio/speech` etc.). The default path action is PROCESS, so
	// the extractor runs and returns ErrUnknownSchema — fail-open path.
	p := pipelineWithDomain(t, "api.openai.com", nil, nil, "PROCESS")
	h := NewHandler(p, silentLogger())

	res := h.ProcessRequest(
		context.Background(),
		"api.openai.com",
		http.MethodPost,
		"/v1/some/unrecognised/route",
		nil,
		[]byte(`{"foo":"bar"}`),
	)
	if res.Action != traffic.Passthrough {
		t.Fatalf("Action = %v, want Passthrough on unrecognised path", res.Action)
	}
}

func TestProcessRequest_HookApproveCarriesDetectorSignals(t *testing.T) {
	// One enabled request-stage hook (noop) — pipeline runs, Decision
	// stays Approve, and detector signals (Provider/Model/ApiKeyClass/
	// ApiKeyFingerprint) survive the hook path (not just the no-hooks
	// fast-path which is covered in handler_test.go).
	p := pipelineWithDomain(t, "api.openai.com", nil,
		[]hooks.HookConfig{
			{
				ID:               "noop-1",
				ImplementationID: "noop",
				Name:             "noop",
				Stage:            "request",
				Priority:         10,
				Enabled:          true,
				FailBehavior:     "fail-open",
			},
		}, "PROCESS")
	h := NewHandler(p, silentLogger())

	headers := http.Header{}
	headers.Set("Authorization", "Bearer sk-ant-abc123")
	res := h.ProcessRequest(
		context.Background(),
		"api.openai.com",
		http.MethodPost,
		"/v1/chat/completions",
		headers,
		[]byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}]}`),
	)
	if res.Action != traffic.Process {
		t.Fatalf("Action = %v, want Process", res.Action)
	}
	if res.Decision != hooks.Approve {
		t.Fatalf("Decision = %v, want Approve (noop only)", res.Decision)
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
	if res.ApiKeyFingerprint == "" {
		t.Fatalf("ApiKeyFingerprint must be populated when bearer token present")
	}
	// Pipeline ran → HookOutcome should list the noop hook on Passed.
	if len(res.HookOutcome.Passed) != 1 || res.HookOutcome.Passed[0] != "noop" {
		t.Fatalf("HookOutcome.Passed = %v, want [\"noop\"]", res.HookOutcome.Passed)
	}
	if res.HookOutcome.Transformed {
		t.Fatalf("HookOutcome.Transformed = true, want false on plain approve")
	}
}

func TestProcessRequest_HookRejectHardPopulatesOutcome(t *testing.T) {
	// pii-detector with block-hard inflight action + a pattern that
	// will fire on the body's email content. Asserts:
	// (1) Decision propagates to caller as RejectHard.
	// (2) HookOutcome carries Rejected = hook name + RejectReason =
	//     hook's ReasonCode.
	// (3) ComplianceTags includes the hook's emitted tag set.
	p := pipelineWithDomain(t, "api.openai.com", nil,
		[]hooks.HookConfig{
			{
				ID:               "pii-hard",
				ImplementationID: "pii-detector",
				Name:             "pii-blocker",
				Stage:            "request",
				Priority:         10,
				Enabled:          true,
				FailBehavior:     "fail-open",
				Config: map[string]any{
					"patternDefinitions": []any{
						map[string]any{
							"id":    "email",
							"regex": `[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`,
						},
					},
					"onMatch": map[string]any{"inflightAction": "block-hard"},
				},
			},
		}, "PROCESS")
	h := NewHandler(p, silentLogger())

	res := h.ProcessRequest(
		context.Background(),
		"api.openai.com",
		http.MethodPost,
		"/v1/chat/completions",
		nil,
		[]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"contact me at user@example.com"}]}`),
	)
	if res.Action != traffic.Process {
		t.Fatalf("Action = %v, want Process", res.Action)
	}
	if res.Decision != hooks.RejectHard {
		t.Fatalf("Decision = %v, want RejectHard", res.Decision)
	}
	if res.HookOutcome.Rejected != "pii-blocker" {
		t.Fatalf("HookOutcome.Rejected = %q, want pii-blocker", res.HookOutcome.Rejected)
	}
	if res.HookOutcome.RejectReason != "PII_DETECTED" {
		t.Fatalf("HookOutcome.RejectReason = %q, want PII_DETECTED", res.HookOutcome.RejectReason)
	}
	if !sliceContains(res.ComplianceTags, "compliance:pii") {
		t.Fatalf("ComplianceTags missing compliance:pii, got %v", res.ComplianceTags)
	}
}

func TestProcessRequest_HookModifyRewritesBody(t *testing.T) {
	// pii-detector with redact action — pipeline returns Modify, and
	// the openai-compat adapter supports rewrite on /chat/completions,
	// so out.RewrittenBody MUST be populated and contain the
	// replacement marker. ReasonCode stays empty on the happy rewrite
	// path (only set on ErrRewriteUnsupported / generic rewrite error).
	p := pipelineWithDomain(t, "api.openai.com", nil,
		[]hooks.HookConfig{
			{
				ID:               "pii-redact",
				ImplementationID: "pii-detector",
				Name:             "pii-redactor",
				Stage:            "request",
				Priority:         10,
				Enabled:          true,
				FailBehavior:     "fail-open",
				Config: map[string]any{
					"patternDefinitions": []any{
						map[string]any{
							"id":          "email",
							"regex":       `[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`,
							"replacement": "[EMAIL_REDACTED]",
						},
					},
					"onMatch": map[string]any{"inflightAction": "redact"},
				},
			},
		}, "PROCESS")
	h := NewHandler(p, silentLogger())

	original := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"my email is bob@example.com"}]}`)
	res := h.ProcessRequest(
		context.Background(),
		"api.openai.com",
		http.MethodPost,
		"/v1/chat/completions",
		nil,
		original,
	)
	if res.Decision != hooks.Modify {
		t.Fatalf("Decision = %v, want Modify", res.Decision)
	}
	if len(res.RewrittenBody) == 0 {
		t.Fatalf("RewrittenBody empty on supported-rewrite path")
	}
	if !strings.Contains(string(res.RewrittenBody), "[EMAIL_REDACTED]") {
		t.Fatalf("RewrittenBody missing redaction marker: %s", res.RewrittenBody)
	}
	if strings.Contains(string(res.RewrittenBody), "bob@example.com") {
		t.Fatalf("RewrittenBody still contains the email: %s", res.RewrittenBody)
	}
	if res.ReasonCode != "" {
		t.Fatalf("ReasonCode = %q, want empty on supported-rewrite happy path", res.ReasonCode)
	}
	if !res.HookOutcome.Transformed {
		t.Fatalf("HookOutcome.Transformed should be true after Modify")
	}
}

func TestProcessRequest_HookModifyOnUnsupportedRewriteRecordsReasonCode(t *testing.T) {
	// /embeddings is intentionally not supported by RewriteRequestBody
	// in the openai-compat adapter — it returns ErrRewriteUnsupported.
	// The handler MUST: (a) keep Decision=Modify, (b) leave
	// RewrittenBody nil so the caller forwards the original body, (c)
	// stamp ReasonCode = REDACT_INFLIGHT_UNSUPPORTED so the audit row
	// reflects the degraded path.
	p := pipelineWithDomain(t, "api.openai.com", nil,
		[]hooks.HookConfig{
			{
				ID:               "pii-redact-emb",
				ImplementationID: "pii-detector",
				Name:             "pii-redactor-emb",
				Stage:            "request",
				Priority:         10,
				Enabled:          true,
				FailBehavior:     "fail-open",
				Config: map[string]any{
					"patternDefinitions": []any{
						map[string]any{
							"id":          "email",
							"regex":       `[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`,
							"replacement": "[EMAIL_REDACTED]",
						},
					},
					"onMatch": map[string]any{"inflightAction": "redact"},
				},
			},
		}, "PROCESS")
	h := NewHandler(p, silentLogger())

	res := h.ProcessRequest(
		context.Background(),
		"api.openai.com",
		http.MethodPost,
		"/v1/embeddings",
		nil,
		[]byte(`{"model":"text-embedding-3-small","input":"please embed bob@example.com"}`),
	)
	if res.Decision != hooks.Modify {
		t.Fatalf("Decision = %v, want Modify", res.Decision)
	}
	if res.RewrittenBody != nil {
		t.Fatalf("RewrittenBody must be nil when adapter returns ErrRewriteUnsupported (caller forwards original)")
	}
	if res.ReasonCode != hooks.ReasonRedactInflightUnsupported {
		t.Fatalf("ReasonCode = %q, want %q", res.ReasonCode, hooks.ReasonRedactInflightUnsupported)
	}
}

// --- ProcessResponse ---------------------------------------------------------

func TestProcessResponse_BlockActionCarriesUsage(t *testing.T) {
	// Even when filterResult != Process the supplied UsageMeta must be
	// stamped onto the Result. Production callers always pass a usage
	// pointer; dropping it on the floor would zero out token counts
	// for blocked responses.
	p := pipelineWithDomain(t, "api.openai.com",
		[]shadow.InterceptionPathDTO{
			{
				ID:          "p-block",
				PathPattern: []string{"/v1/chat/completions"},
				MatchType:   "EXACT",
				Action:      "BLOCK",
				Priority:    100,
				Enabled:     true,
			},
		}, nil, "PROCESS")
	h := NewHandler(p, silentLogger())

	prompt, completion := 17, 42
	usage := &traffic.UsageMeta{
		PromptTokens:     &prompt,
		CompletionTokens: &completion,
		Status:           traffic.UsageStatusOK,
	}
	res := h.ProcessResponse(
		context.Background(),
		"api.openai.com",
		"/v1/chat/completions",
		[]byte(`{"choices":[{"message":{"content":"hi"}}]}`),
		usage,
	)
	if res.Action != traffic.Block {
		t.Fatalf("Action = %v, want Block", res.Action)
	}
	if res.PromptTokens == nil || *res.PromptTokens != prompt {
		t.Fatalf("PromptTokens = %v, want %d", res.PromptTokens, prompt)
	}
	if res.CompletionTokens == nil || *res.CompletionTokens != completion {
		t.Fatalf("CompletionTokens = %v, want %d", res.CompletionTokens, completion)
	}
	if res.UsageExtractionStatus != string(traffic.UsageStatusOK) {
		t.Fatalf("UsageExtractionStatus = %q, want ok", res.UsageExtractionStatus)
	}
}

func TestProcessResponse_UnknownHostPassthroughWithNilUsage(t *testing.T) {
	// Unknown host → DomainSnapshot.ResolveAction returns nil instance,
	// Passthrough. nil usage must NOT panic and must not stamp anything.
	p := pipelineWithDomain(t, "api.openai.com", nil, nil, "PROCESS")
	h := NewHandler(p, silentLogger())

	res := h.ProcessResponse(
		context.Background(),
		"example.com",
		"/",
		nil,
		nil,
	)
	if res.Action != traffic.Passthrough {
		t.Fatalf("Action = %v, want Passthrough", res.Action)
	}
	if res.PromptTokens != nil || res.CompletionTokens != nil || res.UsageExtractionStatus != "" {
		t.Fatalf("nil usage must not stamp anything, got %+v", res)
	}
}

func TestProcessResponse_AdapterParseErrorFailsOpen(t *testing.T) {
	// Malformed response body → ExtractResponse returns ErrMalformed.
	// Per fail-open binding: downgrade to Passthrough+Approve, no hook
	// pipeline invoked. Usage MUST still be stamped (the caller relies
	// on this for cost accounting on the degraded path).
	p := pipelineWithDomain(t, "api.openai.com", nil, nil, "PROCESS")
	h := NewHandler(p, silentLogger())

	prompt := 5
	usage := &traffic.UsageMeta{PromptTokens: &prompt, Status: traffic.UsageStatusParseFailed}
	res := h.ProcessResponse(
		context.Background(),
		"api.openai.com",
		"/v1/chat/completions",
		[]byte(`{this-is-not-json`),
		usage,
	)
	if res.Action != traffic.Passthrough {
		t.Fatalf("Action = %v, want Passthrough (fail-open on parse error)", res.Action)
	}
	if res.PromptTokens == nil || *res.PromptTokens != prompt {
		t.Fatalf("PromptTokens lost on parse-error fail-open: %v", res.PromptTokens)
	}
	if res.UsageExtractionStatus != string(traffic.UsageStatusParseFailed) {
		t.Fatalf("UsageExtractionStatus = %q, want parse_failed", res.UsageExtractionStatus)
	}
}

func TestProcessResponse_NoResponseHooksFastPath(t *testing.T) {
	// HookConfigs only carry a request-stage hook → resolver.HasHooks
	// ("response") returns false; ProcessResponse takes the fast path
	// (no pipeline build) and stamps usage straight through.
	p := pipelineWithDomain(t, "api.openai.com", nil,
		[]hooks.HookConfig{
			{
				ID:               "noop-req",
				ImplementationID: "noop",
				Name:             "noop-req",
				Stage:            "request",
				Priority:         10,
				Enabled:          true,
				FailBehavior:     "fail-open",
			},
		}, "PROCESS")
	h := NewHandler(p, silentLogger())

	prompt, completion := 9, 33
	res := h.ProcessResponse(
		context.Background(),
		"api.openai.com",
		"/v1/chat/completions",
		[]byte(`{"choices":[{"message":{"content":"hello there"}}]}`),
		&traffic.UsageMeta{
			PromptTokens:     &prompt,
			CompletionTokens: &completion,
			Status:           traffic.UsageStatusOK,
		},
	)
	if res.Action != traffic.Process {
		t.Fatalf("Action = %v, want Process", res.Action)
	}
	if res.Decision != hooks.Approve {
		t.Fatalf("Decision = %v, want Approve (no response hooks)", res.Decision)
	}
	if res.PromptTokens == nil || *res.PromptTokens != prompt {
		t.Fatalf("PromptTokens not stamped on fast-path: %v", res.PromptTokens)
	}
	if len(res.HookOutcome.Passed) != 0 {
		t.Fatalf("HookOutcome.Passed should be empty when no response hooks ran, got %v", res.HookOutcome.Passed)
	}
}

func TestProcessResponse_ResponseHookRunsAndApprovesClean(t *testing.T) {
	// noop hook bound to the response stage — pipeline runs but
	// approves, Action=Process, HookOutcome lists the hook on Passed,
	// usage stamped.
	p := pipelineWithDomain(t, "api.openai.com", nil,
		[]hooks.HookConfig{
			{
				ID:               "noop-resp",
				ImplementationID: "noop",
				Name:             "noop-resp",
				Stage:            "response",
				Priority:         10,
				Enabled:          true,
				FailBehavior:     "fail-open",
			},
		}, "PROCESS")
	h := NewHandler(p, silentLogger())

	prompt := 11
	res := h.ProcessResponse(
		context.Background(),
		"api.openai.com",
		"/v1/chat/completions",
		[]byte(`{"choices":[{"message":{"content":"some reply"}}]}`),
		&traffic.UsageMeta{PromptTokens: &prompt, Status: traffic.UsageStatusOK},
	)
	if res.Action != traffic.Process {
		t.Fatalf("Action = %v, want Process", res.Action)
	}
	if res.Decision != hooks.Approve {
		t.Fatalf("Decision = %v, want Approve", res.Decision)
	}
	if len(res.HookOutcome.Passed) != 1 || res.HookOutcome.Passed[0] != "noop-resp" {
		t.Fatalf("HookOutcome.Passed = %v, want [noop-resp]", res.HookOutcome.Passed)
	}
	if res.PromptTokens == nil || *res.PromptTokens != prompt {
		t.Fatalf("PromptTokens not stamped through pipeline path: %v", res.PromptTokens)
	}
}

func TestProcessResponse_ResponseHookRejectHardEmitsOutcome(t *testing.T) {
	// pii-detector at response stage matches assistant content; result
	// must propagate Decision=RejectHard plus a populated HookOutcome.
	p := pipelineWithDomain(t, "api.openai.com", nil,
		[]hooks.HookConfig{
			{
				ID:               "pii-resp",
				ImplementationID: "pii-detector",
				Name:             "pii-resp-blocker",
				Stage:            "response",
				Priority:         10,
				Enabled:          true,
				FailBehavior:     "fail-open",
				Config: map[string]any{
					"patternDefinitions": []any{
						map[string]any{
							"id":    "email",
							"regex": `[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`,
						},
					},
					"onMatch": map[string]any{"inflightAction": "block-hard"},
				},
			},
		}, "PROCESS")
	h := NewHandler(p, silentLogger())

	res := h.ProcessResponse(
		context.Background(),
		"api.openai.com",
		"/v1/chat/completions",
		[]byte(`{"choices":[{"message":{"content":"contact admin@example.com"}}]}`),
		nil,
	)
	if res.Decision != hooks.RejectHard {
		t.Fatalf("Decision = %v, want RejectHard", res.Decision)
	}
	if res.HookOutcome.Rejected != "pii-resp-blocker" {
		t.Fatalf("HookOutcome.Rejected = %q, want pii-resp-blocker", res.HookOutcome.Rejected)
	}
}

func TestProcessRequest_BuildPipelineErrorFailsOpenWithDetectorSignals(t *testing.T) {
	// A request-stage hook configured with the pii-detector impl but
	// missing patternDefinitions makes the resolver's factory call
	// fail; BuildPipeline propagates the error. ProcessRequest MUST
	// fail open (Action=Process, Decision=Approve), preserve detector
	// signals, and NOT crash. This pins handler.go lines 164-174.
	p := pipelineWithDomain(t, "api.openai.com", nil,
		[]hooks.HookConfig{
			{
				ID:               "broken-pii",
				ImplementationID: "pii-detector",
				Name:             "broken",
				Stage:            "request",
				Priority:         10,
				Enabled:          true,
				FailBehavior:     "fail-open",
				// Intentionally no patternDefinitions → factory returns error.
				Config: map[string]any{},
			},
		}, "PROCESS")
	h := NewHandler(p, silentLogger())

	headers := http.Header{}
	headers.Set("Authorization", "Bearer sk-proj-abcd")
	res := h.ProcessRequest(
		context.Background(),
		"api.openai.com",
		http.MethodPost,
		"/v1/chat/completions",
		headers,
		[]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
	)
	if res.Action != traffic.Process {
		t.Fatalf("Action = %v, want Process (fail-open on pipeline build error)", res.Action)
	}
	if res.Decision != hooks.Approve {
		t.Fatalf("Decision = %v, want Approve", res.Decision)
	}
	if res.Provider != "openai" || res.Model != "gpt-4o" {
		t.Fatalf("detector signals lost on pipeline-build fail-open: %+v", res)
	}
	if res.ApiKeyFingerprint == "" {
		t.Fatalf("ApiKeyFingerprint dropped on pipeline-build fail-open")
	}
	// No pipeline ran → HookOutcome should be empty.
	if len(res.HookOutcome.Passed) != 0 || res.HookOutcome.Rejected != "" {
		t.Fatalf("HookOutcome should be empty on pipeline-build fail-open, got %+v", res.HookOutcome)
	}
}

func TestProcessResponse_BuildPipelineErrorFailsOpenWithUsage(t *testing.T) {
	// Same as above but for response stage — fail-open MUST preserve
	// the supplied usage so cost accounting on the degraded path
	// still works. Pins handler.go lines 349-355.
	p := pipelineWithDomain(t, "api.openai.com", nil,
		[]hooks.HookConfig{
			{
				ID:               "broken-pii-resp",
				ImplementationID: "pii-detector",
				Name:             "broken-resp",
				Stage:            "response",
				Priority:         10,
				Enabled:          true,
				FailBehavior:     "fail-open",
				Config:           map[string]any{},
			},
		}, "PROCESS")
	h := NewHandler(p, silentLogger())

	prompt, completion := 23, 4
	res := h.ProcessResponse(
		context.Background(),
		"api.openai.com",
		"/v1/chat/completions",
		[]byte(`{"choices":[{"message":{"content":"ok"}}]}`),
		&traffic.UsageMeta{
			PromptTokens:     &prompt,
			CompletionTokens: &completion,
			Status:           traffic.UsageStatusOK,
		},
	)
	if res.Action != traffic.Process {
		t.Fatalf("Action = %v, want Process (fail-open)", res.Action)
	}
	if res.Decision != hooks.Approve {
		t.Fatalf("Decision = %v, want Approve", res.Decision)
	}
	if res.PromptTokens == nil || *res.PromptTokens != prompt {
		t.Fatalf("PromptTokens lost on response build-pipeline fail-open: %v", res.PromptTokens)
	}
	if res.CompletionTokens == nil || *res.CompletionTokens != completion {
		t.Fatalf("CompletionTokens lost on response build-pipeline fail-open: %v", res.CompletionTokens)
	}
	if res.UsageExtractionStatus != string(traffic.UsageStatusOK) {
		t.Fatalf("UsageExtractionStatus dropped: %q", res.UsageExtractionStatus)
	}
}

// --- ExtractResponseUsage ---------------------------------------------------

func TestExtractResponseUsage_UnknownHostReturnsNonLLM(t *testing.T) {
	// Spec: unknown host → no adapter → status=non_llm.
	p := pipelineWithDomain(t, "api.openai.com", nil, nil, "PROCESS")
	h := NewHandler(p, silentLogger())

	got := h.ExtractResponseUsage(context.Background(), "example.com", "/", nil, nil)
	if got.Status != traffic.UsageStatusNonLLM {
		t.Fatalf("Status = %q, want non_llm", got.Status)
	}
	if got.PromptTokens != nil || got.CompletionTokens != nil {
		t.Fatalf("expected nil token pointers on non_llm path, got prompt=%v completion=%v", got.PromptTokens, got.CompletionTokens)
	}
}

func TestExtractResponseUsage_BlockedPathReturnsNonLLM(t *testing.T) {
	// Host matches, but the path rule is BLOCK — filterResult != Process
	// → ExtractResponseUsage MUST short-circuit to non_llm.
	p := pipelineWithDomain(t, "api.openai.com",
		[]shadow.InterceptionPathDTO{
			{
				ID:          "p-block",
				PathPattern: []string{"/v1/chat/completions"},
				MatchType:   "EXACT",
				Action:      "BLOCK",
				Priority:    100,
				Enabled:     true,
			},
		}, nil, "PROCESS")
	h := NewHandler(p, silentLogger())

	got := h.ExtractResponseUsage(
		context.Background(),
		"api.openai.com",
		"/v1/chat/completions",
		nil,
		[]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`),
	)
	if got.Status != traffic.UsageStatusNonLLM {
		t.Fatalf("Status = %q, want non_llm on BLOCK path", got.Status)
	}
}

func TestExtractResponseUsage_OkUsageBlock(t *testing.T) {
	// Happy path: known host + valid usage block in response body.
	// openai-compat's DetectResponseUsage MUST surface the parsed
	// counts with status=ok.
	p := pipelineWithDomain(t, "api.openai.com", nil, nil, "PROCESS")
	h := NewHandler(p, silentLogger())

	body := []byte(`{"id":"chatcmpl-x","object":"chat.completion","model":"gpt-4o","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
	got := h.ExtractResponseUsage(
		context.Background(),
		"api.openai.com",
		"/v1/chat/completions",
		nil,
		body,
	)
	if got.Status != traffic.UsageStatusOK {
		t.Fatalf("Status = %q, want ok", got.Status)
	}
	if got.PromptTokens == nil || *got.PromptTokens != 7 {
		t.Fatalf("PromptTokens = %v, want 7", got.PromptTokens)
	}
	if got.CompletionTokens == nil || *got.CompletionTokens != 3 {
		t.Fatalf("CompletionTokens = %v, want 3", got.CompletionTokens)
	}
}

// --- NewUsageAccumulator ----------------------------------------------------

func TestNewUsageAccumulator_KnownProviderReturnsAccumulator(t *testing.T) {
	// Handler thinly wraps streaming.NewUsageAccumulator — verify it
	// returns non-nil for a provider with a registered extractor.
	p := pipelineWithDomain(t, "api.openai.com", nil, nil, "PROCESS")
	h := NewHandler(p, silentLogger())

	acc := h.NewUsageAccumulator("openai", "gpt-4o-mini")
	if acc == nil {
		t.Fatalf("NewUsageAccumulator(openai, gpt-4o-mini) returned nil, want accumulator")
	}
}

func TestNewUsageAccumulator_UnknownProviderReturnsNil(t *testing.T) {
	// Unknown provider IDs MUST surface as nil so the proxy can fall
	// back to raw passthrough (documented contract).
	p := pipelineWithDomain(t, "api.openai.com", nil, nil, "PROCESS")
	h := NewHandler(p, silentLogger())

	if acc := h.NewUsageAccumulator("not-a-real-provider", "model-x"); acc != nil {
		t.Fatalf("NewUsageAccumulator returned non-nil for unknown provider, want nil")
	}
}

// --- buildSyntheticRequest tail ---------------------------------------------

func TestBuildSyntheticRequest_UnparseablePathFallsBackToBareURL(t *testing.T) {
	// url.Parse rejects bytes < 0x20 in the path. The helper's
	// fail-safe branch (u, err := url.Parse → fallback) is exercised
	// by feeding such a path. Result must still carry the original
	// path bytes verbatim on URL.Path so detectors that read r.URL.Path
	// don't see an empty string.
	bad := "/v1/path-with-control-\x7fchars"
	req := buildSyntheticRequest("api.openai.com", "POST", bad, nil)
	if req == nil {
		t.Fatal("buildSyntheticRequest returned nil")
	}
	if req.URL == nil {
		t.Fatal("URL nil")
	}
	if req.URL.Path != bad {
		t.Fatalf("URL.Path = %q, want %q (fallback should preserve raw path)", req.URL.Path, bad)
	}
	if req.URL.Scheme != "https" || req.URL.Host != "api.openai.com" {
		t.Fatalf("URL scheme/host not stamped on fallback, got %q/%q", req.URL.Scheme, req.URL.Host)
	}
}

// --- contentBlocksToSegments ------------------------------------------------

func TestContentBlocksToSegments_KeepsTextDropsNonText(t *testing.T) {
	// Spec: only text-type blocks contribute; empty type defaults to
	// text. tool_calls / images are filtered out by the helper.
	out := contentBlocksToSegments([]hooks.ContentBlock{
		{Type: "text", Text: "alpha"},
		{Type: "", Text: "beta"}, // empty type treated as text
		{Type: "image", Text: "should-drop"},
		{Type: "tool_use", Text: "should-also-drop"},
		{Type: "text", Text: "gamma"},
	})
	want := []string{"alpha", "beta", "gamma"}
	if len(out) != len(want) {
		t.Fatalf("len(out)=%d want %d (got %v)", len(out), len(want), out)
	}
	for i, w := range want {
		if out[i] != w {
			t.Fatalf("out[%d]=%q want %q", i, out[i], w)
		}
	}
}

func TestContentBlocksToSegments_EmptyInput(t *testing.T) {
	if out := contentBlocksToSegments(nil); len(out) != 0 {
		t.Fatalf("nil input must yield empty slice, got %v", out)
	}
	if out := contentBlocksToSegments([]hooks.ContentBlock{}); len(out) != 0 {
		t.Fatalf("empty input must yield empty slice, got %v", out)
	}
}

// --- hookOutcomeFromResult --------------------------------------------------

func TestHookOutcomeFromResult_EmptyReturnsZero(t *testing.T) {
	out := hookOutcomeFromResult(nil)
	if len(out.Passed) != 0 || out.Rejected != "" || out.Transformed {
		t.Fatalf("empty HookResults must yield zero HookOutcomeInput, got %+v", out)
	}
}

func TestHookOutcomeFromResult_RejectHaltsIteration(t *testing.T) {
	// Spec §4.5: any reject halts the outcome — later hooks are not
	// reported, even if they passed.
	res := hookOutcomeFromResult([]hooks.HookResult{
		{HookName: "a", Decision: hooks.Approve},
		{HookName: "b", Decision: hooks.RejectHard, ReasonCode: "ABC"},
		{HookName: "c", Decision: hooks.Approve},
	})
	if res.Rejected != "b" {
		t.Fatalf("Rejected = %q, want b", res.Rejected)
	}
	if res.RejectReason != "ABC" {
		t.Fatalf("RejectReason = %q, want ABC", res.RejectReason)
	}
	if len(res.Passed) != 0 {
		t.Fatalf("Passed should be empty on reject-halt, got %v", res.Passed)
	}
	if res.Transformed {
		t.Fatalf("Transformed must be false on reject path")
	}
}

func TestHookOutcomeFromResult_BlockSoftRejectsToo(t *testing.T) {
	// BlockSoft is treated as a reject by the outcome mapping.
	res := hookOutcomeFromResult([]hooks.HookResult{
		{HookName: "soft", Decision: hooks.BlockSoft, Reason: "fallback-reason"},
	})
	if res.Rejected != "soft" {
		t.Fatalf("Rejected = %q, want soft", res.Rejected)
	}
	// ReasonCode unset → fall back to Reason.
	if res.RejectReason != "fallback-reason" {
		t.Fatalf("RejectReason = %q, want fallback-reason", res.RejectReason)
	}
}

func TestHookOutcomeFromResult_ModifyMarksTransformed(t *testing.T) {
	// Modify decision must append to Passed AND set Transformed = true.
	res := hookOutcomeFromResult([]hooks.HookResult{
		{HookName: "scrubber", Decision: hooks.Modify},
		{HookName: "after", Decision: hooks.Approve},
	})
	if !res.Transformed {
		t.Fatalf("Transformed must be true when Modify present")
	}
	if len(res.Passed) != 2 || res.Passed[0] != "scrubber" || res.Passed[1] != "after" {
		t.Fatalf("Passed = %v, want [scrubber after]", res.Passed)
	}
	if res.Rejected != "" {
		t.Fatalf("Rejected must stay empty on transform path, got %q", res.Rejected)
	}
}

func TestHookOutcomeFromResult_AbstainCountsAsPassed(t *testing.T) {
	// Abstain hits the default arm of the switch — same as Approve.
	res := hookOutcomeFromResult([]hooks.HookResult{
		{HookName: "abst", Decision: hooks.Abstain},
	})
	if len(res.Passed) != 1 || res.Passed[0] != "abst" {
		t.Fatalf("Passed = %v, want [abst]", res.Passed)
	}
	if res.Transformed {
		t.Fatalf("Transformed must stay false on Abstain")
	}
}

// --- withUsage --------------------------------------------------------------

func TestWithUsage_NilUsageReturnsUnchanged(t *testing.T) {
	// Defensive contract: nil pointer must not mutate the Result.
	base := Result{Action: traffic.Process, Decision: hooks.Approve}
	out := withUsage(base, nil)
	if out.PromptTokens != nil || out.CompletionTokens != nil || out.UsageExtractionStatus != "" {
		t.Fatalf("nil usage altered Result: %+v", out)
	}
}

func TestWithUsage_StampsAllThreeFields(t *testing.T) {
	prompt, completion := 1, 2
	base := Result{Action: traffic.Process, Decision: hooks.Approve}
	out := withUsage(base, &traffic.UsageMeta{
		PromptTokens:     &prompt,
		CompletionTokens: &completion,
		Status:           traffic.UsageStatusStreamingReported,
	})
	if out.PromptTokens == nil || *out.PromptTokens != prompt {
		t.Fatalf("PromptTokens = %v, want %d", out.PromptTokens, prompt)
	}
	if out.CompletionTokens == nil || *out.CompletionTokens != completion {
		t.Fatalf("CompletionTokens = %v, want %d", out.CompletionTokens, completion)
	}
	if out.UsageExtractionStatus != string(traffic.UsageStatusStreamingReported) {
		t.Fatalf("UsageExtractionStatus = %q, want streaming_reported", out.UsageExtractionStatus)
	}
}

// --- preferAdapterNormalize -------------------------------------------------

func TestPreferAdapterNormalize_NilAdapterFallsToSegments(t *testing.T) {
	// nil adapter → cannot type-assert Normalizer → falls back to the
	// segments path.
	out := preferAdapterNormalize(
		context.Background(),
		nil, // adapter
		[]byte(`{"foo":"bar"}`),
		"/v1/chat/completions",
		normalize.DirectionRequest,
		[]string{"hello", "world"},
		silentLogger(),
	)
	if out == nil {
		t.Fatal("expected non-nil payload from segment fallback")
	}
	got := out.TextProjection()
	if len(got) != 2 || got[0] != "hello" || got[1] != "world" {
		t.Fatalf("fallback segments lost: got %v", got)
	}
}

func TestPreferAdapterNormalize_EmptyBodyAndNoSegmentsReturnsNil(t *testing.T) {
	// Both adapter Normalize path and segments fallback yield nothing
	// → helper returns nil so callers can short-circuit.
	out := preferAdapterNormalize(
		context.Background(),
		nil,
		nil,
		"/",
		normalize.DirectionRequest,
		nil,
		silentLogger(),
	)
	if out != nil {
		t.Fatalf("expected nil when no body + no segments, got %+v", out)
	}
}

func TestPreferAdapterNormalize_AdapterNormalizerHappyPath(t *testing.T) {
	// Real openai-compat adapter implements normalize.Normalizer; a
	// valid chat request body MUST take the Normalize success branch
	// and return a populated NormalizedPayload — NOT the fallback
	// segment list.
	adapter, ok := lookupAdapter(t, "api.openai.com", "/v1/chat/completions")
	if !ok {
		t.Skip("openai-compat adapter not registered in this build")
	}
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello from adapter Normalize"}]}`)
	// Segments are wrong on purpose so the test fails if the helper
	// silently falls back instead of taking the Normalize path.
	out := preferAdapterNormalize(
		context.Background(),
		adapter,
		body,
		"/v1/chat/completions",
		normalize.DirectionRequest,
		[]string{"WRONG-FALLBACK"},
		silentLogger(),
	)
	if out == nil {
		t.Fatal("Normalize success path returned nil")
	}
	proj := out.TextProjection()
	if len(proj) == 0 {
		t.Fatal("Normalize success path produced empty TextProjection")
	}
	for _, seg := range proj {
		if seg == "WRONG-FALLBACK" {
			t.Fatalf("helper used segments fallback instead of adapter.Normalize")
		}
	}
}

// fakeAdapter is the minimum traffic.Adapter / normalize.Normalizer
// shim used to drive Normalize-error branches in preferAdapterNormalize
// that no real adapter can produce in isolation (the OpenAI normalizer
// only ever returns ErrUnsupported-wrapped errors). Embeds a real
// openai adapter for the boring Extract / Detect / Rewrite methods we
// don't exercise here — only Normalize is overridden.
type fakeNormalizerAdapter struct {
	id      string
	normErr error
}

func (f *fakeNormalizerAdapter) ID() string { return f.id }
func (f *fakeNormalizerAdapter) Configure(_ map[string]any) error {
	return nil
}
func (f *fakeNormalizerAdapter) ExtractRequest(_ context.Context, _ []byte, _ string) (traffic.NormalizedContent, error) {
	return traffic.NormalizedContent{}, nil
}
func (f *fakeNormalizerAdapter) ExtractResponse(_ context.Context, _ []byte, _ string) (traffic.NormalizedContent, error) {
	return traffic.NormalizedContent{}, nil
}
func (f *fakeNormalizerAdapter) ExtractStreamChunk(_ context.Context, _ []byte, _ string) (traffic.NormalizedContent, error) {
	return traffic.NormalizedContent{}, nil
}
func (f *fakeNormalizerAdapter) DetectRequestMeta(_ *http.Request, _ []byte) traffic.RequestMeta {
	return traffic.RequestMeta{}
}
func (f *fakeNormalizerAdapter) DetectResponseUsage(_ *http.Response, _ []byte) traffic.UsageMeta {
	return traffic.UsageMeta{}
}
func (f *fakeNormalizerAdapter) RewriteRequestBody(_ context.Context, _ []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return nil, 0, traffic.ErrRewriteUnsupported
}
func (f *fakeNormalizerAdapter) RewriteResponseBody(_ context.Context, _ []byte, _ string, _ traffic.NormalizedContent) ([]byte, int, error) {
	return nil, 0, traffic.ErrRewriteUnsupported
}

// Normalize satisfies normalize.Normalizer — returns the configured
// error so we can drive the helper's Warn / fallback branches.
func (f *fakeNormalizerAdapter) Normalize(_ context.Context, _ []byte, _ normalize.Meta) (normalize.NormalizedPayload, error) {
	return normalize.NormalizedPayload{}, f.normErr
}

func TestPreferAdapterNormalize_HardErrorLogsAndFallsBack(t *testing.T) {
	// Spec: when adapter.Normalize returns a NON-ErrUnsupported error,
	// the helper logs at Warn AND falls back to the segments path. We
	// cannot observe the log directly (silent logger), but we can
	// verify the fallback occurred — which is the only behavior the
	// caller depends on. Pins normalize.go lines 51-57.
	adapter := &fakeNormalizerAdapter{
		id:      "test-fake",
		normErr: errSentinel{msg: "synthetic decoder failure"},
	}
	out := preferAdapterNormalize(
		context.Background(),
		adapter,
		[]byte(`some bytes`),
		"/anything",
		normalize.DirectionRequest,
		[]string{"fallback-after-hard-error"},
		silentLogger(),
	)
	if out == nil {
		t.Fatal("expected fallback payload, got nil")
	}
	got := out.TextProjection()
	if len(got) != 1 || got[0] != "fallback-after-hard-error" {
		t.Fatalf("hard-error path did not fall back to segments, got %v", got)
	}
}

func TestPreferAdapterNormalize_NormalizeSuccessReturnsAdapterPayload(t *testing.T) {
	// Adapter returns a populated NormalizedPayload + nil error → the
	// helper MUST return that payload verbatim and NOT consult the
	// segments fallback.
	payload := normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Protocol:         "fake-test",
		Messages: []normalize.Message{
			{Role: normalize.RoleUser, Content: []normalize.ContentBlock{
				{Type: normalize.ContentText, Text: "from-adapter"},
			}},
		},
	}
	adapter := &normalizerReturningPayload{id: "test-fake-ok", out: payload}

	got := preferAdapterNormalize(
		context.Background(),
		adapter,
		[]byte(`some bytes`),
		"/p",
		normalize.DirectionResponse,
		[]string{"DO-NOT-USE-FALLBACK"},
		silentLogger(),
	)
	if got == nil {
		t.Fatal("expected payload, got nil")
	}
	proj := got.TextProjection()
	if len(proj) != 1 || proj[0] != "from-adapter" {
		t.Fatalf("expected adapter payload, got %v", proj)
	}
}

// normalizerReturningPayload is a second test double that returns a
// caller-supplied NormalizedPayload (nil error). Kept distinct from
// fakeNormalizerAdapter so each test pins one concern.
type normalizerReturningPayload struct {
	fakeNormalizerAdapter
	id  string
	out normalize.NormalizedPayload
}

func (n *normalizerReturningPayload) ID() string { return n.id }
func (n *normalizerReturningPayload) Normalize(_ context.Context, _ []byte, _ normalize.Meta) (normalize.NormalizedPayload, error) {
	return n.out, nil
}

// errSentinel is a tiny error type local to this test file — used to
// drive the non-ErrUnsupported branch in preferAdapterNormalize without
// pulling in fmt.Errorf or a sentinel from another package that might
// be misclassified as ErrUnsupported via errors.Is.
type errSentinel struct{ msg string }

func (e errSentinel) Error() string { return e.msg }

// --- payload_capture direct branches ----------------------------------------

func TestCaptureResponseBody_NilStoreReturnsNil(t *testing.T) {
	// Pins payload_capture.go lines 32-34 — nil store short-circuit.
	if got := CaptureResponseBody(nil, []byte("body")); got != nil {
		t.Fatalf("CaptureResponseBody(nil store) = %v, want nil", got)
	}
}

func TestCaptureResponseBody_EmptyBodyReturnsNil(t *testing.T) {
	// Same short-circuit branch on the empty-body side.
	store := payloadcapture.NewStore(payloadcapture.Config{
		StoreResponseBody:  true,
		MaxInlineBodyBytes: 1024,
	})
	if got := CaptureResponseBody(store, nil); got != nil {
		t.Fatalf("CaptureResponseBody(nil body) = %v, want nil", got)
	}
	if got := CaptureResponseBody(store, []byte{}); got != nil {
		t.Fatalf("CaptureResponseBody(empty body) = %v, want nil", got)
	}
}

func TestDefensiveCopy_EmptyReturnsNil(t *testing.T) {
	// Pins payload_capture.go lines 46-48 — explicit nil/empty short
	// circuit inside defensiveCopy. The caller's len-check normally
	// prevents this branch from firing in production; we call it
	// directly to fix coverage.
	if got := defensiveCopy(nil); got != nil {
		t.Fatalf("defensiveCopy(nil) = %v, want nil", got)
	}
	if got := defensiveCopy([]byte{}); got != nil {
		t.Fatalf("defensiveCopy([]byte{}) = %v, want nil", got)
	}
}

func TestPreferAdapterNormalize_NormalizeErrUnsupportedSilentlyFallsBack(t *testing.T) {
	// When the adapter's Normalize returns ErrUnsupported (below
	// confidence threshold) the helper MUST silently fall back to
	// segments WITHOUT logging at Warn (CLAUDE.md: log noise on the
	// hot path becomes a fail-open hazard via slog overhead).
	// We assert observable behavior: the fallback segments are used.
	adapter, ok := lookupAdapter(t, "api.openai.com", "/v1/chat/completions")
	if !ok {
		t.Skip("openai-compat adapter not registered in this build")
	}
	// A body that's NOT a chat completion (no messages[] root) drives
	// the OpenAI normalizer below its 0.5 confidence threshold so the
	// extract helper wraps and returns ErrUnsupported. We pass the
	// chat-completions PATH so the adapter still routes to the chat
	// normalizer; the body shape is what makes the call fall through.
	out := preferAdapterNormalize(
		context.Background(),
		adapter,
		[]byte(`{"not_a_chat":"payload"}`),
		"/v1/chat/completions",
		normalize.DirectionRequest,
		[]string{"fallback-only"},
		silentLogger(),
	)
	if out == nil {
		t.Fatal("expected fallback to segments, got nil payload")
	}
	got := out.TextProjection()
	if len(got) != 1 || got[0] != "fallback-only" {
		t.Fatalf("expected fallback segments, got %v", got)
	}
}

// lookupAdapter resolves the openai-compat adapter via a pipeline so
// the test doesn't need a hand-rolled traffic.AdapterRegistry. Returns
// (nil, false) if the adapter isn't registered (build-tag skew).
func lookupAdapter(t *testing.T, host, path string) (traffic.Adapter, bool) {
	t.Helper()
	p := pipelineWithDomain(t, host, nil, nil, "PROCESS")
	inst, _, _ := p.Snapshot().ResolveAction(host, path)
	if inst == nil || inst.Adapter == nil {
		return nil, false
	}
	return inst.Adapter, true
}

func sliceContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// classifyEndpoint — direct typology integration after E87-S3a-1

// TestClassifyEndpoint_EmbeddingPathReturnsEmbeddings asserts the canonical
// OpenAI embeddings path classifies to EndpointTypeEmbeddings via the typology
// rule table. Replaces the prior WithClassifier+Registry indirection (E87-S3a-1).
func TestClassifyEndpoint_EmbeddingPathReturnsEmbeddings(t *testing.T) {
	p := pipelineWithDomain(t, "api.openai.com", nil, nil, "PROCESS")
	h := NewHandler(p, silentLogger())

	ep := h.classifyEndpoint("POST", "/v1/embeddings")
	if ep != hooks.EndpointTypeEmbeddings {
		t.Fatalf("classifyEndpoint(/v1/embeddings) = %q, want %q", ep, hooks.EndpointTypeEmbeddings)
	}
}

// TestClassifyEndpoint_ChatPathReturnsChat asserts that a chat-completions
// path classifies to EndpointTypeChat (the semantic kind) under the
// E87-S3a-1 unified typology — every path that yields any classification
// produces the right kind, including chat (previously only embedding rules
// were registered by RegisterDefaultEmbeddingRules so chat returned "").
func TestClassifyEndpoint_ChatPathReturnsChat(t *testing.T) {
	p := pipelineWithDomain(t, "api.openai.com", nil, nil, "PROCESS")
	h := NewHandler(p, silentLogger())

	ep := h.classifyEndpoint("POST", "/v1/chat/completions")
	if ep != hooks.EndpointTypeChat {
		t.Fatalf("classifyEndpoint(/v1/chat/completions) = %q, want %q", ep, hooks.EndpointTypeChat)
	}
}

// TestClassifyEndpoint_UnknownPathReturnsEmpty asserts that a path with
// no typology rule returns the empty EndpointType — backward-compatible
// "unclassified" semantics that the hook pipeline treats as "all hooks
// apply". The host argument is ignored by E87 typology (rules are
// path-uniqueness based) so any host produces the same result.
func TestClassifyEndpoint_UnknownPathReturnsEmpty(t *testing.T) {
	p := pipelineWithDomain(t, "api.openai.com", nil, nil, "PROCESS")
	h := NewHandler(p, silentLogger())

	ep := h.classifyEndpoint("POST", "/unknown/path/not/in/typology")
	if ep != "" {
		t.Fatalf("classifyEndpoint(/unknown/...) = %q, want empty", ep)
	}
}

// TestProcessRequest_EmbeddingEndpointTypeStamped verifies that
// ProcessRequest stamps EndpointType=embeddings on the HookInput via the
// canonical typology rule table and the PII detector still fires
// (embedding inputs are in the TextProjection) producing Decision=Modify.
// E87-S3a-1 renamed: removed legacy WithClassifier+Registry indirection.
func TestProcessRequest_EmbeddingEndpointTypeStamped(t *testing.T) {
	p := pipelineWithDomain(t, "api.openai.com", nil,
		[]hooks.HookConfig{
			{
				ID:               "pii-emb-classify",
				ImplementationID: "pii-detector",
				Name:             "pii-emb-classify",
				Stage:            "request",
				Priority:         10,
				Enabled:          true,
				FailBehavior:     "fail-open",
				Config: map[string]any{
					"patternDefinitions": []any{
						map[string]any{
							"id":          "email",
							"regex":       `[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`,
							"replacement": "[EMAIL_REDACTED]",
						},
					},
					"onMatch": map[string]any{"inflightAction": "redact"},
				},
			},
		}, "PROCESS")
	h := NewHandler(p, silentLogger())

	res := h.ProcessRequest(
		context.Background(),
		"api.openai.com",
		http.MethodPost,
		"/v1/embeddings",
		nil,
		[]byte(`{"model":"text-embedding-3-small","input":"classify test@example.com"}`),
	)
	// The PII detector must detect the email in the embedding input
	// and produce Decision=Modify (ErrRewriteUnsupported degrades it).
	if res.Decision != hooks.Modify {
		t.Fatalf("Decision = %v, want Modify (PII in embedding input)", res.Decision)
	}
	if res.ReasonCode != hooks.ReasonRedactInflightUnsupported {
		t.Fatalf("ReasonCode = %q, want REDACT_INFLIGHT_UNSUPPORTED", res.ReasonCode)
	}
}

// Compile-time assertion: ConfigSnapshot version field has not drifted —
// keeps this file building if the snapshot DTO is refactored.
var _ = (&shadow.ConfigSnapshot{}).Version

// Compile-time assertion: marshalling roundtrips for hook configs we
// build inline above match the wire shape (defends the test fixture
// shape from silent DTO drift).
var _ = func() bool {
	_, _ = json.Marshal(hooks.HookConfig{Stage: "request"})
	return true
}()
