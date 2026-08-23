// Package router implements the routing engine for the AI gateway.
// It evaluates a tree of routing strategies to select provider+model targets,
// applies stage-0 policy narrowing and VK access control, and produces
// a RoutingPlan with trace information.
package core

import (
	"context"
	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Strategy evaluation does not recurse: a multi-target strategy's children are
// provider+model leaves it resolves directly. The depth limit that used to
// bound recursion is gone with the recursion — the hazard is unrepresentable in
// the Strategy interface rather than caught by a counter.

// StrategyNode is a discriminated union representing one node in a strategy tree.
// The Type field determines which struct fields are populated.
type StrategyNode struct {
	Type string `json:"type"` // single | fallback | loadbalance | conditional | ab_split | latency | smart

	// single
	ProviderID string `json:"providerId,omitempty"`
	ModelID    string `json:"modelId,omitempty"`

	// fallback
	Targets       []StrategyNode `json:"targets,omitempty"`
	OnStatusCodes []int          `json:"onStatusCodes,omitempty"`

	// loadbalance
	Algorithm string           `json:"algorithm,omitempty"` // "weighted"
	Weighted  []WeightedTarget `json:"weightedTargets,omitempty"`

	// conditional
	Conditions []ConditionalBranch `json:"conditions,omitempty"`
	Default    *StrategyNode       `json:"default,omitempty"`

	// ab_split
	ABTargets []ABTarget `json:"abTargets,omitempty"`

	// latency
	LatencyTargets []LatencyTarget `json:"latencyTargets,omitempty"`

	// smart (stored flat in config JSON)
	RouterProviderID  string   `json:"routerProviderId,omitempty"`
	RouterModelID     string   `json:"routerModelId,omitempty"`
	SystemPrompt      string   `json:"systemPrompt,omitempty"`
	Temperature       *float64 `json:"temperature,omitempty"`
	MaxTokens         int      `json:"maxTokens,omitempty"`
	TimeoutMs         int      `json:"timeoutMs,omitempty"`
	DefaultProviderID string   `json:"defaultProviderId,omitempty"`
	DefaultModelID    string   `json:"defaultModelId,omitempty"`

	// policy (stage-0 only)
	AllowModelIDs    []string `json:"allowModelIds,omitempty"`
	DenyModelIDs     []string `json:"denyModelIds,omitempty"`
	AllowProviderIDs []string `json:"allowProviderIds,omitempty"`
	DenyProviderIDs  []string `json:"denyProviderIds,omitempty"`
}

// WeightedTarget is a strategy node with an associated weight for loadbalancing.
type WeightedTarget struct {
	Weight int          `json:"weight"`
	Node   StrategyNode `json:"node"`
}

// ConditionalBranch maps a match expression to a strategy node.
type ConditionalBranch struct {
	When map[string]any `json:"when"` // MongoDB-style expression
	Then StrategyNode   `json:"then"`
}

// ABTarget is a simple weighted target for A/B splits.
type ABTarget struct {
	ProviderID string `json:"providerId"`
	ModelID    string `json:"modelId"`
	Weight     int    `json:"weight"`
}

// RoutingContext carries request context for strategy evaluation.
//
// Request is the canonical normalized payload built once by the handler via
// normcore.Registry.Normalize and shared with every L4 consumer. Strategies
// that need request-content visibility (smart, content-aware conditional
// predicates) read Request.Messages directly; they do NOT parse raw bytes
// or smuggle data through Headers.
//
// Request is nil for endpoints without a normalisable body (/v1/models)
// or when normalize itself failed; consumers must nil-check.
//
// Headers is the routing-visible projection of the inbound HTTP header
// set (SafeHeaders). Only Get(name) is exposed — there is no exported
// writer, so internal routing data cannot syntactically land here.
// Auth-bearing headers (Authorization, Cookie, X-API-Key) are filtered
// at construction.
type RoutingContext struct {
	RequestedModel RequestedModel
	// EndpointType is the canonical EndpointKind (typology.EndpointKindChat,
	// EndpointKindEmbeddings, EndpointKindModels, …). Empty when the request
	// path could not be classified (treated as "unknown" by the resolver —
	// no capability filtering, no kind-specific routing).
	// Construction sites must convert any path-segment string via
	// typology.KindFromPathSegment(<segment>) at the boundary.
	EndpointType typology.EndpointKind
	VirtualKey   *VKContext
	Headers      SafeHeaders
	Request      *normcore.NormalizedPayload
	// EmbeddingRequest carries the structured embedding request parameters
	// extracted from the canonical body by the proxy handler. Populated only
	// when EndpointType == typology.EndpointKindEmbeddings. The capability
	// pre-filter in the Resolver reads this to decide which routing
	// candidates are compatible with the request.
	EmbeddingRequest *EmbeddingRequestParams

	// RequiresStructuredOutput reports that the caller asked for an answer held
	// to a JSON Schema (`response_format: {type: "json_schema"}`). Extracted
	// from the canonical body by the proxy handler, the same way
	// EmbeddingRequest is, because the normalized payload carries sampling
	// parameters and not this one.
	//
	// It is an eligibility constraint for the models WE choose, and only those.
	// A caller who names a model owns that model's limits — gpt-4-turbo and the
	// DeepSeek family refuse a json_schema with a loud 400, which terminates
	// (4xx is classNoFailoverNoRetry). When the router picks, a mismatch it
	// caused is ours, and one candidate does not fail loudly: kimi-k2.5 accepts
	// the field and answers in prose with HTTP 200 (probed 2026-08-19). Nothing
	// downstream can turn that into an error, so the choice is the only place
	// it can be prevented.
	RequiresStructuredOutput bool

	// ModelPool is the catalogue of models this request may be served by,
	// resolved once when the routing context is prepared.
	//
	// Every strategy that needs a pool reads this one. `smart` used to fetch
	// its own and narrow it by the virtual key itself, so the same question was
	// asked twice per request against the same snapshot — and two answers to
	// one question is how the defects this program keeps finding begin.
	//
	// Nil means it was not prepared, which is not the same as empty: a strategy
	// that names its own targets (single, loadbalance) never needs a pool, so
	// most requests carry none. A strategy that DOES need one treats nil as
	// "unavailable" and says so, rather than reading it as "no models exist".
	ModelPool []SmartModelRow
}

// EmbeddingRequestParams is the routing-layer view of an embeddings request.
// It is populated once at Phase 4 by the proxy handler and threaded through
// RoutingContext so the capability pre-filter can apply compatibility rules
// without re-parsing the canonical body.
type EmbeddingRequestParams struct {
	Dimensions     *int   // nil = client omitted dimensions parameter
	BatchSize      int    // 1 for single-string input; len(array) for batch
	EncodingFormat string // "" / "float" / "base64"
	InputType      string // nexus.ext.cohere.input_type (Cohere v3)
	TaskType       string // nexus.ext.gemini.taskType (Gemini)
}

// VKContext holds virtual key metadata for routing decisions.
type VKContext struct {
	ID               string
	Name             string
	OrganizationID   string
	OrganizationPath []string // self + ancestors (for hierarchical org matches)
	ProjectID        string
	SourceApp        string
	AllowedModels    []store.AllowedModelRef
}

// RoutingPlan is the output of route resolution.
type RoutingPlan struct {
	Targets []RoutingTarget
	// RecoveryTargets is everything the plan can fall back to: the primary
	// rule's inline chain, and every matching rule BELOW the primary in
	// priority order. Which rule each came from travels on the target
	// (RoutingTarget.RuleID); the walk uses it to refuse to advance past a rule
	// that still has a target it has not ruled out.
	//
	// They live in one field, not two, because every filter that protects a
	// recovery target has to protect all of them — an entry that reached the
	// plan through a different field would skip the capability check and
	// dispatch something the caller's `auto` asked us to exclude.
	RecoveryTargets []RoutingTarget
	Trace           []TraceEntry
	PipelineTrace   []PipelineTraceEntry
	Substituted     bool
	OriginalModelID string
	RuleID          string
	RuleName        string

	// RuleRetryPolicyJSON is the matched primary rule's RoutingRule.retryPolicy
	// JSONB column verbatim (may be empty/null). The handler unmarshals it
	// into a *configtypes.RetryPolicy and field-merges it on top of the
	// YAML default before invoking the executor.
	RuleRetryPolicyJSON json.RawMessage
	// RuleMatchedAndResolvedNothing: a stage-1 rule fitted this request and
	// then produced no target, measured at RESOLUTION time — before any filter.
	//
	// It separates two situations the target list cannot: no rule applies, where
	// serving the model the caller named is right, from a rule applied,
	// redirected the request, and resolved nothing, where serving that model
	// delivers exactly what the rule existed to prevent.
	//
	// Resolution time is the whole point. A filter emptying the plan afterwards
	// is OUR fact — a modality the targets cannot carry, an embedding parameter
	// none accept — and not a rule refusing anything, so passthrough is still
	// the right answer there.
	RuleMatchedAndResolvedNothing bool
	// Branches is populated only by Resolver.Explain (not Resolve).
	// It enumerates every terminal target reachable from the matched
	// primary rule, with cumulative selection probability, so operators
	// can see the full distribution of a stochastic strategy
	// (loadbalance, ab_split, conditional) rather than the single
	// branch that a particular stochastic roll happened to pick.
	Branches []BranchedTarget
}

// BranchedTarget represents a single terminal target reachable from a
// strategy subtree, with the cumulative probability the live router would
// select it and a human-readable path through the strategy tree.
//
// Probability is the product of per-node selection probabilities along the
// path. For deterministic strategies (single, fallback, conditional) it is
// 1.0. For weighted strategies (loadbalance, ab_split) it is weight / sum.
// Matched indicates whether, under the given RoutingContext, that branch is
// actually reachable (false only for conditional branches whose predicate
// does not match).
type BranchedTarget struct {
	Target      RoutingTarget
	Probability float64
	Path        string
	Matched     bool
	Note        string // e.g. "provider disabled", "lookup failed"
}

// TraceEntry records a strategy evaluation step.
type TraceEntry struct {
	RuleID       string `json:"ruleId,omitempty"`
	RuleName     string `json:"ruleName,omitempty"`
	StrategyType string `json:"strategyType"`
	Decision     string `json:"decision"`
	DurationMs   int    `json:"durationMs"`
	// RouterCostUsd / RouterProviderID are set ONLY by the smart strategy, on
	// the entry recording a completed router-LLM call. Every other strategy
	// leaves them zero (omitempty keeps them off the wire). The trace is the
	// existing per-request channel from a strategy up to the proxy stage, which
	// drains these onto the audit record; they also surface in
	// traffic_event.routing_trace for per-request verification.
	RouterCostUsd    float64 `json:"routerCostUsd,omitempty"`
	RouterProviderID string  `json:"routerProviderId,omitempty"`
	// The router call's own token usage and the model that produced it,
	// stamped on the same entry as RouterCostUsd. They exist so the recorded
	// cost has a recoverable basis: router_cost_usd alone is an amount with no
	// way to check it, which is why the period when cached router tokens were
	// billed at the full input rate could only be detected against the vendor
	// bill and can never be recomputed from our own rows. RouterPromptTokens is
	// the TOTAL input; the two cache counts are its cached share, not additions.
	RouterModelID             string `json:"routerModelId,omitempty"`
	RouterPromptTokens        int    `json:"routerPromptTokens,omitempty"`
	RouterCompletionTokens    int    `json:"routerCompletionTokens,omitempty"`
	RouterCacheReadTokens     int    `json:"routerCacheReadTokens,omitempty"`
	RouterCacheCreationTokens int    `json:"routerCacheCreationTokens,omitempty"`
}

// PipelineTraceEntry records a stage-level decision.
type PipelineTraceEntry struct {
	Stage      int    `json:"stage"`
	Decision   string `json:"decision"`
	DurationMs int    `json:"durationMs"`
}

// MatchConditions defines which requests a routing rule matches. Every
// non-empty dimension is AND'd; an empty MatchConditions matches every
// request. Models holds Model.id UUIDs (matched against the request's
// hydrated CandidateIDs) and RequestedModelLiterals carries non-Model.code
// request keywords (e.g. "auto") matched against the raw request string.
type MatchConditions struct {
	Models                 []string `json:"models,omitempty"`
	RequestedModelLiterals []string `json:"requestedModelLiterals,omitempty"`
	ModelTypes             []string `json:"modelTypes,omitempty"`
	Providers              []string `json:"providers,omitempty"`
	VirtualKeys            []string `json:"virtualKeys,omitempty"` // supports glob (*)
	Projects               []string `json:"projects,omitempty"`
}

// FallbackChainEntry is an inline recovery target on a routing rule.
type FallbackChainEntry struct {
	ProviderID string `json:"providerId"`
	ModelID    string `json:"modelId"`
}

// TargetLookup resolves a (providerID, modelID) pair into a RoutingTarget.
// Injected by the resolver to allow strategy implementations to look up DB records.
// The latency strategy's LatencyTarget / LatencyStatsFunc / MaxLatencyTargets
// live in latency.go.
type TargetLookup func(ctx context.Context, providerID, modelID string) (*RoutingTarget, error)

// CandidateCapability describes what a routing candidate would have accepted
// for an embedding request. Used to populate available_capabilities in 400
// errors when all routing candidates are rejected by the capability pre-filter.
type CandidateCapability struct {
	Provider                 string   `json:"provider"`
	Model                    string   `json:"model"`
	SupportedDimensions      []int    `json:"supported_dimensions,omitempty"`
	MinDimension             int      `json:"min_dimension,omitempty"`
	MaxDimension             int      `json:"max_dimension,omitempty"`
	MaxBatchSize             int      `json:"max_batch_size,omitempty"`
	SupportedEncodingFormats []string `json:"supported_encoding_formats,omitempty"`
	RequiredExtensions       []string `json:"required_extensions,omitempty"`
}

// NoCompatibleProviderError is returned by Resolver.Resolve / ResolveTargets
// when the capability pre-filter rejected every routing candidate for the
// current embedding request. Available lists what each candidate supported
// so the caller can surface the detail in a 400 error body.
type NoCompatibleProviderError struct {
	Available []CandidateCapability
}

func (e *NoCompatibleProviderError) Error() string {
	return "no_compatible_provider"
}

// ModelNotAllowedError is returned when the caller NAMED a model their virtual
// key is not permitted to use.
//
// The gateway previously served such a request whenever a routing rule
// redirected it: only the target the rule chose was checked, on the reasoning
// that the rule decides what runs and the caller's string does not. That is
// true of what RUNS and false of what the caller was told. A key restricted to
// one model, with a rule redirecting everything to that model, answered a
// client hard-coded to `gpt-4o` with a 200 — so the client's own configuration
// was silently overridden and every response was attributed to a model the key
// could not use.
//
// It applies ONLY when the caller named a single catalog model. `auto` is not a
// catalog model and can never appear on an allow list; a code that fans out to
// several providers has no single reference to match. Requiring either to be
// allowed would refuse every routed request from every restricted key, which
// deletes routing rather than tightening it — the objection recorded on
// VKAccessFilter, and the reason this is conditional rather than blanket.
type ModelNotAllowedError struct {
	// RequestedModel is the string the caller sent, for the error body.
	RequestedModel string
}

func (e *ModelNotAllowedError) Error() string {
	return "model " + e.RequestedModel + " is not allowed for this virtual key"
}
