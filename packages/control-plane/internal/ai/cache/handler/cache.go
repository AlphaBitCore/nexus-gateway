package cache

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/cacheconfig"
)

// validateAdapterConfigKnobs makes sure an AdapterConfig body contains only
// fields appropriate to the given adapter family. Used by PUT
// /cache/adapter/:adapter_type. Rules are always allowed (they are universal).
func validateAdapterConfigKnobs(adapterType string, cfg cacheconfig.AdapterConfig) error {
	family := cacheconfig.FamilyOf(adapterType)
	switch family {
	case cacheconfig.FamilyAnthropic:
		if cfg.CacheEnabled != nil || cfg.MinSystemChars != nil ||
			cfg.TTLSeconds != nil || cfg.CircuitBreakerThreshold != nil ||
			cfg.CircuitBreakerOpenSecs != nil {
			return fmt.Errorf("gemini knobs (cache_enabled/min_system_chars/ttl_seconds/circuit_breaker_*) are not valid for adapter_type=%s", adapterType)
		}
	case cacheconfig.FamilyGemini:
		if cfg.MarkerInjectEnabled != nil || cfg.MarkerBoundary3Enabled != nil {
			return fmt.Errorf("anthropic marker knobs (marker_inject_enabled/marker_boundary3_enabled) are not valid for adapter_type=%s", adapterType)
		}
	case cacheconfig.FamilyNone:
		if cfg.CacheEnabled != nil || cfg.MinSystemChars != nil ||
			cfg.TTLSeconds != nil || cfg.CircuitBreakerThreshold != nil ||
			cfg.CircuitBreakerOpenSecs != nil ||
			cfg.MarkerInjectEnabled != nil || cfg.MarkerBoundary3Enabled != nil {
			return fmt.Errorf("adapter_type=%s has no admin-tunable cache knobs (only the 'rules' field is accepted)", adapterType)
		}
	}
	return nil
}

// validateProviderConfigKnobs same as the adapter version but for Tier-3
// override shape. Provider-tier rules are not supported (ADR-2) — handled
// via the ProviderConfig struct itself not having a Rules field.
func validateProviderConfigKnobs(adapterType string, cfg cacheconfig.ProviderConfig) error {
	family := cacheconfig.FamilyOf(adapterType)
	switch family {
	case cacheconfig.FamilyAnthropic:
		if cfg.CacheEnabled != nil || cfg.MinSystemChars != nil ||
			cfg.TTLSeconds != nil || cfg.CircuitBreakerThreshold != nil ||
			cfg.CircuitBreakerOpenSecs != nil {
			return fmt.Errorf("gemini knobs are not valid on an Anthropic-family provider (adapter_type=%s)", adapterType)
		}
	case cacheconfig.FamilyGemini:
		if cfg.MarkerInjectEnabled != nil || cfg.MarkerBoundary3Enabled != nil {
			return fmt.Errorf("anthropic marker knobs are not valid on a Gemini-family provider (adapter_type=%s)", adapterType)
		}
	case cacheconfig.FamilyNone:
		empty := cfg.CacheEnabled == nil && cfg.MinSystemChars == nil &&
			cfg.TTLSeconds == nil && cfg.CircuitBreakerThreshold == nil &&
			cfg.CircuitBreakerOpenSecs == nil &&
			cfg.MarkerInjectEnabled == nil && cfg.MarkerBoundary3Enabled == nil
		if !empty {
			return fmt.Errorf("adapter_type=%s has no admin-tunable cache knobs", adapterType)
		}
	}
	return nil
}

func (h *Handler) CacheGetGlobal(c echo.Context) error {
	cfg, err := h.cache.GetCacheGlobalConfig(c.Request().Context())
	if err != nil {
		h.logger.Error("get cache_global_config", "error", err)
		return internalServerError(c, "Internal server error")
	}
	return c.JSON(http.StatusOK, cfg)
}

func (h *Handler) CachePutGlobal(c echo.Context) error {
	var body cacheconfig.GlobalConfig
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", err.Error()))
	}

	ctx := c.Request().Context()
	a := actorFromContext(c)

	if err := h.cache.PutCacheGlobalConfig(ctx, body, a.UserID); err != nil {
		h.logger.Error("put cache_global_config", "error", err)
		return internalServerError(c, "Internal server error")
	}

	if err := h.propagateCacheConfig(ctx, a.UserID, a.Name); err != nil {
		h.logger.Error("notify hub cache (global PUT)", "error", err)
		return hub.RespondPropagationFailure(c, err)
	}

	ae := audit.EntryFor(c, iam.ResourcePromptCache, iam.VerbUpdate)
	ae.EntityID = "global"
	ae.AfterState = body
	h.audit.LogObserved(ctx, ae)

	return c.JSON(http.StatusOK, body)
}

func (h *Handler) CacheListAdapters(c echo.Context) error {
	rows, err := h.cache.ListCacheAdapterConfigs(c.Request().Context())
	if err != nil {
		h.logger.Error("list cache_adapter_config", "error", err)
		return internalServerError(c, "Internal server error")
	}
	return c.JSON(http.StatusOK, map[string]any{
		"items": rows,
		"total": len(rows),
	})
}

func (h *Handler) CacheGetAdapter(c echo.Context) error {
	adapter := c.Param("adapter_type")
	cfg, exists, err := h.cache.GetCacheAdapterConfig(c.Request().Context(), adapter)
	if err != nil {
		h.logger.Error("get cache_adapter_config", "adapter_type", adapter, "error", err)
		return internalServerError(c, "Internal server error")
	}
	if !exists {
		return c.JSON(http.StatusNotFound, errJSON("Unknown adapter_type", "not_found", adapter))
	}
	return c.JSON(http.StatusOK, cfg)
}

func (h *Handler) CachePutAdapter(c echo.Context) error {
	adapter := c.Param("adapter_type")
	var body cacheconfig.AdapterConfig
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", err.Error()))
	}
	if err := validateAdapterConfigKnobs(adapter, body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON(err.Error(), "validation_error", "adapter_mismatch"))
	}

	ctx := c.Request().Context()
	a := actorFromContext(c)

	if err := h.cache.PutCacheAdapterConfig(ctx, adapter, body, a.UserID); err != nil {
		h.logger.Error("put cache_adapter_config", "adapter_type", adapter, "error", err)
		return internalServerError(c, "Internal server error")
	}

	if err := h.propagateCacheConfig(ctx, a.UserID, a.Name); err != nil {
		h.logger.Error("notify hub cache (adapter PUT)", "adapter_type", adapter, "error", err)
		return hub.RespondPropagationFailure(c, err)
	}

	ae := audit.EntryFor(c, iam.ResourcePromptCache, iam.VerbUpdate)
	ae.EntityID = "adapter:" + adapter
	ae.AfterState = body
	h.audit.LogObserved(ctx, ae)

	return c.JSON(http.StatusOK, body)
}

func (h *Handler) CacheGetProvider(c echo.Context) error {
	providerID := c.Param("provider_id")
	cfg, _, err := h.cache.GetCacheProviderConfig(c.Request().Context(), providerID)
	if err != nil {
		h.logger.Error("get cache_provider_config", "provider_id", providerID, "error", err)
		return internalServerError(c, "Internal server error")
	}
	return c.JSON(http.StatusOK, cfg)
}

func (h *Handler) CachePutProvider(c echo.Context) error {
	providerID := c.Param("provider_id")
	var body cacheconfig.ProviderConfig
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", err.Error()))
	}

	ctx := c.Request().Context()
	adapter, exists, err := h.cache.GetProviderAdapterType(ctx, providerID)
	if err != nil {
		h.logger.Error("get provider adapter_type", "provider_id", providerID, "error", err)
		return internalServerError(c, "Internal server error")
	}
	if !exists {
		return c.JSON(http.StatusNotFound, errJSON("Provider not found", "not_found", providerID))
	}

	if err := validateProviderConfigKnobs(adapter, body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON(err.Error(), "validation_error", "adapter_mismatch"))
	}

	a := actorFromContext(c)
	if err := h.cache.PutCacheProviderConfig(ctx, providerID, body, a.UserID); err != nil {
		h.logger.Error("put cache_provider_config", "provider_id", providerID, "error", err)
		return internalServerError(c, "Internal server error")
	}

	if err := h.propagateCacheConfig(ctx, a.UserID, a.Name); err != nil {
		h.logger.Error("notify hub cache (provider PUT)", "provider_id", providerID, "error", err)
		return hub.RespondPropagationFailure(c, err)
	}

	ae := audit.EntryFor(c, iam.ResourcePromptCache, iam.VerbUpdate)
	ae.EntityID = "provider:" + providerID
	ae.AfterState = body
	h.audit.LogObserved(ctx, ae)

	return c.JSON(http.StatusOK, body)
}

func (h *Handler) CacheDeleteProvider(c echo.Context) error {
	providerID := c.Param("provider_id")
	ctx := c.Request().Context()

	if err := h.cache.DeleteCacheProviderConfig(ctx, providerID); err != nil {
		h.logger.Error("delete cache_provider_config", "provider_id", providerID, "error", err)
		return internalServerError(c, "Internal server error")
	}

	a := actorFromContext(c)
	if err := h.propagateCacheConfig(ctx, a.UserID, a.Name); err != nil {
		h.logger.Error("notify hub cache (provider DELETE)", "provider_id", providerID, "error", err)
		return hub.RespondPropagationFailure(c, err)
	}

	ae := audit.EntryFor(c, iam.ResourcePromptCache, iam.VerbUpdate)
	ae.EntityID = "provider:" + providerID
	ae.AfterState = map[string]any{"deleted": true}
	h.audit.LogObserved(ctx, ae)

	return c.NoContent(http.StatusNoContent)
}

type effectiveResponse struct {
	ProviderID   string                              `json:"provider_id"`
	ProviderName string                              `json:"provider_name"`
	AdapterType  string                              `json:"adapter_type"`
	Effective    map[string]any                      `json:"effective"`
	Sources      map[string]cacheconfig.Source       `json:"sources"`
	Rules        map[string]cacheconfig.RuleOverride `json:"rules,omitempty"`
}

func (h *Handler) CacheGetEffective(c echo.Context) error {
	providerID := c.QueryParam("provider_id")
	if providerID == "" {
		return c.JSON(http.StatusBadRequest, errJSON("provider_id is required", "validation_error", ""))
	}
	ctx := c.Request().Context()

	adapter, exists, err := h.cache.GetProviderAdapterType(ctx, providerID)
	if err != nil {
		h.logger.Error("get provider adapter_type for effective", "provider_id", providerID, "error", err)
		return internalServerError(c, "Internal server error")
	}
	if !exists {
		return c.JSON(http.StatusNotFound, errJSON("Provider not found", "not_found", providerID))
	}
	name, err := h.cache.GetProviderName(ctx, providerID)
	if err != nil {
		h.logger.Error("get provider name", "provider_id", providerID, "error", err)
	}

	blob, err := h.cache.AssembleCacheConfigBlob(ctx)
	if err != nil {
		h.logger.Error("assemble cache blob", "error", err)
		return internalServerError(c, "Internal server error")
	}
	eff := cacheconfig.Resolve(blob, providerID, adapter)

	effectiveMap := map[string]any{
		"normaliser_enabled":        eff.NormaliserEnabled,
		"cache_master_kill_switch":  eff.CacheMasterKillSwitch,
		"marker_inject_enabled":     eff.MarkerInjectEnabled,
		"marker_boundary3_enabled":  eff.MarkerBoundary3Enabled,
		"cache_enabled":             eff.CacheEnabled,
		"min_system_chars":          eff.MinSystemChars,
		"ttl_seconds":               eff.TTLSeconds,
		"circuit_breaker_threshold": eff.CircuitBreakerThreshold,
		"circuit_breaker_open_secs": eff.CircuitBreakerOpenSecs,
	}
	return c.JSON(http.StatusOK, effectiveResponse{
		ProviderID:   providerID,
		ProviderName: name,
		AdapterType:  adapter,
		Effective:    effectiveMap,
		Sources:      eff.Sources,
		Rules:        eff.RuleOverrides,
	})
}

type overrideRow struct {
	ProviderID     string                  `json:"provider_id"`
	ProviderName   string                  `json:"provider_name"`
	AdapterType    string                  `json:"adapter_type"`
	OverriddenKeys []string                `json:"overridden_keys"`
	Diff           map[string]overrideDiff `json:"diff"`
	UpdatedAt      string                  `json:"updated_at,omitempty"`
	UpdatedBy      string                  `json:"updated_by,omitempty"`
}

type overrideDiff struct {
	Inherited       any                `json:"inherited"`
	Override        any                `json:"override"`
	InheritedSource cacheconfig.Source `json:"inherited_source"`
}

func (h *Handler) CacheListOverrides(c echo.Context) error {
	ctx := c.Request().Context()

	blob, err := h.cache.AssembleCacheConfigBlob(ctx)
	if err != nil {
		h.logger.Error("assemble cache blob (overrides)", "error", err)
		return internalServerError(c, "Internal server error")
	}

	rows := make([]overrideRow, 0, len(blob.Providers))
	for providerID, override := range blob.Providers {
		if isProviderConfigEmpty(override) {
			continue
		}
		adapter, exists, err := h.cache.GetProviderAdapterType(ctx, providerID)
		if err != nil || !exists {
			// Orphaned override row (Provider deleted but FK CASCADE pending) —
			// skip silently. CASCADE will normally have already cleaned up.
			continue
		}
		name, _ := h.cache.GetProviderName(ctx, providerID)
		eff := cacheconfig.Resolve(blob, providerID, adapter)

		// Compute diff: for each non-nil field in `override`, record the
		// inherited value (resolved without override) and the override value.
		blobNoOverride := blob
		delete(blobNoOverride.Providers, providerID) // shallow ok; we don't write back
		baseline := cacheconfig.Resolve(blobNoOverride, providerID, adapter)

		diff := map[string]overrideDiff{}
		keys := []string{}
		recordDiff := func(name string, hasOverride bool, override, inherited any, src cacheconfig.Source) {
			if !hasOverride {
				return
			}
			diff[name] = overrideDiff{Inherited: inherited, Override: override, InheritedSource: src}
			keys = append(keys, name)
		}
		recordDiff("marker_inject_enabled", override.MarkerInjectEnabled != nil, eff.MarkerInjectEnabled, baseline.MarkerInjectEnabled, baseline.Sources["marker_inject_enabled"])
		recordDiff("marker_boundary3_enabled", override.MarkerBoundary3Enabled != nil, eff.MarkerBoundary3Enabled, baseline.MarkerBoundary3Enabled, baseline.Sources["marker_boundary3_enabled"])
		recordDiff("cache_enabled", override.CacheEnabled != nil, eff.CacheEnabled, baseline.CacheEnabled, baseline.Sources["cache_enabled"])
		recordDiff("min_system_chars", override.MinSystemChars != nil, eff.MinSystemChars, baseline.MinSystemChars, baseline.Sources["min_system_chars"])
		recordDiff("ttl_seconds", override.TTLSeconds != nil, eff.TTLSeconds, baseline.TTLSeconds, baseline.Sources["ttl_seconds"])
		recordDiff("circuit_breaker_threshold", override.CircuitBreakerThreshold != nil, eff.CircuitBreakerThreshold, baseline.CircuitBreakerThreshold, baseline.Sources["circuit_breaker_threshold"])
		recordDiff("circuit_breaker_open_secs", override.CircuitBreakerOpenSecs != nil, eff.CircuitBreakerOpenSecs, baseline.CircuitBreakerOpenSecs, baseline.Sources["circuit_breaker_open_secs"])
		sort.Strings(keys)

		rows = append(rows, overrideRow{
			ProviderID:     providerID,
			ProviderName:   name,
			AdapterType:    adapter,
			OverriddenKeys: keys,
			Diff:           diff,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ProviderName < rows[j].ProviderName })

	return c.JSON(http.StatusOK, map[string]any{
		"items": rows,
		"total": len(rows),
	})
}

// isProviderConfigEmpty returns true when no fields are set (all pointers nil).
func isProviderConfigEmpty(p cacheconfig.ProviderConfig) bool {
	return p.MarkerInjectEnabled == nil && p.MarkerBoundary3Enabled == nil &&
		p.CacheEnabled == nil && p.MinSystemChars == nil &&
		p.TTLSeconds == nil && p.CircuitBreakerThreshold == nil &&
		p.CircuitBreakerOpenSecs == nil
}
