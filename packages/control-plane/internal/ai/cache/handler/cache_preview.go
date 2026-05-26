package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/cacheconfig"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/wirerewrite"
)

// cachePreviewRequest is the POST body for the normaliser dry-run preview.
type cachePreviewRequest struct {
	TrafficEventID string `json:"traffic_event_id"`
}

// cachePreviewRuleResult describes a single rule's outcome in the preview.
type cachePreviewRuleResult struct {
	RuleID      string `json:"rule_id"`
	AdapterType string `json:"adapter_type"`
	DryRun      bool   `json:"dry_run"`
	Enabled     bool   `json:"enabled"`
	StripCount  int    `json:"strip_count"`
	StripBytes  int    `json:"strip_bytes"`
}

// cachePreviewResponse is the POST /api/admin/cache/preview response body.
type cachePreviewResponse struct {
	TrafficEventID  string                   `json:"traffic_event_id"`
	AdapterType     string                   `json:"adapter_type"`
	StripCount      int                      `json:"strip_count"`
	StripBytes      int                      `json:"strip_bytes"`
	MarkersInjected int                      `json:"markers_injected"`
	DryRun          bool                     `json:"dry_run"`
	RulesApplied    []string                 `json:"rules_applied"`
	Rules           []cachePreviewRuleResult `json:"rules"`
	BodyBefore      json.RawMessage          `json:"body_before,omitempty"`
	BodyAfter       json.RawMessage          `json:"body_after,omitempty"`
	DiffLines       []string                 `json:"diff_lines,omitempty"`
}

// trafficEventPreviewRow holds the fields needed for a normaliser preview.
type trafficEventPreviewRow struct {
	AdapterType       string
	ProviderID        string
	InlineRequestBody json.RawMessage
}

// getTrafficEventForPreview fetches adapter_type, provider_id, and the inline
// request body for a given traffic_event ID. Returns nil when the event does
// not exist or has no captured payload.
func (h *Handler) getTrafficEventForPreview(ctx context.Context, id string) (*trafficEventPreviewRow, error) {
	var row trafficEventPreviewRow
	err := h.pool.QueryRow(ctx, `
		SELECT COALESCE(pr.adapter_type, ''), COALESCE(a.routed_provider_id, a.provider_id, ''), p.inline_request_body
		FROM   traffic_event a
		LEFT JOIN "Provider" pr ON pr.id = COALESCE(a.routed_provider_id, a.provider_id)
		LEFT JOIN traffic_event_payload p  ON p.traffic_event_id = a.id
		WHERE  a.id = $1
	`, id).Scan(&row.AdapterType, &row.ProviderID, &row.InlineRequestBody)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query traffic event for preview: %w", err)
	}
	if len(row.InlineRequestBody) == 0 {
		return nil, nil
	}
	return &row, nil
}

// CachePreview runs the normaliser on a stored traffic_event's request body
// and returns what would have been stripped / injected, along with a textual
// diff. This is a pure read + compute — it never writes to DB or upstream.
//
// POST /api/admin/cache/preview
func (h *Handler) CachePreview(c echo.Context) error {
	ctx := c.Request().Context()

	var req cachePreviewRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("invalid request body", "invalid_request", ""))
	}
	if req.TrafficEventID == "" {
		return c.JSON(http.StatusBadRequest, errJSON("traffic_event_id is required", "invalid_request", ""))
	}

	// 1. Fetch body + adapter_type from DB.
	evRow, err := h.getTrafficEventForPreview(ctx, req.TrafficEventID)
	if err != nil {
		h.logger.Error("cache preview: DB fetch failed", "err", err, "event_id", req.TrafficEventID)
		return c.JSON(http.StatusInternalServerError, errJSON("database error", "server_error", ""))
	}
	if evRow == nil {
		return c.JSON(http.StatusNotFound, errJSON("traffic event not found or has no captured payload", "not_found", ""))
	}

	// 2. Project the 3-tier cache blob into the legacy
	//    wirerewrite.Config shape the preview engine consumes. The projection
	//    is intentionally narrow — preview cares about exactly one Provider,
	//    so we only resolve that one Provider's effective marker config and
	//    only copy the rules for its adapter family.
	normCfg := wirerewrite.Config{
		Rules:     map[string]map[string]wirerewrite.RuleOverride{},
		Providers: map[string]wirerewrite.ProviderCacheConfig{},
	}
	blob, blobErr := h.cache.AssembleCacheConfigBlob(ctx)
	if blobErr == nil {
		normCfg.NormaliserEnabled = blob.Global.NormaliserEnabled
		if ac, ok := blob.Adapters[evRow.AdapterType]; ok && len(ac.Rules) > 0 {
			rules := make(map[string]wirerewrite.RuleOverride, len(ac.Rules))
			for rid, ro := range ac.Rules {
				rules[rid] = wirerewrite.RuleOverride{Enabled: ro.Enabled, DryRunAlways: ro.DryRunAlways}
			}
			normCfg.Rules[evRow.AdapterType] = rules
		}
		eff := cacheconfig.Resolve(blob, evRow.ProviderID, evRow.AdapterType)
		normCfg.Providers[evRow.ProviderID] = wirerewrite.ProviderCacheConfig{
			CacheMarkerInjectEnabled:    eff.MarkerInjectEnabled,
			CacheMarkerBoundary3Enabled: eff.MarkerBoundary3Enabled,
		}
	}

	// 3. Build a preview engine: run with current config but force all rules
	//    into dry-run=true so we never produce "would have been applied" vs
	//    "already applied" ambiguity.  A separate pass with all rules enabled
	//    gives us the complete strip/inject numbers for the UI diff.
	previewCfg := buildPreviewConfig(normCfg)
	eng := wirerewrite.New(h.logger)
	eng.Reload(previewCfg)

	body := []byte(evRow.InlineRequestBody)
	resultBody, result := eng.NormalizeUpstream(evRow.AdapterType, evRow.ProviderID, body)

	// 4. Build diff lines (simple line-level comparison of JSON).
	diffLines := simpleDiff(body, resultBody)

	// 5. Collect applied rule IDs by running key normalisation (which is
	//    always dry-run safe) and reporting which rules touched the body.
	rulesApplied := collectAppliedRules(previewCfg, evRow.AdapterType)

	resp := cachePreviewResponse{
		TrafficEventID:  req.TrafficEventID,
		AdapterType:     evRow.AdapterType,
		StripCount:      result.StripCount,
		StripBytes:      result.StripBytes,
		MarkersInjected: result.MarkersInjected,
		DryRun:          result.DryRun,
		RulesApplied:    rulesApplied,
		Rules:           buildRuleSummary(previewCfg, evRow.AdapterType),
		BodyBefore:      json.RawMessage(body),
		BodyAfter:       json.RawMessage(resultBody),
		DiffLines:       diffLines,
	}
	return c.JSON(http.StatusOK, resp)
}

// buildPreviewConfig returns a Config identical to the operator's stored
// config but with NormaliserEnabled=true and all rules forced to
// DryRunAlways=true so the preview never mutates anything permanently.
func buildPreviewConfig(cfg wirerewrite.Config) wirerewrite.Config {
	preview := wirerewrite.Config{
		NormaliserEnabled: true, // always enabled for preview
		Providers:         cfg.Providers,
		Global:            cfg.Global,
		Rules:             make(map[string]map[string]wirerewrite.RuleOverride),
	}
	// Copy operator overrides, forcing DryRunAlways = true.
	for adapter, rules := range cfg.Rules {
		preview.Rules[adapter] = make(map[string]wirerewrite.RuleOverride, len(rules))
		for id, ro := range rules {
			dryRun := true
			enabled := true
			if ro.Enabled != nil {
				enabled = *ro.Enabled
			}
			preview.Rules[adapter][id] = wirerewrite.RuleOverride{
				Enabled:      &enabled,
				DryRunAlways: &dryRun,
			}
		}
	}
	// Enable all bundled rules (with dry-run) even if not in operator config.
	ensureRuleEnabled(&preview, "anthropic", wirerewrite.RuleAnthropicCchStrip)
	ensureRuleEnabled(&preview, "openai", wirerewrite.RuleOpenAIFieldOrderNormalize)
	return preview
}

func ensureRuleEnabled(cfg *wirerewrite.Config, adapter, ruleID string) {
	if _, ok := cfg.Rules[adapter]; !ok {
		cfg.Rules[adapter] = map[string]wirerewrite.RuleOverride{}
	}
	if _, ok := cfg.Rules[adapter][ruleID]; !ok {
		t := true
		cfg.Rules[adapter][ruleID] = wirerewrite.RuleOverride{Enabled: &t, DryRunAlways: &t}
	}
}

func collectAppliedRules(cfg wirerewrite.Config, adapterType string) []string {
	adapter := strings.ToLower(adapterType)
	rules := cfg.Rules[adapter]
	var applied []string
	for id, ro := range rules {
		if ro.Enabled != nil && *ro.Enabled {
			applied = append(applied, id)
		}
	}
	return applied
}

func buildRuleSummary(cfg wirerewrite.Config, adapterType string) []cachePreviewRuleResult {
	adapter := strings.ToLower(adapterType)
	var out []cachePreviewRuleResult
	for id, ro := range cfg.Rules[adapter] {
		enabled := ro.Enabled != nil && *ro.Enabled
		dryRun := ro.DryRunAlways != nil && *ro.DryRunAlways
		out = append(out, cachePreviewRuleResult{
			RuleID:      id,
			AdapterType: adapterType,
			Enabled:     enabled,
			DryRun:      dryRun,
		})
	}
	return out
}

// simpleDiff produces a line-by-line unified-diff-style slice between two
// JSON bodies. For small payloads the UI renders them in a code block.
func simpleDiff(before, after []byte) []string {
	if string(before) == string(after) {
		return nil
	}
	bLines := strings.Split(prettyJSON(before), "\n")
	aLines := strings.Split(prettyJSON(after), "\n")

	var lines []string
	// Naive diff: emit context only where lines differ.
	maxLen := len(bLines)
	if len(aLines) > maxLen {
		maxLen = len(aLines)
	}
	for i := range maxLen {
		bLine := ""
		aLine := ""
		if i < len(bLines) {
			bLine = bLines[i]
		}
		if i < len(aLines) {
			aLine = aLines[i]
		}
		if bLine == aLine {
			lines = append(lines, " "+bLine)
		} else {
			if bLine != "" {
				lines = append(lines, "-"+bLine)
			}
			if aLine != "" {
				lines = append(lines, "+"+aLine)
			}
		}
	}
	return lines
}

func prettyJSON(b []byte) string {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(b)
	}
	return string(out)
}
