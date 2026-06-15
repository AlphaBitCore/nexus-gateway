// Package configkey is the single source of truth for the well-known
// thing_config_template config_key values used across the platform.
// Every Go call site that references a configKey by string literal
// should import this package and use the constant — a typo or stray
// "auth_config" (vs "auth") silently orphans the row from receivers,
// startup audits, and the admin UI catalog.
//
// The constant set + ValidByThingType map are validated at Hub startup
// (see validation.go); a (type, key) row in thing_config_template that
// isn't in ValidByThingType emits a WARN, and a receiver registered
// for a (type, key) not in ValidByThingType also emits a WARN.
//
// All configKeys are snake_case and have no `_config` / `_settings` /
// `.config` suffix.
//
// NOTE on system_metadata vs thing_config_template — these are TWO
// different DB tables with different key namespaces. The constants
// below are ONLY for thing_config_template.config_key values. Keys
// such as "siem.config", "payload_capture.config",
// "streaming_compliance.config", "gateway.credential_reliability.config",
// "gateway.settings", and "agent.settings" are system_metadata row keys
// and are NOT covered by this package — they keep their local string
// literals at their owning sites.
package configkey

// Type A keys (config blob — state IS the config)
const (
	// LogLevel — per-Thing slog level.
	LogLevel = "log_level"

	// Killswitch — kill switch toggle. Wire shape: {engaged: bool}.
	Killswitch = "killswitch"

	// AIGuard — AI Guard config blob.
	AIGuard = "ai_guard"

	// Cache — AI Gateway response cache config.
	Cache = "cache"

	// GatewayPassthrough — AI Gateway emergency passthrough toggle.
	// NOTE: this configKey is unrelated to the three Prisma SQL tables
	// `gateway_passthrough_config_global` / `_adapter` / `_provider`,
	// which back the 3-tier assembly blob CP writes during the
	// /api/admin/passthrough/* admin flow. The configKey is the
	// thing_config_template invalidation channel name; the tables are
	// the real SQL entities holding tier data. They share the same
	// "gateway_passthrough" prefix by historical alignment, not because
	// they're the same thing.
	GatewayPassthrough = "gateway_passthrough"

	// AgentSettings — agent runtime settings (heartbeat interval, etc.).
	AgentSettings = "agent_settings"

	// DiagMode — agent diagnostic mode toggle.
	DiagMode = "diag_mode"

	// Onboarding — compliance-proxy onboarding state.
	Onboarding = "onboarding"

	// PayloadCapture — payload capture flags + caps. NOTE: For
	// ai-gateway and compliance-proxy, the receivers IGNORE the
	// pushed state bytes and re-read from system_metadata
	// 'payload_capture.config' (effectively Type B — invalidation
	// only). The agent variant DOES parse {enabled: bool} from the
	// state. Seed rows for ai-gateway / compliance-proxy carry
	// `null` to make the invalidation-only contract explicit.
	PayloadCapture = "payload_capture"

	// Observability — per-Thing observability toggles. NOTE: This
	// key is Type B everywhere — every receiver (ai-gateway,
	// compliance-proxy, control-plane, nexus-hub) ignores the
	// pushed state and re-reads from system_metadata
	// 'observability.config'. Seed rows therefore carry `null`;
	// templates / introspection should treat the state as opaque.
	Observability = "observability"

	// ResponseCacheTimeSensitivePatterns — cluster-wide list of co-occurrence
	// patterns pushed from Hub to every ai-gateway Thing via shadow. Each
	// pattern: {id, keywords[], require_question_mark, require_entity,
	// languages[]}. When any rule fires on an incoming prompt, both L1 and
	// L2 cache tiers skip (lookup AND write) for that request. Receivers
	// swap the freshness.Detector atomically on each push. Defined here
	// so receiver registration can import it without a circular dep.
	ResponseCacheTimeSensitivePatterns = "response_cache.time_sensitive_patterns"

	// SemanticCacheConfig — fleet-wide L1 embedding singleton config blob
	// pushed from Hub to every ai-gateway Thing on L1 save. Shape mirrors
	// ai_guard_config: {embedding_provider_id, embedding_model_id,
	// embedding_dimension, embedding_fingerprint, redis_index_name, enabled}.
	// Receiver reloads the semantic cache client and, when fingerprint
	// changes, emits the semantic_cache.invalidate_all job.
	SemanticCacheConfig = "semantic_cache.config"

	// ResponseCacheExtractConfig — fleet-wide L1 extract (exact-match) cache
	// singleton config pushed from Hub to every ai-gateway Thing on save.
	// Shape: {enabled, ttlSeconds, applyFreshnessRules}. Receiver
	// (registerAGExtractCacheConfig) hot-swaps cache.Cache via atomic.Pointer so
	// admins can disable cache or stop freshness rules from firing without a
	// service restart. The applyFreshnessRules flag governs the
	// classifyCachePreLookup time-sensitive skip behaviour for BOTH L1 and L2
	// (the gate lives upstream of the L1/L2 split).
	ResponseCacheExtractConfig = "response_cache.extract_config"
)

// Type B keys (invalidation trigger — state stays null/{})
const (
	// Providers — invalidate the cached provider list.
	Providers = "providers"

	// Models — invalidate the cached model catalog.
	Models = "models"

	// Credentials — invalidate the cached credential snapshot.
	Credentials = "credentials"

	// RoutingRules — invalidate the cached routing rule snapshot.
	RoutingRules = "routing_rules"

	// VirtualKeys — Type B with structured payload
	// {op:"invalidate", ids:[...]} so the gateway can scope the eviction
	// instead of full reload.
	VirtualKeys = "virtual_keys"

	// QuotaPolicies — invalidate the cached quota policy snapshot.
	QuotaPolicies = "quota_policies"

	// QuotaOverrides — invalidate the cached quota override snapshot.
	QuotaOverrides = "quota_overrides"

	// Organizations — invalidate the cached organization snapshot.
	Organizations = "organizations"

	// InterceptionDomains — invalidate the cached domain interception
	// list (compliance-proxy + agent consumers).
	InterceptionDomains = "interception_domains"

	// Hooks — invalidate the cached hooks snapshot.
	Hooks = "hooks"

	// Exemptions — invalidate / push the active exemption list. Shared
	// value across compliance-proxy and agent.
	Exemptions = "exemptions"

	// StreamingCompliance — invalidate the streaming compliance policy
	// snapshot consumed by the data planes.
	StreamingCompliance = "streaming_compliance"

	// CredentialReliability — invalidate the credential reliability
	// thresholds snapshot used by AI Gateway and Hub jobs.
	CredentialReliability = "credential_reliability"

	// SIEM — canonical SIEM config invalidation key. Some publishers
	// still pass "siem.config" — those system_metadata keys are NOT
	// this constant; this is the thing_config_template-side name.
	SIEM = "siem"

	// InstalledRulePacks — agent's installed rule pack snapshot
	// invalidation. Pulled Cat B via catbagent.
	InstalledRulePacks = "installed_rule_packs"

	// UserContext — agent's per-device user context snapshot
	// invalidation. Pulled Cat B via catbagent.
	UserContext = "user_context"
)
