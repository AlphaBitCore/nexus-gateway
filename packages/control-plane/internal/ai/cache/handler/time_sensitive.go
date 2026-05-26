// Package cache: time_sensitive.go — admin API for the fleet-wide
// time-sensitive freshness rule list.
//
// Endpoints (mounted under /api/admin/):
//
//	GET    /cache/time-sensitive-patterns        — list the active rule set
//	PUT    /cache/time-sensitive-patterns/:id    — edit a rule (enable/disable/edit)
//	POST   /cache/time-sensitive-patterns        — add a new rule
//	DELETE /cache/time-sensitive-patterns/:id    — remove a rule
//	POST   /cache/time-sensitive-patterns/test   — dry-run a prompt against the list
//
// Persistence: the full rule set is stored in
// semantic_cache_config.time_sensitive_overrides (JSONB). seed.ts populates
// the default rules on a fresh DB
// (tools/db-migrate/seed/data/time-sensitive-rules.json). Every mutating
// endpoint reads, mutates, writes back, and pushes the full list to the Hub
// shadow under configkey ResponseCacheTimeSensitivePatterns. There is no
// Go-side default list — if seed didn't run, the rule list is empty and the
// freshness gate is off (the correct behaviour).
//
// IAM: iam.ResourceSemanticCache.{Read, Update}

package cache

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
)

// TimeSensitivePattern mirrors freshness.Rule from the ai-gateway package.
// Duplicated here to avoid a reverse dependency from control-plane → ai-gateway.
// The two structs must stay in sync with the json tags.
type TimeSensitivePattern struct {
	ID                  string   `json:"id"`
	Keywords            []string `json:"keywords"`
	RequireQuestionMark bool     `json:"requireQuestionMark"`
	RequireEntity       bool     `json:"requireEntity"`
	Languages           []string `json:"languages"`
	Enabled             bool     `json:"enabled"`
}

// TimeSensitiveStore is the narrow DB surface the time-sensitive handler needs.
// Satisfied by *configstore.SemanticCacheStore in production; in-memory doubles
// satisfy it in tests.
type TimeSensitiveStore interface {
	GetOverrides(ctx context.Context) (configstore.TimeSensitiveOverridesBlob, error)
	SaveOverrides(ctx context.Context, blob configstore.TimeSensitiveOverridesBlob) error
}

// timeSensitivePatternsResponse is the GET response body.
type timeSensitivePatternsResponse struct {
	Patterns []TimeSensitivePattern `json:"patterns"`
	Source   string                 `json:"source"` // "seed" | "shadow" | "db"
}

// timeSensitiveTestRequest is the POST /test body.
type timeSensitiveTestRequest struct {
	Prompt   string `json:"prompt"`
	Language string `json:"language"`
}

// timeSensitiveTestResponse is the POST /test response body.
type timeSensitiveTestResponse struct {
	Decision        string   `json:"decision"` // "match" | "no_match"
	MatchedRuleID   *string  `json:"matchedRuleId"`
	MatchedKeywords []string `json:"matchedKeywords"`
}

// RegisterTimeSensitiveRoutes mounts the five time-sensitive pattern endpoints
// under the caller-supplied admin group, IAM-gated on the SemanticCache resource.
func (h *Handler) RegisterTimeSensitiveRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/cache/time-sensitive-patterns", h.GetTimeSensitivePatterns, iamMW(iam.ResourceSemanticCache.Action(iam.VerbRead)))
	g.POST("/cache/time-sensitive-patterns/test", h.TestTimeSensitivePattern, iamMW(iam.ResourceSemanticCache.Action(iam.VerbRead)))
	g.PUT("/cache/time-sensitive-patterns/:id", h.PutTimeSensitivePattern, iamMW(iam.ResourceSemanticCache.Action(iam.VerbUpdate)))
	g.POST("/cache/time-sensitive-patterns", h.PostTimeSensitivePattern, iamMW(iam.ResourceSemanticCache.Action(iam.VerbUpdate)))
	g.DELETE("/cache/time-sensitive-patterns/:id", h.DeleteTimeSensitivePattern, iamMW(iam.ResourceSemanticCache.Action(iam.VerbUpdate)))
}

// GetTimeSensitivePatterns returns the rule list as persisted in the DB
// blob. Defaults are seeded by tools/db-migrate/seed/seed.ts on a fresh
// install; an empty list (seed did not run, or admin deleted everything)
// returns an empty patterns array and the freshness gate is effectively off.
func (h *Handler) GetTimeSensitivePatterns(c echo.Context) error {
	patterns, err := h.loadPatterns(c.Request().Context())
	if err != nil {
		h.logger.Error("time-sensitive: get patterns", "error", err)
		return internalServerError(c, "failed to load patterns: "+err.Error())
	}
	return c.JSON(http.StatusOK, timeSensitivePatternsResponse{
		Patterns: patterns,
		Source:   "db",
	})
}

// PutTimeSensitivePattern updates a single rule in the list (enable/disable/edit).
// Operates directly on the DB blob; pushes the full rule list to Hub.
func (h *Handler) PutTimeSensitivePattern(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("rule id is required", "validation_error", ""))
	}

	var req TimeSensitivePattern
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("malformed request body: "+err.Error(), "validation_error", ""))
	}
	req.ID = id // force ID from path to prevent body/path mismatch

	blob, err := h.loadOverrides(c.Request().Context())
	if err != nil {
		return internalServerError(c, "failed to load patterns: "+err.Error())
	}

	found := false
	for i, r := range blob.Rules {
		if r.ID == id {
			blob.Rules[i] = patternToOverrideRule(req)
			found = true
			break
		}
	}
	if !found {
		return c.JSON(http.StatusNotFound, errJSON("rule id not found", "not_found", ""))
	}

	if err := h.saveOverrides(c.Request().Context(), blob); err != nil {
		return internalServerError(c, "failed to save: "+err.Error())
	}

	patterns := blobToPatterns(blob)
	if err := h.pushTimeSensitiveToHub(c, patterns); err != nil {
		return internalServerError(c, "failed to push rules to Hub: "+err.Error())
	}
	return c.JSON(http.StatusOK, timeSensitivePatternsResponse{Patterns: patterns, Source: "db"})
}

// PostTimeSensitivePattern adds a new rule to the list. Rejects duplicate IDs;
// admin should use PUT to modify an existing rule (including a default that
// matches by ID, since defaults are no longer a separate namespace).
func (h *Handler) PostTimeSensitivePattern(c echo.Context) error {
	var req TimeSensitivePattern
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("malformed request body: "+err.Error(), "validation_error", ""))
	}
	if req.ID == "" {
		return c.JSON(http.StatusBadRequest, errJSON("rule id is required", "validation_error", ""))
	}
	if len(req.Keywords) == 0 {
		return c.JSON(http.StatusBadRequest, errJSON("at least one keyword is required", "validation_error", ""))
	}

	blob, err := h.loadOverrides(c.Request().Context())
	if err != nil {
		return internalServerError(c, "failed to load patterns: "+err.Error())
	}

	for _, r := range blob.Rules {
		if r.ID == req.ID {
			return c.JSON(http.StatusConflict, errJSON("rule id already exists; use PUT to update it", "conflict", ""))
		}
	}

	blob.Rules = append(blob.Rules, patternToOverrideRule(req))
	if err := h.saveOverrides(c.Request().Context(), blob); err != nil {
		return internalServerError(c, "failed to save: "+err.Error())
	}

	patterns := blobToPatterns(blob)
	if err := h.pushTimeSensitiveToHub(c, patterns); err != nil {
		return internalServerError(c, "failed to push rules to Hub: "+err.Error())
	}
	return c.JSON(http.StatusCreated, req)
}

// DeleteTimeSensitivePattern removes a rule from the list. Any rule may be
// deleted — defaults live in the DB blob (written by seed.ts), not in Go
// code, so deletion is a normal DB row removal. Admin who later regrets
// removing a default can recreate it via POST with the same ID + keywords,
// or re-run seed (which is idempotent and skips when rules already exist).
func (h *Handler) DeleteTimeSensitivePattern(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("rule id is required", "validation_error", ""))
	}

	blob, err := h.loadOverrides(c.Request().Context())
	if err != nil {
		return internalServerError(c, "failed to load patterns: "+err.Error())
	}

	found := false
	out := make([]configstore.TimeSensitiveOverrideRule, 0, len(blob.Rules))
	for _, r := range blob.Rules {
		if r.ID == id {
			found = true
			continue
		}
		out = append(out, r)
	}
	if !found {
		return c.JSON(http.StatusNotFound, errJSON("rule id not found", "not_found", ""))
	}
	blob.Rules = out

	if err := h.saveOverrides(c.Request().Context(), blob); err != nil {
		return internalServerError(c, "failed to save: "+err.Error())
	}

	patterns := blobToPatterns(blob)
	if err := h.pushTimeSensitiveToHub(c, patterns); err != nil {
		return internalServerError(c, "failed to push rules to Hub: "+err.Error())
	}
	return c.JSON(http.StatusOK, timeSensitivePatternsResponse{Patterns: patterns, Source: "db"})
}

// TestTimeSensitivePattern runs a dry-run detection on the provided prompt
// against the merged active rule list. Returns which rule (if any) would fire.
func (h *Handler) TestTimeSensitivePattern(c echo.Context) error {
	var req timeSensitiveTestRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("malformed request body: "+err.Error(), "validation_error", ""))
	}
	if req.Prompt == "" {
		return c.JSON(http.StatusBadRequest, errJSON("prompt is required", "validation_error", ""))
	}

	patterns, err := h.loadPatterns(c.Request().Context())
	if err != nil {
		h.logger.Error("time-sensitive: test: load patterns", "error", err)
		return internalServerError(c, "failed to load patterns: "+err.Error())
	}

	result := detectPrompt(req.Prompt, patterns)
	return c.JSON(http.StatusOK, result)
}

// --- Store helpers -----------------------------------------------------------

// loadOverrides reads the DB overrides blob. Returns empty blob on nil store.
func (h *Handler) loadOverrides(ctx context.Context) (configstore.TimeSensitiveOverridesBlob, error) {
	if h.tsStore == nil {
		return configstore.TimeSensitiveOverridesBlob{}, nil
	}
	return h.tsStore.GetOverrides(ctx)
}

// saveOverrides writes the DB overrides blob. No-op on nil store.
func (h *Handler) saveOverrides(ctx context.Context, blob configstore.TimeSensitiveOverridesBlob) error {
	if h.tsStore == nil {
		return nil
	}
	return h.tsStore.SaveOverrides(ctx, blob)
}

// loadPatterns is the read path for both GET and the test endpoint.
// Returns whatever is persisted; an empty list is a legitimate state
// (seed didn't run, or admin disabled everything).
func (h *Handler) loadPatterns(ctx context.Context) ([]TimeSensitivePattern, error) {
	blob, err := h.loadOverrides(ctx)
	if err != nil {
		return nil, err
	}
	return blobToPatterns(blob), nil
}

// blobToPatterns converts the store-side rule list to the wire shape.
func blobToPatterns(blob configstore.TimeSensitiveOverridesBlob) []TimeSensitivePattern {
	out := make([]TimeSensitivePattern, 0, len(blob.Rules))
	for _, r := range blob.Rules {
		out = append(out, overrideRuleToPattern(r))
	}
	return out
}

// --- Hub helper --------------------------------------------------------------

// pushTimeSensitiveToHub writes the full rule list to the Hub shadow under
// ResponseCacheTimeSensitivePatterns. The AI Gateway's
// registerAGTimeSensitivePatterns handler expects the shadow `state` field
// to decode directly into `[]freshness.Rule`, so we MUST pass the slice
// itself — not pre-marshalled bytes. Passing `[]byte` here would cause
// hub.NotifyConfigChange's `json.Marshal(payload)` to base64-encode the
// bytes into a JSON string, and the AIGw `json.Unmarshal(raw, &rules)`
// then fails with "cannot unmarshal string into Go value of type
// []freshness.Rule". On Hub nil (dev/test mode) this is a no-op.
func (h *Handler) pushTimeSensitiveToHub(c echo.Context, rules []TimeSensitivePattern) error {
	if h.hub == nil {
		return nil
	}
	act := actorFromContext(c)
	_, err := h.hub.NotifyConfigChange(c.Request().Context(), hub.ConfigChangeRequest{
		ThingType: "ai-gateway",
		ConfigKey: configkey.ResponseCacheTimeSensitivePatterns,
		State:     rules,
		ActorID:   act.UserID,
		ActorName: act.Name,
	})
	return err
}

// --- Override blob helpers ---------------------------------------------------

// patternToOverrideRule converts a TimeSensitivePattern to the store's
// TimeSensitiveOverrideRule type.
func patternToOverrideRule(p TimeSensitivePattern) configstore.TimeSensitiveOverrideRule {
	return configstore.TimeSensitiveOverrideRule{
		ID:                  p.ID,
		Keywords:            p.Keywords,
		RequireQuestionMark: p.RequireQuestionMark,
		RequireEntity:       p.RequireEntity,
		Languages:           p.Languages,
		Enabled:             p.Enabled,
	}
}

// overrideRuleToPattern converts a TimeSensitiveOverrideRule to the handler's
// TimeSensitivePattern type.
func overrideRuleToPattern(r configstore.TimeSensitiveOverrideRule) TimeSensitivePattern {
	return TimeSensitivePattern{
		ID:                  r.ID,
		Keywords:            r.Keywords,
		RequireQuestionMark: r.RequireQuestionMark,
		RequireEntity:       r.RequireEntity,
		Languages:           r.Languages,
		Enabled:             r.Enabled,
	}
}

// upsertOverrideRule adds or replaces the override rule with the same ID.
func upsertOverrideRule(blob configstore.TimeSensitiveOverridesBlob, rule configstore.TimeSensitiveOverrideRule) configstore.TimeSensitiveOverridesBlob {
	out := configstore.TimeSensitiveOverridesBlob{Rules: make([]configstore.TimeSensitiveOverrideRule, 0, len(blob.Rules)+1)}
	found := false
	for _, r := range blob.Rules {
		if r.ID == rule.ID {
			out.Rules = append(out.Rules, rule)
			found = true
		} else {
			out.Rules = append(out.Rules, r)
		}
	}
	if !found {
		out.Rules = append(out.Rules, rule)
	}
	return out
}

// removeOverrideRule removes the rule with the given ID from the blob.
func removeOverrideRule(blob configstore.TimeSensitiveOverridesBlob, id string) configstore.TimeSensitiveOverridesBlob {
	out := configstore.TimeSensitiveOverridesBlob{Rules: make([]configstore.TimeSensitiveOverrideRule, 0, len(blob.Rules))}
	for _, r := range blob.Rules {
		if r.ID != id {
			out.Rules = append(out.Rules, r)
		}
	}
	return out
}

// --- Detection helpers (mirror of ai-gateway/internal/cache/freshness logic,
// extracted here to avoid importing the ai-gateway package) ---

// containsKeyword reports true if text contains kw case-insensitively.
func containsKeyword(text, kw string) bool {
	tl := toLower(text)
	return contains(tl, toLower(kw))
}

// detectPrompt runs the rule list against prompt and returns a test result.
func detectPrompt(prompt string, rules []TimeSensitivePattern) timeSensitiveTestResponse {
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		matched, kws := ruleMatches(r, prompt)
		if matched {
			id := r.ID
			return timeSensitiveTestResponse{
				Decision:        "match",
				MatchedRuleID:   &id,
				MatchedKeywords: kws,
			}
		}
	}
	return timeSensitiveTestResponse{Decision: "no_match", MatchedKeywords: []string{}}
}

// ruleMatches returns whether the rule fires for the given text, plus the
// matched keywords.
func ruleMatches(r TimeSensitivePattern, text string) (bool, []string) {
	var matched []string
	for _, kw := range r.Keywords {
		if containsKeyword(text, kw) {
			matched = append(matched, kw)
		}
	}
	if len(matched) == 0 {
		return false, nil
	}
	if r.RequireQuestionMark {
		if !contains(text, "?") && !contains(text, "？") {
			return false, nil
		}
	}
	if r.RequireEntity {
		if !entityHeuristicCP(text) {
			return false, nil
		}
	}
	return true, matched
}

// toLower is a copy-free case normaliser for ASCII range (sufficient for keyword matching).
func toLower(s string) string {
	var b []byte
	for i := range len(s) {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if b == nil {
				b = []byte(s)
			}
			b[i] = c + 32
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}

// contains is a plain substring check without allocation.
func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && findSub(s, sub)
}

func findSub(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// entityHeuristicCP is a simplified entity detector for the test endpoint
// (mirrors freshness.entityHeuristic from the ai-gateway package).
func entityHeuristicCP(text string) bool {
	currencySymbols := map[rune]bool{'$': true, '€': true, '£': true, '¥': true, '₩': true, '₹': true, '₿': true}
	zhWords := []string{"元", "美元", "欧元", "港元", "日元", "英镑", "人民币"}
	for _, r := range text {
		if currencySymbols[r] {
			return true
		}
	}
	for _, w := range zhWords {
		if contains(text, w) {
			return true
		}
	}
	digitRun := 0
	upperRun := 0
	for _, r := range text {
		switch {
		case r >= '0' && r <= '9':
			digitRun++
			if digitRun >= 2 {
				return true
			}
			upperRun = 0
		case r >= 'A' && r <= 'Z':
			upperRun++
			digitRun = 0
		default:
			if upperRun >= 2 {
				return true
			}
			upperRun = 0
			digitRun = 0
		}
	}
	return upperRun >= 2
}
