package core

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
	// to clients in the `X-Nexus-Routed-Model` response header so they can
	// correlate logs without exposing the internal UUID.
	ModelCode string
	// ModelType is the catalog Model.type (chat/embedding/image/audio/video/
	// rerank). Carried on the target so the resolver's modality guard can drop
	// any target whose modality does not match the request's EndpointKind —
	// keeping every routing strategy (and the requested-model passthrough)
	// from dispatching a cross-modality model. Empty = unknown, which the
	// guard treats as "no constraint" (fail-open) so an unpopulated target is
	// never silently dropped.
	ModelType string
	// ContextUpgradeOnly marks a target the smart strategy armed purely
	// as a context-overflow escape: the executor uses it only when a
	// previously-attempted target failed with a context overflow, never
	// for transient failures (5xx/429/network) — spilling those onto the
	// larger, typically pricier, model would be a cost surprise. If a
	// downstream stage (health rank, narrowing, cross-format filter)
	// reorders it to the front or leaves it the only survivor, the
	// executor runs it as an ordinary target rather than dropping it.
	ContextUpgradeOnly bool
	ModelName          string
	ProviderModelID    string
	BaseURL            string
	Region             string
	Source             string // "primary", "fallback", "recovery"
	// RuleID names the routing rule this target came from.
	//
	// The walk needs it to know where one rule's answer ends and the next
	// begins: a rule is advanced past only when every one of ITS targets has
	// been ELIMINATED, never because the call budget ran out. Without the
	// boundary the plan is a flat list of equals, and a rule an admin wrote as
	// a lower-priority alternative starts serving traffic the moment the rule
	// above it hits one transient failure.
	RuleID string
	// ServesResponsesAPI mirrors Provider.serves_responses_api (nil = adapter
	// RequestShapes default). Carried on the routing snapshot so the proxy
	// stages (cross-format guard, body canonicalization, egress reshape) and
	// the executor resolve the /v1/responses capability identically without a
	// per-request DB read.
	ServesResponsesAPI *bool
	// MaxOutputTokens mirrors Model.maxOutputTokens (0 = NULL column), the
	// ceiling /v1/models advertises. Carried here like ServesResponsesAPI so
	// the cache stage's prepared body (which serves targets[0]'s first
	// attempt) hands the codec the real cap; 0 here breaks the anthropic
	// clamp+fill. See §5 of provider-adapter-architecture.md.
	MaxOutputTokens int
	// Reasons mirrors the catalogue's `reasoning` feature: this model thinks
	// before answering, whatever the vendor calls it.
	//
	// A named field rather than the whole `features` slice, following
	// ServesResponsesAPI beside it. A bag would let the next capability ride
	// along untyped and unasked-for, and the question a codec has is not "what
	// are this model's features" but "may I send a reasoning parameter to it" —
	// which today it cannot ask at all, so `reasoning_effort` reaches models
	// that do not reason by accident of an identity codec.
	Reasons bool
	// MaxContextTokens mirrors Model.maxContextTokens (0 = NULL column).
	//
	// The executor reads it when an attempt overflows: the next target to try
	// is then the one with the largest remaining window, not the next one in
	// the list. Position says nothing useful here — a list ordered by price
	// puts the next-cheapest model next, whose window is as likely to be
	// smaller as larger, so a walk can spend several calls overflowing in a
	// row. Sorting the remainder by window is the only move that improves the
	// odds on the one dimension that just failed.
	MaxContextTokens int
}

// FeatureReasoning is the catalogue's spelling for "this model thinks before
// answering". One constant so the string is written once: it was spelled
// `thinking` on some rows and `reasoning` on others until the catalogue was
// de-duplicated, and a literal at each read site is how that happens.
const FeatureReasoning = "reasoning"
