package settings

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
)

// CacheFlush triggers a Hub shadow invalidation for every AI-traffic
// config category and logs an audit entry. Mounted at POST /api/admin/cache/flush.
//
// Several of these keys are security-sensitive (credentials, virtual_keys,
// routing_rules, quota_*). A flush whose purpose is to re-propagate them must
// fail loud (HTTP 502) on a Hub push failure rather than report a false
// success — otherwise the admin believes the fleet was flushed when it was not.
func (h *Handler) CacheFlush(c echo.Context) error {
	ctx := c.Request().Context()
	if h.hub != nil {
		type fanout struct {
			thingType string
			configKey string
		}
		for _, f := range []fanout{
			{"ai-gateway", configkey.Providers},
			{"ai-gateway", configkey.Credentials},
			{"ai-gateway", configkey.RoutingRules},
			{"ai-gateway", configkey.Hooks},
			{"ai-gateway", configkey.VirtualKeys},
			{"ai-gateway", configkey.QuotaPolicies},
			{"ai-gateway", configkey.QuotaOverrides},
			{"compliance-proxy", configkey.Hooks},
			{"agent", configkey.Exemptions},
		} {
			if err := h.hub.InvalidateConfigE(ctx, f.thingType, f.configKey); err != nil {
				h.logger.Error("cache flush: hub invalidate failed",
					"thingType", f.thingType, "configKey", f.configKey, "error", err)
				return c.JSON(http.StatusBadGateway, map[string]any{
					"error": map[string]any{
						"message": "Cache flush did not reach the gateway fleet; verify Hub health and retry.",
						"type":    "propagation_error",
						"code":    "HUB_PROPAGATION_FAILED",
						"detail":  err.Error(),
					},
				})
			}
		}
	}

	categories := []string{"providers", "models", "credentials", "routing", "hooks", "virtual-keys", "quotas", "interceptionDomains"}
	ae := audit.EntryFor(c, iam.ResourceSettings, iam.VerbUpdate)
	ae.AfterState = map[string]any{"categories": categories}
	h.audit.LogObserved(ctx, ae)

	return c.JSON(http.StatusOK, map[string]any{"flushed": true})
}
