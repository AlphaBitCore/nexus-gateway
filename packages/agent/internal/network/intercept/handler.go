// Package intercept implements the agent's local traffic interception handler
// that runs the shared traffic filter → adapter → compliance pipeline
// per V2 §2.6 layered architecture.
package intercept

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	agentcompliance "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/compliance"
	hookscore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normalizecore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// Result is the outcome of processing an intercepted request.
type Result struct {
	Action   traffic.FilterResult
	Decision hookscore.Decision
	Reason   string
	// ComplianceTags is the merged compliance tag set emitted by the hook
	// pipeline (severity:*, detector:*, category:*, …). Persisted into
	// traffic_event.compliance_tags on the Hub; nil when the pipeline
	// produced no tags (e.g. no hook ran or all hooks approved cleanly).
	ComplianceTags []string

	// HookOutcome carries the aggregated hook pipeline outcome in the form
	// expected by traffic.FormatHookOutcome. Populated only when the
	// compliance pipeline ran (i.e. Action == traffic.Process and at least
	// one hook was configured); empty HookOutcomeInput produces "none" via
	// FormatHookOutcome.
	HookOutcome traffic.HookOutcomeInput

	// Request-side LLM signals extracted by the adapter's
	// DetectRequestMeta. Empty strings when the request did not match a
	// known provider or no adapter was selected.
	Provider          string
	Model             string
	ApiKeyClass       string
	ApiKeyFingerprint string

	// Response-side usage populated on ProcessResponse when the caller
	// supplies a UsageMeta (from the adapter's DetectResponseUsage on a
	// buffered body, or from a streaming accumulator's Finalize). Pointers
	// are nil when usage was unavailable; UsageExtractionStatus is empty
	// when no usage was computed for the flow (pure passthrough, no body).
	PromptTokens          *int
	CompletionTokens      *int
	UsageExtractionStatus string

	// Rewritten request body produced when a hook returned MODIFY and the
	// adapter supported inflight rewrite. Nil when the request body was
	// forwarded as-is. ReasonCode carries REDACT_INFLIGHT_UNSUPPORTED when
	// the adapter could not reverse-encode (caller should forward the
	// original body but record the degraded path in the audit event).
	RewrittenBody []byte
	ReasonCode    string
}

// Handler processes intercepted traffic through the V2 layered pipeline.
type Handler struct {
	pipeline       *agentcompliance.AgentPipeline
	perHookTimeout time.Duration
	totalTimeout   time.Duration
	logger         *slog.Logger
}

// NewHandler creates an intercept handler.
func NewHandler(pipeline *agentcompliance.AgentPipeline, logger *slog.Logger) *Handler {
	return &Handler{
		pipeline:       pipeline,
		perHookTimeout: 5 * time.Second,
		totalTimeout:   15 * time.Second,
		logger:         logger,
	}
}

// classifyEndpoint returns the EndpointType for the given (method, path)
// pair by consulting the canonical typology rule table. Returns the empty
// EndpointType when no rule matches — caller treats this as "unclassified"
// and runs all hooks.
func (h *Handler) classifyEndpoint(method, path string) hookscore.EndpointType {
	kind, _, _ := typology.ClassifyPath(method, path)
	return kind
}

// ProcessRequest runs the full interception pipeline for a request:
// 1. Filter: domain+path matching via DomainSnapshot
// 2. Extract: adapter extracts NormalizedContent (if Process)
// 3. Detect: adapter's DetectRequestMeta pulls provider/model/api-key signals
// 4. Hooks: compliance pipeline evaluates hooks
//
// Returns the result including the filter action, hook decision, and the
// detected LLM signals (populated even when no hooks are configured, so the
// audit event captures them for non-hook AI traffic).
func (h *Handler) ProcessRequest(ctx context.Context, host, method, path string, headers http.Header, body []byte) Result {
	snap := h.pipeline.Snapshot()

	// Step 1: Filter — does this request need hook processing?
	inst, filterResult, _ := snap.ResolveAction(host, path)

	if filterResult != traffic.Process {
		return Result{
			Action:   filterResult,
			Decision: hookscore.Approve,
		}
	}

	if inst == nil || inst.Adapter == nil {
		// Domain matched but no adapter — should not happen if snapshot is consistent.
		traffic.RecordUnmatched(host, "no_adapter")
		return Result{
			Action:   traffic.Passthrough,
			Decision: hookscore.Approve,
		}
	}

	// Step 2: Extract — parse body into NormalizedContent.
	nc, err := inst.Adapter.ExtractRequest(ctx, body, path)
	if err != nil {
		h.logger.Warn("adapter extraction failed",
			slog.String("host", host),
			slog.String("path", path),
			slog.String("error", err.Error()),
		)
		traffic.RecordUnmatched(host, "parse_error")
		// Apply domain's onAdapterError policy.
		return Result{
			Action:   traffic.Passthrough,
			Decision: hookscore.Approve,
		}
	}

	// Step 3: Detect — run the adapter's LLM signal detector against a
	// synthetic request carrying the intercepted headers. The adapter
	// never returns an error; empty fields mean "unknown".
	reqMeta := inst.Adapter.DetectRequestMeta(buildSyntheticRequest(host, method, path, headers), body)

	// Step 4: Hooks — run compliance pipeline. Skip the pipeline build when
	// no request-stage hooks are configured, but still surface detected meta.
	resolver := h.pipeline.Resolver()
	if !resolver.HasHooks("request") {
		return Result{
			Action:            traffic.Process,
			Decision:          hookscore.Approve,
			Provider:          reqMeta.Provider,
			Model:             reqMeta.Model,
			ApiKeyClass:       reqMeta.ApiKeyClass,
			ApiKeyFingerprint: reqMeta.ApiKeyFingerprint,
		}
	}

	// Classify endpoint type from HTTP metadata.
	endpointType := h.classifyEndpoint(method, path)

	input := &hookscore.HookInput{
		Stage:             "request",
		Normalized:        preferAdapterNormalize(ctx, inst.Adapter, body, path, normalizecore.DirectionRequest, nc.Segments, h.logger),
		TargetHost:        host,
		Path:              path,
		IngressType:       "AGENT",
		BodySize:          int64(len(body)),
		DetectedProvider:  reqMeta.Provider,
		DetectedModel:     reqMeta.Model,
		ApiKeyClass:       reqMeta.ApiKeyClass,
		ApiKeyFingerprint: reqMeta.ApiKeyFingerprint,
		EndpointType:      endpointType,
	}

	pipeline, err := resolver.BuildPipeline("request", "AGENT", endpointType, nil, h.perHookTimeout, h.totalTimeout, false, h.logger)
	if err != nil {
		h.logger.Warn("failed to build pipeline", "error", err)
		return Result{
			Action:            traffic.Process,
			Decision:          hookscore.Approve,
			Provider:          reqMeta.Provider,
			Model:             reqMeta.Model,
			ApiKeyClass:       reqMeta.ApiKeyClass,
			ApiKeyFingerprint: reqMeta.ApiKeyFingerprint,
		}
	}

	pipeline.SetClearSoftOnApprove(true)
	result := pipeline.Execute(ctx, input)

	out := Result{
		Action:            traffic.Process,
		Decision:          result.Decision,
		Reason:            result.Reason,
		ComplianceTags:    result.Tags,
		HookOutcome:       hookOutcomeFromResult(result.HookResults),
		Provider:          reqMeta.Provider,
		Model:             reqMeta.Model,
		ApiKeyClass:       reqMeta.ApiKeyClass,
		ApiKeyFingerprint: reqMeta.ApiKeyFingerprint,
	}

	// MODIFY → invoke adapter.RewriteRequestBody. On ErrRewriteUnsupported,
	// downgrade to storage-only redact and record REDACT_INFLIGHT_UNSUPPORTED
	// so the audit event reflects the degraded path. Caller forwards the
	// original body in either case when out.RewrittenBody is nil.
	if result.Decision == hookscore.Modify && len(result.ModifiedContent) > 0 && inst.Adapter != nil {
		rewriteContent := traffic.NormalizedContent{Segments: contentBlocksToSegments(result.ModifiedContent)}
		rewritten, _, rErr := inst.Adapter.RewriteRequestBody(ctx, body, path, rewriteContent)
		switch {
		case errors.Is(rErr, traffic.ErrRewriteUnsupported):
			h.logger.Warn("agent inflight rewrite unsupported; forwarding original body",
				slog.String("host", host),
				slog.String("adapter", inst.Adapter.ID()),
			)
			out.ReasonCode = hookscore.ReasonRedactInflightUnsupported
		case rErr != nil:
			h.logger.Error("agent inflight rewrite failed",
				slog.String("host", host),
				slog.String("error", rErr.Error()),
			)
			out.ReasonCode = hookscore.ReasonRedactInflightUnsupported
		default:
			out.RewrittenBody = rewritten
		}
	}

	return out
}

// contentBlocksToSegments flattens hook ModifiedContent into the
// segments slice TrafficAdapter.RewriteRequestBody expects. Only
// text-type blocks contribute, matching ai-gateway's helper.
func contentBlocksToSegments(blocks []hookscore.ContentBlock) []string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type != "" && b.Type != "text" {
			continue
		}
		out = append(out, b.Text)
	}
	return out
}

// buildSyntheticRequest produces a minimal *http.Request carrying the
// intercepted host/method/path/headers — enough for traffic adapters whose
// DetectRequestMeta only reads r.Header, r.URL, r.Host. Constructing one
// here lets the agent's transport layer (NE IPC, iptables, CONNECT proxy)
// stay header-agnostic at the boundary and only reassemble when we detect.
func buildSyntheticRequest(host, method, path string, headers http.Header) *http.Request {
	m := method
	if m == "" {
		m = http.MethodPost
	}
	// path may already include a query string (Linux/Windows pass
	// req.URL.RequestURI()). Fall back to a bare URL if parsing fails.
	u, err := url.Parse(path)
	if err != nil || u == nil {
		u = &url.URL{Path: path}
	}
	u.Scheme = "https"
	u.Host = host
	return &http.Request{
		Method: m,
		URL:    u,
		Host:   host,
		Header: headers,
	}
}

// NewUsageAccumulator returns a streaming UsageAccumulator bound to the given
// provider and model. Callers in the proxy layer obtain provider/model from
// the request-side Result populated by ProcessRequest. Returns nil when the
// provider has no registered extractor — the proxy should fall back to raw
// passthrough in that case.
func (h *Handler) NewUsageAccumulator(provider, model string) streaming.UsageAccumulator {
	return streaming.NewUsageAccumulator(provider, model)
}

// ExtractResponseUsage resolves the adapter for host+path and invokes
// DetectResponseUsage against a buffered (non-streaming) response body.
// Returns an empty UsageMeta with Status=UsageStatusNonLLM when no adapter
// matches, preserving the schema's non_llm convention.
func (h *Handler) ExtractResponseUsage(_ context.Context, host, path string, resp *http.Response, body []byte) traffic.UsageMeta {
	snap := h.pipeline.Snapshot()
	inst, filterResult, _ := snap.ResolveAction(host, path)
	if filterResult != traffic.Process || inst == nil || inst.Adapter == nil {
		return traffic.UsageMeta{Status: traffic.UsageStatusNonLLM}
	}
	return inst.Adapter.DetectResponseUsage(resp, body)
}

// ProcessResponse runs the full interception pipeline for a response:
// 1. Filter: domain+path matching via DomainSnapshot
// 2. Extract: adapter extracts NormalizedContent from the response body
// 3. Hooks: compliance pipeline evaluates response-stage hooks
//
// The optional usage argument carries a pre-computed UsageMeta supplied by the
// proxy layer — buffered JSON responses pass the result of
// ExtractResponseUsage; SSE responses pass the result of the accumulator's
// Finalize. nil means no usage was computed for this flow.
//
// Returns the result including the filter action, hook decision, and the
// usage fields (copied from the supplied UsageMeta when non-nil).
func (h *Handler) ProcessResponse(ctx context.Context, host, path string, body []byte, usage *traffic.UsageMeta) Result {
	snap := h.pipeline.Snapshot()

	// Step 1: Filter — does this response's domain need hook processing?
	inst, filterResult, _ := snap.ResolveAction(host, path)

	if filterResult != traffic.Process {
		return withUsage(Result{
			Action:   filterResult,
			Decision: hookscore.Approve,
		}, usage)
	}

	if inst == nil || inst.Adapter == nil {
		traffic.RecordUnmatched(host, "no_adapter")
		return withUsage(Result{
			Action:   traffic.Passthrough,
			Decision: hookscore.Approve,
		}, usage)
	}

	// Step 2: Extract — parse response body into NormalizedContent.
	nc, err := inst.Adapter.ExtractResponse(ctx, body, path)
	if err != nil {
		h.logger.Warn("adapter response extraction failed",
			slog.String("host", host),
			slog.String("path", path),
			slog.String("error", err.Error()),
		)
		traffic.RecordUnmatched(host, "response_parse_error")
		return withUsage(Result{
			Action:   traffic.Passthrough,
			Decision: hookscore.Approve,
		}, usage)
	}

	// Step 3: Hooks — run response-stage compliance pipeline.
	resolver := h.pipeline.Resolver()
	if !resolver.HasHooks("response") {
		return withUsage(Result{
			Action:   traffic.Process,
			Decision: hookscore.Approve,
		}, usage)
	}

	// Classify endpoint type from HTTP metadata.
	respEndpointType := h.classifyEndpoint("" /* method not available at response stage */, path)

	input := &hookscore.HookInput{
		Stage:        "response",
		Normalized:   preferAdapterNormalize(ctx, inst.Adapter, body, path, normalizecore.DirectionResponse, nc.Segments, h.logger),
		TargetHost:   host,
		Path:         path,
		IngressType:  "AGENT",
		BodySize:     int64(len(body)),
		EndpointType: respEndpointType,
	}

	pipeline, err := resolver.BuildPipeline("response", "AGENT", respEndpointType, nil, h.perHookTimeout, h.totalTimeout, false, h.logger)
	if err != nil {
		h.logger.Warn("failed to build response pipeline", "error", err)
		return withUsage(Result{
			Action:   traffic.Process,
			Decision: hookscore.Approve,
		}, usage)
	}

	pipeline.SetClearSoftOnApprove(true)
	result := pipeline.Execute(ctx, input)
	return withUsage(Result{
		Action:         traffic.Process,
		Decision:       result.Decision,
		Reason:         result.Reason,
		ComplianceTags: result.Tags,
		HookOutcome:    hookOutcomeFromResult(result.HookResults),
	}, usage)
}

// withUsage copies a UsageMeta into a Result when present.
func withUsage(r Result, u *traffic.UsageMeta) Result {
	if u == nil {
		return r
	}
	r.PromptTokens = u.PromptTokens
	r.CompletionTokens = u.CompletionTokens
	r.UsageExtractionStatus = string(u.Status)
	return r
}

// hookOutcomeFromResult converts the ordered HookResult slice from a
// CompliancePipelineResult into a traffic.HookOutcomeInput suitable for
// traffic.FormatHookOutcome. Follows the same spec §4.5 mapping used by
// ai-gateway's aigwHookOutcomeFromResult:
//   - RejectHard / BlockSoft → Rejected = hookName, RejectReason = reasonCode (or reason)
//   - Modify → appended to Passed + Transformed = true
//   - Approve / Abstain → appended to Passed
//   - Any reject halts iteration (later hooks are not reported).
//
// Returns an empty HookOutcomeInput (→ "none") when hookResults is empty.
func hookOutcomeFromResult(hookResults []hookscore.HookResult) traffic.HookOutcomeInput {
	if len(hookResults) == 0 {
		return traffic.HookOutcomeInput{}
	}
	in := traffic.HookOutcomeInput{}
	for _, hr := range hookResults {
		switch hr.Decision {
		case hookscore.RejectHard, hookscore.BlockSoft:
			reason := hr.ReasonCode
			if reason == "" {
				reason = hr.Reason
			}
			return traffic.HookOutcomeInput{
				Rejected:     hr.HookName,
				RejectReason: reason,
			}
		case hookscore.Modify:
			in.Passed = append(in.Passed, hr.HookName)
			in.Transformed = true
		default:
			in.Passed = append(in.Passed, hr.HookName)
		}
	}
	return in
}
