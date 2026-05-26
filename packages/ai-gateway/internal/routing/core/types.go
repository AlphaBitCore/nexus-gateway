// Package router implements the routing engine for the AI gateway.
// It evaluates a tree of routing strategies to select provider+model targets,
// applies stage-0 policy narrowing and VK access control, and produces
// a RoutingPlan with trace information.
package core

import (
	"context"
	"encoding/json"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// MaxRoutingDepth is the recursion limit for strategy evaluation.
const MaxRoutingDepth = 10

// StrategyNode is a discriminated union representing one node in a strategy tree.
// The Type field determines which struct fields are populated.
type StrategyNode struct {
	Type string `json:"type"` // single | fallback | loadbalance | conditional | ab_split | policy | smart

	// single
	ProviderID string `json:"providerId,omitempty"`
	ModelID    string `json:"modelId,omitempty"`

	// fallback
	Targets       []StrategyNode `json:"targets,omitempty"`
	OnStatusCodes []int          `json:"onStatusCodes,omitempty"`

	// loadbalance
	Algorithm string           `json:"algorithm,omitempty"` // "weighted"
	Weighted  []WeightedTarget `json:"weightedTargets,omitempty"`
	StickyOn  string           `json:"stickyOn,omitempty"`
	StickyTTL int              `json:"stickyTtlMs,omitempty"`

	// conditional
	Conditions []ConditionalBranch `json:"conditions,omitempty"`
	Default    *StrategyNode       `json:"default,omitempty"`

	// ab_split
	ABTargets []ABTarget `json:"abTargets,omitempty"`

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
	// E87-S3a-2 (2026-05-25): retyped from legacy audit-string to
	// typology.EndpointKind. Construction sites must call
	// typology.KindFromPathSegment(<legacy>) at boundary.
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

// RequestedModel identifies the model requested by the client.
//
// ID holds the raw `model` string sent in the request body (a Model.code
// like "gpt-4o" or a request-side sentinel like "auto"); it is not a
// UUID despite the field name.
//
// CandidateIDs is populated by Resolver.hydrateRequestedModel and lists
// every Model.id (UUID) that ID resolves to via Model.code exact match
// or Model.aliases membership. matchConditions.models is matched by
// intersecting against this slice.
type RequestedModel struct {
	ID              string
	Type            string // chat | embedding | image | audio
	ProviderID      string
	ProviderModelID string
	CandidateIDs    []string
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

// RoutingTarget is a resolved provider+model ready for upstream dispatch.
//
// Region mirrors Provider.region and is the authoritative deployment
// region consumed by the data-residency compliance hook. An empty string
// means the operator has not classified this provider yet; downstream
// hooks must treat it as "unknown region" rather than "any region".
type RoutingTarget struct {
	ProviderID   string
	ProviderName string
	// AdapterType is the provider's canonical wire adapter, copied
	// verbatim from Provider.adapter_type. Downstream consumers (the
	// target executor, smart router, cross-format filter, and
	// /internal/routing-simulate) read it instead of deriving the
	// wire format from ProviderName.
	AdapterType string
	// ModelID is the Model row's UUID PK — used for FK references
	// (allowedModels matching, traffic_event.model_id, audit Record).
	ModelID string
	// ModelCode is the customer-facing identifier ("gpt-4o"). Returned
	// to clients in the `x-nexus-aigw-model` response header so they can
	// correlate logs without exposing the internal UUID.
	ModelCode       string
	ModelName       string
	ProviderModelID string
	BaseURL         string
	Region          string
	Source          string // "primary", "fallback", "recovery"
}

// RoutingPlan is the output of route resolution.
type RoutingPlan struct {
	Targets         []RoutingTarget
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
	NarrowingSummary    *NarrowingSummary
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

// NarrowingSummary is the serializable summary of stage-0 narrowing state.
type NarrowingSummary struct {
	AllowModelIDs    []string `json:"allowModelIds"`
	DenyModelIDs     []string `json:"denyModelIds"`
	AllowProviderIDs []string `json:"allowProviderIds"`
	DenyProviderIDs  []string `json:"denyProviderIds"`
}

// TargetLookup resolves a (providerID, modelID) pair into a RoutingTarget.
// Injected by the resolver to allow strategy implementations to look up DB records.
type TargetLookup func(ctx context.Context, providerID, modelID string) (*RoutingTarget, error)

// CandidateCapability describes what a routing candidate would have accepted
// for an embedding request. Used to populate available_capabilities in 400
// errors when all routing candidates are rejected by the capability pre-filter.
type CandidateCapability struct {
	Provider                 string   `json:"provider"`
	Model                    string   `json:"model"`
	SupportedDimensions      []int    `json:"supported_dimensions,omitempty"`
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

// RouteResult is the output of Resolver.ResolveTargets.
// Targets is a flat ordered list: primary + fallback + recovery, already filtered and health-ranked.
type RouteResult struct {
	Targets         []RoutingTarget
	Trace           []TraceEntry
	PipelineTrace   []PipelineTraceEntry
	RuleID          string
	RuleName        string
	Substituted     bool   // true when the routing engine replaced the requested model
	OriginalModelID string // model ID as requested by the client before substitution
	// RuleRetryPolicyJSON carries the matched primary rule's
	// RoutingRule.retryPolicy JSONB column verbatim (may be empty/null).
	// The proxy handler field-merges it on top of cfg.Routing.DefaultRetryPolicy
	// before passing the effective policy to the executor.
	RuleRetryPolicyJSON json.RawMessage
}
