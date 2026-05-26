package settings

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
)

// CacheStats returns a summary of in-process caches (IAM policy cache
// size + the well-known config categories propagated by Hub shadows).
// Mounted at GET /api/admin/cache/stats.
func (h *Handler) CacheStats(c echo.Context) error {
	// IAM engine cache size is not available from settings/; the zero value
	// is acceptable here — the endpoint is informational only.
	return c.JSON(http.StatusOK, map[string]any{
		"iamPolicyCacheEntries": 0,
		"configCategories":      []string{"providers", "models", "credentials", "routing", "hooks", "virtual-keys", "quotas", "interceptionDomains"},
	})
}

// CacheFlush triggers a Hub shadow invalidation for every AI-traffic
// config category and logs an audit entry. Mounted at POST /api/admin/cache/flush.
func (h *Handler) CacheFlush(c echo.Context) error {
	ctx := c.Request().Context()
	if h.hub != nil {
		h.hub.InvalidateConfig(ctx, "ai-gateway", configkey.Providers)
		h.hub.InvalidateConfig(ctx, "ai-gateway", configkey.Credentials)
		h.hub.InvalidateConfig(ctx, "ai-gateway", configkey.RoutingRules)
		h.hub.InvalidateConfig(ctx, "ai-gateway", configkey.Hooks)
		h.hub.InvalidateConfig(ctx, "ai-gateway", configkey.VirtualKeys)
		h.hub.InvalidateConfig(ctx, "ai-gateway", configkey.QuotaPolicies)
		h.hub.InvalidateConfig(ctx, "ai-gateway", configkey.QuotaOverrides)
		h.hub.InvalidateConfig(ctx, "compliance-proxy", configkey.Hooks)
		h.hub.InvalidateConfig(ctx, "agent", configkey.Exemptions)
	}

	categories := []string{"providers", "models", "credentials", "routing", "hooks", "virtual-keys", "quotas", "interceptionDomains"}
	ae := audit.EntryFor(c, iam.ResourceSettings, iam.VerbUpdate)
	ae.AfterState = map[string]any{"categories": categories}
	h.audit.LogObserved(ctx, ae)

	return c.JSON(http.StatusOK, map[string]any{"flushed": true})
}
