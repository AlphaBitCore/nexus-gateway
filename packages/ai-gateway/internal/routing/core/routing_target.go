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
}
