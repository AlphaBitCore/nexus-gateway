package handler

import (
	"context"
	"encoding/json"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/httperr"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"

	cachehandler "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/cache/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/aiguard/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/exemptions/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/rulepacks/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/iamstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/infrastructure/infra"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/thingstats/thingstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/crypto"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/settings/store/metricsstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/trafficstore"
	metricspkg "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// ProxyConfig holds BFF proxy settings for data-plane services.
type ProxyConfig struct {
	ComplianceProxyRuntimeURL string
	ComplianceProxyAPIToken   string
	AIGatewayURL              string
	// AIGatewayInternalToken is the shared internal-service bearer token
	// (env INTERNAL_SERVICE_TOKEN) the BFF presents on every CP→ai-gateway
	// /internal/* admin call. Must match the gateway's INTERNAL_SERVICE_TOKEN.
	AIGatewayInternalToken string
}

// HubNotifier is the narrow seam AdminHandler uses to push config changes to
// Nexus Hub and mint enrollment tokens. Implemented by *hub.Client in
// production and by spies in tests. Only the methods CP actually calls are
// exposed, so tests can wire an in-memory double without standing up a real
// Hub.
type HubNotifier interface {
	NotifyConfigChange(ctx context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error)
	InvalidateConfig(ctx context.Context, thingType, configKey string)
	InvalidateConfigE(ctx context.Context, thingType, configKey string) error
	CreateEnrollmentToken(ctx context.Context, req hub.CreateEnrollmentTokenRequest) (*hub.CreateEnrollmentTokenResponse, error)
	GetThingRuntime(ctx context.Context, thingID string) ([]byte, int, error)
	GetThingServiceMeta(ctx context.Context, thingID string) (*hub.ThingServiceMeta, error)
	ForceResyncAll(ctx context.Context, thingID string) (map[string]any, error)
	ListDLQ(ctx context.Context, subject, limit, cursor string) ([]byte, int, error)
	RetryDLQ(ctx context.Context, id string) ([]byte, int, error)
	BaseURL() string
	Token() string
}

// AdminHandler holds shared dependencies for all admin CRUD handlers.
type AdminHandler struct {
	DB         *store.DB
	IAM        *iam.Engine
	Audit      *audit.Writer
	Hub        HubNotifier
	Vault      *crypto.Vault      // nil if encryption unavailable
	MultiVault *crypto.MultiVault // nil if CREDENTIAL_KEY_MAP not set; takes precedence over Vault
	Logger     *slog.Logger
	Proxy      ProxyConfig
	// ExcludeInternalOpsFromBilledCost mirrors Hub's billed-cost policy.
	// Surfaced through the cost-summary handler so the CP-UI traffic event
	// drawer can render the "internal-ops counted/excluded" hint.
	// Default false (zero-value): internal ops are included in billed cost.
	ExcludeInternalOpsFromBilledCost bool

	// HubProxyClient is the HTTP client used by /api/admin/hub/*
	// passthrough handlers (admin_hub_proxy.go). Nil falls back to a
	// default 10s client at first use.
	HubProxyClient *http.Client
	// ComplianceProxyClient is the HTTP client used by /api/admin/proxy/*
	// passthrough handlers (admin_proxy.go). Nil falls back to a default
	// 10s client at first use.
	ComplianceProxyClient *http.Client

	// Revocation records admin-initiated token revocations and fans them out
	// over MQ. Nil when MQ is not configured; dependent routes then return 503
	// instead of panicking on first call.
	Revocation *revocation.Service
	// RevocationStore is the durable read-side of the revocation log. The
	// /api/admin/revocations replay endpoint reads directly from it.
	RevocationStore *revocation.Store
	// AuthRefreshTTL mirrors the auth server's default refresh-token lifetime
	// (24h in production). The internal revoke-device endpoint uses it as the
	// default ExpiresAt for ScopeDevice revocations so downstream RS-side
	// checkers can safely prune the row after the window elapses.
	AuthRefreshTTL time.Duration

	// ExemptionStore overrides the compliance exemption grant + request store
	// in tests. Nil in production: the handler falls back to h.DB.
	// R8-B1 4th sub-cluster — type moved to handler/exemption/.
	ExemptionStore exemption.DataLayer

	// Exemption owns the exemption admin API surface. Nil when DB is
	// unavailable; routes are silently skipped so startup doesn't fail.
	Exemption *exemption.Handler

	// SpillStore resolves traffic_event_payload.{request,response}_spill_ref
	// JSONB refs to actual body bytes for the GetTrafficEvent detail
	// endpoint. Nil when spill is disabled in YAML — the handler then
	// returns the row's raw spill_ref alongside a "not resolved" marker
	// instead of fetching bytes.
	SpillStore spillstore.SpillStore

	// AppliedConfigStore overrides the /things/:id/applied-config handler's DB
	// reads in tests. Nil in production: the handler falls back to h.DB. See
	// admin_things_applied_config.go for the interface shape.
	AppliedConfigStore appliedConfigStore

	// AppliedConfigOverrideFetcher overrides the override-list fetcher the
	// /things/:id/applied-config handler uses to enrich each entry with
	// override metadata. Nil in production: the handler builds a Hub-HTTP
	// fetcher from h.Hub. See admin_things_applied_config.go for the
	// interface shape.
	AppliedConfigOverrideFetcher appliedConfigOverrideFetcher

	// ThingOverrideGroupLookup overrides the IAM group lookup the
	// per-Thing override + force-sync handlers run for type-scope RBAC.
	// Nil in production: the handler falls back to h.DB. See
	// admin_thing_overrides.go for the interface shape.
	ThingOverrideGroupLookup infra.ThingOverrideGroupLookup

	// AIGuard owns /api/admin/ai-guard/*. Nil when the pool / configstore
	// are unavailable; routes are then silently skipped so startup does
	// not fail on optional deps. R8-B1 3rd sub-cluster — extracted into
	// handler/aiguard/ subpackage.
	AIGuard *aiguard.Handler

	// SemanticCache owns /api/admin/semantic-cache/*. Nil when the pool /
	// configstore are unavailable; routes are silently skipped.
	SemanticCache *cachehandler.SemanticCacheHandler

	// ExtractCache owns /api/admin/extract-cache/*. Nil when the pool /
	// configstore are unavailable; routes are silently skipped.
	ExtractCache *cachehandler.ExtractCacheHandler

	// RulePacks owns /api/admin/rule-packs/* and related install routes.
	// Nil when the DB-backed rulepack store is unavailable.
	RulePacks *rulepacks.Handler

	// Redis is an optional Redis client used to read circuit breaker state and
	// perform circuit resets for credential pool management. Nil when Redis is
	// not configured; circuit-state endpoints return empty values instead.
	Redis redis.UniversalClient
}

// Pagination holds parsed limit/offset values.
type Pagination struct {
	Limit  int
	Offset int
}

// parseAdminAuditParams parses the admin-audit-log query parameters.
// Used by my_routes.go's ListMyAdminAuditLogs under /api/my/.
func parseAdminAuditParams(c echo.Context) trafficstore.AdminAuditLogListParams {
	pg := parsePagination(c)
	params := trafficstore.AdminAuditLogListParams{
		ActorID:        c.QueryParam("actorId"),
		ActorLabel:     c.QueryParam("actorLabel"),
		ActorRole:      c.QueryParam("actorRole"),
		Action:         c.QueryParam("action"),
		EntityType:     c.QueryParam("entityType"),
		NexusRequestID: c.QueryParam("nexusRequestId"),
		Limit:          pg.Limit,
		Offset:         pg.Offset,
	}
	if v := c.QueryParam("startTime"); v != "" {
		if t, ok := parseRFC3339Flexible(v); ok {
			params.StartTime = &t
		}
	}
	if v := c.QueryParam("endTime"); v != "" {
		if t, ok := parseRFC3339Flexible(v); ok {
			params.EndTime = &t
		}
	}
	return params
}

// queryMetricsOrFallback runs the rollup query (TimeSeries-aware or cascade)
// and returns the built result. Used by admin_proxy_rollup.go and admin_extras.go.
func (h *AdminHandler) queryMetricsOrFallback(ctx context.Context, q metricspkg.MetricsQuery) (*metricspkg.MetricsResult, error) {
	ms := metricsstore.New(h.DB.InternalPool())
	var rows []metricspkg.RollupRow
	var err error
	if q.TimeSeries {
		rows, err = ms.QueryRollupAware(ctx, q)
	} else {
		rows, err = ms.QueryRollupCascade(ctx, q)
	}
	if err == nil && len(rows) > 0 {
		gran := metricspkg.SelectGranularity(q.StartTime, q.EndTime)
		return metricspkg.BuildResult(q, rows, gran), nil
	}
	return nil, nil
}

// parseTimeRange extracts the optional `startTime`/`endTime` (or `start`/`end`)
// RFC3339 query params and returns them as *time.Time pointers.
// Used by admin_proxy.go and admin_compliance.go.
func parseTimeRange(c echo.Context) (start, end *time.Time) {
	startStr := c.QueryParam("startTime")
	if startStr == "" {
		startStr = c.QueryParam("start")
	}
	if startStr != "" {
		if t, ok := parseRFC3339Flexible(startStr); ok {
			start = &t
		}
	}
	endStr := c.QueryParam("endTime")
	if endStr == "" {
		endStr = c.QueryParam("end")
	}
	if endStr != "" {
		if t, ok := parseRFC3339Flexible(endStr); ok {
			end = &t
		}
	}
	return
}

// currentUserID extracts the authenticated user's key ID from the session.
// Returns "" if no session is present. Used by /api/my/* handlers for ownership checks.
func currentUserID(c echo.Context) string {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return ""
	}
	return aa.KeyID
}

// deref returns the pointee or "" if p is nil. Used across several admin_*.go
// files; intentionally duplicated from handler/hooks/deref under the
// helper-copy strategy (avoids a new shared package for a one-liner).
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// errJSON is the canonical admin error envelope helper (see internal/platform/httperr).
var errJSON = httperr.ErrJSON

// parseRFC3339Flexible parses a time string in either RFC3339Nano (e.g.
// "2024-01-01T00:00:00.000Z" as produced by JS toISOString()) or plain
// RFC3339 (no fractional seconds). Returns (zero, false) on failure.
// Use this instead of time.Parse(time.RFC3339, ...) wherever query params
// may originate from the browser Date API.
func parseRFC3339Flexible(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// firstNonEmpty returns the first non-empty string from the provided values.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// parsePagination extracts limit and offset from query parameters.
func parsePagination(c echo.Context) Pagination {
	limit := 50
	offset := 0

	if v := c.QueryParam("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
			if limit > 1000 {
				limit = 1000
			}
		}
	}
	if v := c.QueryParam("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	return Pagination{Limit: limit, Offset: offset}
}

// isSuperAdmin checks whether the authenticated principal belongs to the
// "super-admins" IAM group. Returns false when aa is nil or DB lookup fails.
func (h *AdminHandler) isSuperAdmin(c echo.Context, aa *auth.AdminAuth) bool {
	if aa == nil {
		return false
	}
	pt := aa.AuthPrincipalType
	if pt == "admin_user" {
		pt = "nexus_user"
	}
	groups, err := iamstore.New(h.DB.InternalPool()).ListGroupNamesForPrincipal(c.Request().Context(), pt, aa.KeyID)
	if err != nil {
		return false
	}
	for _, g := range groups {
		if g == "super-admins" {
			return true
		}
	}
	return false
}

// defaultHandlerHubProxyClient is the fallback HTTP client used when
// AdminHandler.HubProxyClient is unset. Mirrors infra.defaultHubHTTPClient.
var defaultHandlerHubProxyClient = nexushttp.New(nexushttp.Config{
	Timeout:        10 * time.Second,
	Caller:         "cp-admin-hub-proxy",
	PropagateReqID: true,
})

// handlerHubProxyClient returns the test-overridable client or the
// shared default. Kept in handler/ flat so admin_things_applied_config.go
// can compose the override fetcher without importing handler/infra/.
func handlerHubProxyClient(override *http.Client) *http.Client {
	if override != nil {
		return override
	}
	return defaultHandlerHubProxyClient
}

// Actor captures the authenticated principal identity propagated into
// hub.ConfigChangeRequest so Hub records the admin identity in its
// audit ledger.
type Actor struct {
	UserID string
	Name   string
}

// actorFromContext extracts the caller identity attached by the admin auth
// middleware. Returns zero-value Actor when no AdminAuth is present — the
// caller decides how to surface that.
func actorFromContext(c echo.Context) Actor {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return Actor{}
	}
	return Actor{UserID: aa.KeyID, Name: aa.KeyName}
}

// sourceIP returns the remote client IP for audit purposes, honouring Echo's
// configured RealIP extractors (X-Forwarded-For / X-Real-IP).
func sourceIP(c echo.Context) string {
	return c.RealIP()
}

// templateVersion returns the template version for admin API responses.
// Returns 0 when the template is nil (unseeded), which the UI treats as
// "no config set yet".
func templateVersion(tpl *thingstore.ThingConfigTemplate) int64 {
	if tpl == nil {
		return 0
	}
	return tpl.Version
}

// incrementConfigVersion atomically increments the agent config version stored
// in system_metadata so that agents can detect configuration changes. Errors
// are logged but not propagated — a missed increment is non-fatal; the agent
// will pick up changes on the next full poll.
func (h *AdminHandler) incrementConfigVersion(ctx context.Context) {
	const key = "agent.config.version"
	version := 0
	meta := systemmetastore.New(h.DB.InternalPool())
	raw, err := meta.GetSystemMetadata(ctx, key)
	if err == nil && raw != nil {
		var v int
		if json.Unmarshal(raw, &v) == nil {
			version = v
		}
	}
	version++
	if err := meta.SetSystemMetadata(ctx, key, version, "system"); err != nil {
		h.Logger.Error("increment agent config version", "error", err)
	}
}
