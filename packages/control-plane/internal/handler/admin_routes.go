package handler

import (
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/cache/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/quota/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/routing/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/virtualkeys/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/fleet/handler/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/fleet/store/agentstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/dsar/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/hooks/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/interception/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/killswitch/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/passthrough/handler"
	handleriam "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/infrastructure/infra"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/alerts/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/dlq/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/opsmetrics/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/retention/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/siem/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/thingstats/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/settings/handler/settings"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/analytics/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/handler/traffic"
)

// RegisterAdminRoutes registers all admin CRUD routes on the given Echo group.
// The group should be /api/admin with auth middleware already applied.
func (h *AdminHandler) RegisterAdminRoutes(g *echo.Group) {
	iamMW := func(action string) echo.MiddlewareFunc {
		return middleware.RequireIAMPermission(h.IAM, action, nil)
	}
	// Device-aware IAM middleware factory used by routes that operate
	// on a single agent-device identified by a path parameter
	// (`/agent-devices/:id/...`). Resolves the device's group
	// memberships at request time so policies with Resource scoped to
	// `agent-device/group:<id>/*` enforce membership. h.DB satisfies
	// middleware.DeviceGroupLookup via GroupsOfDevice; nil callers
	// degrade gracefully to unscoped-only.
	iamBundle := handleriam.New(handleriam.Deps{
		Pool:            h.DB.InternalPool(),
		Hub:             h.Hub,
		Audit:           h.Audit,
		Logger:          h.Logger,
		IAM:             h.IAM,
		Revocation:      h.Revocation,
		RevocationStore: h.RevocationStore,
		AuthRefreshTTL:  h.AuthRefreshTTL,
	})
	iamMWDevice := func(action, deviceIDParam string) echo.MiddlewareFunc {
		return middleware.RequireIAMPermissionForDevice(h.IAM, action, deviceIDParam, agentstore.New(h.DB.InternalPool()))
	}

	// Auth routes (login/logout/refresh/whoami/SSO/SAML) are served by the
	// separate auth server — not registered here.
	//
	// Provider/Model/Credential/Reliability — R6 sixth domain
	// (handler/providers/ subpackage).
	provHandler := providers.New(providers.Deps{
		Pool:       h.DB.InternalPool(),
		Hub:        h.Hub,
		Audit:      h.Audit,
		Logger:     h.Logger,
		Vault:      h.Vault,
		MultiVault: h.MultiVault,
		Proxy: providers.ProxyConfig{
			ComplianceProxyRuntimeURL: h.Proxy.ComplianceProxyRuntimeURL,
			ComplianceProxyAPIToken:   h.Proxy.ComplianceProxyAPIToken,
			AIGatewayURL:              h.Proxy.AIGatewayURL,
		},
		Redis: h.Redis,
	})
	provHandler.RegisterProviderRoutes(g, iamMW)
	provHandler.RegisterModelRoutes(g, iamMW)
	provHandler.RegisterCredentialRoutes(g, iamMW)
	// Virtual key routes — R6 fifth domain (handler/virtualkey/ subpackage).
	// HubVKInvalidator combines NotifyConfigChange (targeted invalidate-by-hash)
	// and InvalidateConfig (fire-and-forget).
	vkHandler := virtualkey.New(virtualkey.Deps{
		Pool:   h.DB.InternalPool(),
		Hub:    h.Hub,
		Audit:  h.Audit,
		Logger: h.Logger,
	})
	vkHandler.RegisterRoutes(g, iamMW)
	// Routing rule routes — R6 seventh domain (handler/routing/ subpackage).
	routing.New(routing.Deps{
		Pool:   h.DB.InternalPool(),
		Meta:   systemmetastore.New(h.DB.Pool),
		Hub:    h.Hub,
		Audit:  h.Audit,
		Logger: h.Logger,
		Proxy:  routing.ProxyConfig{AIGatewayURL: h.Proxy.AIGatewayURL},
	}).RegisterRoutingRoutes(g, iamMW)
	// Quota policy + override + analytics routes — R8-B16 extracted into
	// handler/quota/ subpackage.
	quotaHandler := quota.New(quota.Deps{Pool: h.DB.InternalPool(), Hub: h.Hub, Audit: h.Audit, Logger: h.Logger})
	quotaHandler.RegisterQuotaPolicyRoutes(g, iamMW)
	quotaHandler.RegisterQuotaOverrideRoutes(g, iamMW)
	// Unified alerting routes — R6 second domain (handler/alerts/ subpackage).
	// Narrow HubBaseURLToken interface decouples alerts/ from the full HubNotifier surface.
	alerts.New(alerts.Deps{
		Hub:    h.Hub,
		Audit:  h.Audit,
		Logger: h.Logger,
	}).RegisterRoutes(g, iamMW)
	// VK approval routes (same handler, separate route group).
	vkHandler.RegisterApprovalRoutes(g, iamMW)
	// Quota analytics routes — same handler/quota/ subpackage.
	quotaHandler.RegisterQuotaAnalyticsRoutes(g, iamMW)
	// Hook config routes — R6 third domain (handler/hooks/ subpackage).
	hooksHandler := hooks.New(hooks.Deps{
		Pool:   h.DB.InternalPool(),
		Meta:   systemmetastore.New(h.DB.Pool),
		Hub:    h.Hub,
		Audit:  h.Audit,
		Logger: h.Logger,
	})
	hooksHandler.RegisterRoutes(g, iamMW)
	// Interception domain + path routes.
	interception.New(interception.Deps{Pool: h.DB.InternalPool(), Meta: systemmetastore.New(h.DB.Pool), Hub: h.Hub, Audit: h.Audit, Logger: h.Logger}).RegisterInterceptionDomainRoutes(g, iamMW)
	// Infrastructure admin — nodes BFF, runtime, config-sync, jobs,
	// thing-overrides, service-URLs, agent setup, diag silences +
	// events + mode. R8-B3 — handler/infra/ subpackage.
	infraHandler := infra.New(infra.Deps{
		DB:                       h.DB,
		Hub:                      h.Hub,
		Audit:                    h.Audit,
		Logger:                   h.Logger,
		ThingOverrideGroupLookup: h.ThingOverrideGroupLookup,
		HubProxyClient:           h.HubProxyClient,
		ComplianceProxyClient:    h.ComplianceProxyClient,
		Proxy: infra.ProxyConfig{
			ComplianceProxyRuntimeURL: h.Proxy.ComplianceProxyRuntimeURL,
			ComplianceProxyAPIToken:   h.Proxy.ComplianceProxyAPIToken,
			AIGatewayURL:              h.Proxy.AIGatewayURL,
		},
	})
	infraHandler.RegisterRoutes(g, iamMW)
	// Traffic events + admin audit logs + forward-proxy dashboard —
	// R6 ninth domain (handler/traffic/ subpackage).
	trafficHandler := traffic.New(traffic.Deps{
		Pool:       h.DB.InternalPool(),
		Audit:      h.Audit,
		Logger:     h.Logger,
		SpillStore: h.SpillStore,
		Proxy: traffic.ProxyConfig{
			ComplianceProxyRuntimeURL: h.Proxy.ComplianceProxyRuntimeURL,
			ComplianceProxyAPIToken:   h.Proxy.ComplianceProxyAPIToken,
		},
		HTTPClient: h.ComplianceProxyClient,
	})
	// Built-in traffic adapter ID catalog (shared/traffic/adapters)
	trafficHandler.RegisterTrafficAdapterCatalogRoute(g, iamMW)
	// Agent device + group + fleet admin routes — R8-B2 extracted into
	// handler/agent/ subpackage. One Handler instance wires the full
	// agent admin surface (devices, groups, group_config, bulk ops,
	// smart-group eval, fleet user admin, device tags).
	agentHandler := agent.New(agent.Deps{
		Pool:   h.DB.Pool,
		Hub:    h.Hub,
		Audit:  h.Audit,
		Logger: h.Logger,
	})
	agentHandler.RegisterRoutes(g, iamMW, iamMWDevice)
	// Agent exemption management routes — owned by handler/exemption/
	// (registered once via h.Exemption.RegisterRoutes below).
	// Traffic event + admin audit log routes
	trafficHandler.RegisterTrafficRoutes(g, iamMW)
	// Analytics routes
	analyticsHandler := analytics.New(analytics.Deps{
		Pool:                             h.DB.InternalPool(),
		Logger:                           h.Logger,
		ExcludeInternalOpsFromBilledCost: h.ExcludeInternalOpsFromBilledCost,
	})
	analyticsHandler.RegisterAnalyticsRoutes(g, iamMW)
	// Metric rollup routes
	analyticsHandler.RegisterMetricsRoutes(g, iamMW)
	// Per-Thing stats routes — reads thing_metric_rollup_* tables via Hub.
	thingstats.New(thingstats.Deps{Pool: h.DB.Pool, Audit: h.Audit, Logger: h.Logger}).RegisterAdminThingStatsRoutes(g, iamMW)
	// DSAR routes — R8-B4 — handler/dsar/ subpackage.
	dsar.New(dsar.Deps{Pool: h.DB.Pool, Audit: h.Audit, Logger: h.Logger}).RegisterDSARRoutes(g, iamMW)
	// Compliance report routes — moved to traffic/ subpackage as the
	// reports are traffic-event analyses; the kill-switch + exemption
	// admin lives in a future compliance/ extraction.
	trafficHandler.RegisterComplianceRoutes(g, iamMW)
	// SIEM settings routes
	siem.New(siem.Deps{Meta: systemmetastore.New(h.DB.Pool), Hub: h.Hub, Audit: h.Audit, Logger: h.Logger}).RegisterSIEMRoutes(g, iamMW)
	// Dead-letter queue admin routes — list traffic_event_dlq rows + retry
	// individual messages back onto the original MQ subject. Both proxy
	// to Hub /api/hub/dlq; CP wraps IAM check + AdminAuditLog stamp.
	dlq.New(dlq.Deps{Hub: h.Hub, Audit: h.Audit, Logger: h.Logger}).RegisterDLQRoutes(g, iamMW)
	// Organization + project routes
	iamBundle.RegisterOrganizationRoutes(g, iamMW)
	// Exemption request workflow routes — owned by handler/exemption/
	// (registered once via h.Exemption.RegisterRoutes above).
	// System settings + device-auth + device-defaults routes, plus
	// setup-state, cache mgmt, observability, payload-capture, streaming-compliance —
	// settings/handler/settings subpackage. Hub is needed for cache flush
	// and multi-plane shadow invalidation (P8.12 extras migration).
	settings.New(settings.Deps{
		Pool:   h.DB.Pool,
		Meta:   systemmetastore.New(h.DB.Pool),
		Hub:    h.Hub,
		Audit:  h.Audit,
		Logger: h.Logger,
	}).RegisterRoutes(g, iamMW)
	// Service public URLs (Hub / CP / AI Gateway / Compliance Proxy)
	// from each server Thing's staticInfo.publicURL; consumed by UI
	// pages that need real-environment URLs (agent-setup, etc.).
	// h.RegisterServiceURLRoutes(g, iamMW) — owned by handler/infra/
	// Global credential reliability threshold routes (Settings page).
	provHandler.RegisterReliabilitySettingsRoutes(g, iamMW)
	// Device group management routes — owned by handler/agent/
	// (registered once via agentHandler.RegisterRoutes above).
	// Forward-proxy dashboard: runtime probes forward to compliance-proxy; reads use CP Postgres.
	trafficHandler.RegisterProxyRoutes(g, iamMW)
	// Compliance killswitch admin API (reads CP tables, writes via Hub).
	// ai-gateway has no killswitch — only compliance-proxy does.
	// R8-B1 first sub-cluster — extracted into handler/killswitch/
	// subpackage; narrow HubConfigChanger interface.
	killswitch.New(killswitch.Deps{
		Hub:    h.Hub,
		Audit:  h.Audit,
		Logger: h.Logger,
	}).RegisterRoutes(g, iamMW)
	// AI Guard (built-in hook) config + dry-run admin API.
	// R8-B1 3rd sub-cluster — handler/aiguard/ subpackage. AIGuard is nil
	// when the pool / configstore are unavailable; routes are silently
	// skipped so startup doesn't fail on optional deps.
	if h.AIGuard != nil {
		h.AIGuard.RegisterRoutes(g, iamMW)
	}
	// Semantic cache singleton config routes — GET/PUT /api/admin/semantic-cache/config.
	// IAM-gated on ResourceSemanticCache. Nil when pool/configstore unavailable.
	if h.SemanticCache != nil {
		h.SemanticCache.RegisterSemanticCacheRoutes(g, iamMW)
	}
	// Extract (L1 exact-match) cache fleet config routes — GET/PUT /api/admin/extract-cache/config.
	if h.ExtractCache != nil {
		h.ExtractCache.RegisterExtractCacheRoutes(g, iamMW)
	}
	// Rule Pack catalog/import/install admin API
	if h.RulePacks != nil {
		h.RulePacks.RegisterRoutes(g, iamMW)
	}
	// Compliance + agent exemption admin API.
	// R8-B1 4th sub-cluster — handler/exemption/ subpackage owns the
	// full exemption surface (compliance grants + request approve/reject
	// + employee submit + agent admin).
	if h.Exemption != nil {
		h.Exemption.RegisterRoutes(g, iamMW)
	}
	// BFF reverse proxy to Nexus Hub (product terminology: nodes / config-sync)
	// h.RegisterNodeRoutes(g, iamMW) — owned by handler/infra/
	// Admin UI Node detail → Applied Config tab (merges desired/reported/history per config_key)
	h.RegisterAdminNodesAppliedConfigRoutes(g, iamMW)
	// Per-Thing override CRUD + global registry + force-sync — owned by handler/infra/.
	// Admin user management
	iamBundle.RegisterUserRoutes(g, iamMW)
	// Fleet management (agent users + device extended endpoints) —
	// owned by handler/agent/ (registered above).
	// Admin API key management
	iamBundle.RegisterAPIKeyRoutes(g, iamMW)
	// OAuth client registrations (issue #40) — separate from per-user
	// admin API keys above; same audience (IAM admins), distinct lifecycle.
	iamBundle.RegisterOAuthClientRoutes(g, iamMW)
	// IAM policies/groups/attachments
	iamBundle.RegisterIAMRoutes(g, iamMW)
	// Provider connectivity tests, provider health, and pricing —
	// moved to ai/providers/handler per P8.12.
	provHandler.RegisterProviderTestRoutes(g, iamMW)
	provHandler.RegisterPricingRoutes(g, iamMW)
	// Embedding probe route — admin "Test Embedding" on Cache Settings.
	provHandler.RegisterEmbeddingProbeRoutes(g, iamMW)
	// Hook extras (implementations registry, execution chain, hook test/dry-run) —
	// moved to governance/hooks/handler per P8.12.
	hooksHandler.RegisterHookExtrasRoutes(g, iamMW, hooks.ProxyConfig{AIGatewayURL: h.Proxy.AIGatewayURL})
	// Fleet analytics — moved to fleet/handler/agent per P8.12.
	// (registered above via agentHandler.RegisterRoutes → RegisterFleetAnalyticsRoutes)
	// /me + /me/permissions + PATCH /me + /iam/action-catalog + /organizations/tree —
	// moved to identity/users/handler per P8.12.
	iamBundle.RegisterMeRoutes(g, iamMW)
	iamBundle.RegisterOrganizationTreeRoute(g, iamMW)
	// Setup state, cache management, observability, payload capture, streaming compliance —
	// moved to settings/handler/settings per P8.12 (registered above via settingsHandler.RegisterRoutes).
	// Readiness + instances — moved to infrastructure/infra per P8.12.
	infraHandler.RegisterReadinessRoutes(g, iamMW)
	// Auth sessions + revocation replay
	iamBundle.RegisterAuthSessionRoutes(g, iamMW)
	// Ops metrics endpoints (current / timeseries / fleet) — Phase 6 of the
	// ops-metrics-and-diag rollout. Reads metric_ops_* tables directly via pgx.
	opsmetrics.New(opsmetrics.Deps{Pool: h.DB.InternalPool(), Logger: h.Logger}).RegisterOpsMetricsRoutes(g, iamMW)
	// Diagnostic events list / groups / crash-cohorts (read-only).
	// h.RegisterDiagEventsRoutes(g, iamMW) — owned by handler/infra/
	// Diag silence registry — owned by handler/infra/.
	// Diagnostic mode windows: single + bulk + list. Mutates thing.metadata
	// + thing_diag_mode_window in a single transaction.
	// h.RegisterDiagModeRoutes(g, iamMW) — owned by handler/infra/
	// Observability retention config GET + PUT (one row per layer).
	observability.New(observability.Deps{Pool: h.DB.InternalPool(), Hub: h.Hub, Audit: h.Audit, Logger: h.Logger}).RegisterObservabilityRetentionRoutes(g, iamMW)
	// Setup guide relay endpoints (CA cert, MDM profile, PAC file, onboarding toggle) — owned by handler/infra/.
	// IdP: SCIM token management + group→IamGroup mappings.
	iamBundle.RegisterIdentityProviderRoutes(g, iamMW)
	// Prompt cache config routes (three-tier: global / adapter / per-provider) —
	// R6 fourth domain (handler/cache/ subpackage). Narrow HubConfigChanger
	// interface: only NotifyConfigChange is called, pushing the assembled
	// 3-tier blob under shadow key `cache`.
	cacheH := cache.New(cache.Deps{
		Pool:   h.DB.Pool,
		Hub:    h.Hub,
		Audit:  h.Audit,
		Logger: h.Logger,
	})
	cacheH.RegisterRoutes(g, iamMW)
	// Time-sensitive freshness rule editor — GET/PUT/POST/DELETE/test.
	// IAM-gated on ResourceSemanticCache.
	cacheH.RegisterTimeSensitiveRoutes(g, iamMW)
	// Emergency passthrough config routes (three-tier: global / adapter / per-provider) —
	// R8-B1 second sub-cluster (handler/passthrough/ subpackage).
	passthrough.New(passthrough.Deps{
		Pool:   h.DB.Pool,
		Hub:    h.Hub,
		Audit:  h.Audit,
		Logger: h.Logger,
	}).RegisterRoutes(g, iamMW)
}
