package iam

// Catalog is the canonical list of admin resources and their verbs.
//
// Ordering groups resources by Service (gateway → compliance → agent
// → platform → iam) for human readability. AllActions() sorts before
// returning, so ordering here does not affect any consumer's output.
var Catalog = []ResourceDef{
	// ── AI Traffic Gateway (Service: gateway) ─────────────────────────
	// Configuration + operational data for the /v1 AI API stack.
	{Name: "provider", Service: ServiceGateway, Verbs: crud()},
	{Name: "model", Service: ServiceGateway, Verbs: crud()},
	{Name: "model-pricing", Service: ServiceGateway, Verbs: []Verb{VerbRead, VerbCreate, VerbDelete}},
	{Name: "credential", Service: ServiceGateway, Verbs: append(crud(), VerbProbe, VerbRotate)},
	{Name: "virtual-key", Service: ServiceGateway, Verbs: append(crud(),
		VerbApprove, VerbReject, VerbRevoke, VerbRenew)},
	{Name: "routing-rule", Service: ServiceGateway, Verbs: append(crud(), VerbSimulate)},
	{Name: "quota-policy", Service: ServiceGateway, Verbs: crud()},
	{Name: "quota-override", Service: ServiceGateway, Verbs: crud()},
	{Name: "quota-analytics", Service: ServiceGateway, Verbs: []Verb{VerbRead}},
	{Name: "analytics", Service: ServiceGateway, Verbs: []Verb{VerbRead}},
	{Name: "traffic-log", Service: ServiceGateway, Verbs: []Verb{VerbRead}},
	// prompt-cache holds normalisation rules + per-provider cache_control
	// injection that govern the LLM response cache. Distinct from generic
	// settings: granting it shouldn't imply granting unrelated platform
	// configs (SIEM, observability, payload retention).
	{Name: "prompt-cache", Service: ServiceGateway, Verbs: []Verb{VerbRead, VerbUpdate}},
	// passthrough holds the emergency kill-switch configuration
	// (3-tier global/adapter/provider). The emergency-enable verb is
	// scoped narrowly (incident-response role only) because flipping
	// it on silences compliance hooks for matched traffic; read +
	// write (disable / delete) are wider (compliance + provider admins).
	{Name: "passthrough", Service: ServiceGateway, Verbs: []Verb{VerbRead, VerbWrite, VerbEmergencyEnable}},
	// semantic-cache governs BOTH the L1 embedding singleton
	// (/api/admin/semantic-cache/config — fleet-wide embedding provider +
	// model + kill-switch) AND the cluster-wide time-sensitive freshness rule
	// list (/api/admin/cache/time-sensitive-patterns + /test + per-rule PUT).
	// Both control surfaces share the same operator persona (Platform/Ops) and
	// the same blast radius (cluster-wide). A dedicated cache-freshness-rules
	// resource was considered and rejected — would force admins to wire two
	// separate IAM policies for the same operational role.
	{Name: "semantic-cache", Service: ServiceGateway, Verbs: []Verb{VerbRead, VerbUpdate}},
	// extract-cache governs the L1 exact-match response cache fleet-wide
	// config: enabled toggle, TTL, and the apply_freshness_rules gate
	// that controls whether freshness-rule matches actually skip L1+L2.
	// Separate from semantic-cache because (a) the IAM model already splits
	// gateway-cache concerns by layer (prompt-cache vs semantic-cache) and
	// (b) admins may want to grant extract-cache write to a wider audience
	// (incident response wanting to disable cache during a stampede) than
	// semantic-cache (which also gates embedding-model selection and
	// freshness-rule editing).
	{Name: "extract-cache", Service: ServiceGateway, Verbs: []Verb{VerbRead, VerbUpdate}},

	// ── Compliance Pipeline (Service: compliance) ─────────────────────
	// Compliance configuration owned by the security/compliance team.
	{Name: "hook", Service: ServiceCompliance, Verbs: crud()},
	{Name: "rule-pack", Service: ServiceCompliance, Verbs: append(crud(), VerbImport)},
	{Name: "compliance-exemption", Service: ServiceCompliance, Verbs: append(crud(), VerbReject)},
	{Name: "compliance-report", Service: ServiceCompliance, Verbs: []Verb{VerbRead}},
	{Name: "interception-domain", Service: ServiceCompliance, Verbs: crud()},
	{Name: "dsar", Service: ServiceCompliance, Verbs: append(crud(), VerbFulfill)},
	// payload-capture controls whether request/response bodies are
	// persisted across ai-gateway, compliance-proxy, AND agent. It is
	// fundamentally a data-retention / privacy compliance decision, not
	// a per-service feature flag — granting it shouldn't imply granting
	// unrelated settings such as SIEM or generic platform config.
	{Name: "payload-capture", Service: ServiceCompliance, Verbs: []Verb{VerbRead, VerbUpdate}},
	{Name: "ai-guard-config", Service: ServiceCompliance, Verbs: []Verb{VerbRead, VerbUpdate}},
	{Name: "kill-switch", Service: ServiceCompliance, Verbs: []Verb{VerbToggle}},

	// ── Agent Fleet (Service: agent) ──────────────────────────────────
	// Desktop Agent product line — devices and how they're grouped /
	// bound to users.
	// VerbForceResync mirrors the node verb — manual push of Hub-side
	// desired config to this device when admin wants instant propagation
	// instead of waiting for the next shadow tick. Used by the Devices
	// detail "Force config refresh" action.
	// VerbRotate triggers an immediate mTLS cert rotation on the device.
	// Backend marks thing_agent.cert_expires_at = NOW() + 5min so the
	// agent's next heartbeat (every 15s) sees "expiring" and calls
	// /api/internal/things/renew-cert. Useful when ops needs a fresh
	// cert without waiting for the auto-renew threshold (e.g. a CA root
	// rotation or a suspected key compromise).
	{Name: "agent-device", Service: ServiceAgent, Verbs: append(crud(), VerbForceResync, VerbRotate)},
	{Name: "device-group", Service: ServiceAgent, Verbs: crud()},
	// device-assignment.update is an audit-only action emitted when Hub's
	// IdentityEnricher binds an agent device to a user (see
	// packages/control-plane/internal/handler/fleet.go). No admin
	// handler iamMW() gates on it — admin users never invoke this path
	// directly — but the resource stays in the catalog so the audit
	// stream carries a stable, queryable action label for compliance
	// and SIEM consumers.
	{Name: "device-assignment", Service: ServiceAgent, Verbs: []Verb{VerbUpdate}},
	// Fleet-wide runtime defaults applied to enrolled devices (audit policy,
	// forensics toggle, shutdown warning copy). Carved out of the generic
	// `settings` resource so the compliance team can manage agent audit
	// behavior without holding write on every platform setting.
	{Name: "device-defaults", Service: ServiceAgent, Verbs: []Verb{VerbRead, VerbUpdate}},
	// agent-attestation gates the per-cluster + per-agent capability that
	// lets the desktop Agent cryptographically attest "I already inspected
	// this request" to the Compliance-Proxy. Read covers visibility of
	// the toggle and any future per-agent attestation state; Update covers
	// flipping the toggle. Distinct resource so granting "manage attestation"
	// does NOT imply granting generic agent-device CRUD or platform settings
	// write — these are different operational responsibilities (security
	// engineering owns attestation policy, IT owns device CRUD). For S2 the
	// PATCH on agent_settings still audits as `settings.update` (single
	// payload, single event); per-agent attestation overrides land in a
	// later epic and will iamMW() this action directly.
	{Name: "agent-attestation", Service: ServiceAgent, Verbs: []Verb{VerbRead, VerbUpdate}},

	// ── Platform Operations (Service: platform) ───────────────────────
	// Cross-service operational surface — service health, alerts,
	// node sync, diagnostic mode, general settings.
	{Name: "alert", Service: ServicePlatform, Verbs: append(crud(), VerbAcknowledge)},
	{Name: "observability", Service: ServicePlatform, Verbs: []Verb{VerbRead, VerbWrite}},
	// Dead-letter queue admin surface. Distinct from generic
	// `observability` because the resource carries a destructive verb
	// (manage: retry a row, which republishes raw bytes back to MQ).
	// Granting `observability.write` should NOT imply the right to
	// re-inject arbitrary captured payloads back into the audit
	// pipeline. Read is the list endpoint; manage is the retry button.
	{Name: "observability-dlq", Service: ServicePlatform, Verbs: []Verb{VerbRead, VerbManage}},
	{Name: "settings", Service: ServicePlatform, Verbs: []Verb{VerbRead, VerbUpdate, VerbWrite}},
	{Name: "diagnostic-mode", Service: ServicePlatform, Verbs: []Verb{VerbRead, VerbUpdate}},
	{Name: "node", Service: ServicePlatform, Verbs: append(crud(),
		VerbForceResync, VerbWriteOverride,
	)},

	// ── Identity & Access (Service: iam) ──────────────────────────────
	// Users, RBAC, audit, sessions.
	{Name: "user", Service: ServiceIAM, Verbs: append(crud(), VerbRevoke)},
	{Name: "api-key", Service: ServiceIAM, Verbs: append(crud(), VerbRotate)},
	// OAuth client registrations (third-party apps that authenticate to
	// the platform via the /authserver OAuth2 endpoints). Carved out as
	// its own resource so granting api-key:* (per-user authn tokens) does
	// not implicitly grant the ability to register new third-party apps.
	// VerbRotate replaces clientSecretHash without changing clientId, so
	// rotations do not break registered redirectUris or invalidate the
	// currently-issued refresh tokens.
	{Name: "oauth-client", Service: ServiceIAM, Verbs: append(crud(), VerbRotate)},
	{Name: "organization", Service: ServiceIAM, Verbs: crud()},
	{Name: "project", Service: ServiceIAM, Verbs: crud()},
	{Name: "iam-policy", Service: ServiceIAM, Verbs: crud()},
	{Name: "iam-group", Service: ServiceIAM, Verbs: crud()},
	{Name: "audit-log", Service: ServiceIAM, Verbs: []Verb{VerbRead, VerbWrite, VerbExport}},
	{Name: "revocation", Service: ServiceIAM, Verbs: []Verb{VerbRead}},
	{Name: "nexus-session", Service: ServiceIAM, Verbs: []Verb{VerbRevoke}}, // force-logout / revoke-device emit revoke audit events
	// External Identity Providers. The platform is the SP; this
	// resource controls who can register/edit/delete external IdPs
	// (Okta, Azure AD, Google Workspace, …). VerbProbe is the
	// connectivity-test endpoint for both saved and candidate configs.
	{Name: "identity-provider", Service: ServiceIAM, Verbs: append(crud(), VerbProbe)},
	// Desktop-agent device enrollment. Sole gate on
	// POST /api/agent/sso-enroll; the call consumes an OAuth auth
	// code rather than a Bearer token, so the IAM check runs inside
	// the handler against the auth-code's owning user. Carved out as
	// its own resource so granting "enroll devices" never implies
	// settings.update, device-defaults.update, or any IdP CRUD —
	// applies uniformly across the enterprise-login and local-login
	// device-auth modes.
	{Name: "device-enrollment", Service: ServiceIAM, Verbs: []Verb{VerbEnroll}},
}

// crud returns a fresh slice of the four standard verbs. A fresh slice
// per call (rather than a shared package var) prevents accidental mutation
// from one resource's Verbs leaking into another's when callers use
// append(crud(), ...) to extend the verb set.
func crud() []Verb {
	return []Verb{VerbCreate, VerbRead, VerbUpdate, VerbDelete}
}

// byName indexes Catalog for O(1) MustFind lookup. Populated by the
// package-level initializer below; downstream readers see a fully
// populated map by the time any other init() runs in the importing
// package.
var byName = buildIndex(Catalog)

func buildIndex(catalog []ResourceDef) map[string]*ResourceDef {
	m := make(map[string]*ResourceDef, len(catalog))
	for i := range catalog {
		m[catalog[i].Name] = &catalog[i]
	}
	return m
}

// Convenience pointers for the common call sites. Handlers may write
// iamMW(iam.ResourceProvider.Action(iam.VerbCreate)) instead of
// iam.MustFind("provider").Action(iam.VerbCreate). Both forms produce
// identical output; the convenience form gives compile-time validation
// of the resource name (typo → undefined symbol) and is the recommended
// path for handler code.
//
// Convenience var ordering mirrors Catalog above so editing one and
// forgetting the other surfaces immediately in code review.
var (
	// AI traffic plane.
	ResourceProvider         = MustFind("provider")
	ResourceModel            = MustFind("model")
	ResourceModelPricing     = MustFind("model-pricing")
	ResourceCredential       = MustFind("credential")
	ResourceVirtualKey       = MustFind("virtual-key")
	ResourceRoutingRule      = MustFind("routing-rule")
	ResourceQuotaPolicy      = MustFind("quota-policy")
	ResourceQuotaOverride    = MustFind("quota-override")
	ResourceQuotaAnalytics   = MustFind("quota-analytics")
	ResourceAnalytics        = MustFind("analytics")
	ResourceTrafficLog       = MustFind("traffic-log")
	ResourcePromptCache      = MustFind("prompt-cache")
	ResourcePassthrough      = MustFind("passthrough")
	ResourceSemanticCache    = MustFind("semantic-cache")
	ResourceExtractCache     = MustFind("extract-cache")
	ResourceAgentDevice      = MustFind("agent-device")
	ResourceDeviceGroup      = MustFind("device-group")
	ResourceDeviceAssignment = MustFind("device-assignment")
	ResourceDeviceDefaults   = MustFind("device-defaults")
	ResourceAgentAttestation = MustFind("agent-attestation")

	// IAM / admin platform.
	ResourceUser             = MustFind("user")
	ResourceApiKey           = MustFind("api-key")
	ResourceOAuthClient      = MustFind("oauth-client")
	ResourceOrganization     = MustFind("organization")
	ResourceProject          = MustFind("project")
	ResourceIamPolicy        = MustFind("iam-policy")
	ResourceIamGroup         = MustFind("iam-group")
	ResourceIdentityProvider = MustFind("identity-provider")
	ResourceDeviceEnrollment = MustFind("device-enrollment")
	ResourceAuditLog         = MustFind("audit-log")
	ResourceRevocation       = MustFind("revocation")
	ResourceAlert            = MustFind("alert")
	ResourceKillSwitch       = MustFind("kill-switch")
	ResourceAIGuardConfig    = MustFind("ai-guard-config")
	ResourceObservability    = MustFind("observability")
	ResourceObservabilityDLQ = MustFind("observability-dlq")
	ResourceSettings         = MustFind("settings")
	ResourceDiagnosticMode   = MustFind("diagnostic-mode")
	ResourceNode             = MustFind("node")
	ResourceNexusSession     = MustFind("nexus-session")

	// Compliance plane.
	ResourceHook                = MustFind("hook")
	ResourceRulePack            = MustFind("rule-pack")
	ResourceComplianceExemption = MustFind("compliance-exemption")
	ResourceComplianceReport    = MustFind("compliance-report")
	ResourceInterceptionDomain  = MustFind("interception-domain")
	ResourceDSAR                = MustFind("dsar")
	ResourcePayloadCapture      = MustFind("payload-capture")
)
