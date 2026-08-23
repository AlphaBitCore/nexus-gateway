// packages/ai-gateway/internal/policy/aiguard/types.go
package aiguard

import "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/inputstaging"

// Request is the POST body of /v1/ai-guard/classify. Schema locked to
// spec §4.2 — JSON tag names are stable customer-facing contract.
//
// Content is a flat text projection of the material the judge should
// inspect. Callers that hold a NormalizedPayload should pass
// NormalizedPayload.JoinedText("\n") so the judge sees the full
// conversation in turn order. The offsets in Response.Redactions are
// indices into this Content string.
//
// Messages is an optional structured alternative to Content. When
// non-empty, classifyImpl applies inputstaging.Plan (using the strategy
// from RuntimeConfig) to select the subset of messages that fits within
// the judge model's context window, then joins them into a flat string
// for Content. Callers that hold individual normcore.Messages should
// prefer this path over pre-joining; doing so lets the classify pipeline
// apply the admin-configured truncation strategy rather than the caller's
// ad-hoc truncation.
//
// PayloadKind is an optional hint (e.g. "ai-chat") so the judge can
// adjust its expectations; ignored when empty.
type Request struct {
	DetectorType string `json:"detector_type"`
	Content      string `json:"content"`
	// Messages is an optional structured message list. When non-empty it
	// takes precedence over Content for inputstaging. After Plan() the
	// staged messages are joined with "\n" and used as Content.
	Messages    []inputstaging.Message `json:"messages,omitempty"`
	PayloadKind string                 `json:"payload_kind,omitempty"`
	Context     Context                `json:"context"`
}

// Context carries the caller-supplied metadata used by the judge prompt
// and audit pipeline. All fields optional.
type Context struct {
	Ingress        string   `json:"ingress,omitempty"`
	TargetProvider string   `json:"target_provider,omitempty"`
	TargetModel    string   `json:"target_model,omitempty"`
	UpstreamTags   []string `json:"upstream_tags,omitempty"`
	HookName       string   `json:"hook_name,omitempty"`
}

// Redaction is one structured replacement suggestion from the judge.
// Offsets are byte indices into Request.Content (UTF-8). The caller
// applies them to its NormalizedPayload via the shared/normalize
// TransformSpan framework — the caller knows the mapping from flat
// content offsets back to messages.<i>.content.<j> addresses.
//
// Action is one of "redact" / "strip" / "replace" matching the
// normalize.TransformAction enum. For pure redaction, the judge
// supplies a Replacement string (typically "[REDACTED_<LABEL>]");
// strip uses Replacement = "".
type Redaction struct {
	Start       int    `json:"start"`
	End         int    `json:"end"`
	Replacement string `json:"replacement,omitempty"`
	Action      string `json:"action,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// Response is the structured output returned to the caller. See spec §4.5.
// Decision values: "approve" | "reject_hard" | "block_soft" | "modify".
//
// Redactions carries one entry per sensitive span instead of returning the
// whole sanitised body, so the caller can apply them uniformly alongside
// hook-emitted TransformSpan values.
type Response struct {
	Decision   string      `json:"decision"`
	Confidence float64     `json:"confidence,omitempty"`
	Reason     string      `json:"reason,omitempty"`
	Labels     []string    `json:"labels,omitempty"`
	Redactions []Redaction `json:"redactions,omitempty"`
	Metadata   Metadata    `json:"metadata"`
}

// Metadata carries non-decision-bearing diagnostic fields.
type Metadata struct {
	JudgeModel     string `json:"judge_model,omitempty"`
	JudgeLatencyMs int    `json:"judge_latency_ms"`
	CacheHit       bool   `json:"cache_hit"`
	BackendMode    string `json:"backend_mode,omitempty"`

	// Populated by AdapterBackend when the classifier model has pricing
	// wired; zero on CacheHit, backend failures, or when no pricing is
	// configured. The sink reads these to stamp traffic_event.{prompt_tokens,
	// completion_tokens, ai_guard_cost_usd} so the cost of the classifier
	// call lands on the same row as the classify event.
	PromptTokens     int     `json:"prompt_tokens,omitempty"`
	CompletionTokens int     `json:"completion_tokens,omitempty"`
	CostUsd          float64 `json:"cost_usd,omitempty"`

	// CacheReadTokens / CacheCreationTokens are the provider-cached share of
	// PromptTokens, which is the TOTAL input on every adapter (see
	// costing.Tokens). The judge prompt is a fixed template, so on a warm
	// provider cache most of the input lands in these buckets and bills at a
	// fraction of the input rate; CostUsd above is computed with that split.
	// The sink writes them to traffic_event.{cache_read_tokens,
	// cache_creation_tokens} on the classifier's own row.
	CacheReadTokens     int `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`

	// ProviderID / ProviderName identify the provider that actually served
	// this classify call, stamped by AdapterBackend from the resolved
	// provtarget.Resolver call target (never inferred from the model id or
	// a caller-supplied string). Empty when the backend has no provider
	// concept (e.g. ExternalBackend, which hits an operator-owned URL, not
	// a Nexus-configured provider). classifyImpl copies these onto
	// TrafficEvent so the classifier's cost can be attributed to a
	// provider dimension in the traffic_event rollup.
	ProviderID   string `json:"provider_id,omitempty"`
	ProviderName string `json:"provider_name,omitempty"`
}

// ErrorBody is the JSON shape of 4xx/5xx responses.
type ErrorBody struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}
