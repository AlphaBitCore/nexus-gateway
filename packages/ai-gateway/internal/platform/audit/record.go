package audit

import (
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

// Record holds all fields for a single audit log entry.
type Record struct {
	RequestID       string
	ClientRequestID string
	// TraceID links this event to upstream agent/compliance-proxy events for the
	// same request. Extracted from the X-Nexus-Request-Id header; falls back to
	// RequestID when the header is absent (direct-to-gateway traffic).
	TraceID   string
	Timestamp time.Time
	Method    string
	Path      string
	// EndpointType classifies the request modality. Values are canonical
	// typology.EndpointKind strings — "chat", "embeddings", "stt",
	// "tts", "image_generation", "batch". Stamped at handler dispatch
	// time from typology.KindFromWireShape(resolved.WireShape).
	// Persisted onto traffic_event.endpoint_type via the MQ wire
	// envelope. Empty for early-failure rows rejected before endpoint
	// resolution.
	EndpointType string
	// TargetMethod and TargetPath are the HTTP method + path actually sent to
	// the upstream provider. May differ from Method/Path when the gateway
	// re-routes across formats (e.g. auto-upgrade routes a chat-completions
	// request to OpenAI Responses API; or Anthropic ingress → Bedrock
	// InvokeModel). Same as Method/Path for compliance-proxy / agent
	// transparent traffic.
	TargetMethod string
	TargetPath   string
	StatusCode   int
	LatencyMs    int
	SourceIP     string
	TargetHost   string

	UserID           string
	UserDisplayName  string
	OrganizationID   string
	OrganizationName string
	ProjectID        string
	ProjectName      string
	SourceApp        string
	VirtualKeyID     string
	VirtualKeyName   string
	// VKType classifies the virtual key as "personal" (owned by a
	// NexusUser, identity.user populated) or "application" (owned by a
	// Project, identity.project populated, no NexusUser). Drives
	// EntityType dispatch in recordToMessage so the analytics entity_id
	// column carries the correct foreign key (user.id vs project.id)
	// instead of the VK name. Empty when authentication didn't resolve a
	// VK (raw API key / unknown caller).
	VKType         string
	CredentialID   string
	CredentialName string

	HookDecision   string
	HookReason     string
	HookReasonCode string

	// PassthroughFlags is the canonical-order slice of bypass-kind
	// strings (["bypassHooks", "bypassCache", "bypassNormalize"]) that
	// fired for this request, sourced from passthrough.Config.Flags().
	// Empty when no emergency passthrough is active for the routed
	// primary target's provider.
	PassthroughFlags []string

	// PassthroughReason is the operator-supplied justification from
	// the most-specific active passthrough tier. Empty unless
	// PassthroughFlags is non-empty.
	PassthroughReason string

	// HookRewritten is true when a request-stage hook produced a Modify
	// decision and the traffic adapter successfully rewrote the upstream
	// body from hookResult.ModifiedContent. HookRewriteCount records the
	// number of content slots the adapter actually overwrote. Both fields
	// are zero/false for requests that were not rewritten (the majority).
	// See packages/shared/traffic/Adapter.RewriteRequestBody.
	HookRewritten    bool
	HookRewriteCount int

	ResponseHookDecision   string
	ResponseHookReason     string
	ResponseHookReasonCode string

	// ResponseHookRewritten is true when a response-stage hook produced Modify
	// and the gateway rewrote the outbound body (non-streaming) or the first
	// held-back SSE buffer (streaming). ResponseHookRewriteCount counts
	// adapter text slots overwritten for buffered responses, or 1 when a
	// streaming buffer was replaced with a synthetic delta payload.
	ResponseHookRewritten    bool
	ResponseHookRewriteCount int

	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	// ReasoningTokens counts chain-of-thought / thinking tokens reported
	// by the provider. int64 (not *int64) — 0 means "upstream did not
	// report" which is byte-identical to "no reasoning happened" for
	// billing purposes. The MQ message and DB column use omitempty /
	// NULL respectively to keep payloads lean.
	ReasoningTokens  int64
	EstimatedCostUsd float64
	// ReasoningCostUsd is the subset of EstimatedCostUsd attributable
	// to ReasoningTokens (ReasoningTokens × OutputPricePerMillion / 1e6,
	// since all three frontier providers bill reasoning at the output rate).
	// Already counted inside EstimatedCostUsd; surfaced separately for the
	// Cost dashboard's "reasoning ratio" widget. 0 when ReasoningTokens is 0.
	ReasoningCostUsd float64

	// ComplianceTags is the merged compliance tag set emitted by the hook
	// pipeline (severity:*, detector:*, category:*, …). Persisted on
	// traffic_event.compliance_tags (text[]); consumers of the MQ envelope
	// read it as []string under the complianceTags key.
	ComplianceTags []string

	ProviderID   string
	ProviderName string

	ModelID   string
	ModelName string

	RoutingRuleID      string
	RoutingRuleName    string
	RoutedProviderID   string
	RoutedProviderName string
	RoutedModelID      string
	RoutedModelName    string

	// IngressFormat is the wire shape the client used on the request
	// (`openai`, `anthropic`, `gemini`, ...). Captured at the earliest
	// point in handler.ServeProxy from `resolved.BodyFormat`. ai-gateway
	// re-encodes BOTH directions back to the ingress wire shape before
	// the bytes are captured, so the audit's RequestBody / ResponseBody
	// always match the ingress format — there is no capture site where
	// the stored bytes are in the upstream adapter's shape (non-stream
	// captures happen after egressReshapeNonStream; the streaming tee
	// wraps the client ResponseWriter so it buffers the reshaped SSE;
	// error bodies are EncodeErrorEnvelopeForIngress output). This is the
	// single routing key shared/normalize uses for both directions —
	// see normalizeAdapterType.
	IngressFormat string

	// CacheStatus is the UNIFIED cache outcome (HIT | MISS) recorded on
	// traffic_event.cache_status. Derived at audit-write time from
	// GatewayCacheStatus + ProviderCacheStatus via DeriveCacheStatus.
	// Callers that already know the unified value (e.g. ai-guard for its
	// classify cache) may set this field directly and leave the four
	// detail fields empty; the audit writer respects a pre-set value.
	// Empty string is the zero value (cache phase didn't run / request
	// rejected before Phase 5.5).
	CacheStatus CacheStatus

	// Four detail columns (drill-down only; never exposed to filter UIs).
	// See cost-estimation-architecture.md § 6.4 for derivation + rendering.
	GatewayCacheStatus     GatewayCacheStatus
	GatewayCacheSkipReason GatewayCacheSkipReason
	GatewayCacheKind       GatewayCacheKind
	// GatewayCacheL2EntryKey is the Redis HASH key of the L2 semantic-cache
	// entry that served this row. Stamped at L2 hit time from
	// semantic.Entry.EntryKey; empty on extract-cache hits, MISS, SKIPPED,
	// and on streams that fell back to the broker after a chunk-conversion
	// error. Format: "<redis_index_name>:<sha256(EmbeddingInput)[:16]>".
	// Persisted to traffic_event.gateway_cache_l2_entry_key so the audit
	// drawer's "Mark as bad cache hit" thumbs-down can post the real key
	// the gateway will check on future FT.SEARCH hits.
	GatewayCacheL2EntryKey string
	ProviderCacheStatus    ProviderCacheStatus

	CacheKey string

	// Gateway response cache savings.
	// Set to estimatedCostUSD(...) on a cache HIT/HIT_LIVE; zero on miss.
	// EstimatedCostUsd is 0 for these requests (no upstream call was made).
	GatewayCacheSavingsUsd float64

	// Provider-side prompt cache metrics. All zero when the provider
	// did not report cache usage or the request did not reach the provider.
	CacheCreationTokens int64   // tokens written to provider cache (write cost)
	CacheReadTokens     int64   // tokens served from provider cache (savings)
	CacheWriteCostUsd   float64 // cost of writing to cache
	CacheReadSavingsUsd float64 // savings from reading from cache
	CacheNetSavingsUsd  float64 // = CacheReadSavingsUsd - CacheWriteCostUsd

	// Normaliser audit. NormalizerRan distinguishes "normaliser executed
	// and stripped nothing" (NormalizerRan=true, counts 0 → persist real 0)
	// from "normaliser never ran" (NormalizerRan=false → persist NULL). The
	// strip counts alone are ambiguous because 0 is both a legitimate result
	// and the zero value; the message writer keys on NormalizerRan to decide
	// NULL vs 0.
	NormalizerRan        bool
	NormalizedStripCount int // number of rule matches that stripped bytes
	NormalizedStripBytes int // total bytes removed by normaliser
	CacheMarkerInjected  int // cache_control markers injected by L4

	// APIKeyFingerprint is a SHA256[:8] of the presented virtual key (not the
	// real provider key) — it attributes spend to the internal caller, not
	// the upstream account.
	APIKeyClass           string
	APIKeyFingerprint     string
	UsageExtractionStatus string

	// ErrorCode is the structured failure-reason classification populated
	// by writeDetailedErr (RATE_LIMITED / QUOTA_EXCEEDED / ROUTING_NO_MATCH /
	// AUTH_INVALID / AUTH_KEY_EXPIRED / etc.). Empty on success and on
	// upstream-provider-side failures (where ai-gateway did not classify).
	// Persisted to traffic_event.error_code via recordToMessage.
	ErrorCode string
	// ErrorReason is the human-readable failure description (the `message`
	// arg to writeDetailedError). For ops to skim without looking up the
	// code dictionary. Persisted to traffic_event.error_reason.
	ErrorReason string

	RoutingTrace    any
	RoutingDecision any
	// HooksPipeline is the per-hook execution trace persisted onto
	// traffic_event.hooks_pipeline as JSON. Built by the proxy handler
	// from each pipeline.Execute() result and grown across stages so a
	// single audit row carries both the request-stage and the response-
	// stage hook chain. Empty when no hook ran. The aggregate decision
	// stays on rec.HookDecision / rec.ResponseHookDecision.
	HooksPipeline   []HookExecRecord
	QualitySignals  any
	ComplianceFlags any
	Metadata        any

	// Latency phase fields. Pointer types so the audit Writer can
	// distinguish "not measured" (nil → SQL NULL) from "measured zero"
	// (non-nil 0). Populated by the proxy handler + executor via the
	// shared/traffic PhaseTimer helper and an httptrace.ClientTrace
	// wrapping the upstream RoundTrip.
	//
	//   UpstreamTtfbMs    — TTFB from the routed provider. Streaming:
	//                       first SSE chunk arrival.
	//   UpstreamTotalMs   — Full upstream round-trip (header + body / stream close).
	//   RequestHooksMs    — Aggregate of per-hook request-side latency,
	//                       computed from HooksPipeline at recordToMessage time.
	//   ResponseHooksMs   — Aggregate of per-hook response-side latency.
	//   LatencyBreakdown  — Long-tail per-source phase durations (ms).
	//                       For ai-gateway: auth_ms, quota_ms, routing_ms,
	//                       cache_lookup_ms, req_adapter_ms, resp_adapter_ms.
	UpstreamTtfbMs   *int
	UpstreamTotalMs  *int
	RequestHooksMs   *int
	ResponseHooksMs  *int
	LatencyBreakdown map[string]int

	// EmbeddingCostUsd is stamped on every L1 miss that triggered an embedding
	// call — regardless of whether L2 hit or missed.
	// EmbeddingModelID identifies the fleet-wide embedding model used for the
	// embedding call (from semantic_cache_config.embedding_model_id).
	// Both fields are 0/"" when no embedding call was made (time-sensitive skip,
	// L2 disabled, oversize, or L1 HIT path).
	EmbeddingCostUsd float64

	// AIGuardCostUsd is the cost of the ai-guard classifier LLM call (in
	// USD). Stamped on rows where internal_purpose='ai-guard' (the ai-guard
	// emits its own row per classify) — the user request's row stays NULL
	// unless the user request directly invoked an ai-guard hook. Same
	// numeric(20,10) shape as embedding_cost_usd; same semantics: NULL on
	// non-ai-guard rows, non-zero on ai-guard rows that ran a classifier.
	AIGuardCostUsd   float64
	EmbeddingModelID string

	// RequestBody and ResponseBody are optionally captured based on the
	// runtime payload-capture config (see packages/shared/policy/payloadcapture).
	// Populated by the proxy handler when the corresponding flag is
	// enabled. The audit Writer feeds them straight into
	// spillstore.EmitBody, which routes <= MaxInlineBodyBytes inline
	// onto traffic_event_payload.inline_*_body and > MaxInlineBodyBytes
	// to the spill backend with a *_spill_ref. Nil when capture is off.
	//
	// RequestTruncated / ResponseTruncated propagate the producer's
	// "we capped streaming capture at perObjectCap" decision so the
	// resulting traffic_event_payload row reflects reality. The bytes
	// forwarded upstream are NOT affected by any of these caps — those
	// are bounded independently by MaxRequestBytes / MaxResponseBytes.
	RequestBody       []byte
	RequestTruncated  bool
	ResponseBody      []byte
	ResponseTruncated bool

	// reqBodyHandle is the pool handle when RequestBody points at a POOLED
	// buffer (set via AttachPooledRequestBody). The writer returns it to the
	// request-body pool at the record's terminal resolution (reclaimRecordBody);
	// nil means RequestBody is an ordinary GC-managed slice.
	reqBodyHandle *[]byte

	// respBodyHandle is the analogous pool handle when ResponseBody points at a
	// POOLED buffer (the streaming capture tee's backing array, set via
	// AttachPooledResponseBody). Returned to the response-body pool at the
	// record's terminal resolution; nil means ResponseBody is an ordinary
	// GC-managed slice (non-stream paths, or a stream whose body was not stored).
	respBodyHandle *[]byte

	// marshaled, when non-nil, is this record's already-encoded wire bytes, set
	// when a publish failure re-buffers the record for retry. The pooled request/
	// response body buffers are reclaimed the MOMENT a record is marshaled (the
	// marshaled bytes already hold a full copy — see marshalRecord), so the pool
	// working set is O(in-flight marshals) instead of O(queue depth). A re-buffered
	// record can therefore no longer be re-marshaled from its (reclaimed) body; the
	// retry unit is these bytes. marshalRecord returns them directly when set, and
	// the re-Normalize/recordToMessage path is never reached for such a record.
	// In-process only — never serialized.
	marshaled []byte

	// RequestContentType / ResponseContentType travel with the captured
	// bytes onto traffic_event_payload.{request,response}_content_type.
	// Empty when not detected; consumers default to inferring from the
	// body shape (raw JSON vs base64).
	RequestContentType  string
	ResponseContentType string

	// InternalPurpose mirrors TrafficEventMessage.InternalPurpose. Set to
	// "ai-guard" for classify calls issued by internal subsystems so the
	// admin UI can hide those rows from customer billing views by default.
	// Empty for customer-facing traffic.
	InternalPurpose string

	// OriginTZ is the IANA timezone of the owning org at the time of
	// capture (e.g. "Asia/Shanghai"). The Timestamp field above is
	// always a UTC instant; OriginTZ records what the local civil
	// time was at the place the event was generated, so compliance
	// reports can render in jurisdiction-local time without
	// ambiguity. Empty when the event has no tenant binding (system
	// rows).
	OriginTZ string

	// BlockingRule identifies which pack+version+rule triggered the block.
	// Marshaled to TrafficEventMessage.BlockingRule (raw JSON) by the
	// writer and persisted on traffic_event.blocking_rule. Nil for
	// requests that were not rejected by a rule pack.
	BlockingRule *rulepack.BlockingRule `json:"blockingRule,omitempty"`

	// RequestAction / ResponseAction carry the single hook action from the
	// compliance pipeline to recordToMessage, governing what raw body is
	// persisted: approve stores the captured bytes, redact/block store the
	// redacted copy (below), anything else stores nothing.
	RequestAction  decision.Action
	ResponseAction decision.Action

	// RequestBodyRedacted / ResponseBodyRedacted carry the redacted
	// wire-shape copy of the body — the adapter Rewrite*Body output the
	// proxy produced when a hook's redaction was applied inflight. The
	// writer persists the RAW payload copy under the action: approve stores
	// the captured (pre-hook) bytes, redact/block store ONLY this redacted
	// copy (absent → the raw copy is dropped, because persisting bytes we
	// could not redact would make the audit store itself the leak — the
	// rule-ID / category metadata on the row still records what tripped).
	RequestBodyRedacted  []byte
	ResponseBodyRedacted []byte
}

// ApplyVKMeta populates identity fields from virtual key metadata.
//
// Branches on meta.VKType so the entity / identity fields carry the
// correct foreign key for downstream analytics:
//
//   - "personal" VK (owned by a NexusUser): UserID = meta.OwnerID (the
//     real NexusUser.id, not the VK's name). identity.user is populated.
//   - "application" VK (owned by a Project): UserID stays empty so the
//     analytics entity_id column doesn't pretend the caller is a user.
//     identity.project carries Project.id/name instead.
//
// Pre-fix this method set `r.UserID = meta.Name` unconditionally,
// which overloaded the column: for personal VKs whose name happened to
// equal the owner's user.id (seed.ts convention) it worked by
// coincidence, but for application VKs entity_id was a VK-name slug
// that joined to no NexusUser row. The dispatch above eliminates the
// coincidence and produces correct foreign keys in both cases.
func (r *Record) ApplyVKMeta(meta *vkauth.VKMeta) {
	r.VirtualKeyID = meta.ID
	r.VirtualKeyName = meta.Name
	r.VKType = meta.VKType
	r.OrganizationID = meta.OrganizationID
	r.OrganizationName = meta.OrganizationName
	r.ProjectID = meta.ProjectID
	r.ProjectName = meta.ProjectName
	r.SourceApp = meta.SourceApp
	// Personal VKs carry an owning NexusUser; application VKs don't.
	// Only stamp user fields when the VK is personal so empty entity_id
	// downstream signals "no user", not "lookup failure".
	if meta.VKType == "personal" {
		r.UserID = meta.OwnerID
		r.UserDisplayName = meta.UserDisplayName
	}
	// Stamp the org's IANA timezone so audit/compliance reports
	// can render in jurisdiction-local time even though the
	// timestamp column is a UTC instant.
	r.OriginTZ = meta.OrganizationTimezone
}
