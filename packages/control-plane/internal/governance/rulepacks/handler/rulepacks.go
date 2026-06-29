// Package handler: admin_rulepacks.go — Rule Pack admin API surface.
//
// Endpoints (all under /api/admin):
//
//	GET    /rule-packs                 → list pack metadata
//	GET    /rule-packs/:id             → pack + rules
//	POST   /rule-packs/preview         → dry-parse YAML (always 200)
//
// The handler depends only on the narrow RulePackStore seam so unit tests
// can substitute an in-memory double without standing up a real pool.
package rulepacks

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
)

// HubInvalidator is the narrow Hub surface rulepacks/ needs.
type HubInvalidator interface {
	InvalidateConfig(ctx context.Context, thingType, configKey string)
}

// RulePackStore is the persistence seam used by Handler. The
// production implementation is *rulepack.Store backed by pgxpool; tests
// supply an in-memory double. Methods mirror rulepack.Store so the
// satisfaction is structural.
type RulePackStore interface {
	ListPacks(ctx context.Context) ([]rulepack.Pack, error)
	GetPack(ctx context.Context, id string) (*rulepack.Pack, error)
	ImportPack(ctx context.Context, p *rulepack.Pack) (*rulepack.Pack, error)
	UpdatePack(ctx context.Context, packID string, u rulepack.PackUpdate) error
	DeletePack(ctx context.Context, packID string) error
	Install(ctx context.Context, in rulepack.Install) (*rulepack.Install, error)
	UpdateInstall(ctx context.Context, installID string, enabled bool) error
	UpgradeInstallToLatest(ctx context.Context, installID string) (*rulepack.UpgradeResult, error)
	DeleteInstall(ctx context.Context, installID string) error
	ListInstallsForHook(ctx context.Context, hookID string) ([]rulepack.Install, error)
	UpsertOverrides(ctx context.Context, installID string, o []rulepack.Override) error
	LoadForInstall(ctx context.Context, installID string) (*rulepack.EffectiveRuleSet, error)
}

// Handler owns /api/admin/rule-packs/* and /api/admin/hooks/:hookId/rule-packs.
type Handler struct {
	store RulePackStore
	// audit may be nil in tests that do not assert on audit emission;
	// emitAudit nil-guards every call site.
	audit *audit.Writer
	// hub is the same HubInvalidator interface AdminHandler uses to push
	// config changes. Nil in unit tests; production wiring sets it via
	// NewHandler so every mutating endpoint can fan out
	// `hooks` invalidation to the three data-plane Thing types
	// after a successful DB write. Previously omitted from this struct,
	// which made rule-pack edits silently ignored by the running
	// gateway / proxy / agent until restart — the original `pii-hooks
	// not blocking PII` prod bug.
	hub HubInvalidator
}

// NewHandler constructs the handler. store must be non-nil; main.go
// only instantiates the handler once a DB pool is available. auditWriter
// may be nil (unit-test construction); when nil, mutating endpoints skip
// audit emission silently. hub may be nil in tests; production wiring
// passes the AdminHandler.Hub instance so Hub config-sync calls fan out
// on every successful mutation.
func New(store RulePackStore, auditWriter *audit.Writer, hub HubInvalidator) *Handler {
	return &Handler{store: store, audit: auditWriter, hub: hub}
}

// invalidateHookConfig fans `hooks` Cat B invalidation to the three
// data-plane Thing types that subscribe to the key. Hub.UpdateConfig
// broadcasts to one ThingType per call, so we issue three calls. All
// three OnConfigChanged consumers (ai-gateway / compliance-proxy / agent)
// pull the rebuilt hook chain from DB on receipt, including the effective
// rule pack content — that's the path the `pii-hooks` regression went
// missing on prior to this fix.
//
// Fire-and-forget: errors are logged inside InvalidateConfig and never
// surface to the admin operator — the DB write already succeeded; cache
// staleness self-heals on the next Hub config-push tick worst-case.
func (h *Handler) invalidateHookConfig(ctx context.Context) {
	if h.hub == nil {
		return
	}
	h.hub.InvalidateConfig(ctx, "ai-gateway", configkey.Hooks)
	h.hub.InvalidateConfig(ctx, "compliance-proxy", configkey.Hooks)
	h.hub.InvalidateConfig(ctx, "agent", configkey.Hooks)
}

// emitAudit publishes an admin-audit entry on successful mutation. It is
// nil-safe on the writer pointer so test construction stays ergonomic.
func (h *Handler) emitAudit(c echo.Context, e audit.Entry) {
	if h.audit == nil {
		return
	}
	h.audit.LogObserved(c.Request().Context(), e)
}

// RegisterRoutes wires the rule-pack admin routes onto the provided admin group.
func (h *Handler) RegisterRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	rp := g.Group("/rule-packs", iamMW(iam.ResourceRulePack.Action(iam.VerbRead)))
	rp.GET("", h.List)
	rp.GET("/:id", h.Get)
	rp.POST("/:id/dry-run", h.DryRun)
	g.POST("/rule-packs", h.Create, iamMW(iam.ResourceRulePack.Action(iam.VerbCreate)))
	g.POST("/rule-packs/preview", h.Preview, iamMW(iam.ResourceRulePack.Action(iam.VerbUpdate)))
	g.POST("/rule-packs/import", h.Import, iamMW(iam.ResourceRulePack.Action(iam.VerbImport)))
	g.PATCH("/rule-packs/:id", h.Update, iamMW(iam.ResourceRulePack.Action(iam.VerbUpdate)))
	g.DELETE("/rule-packs/:id", h.Delete, iamMW(iam.ResourceRulePack.Action(iam.VerbDelete)))
	g.POST("/hooks/:hookId/rule-packs", h.Install, iamMW(iam.ResourceHook.Action(iam.VerbUpdate)))
	g.GET("/hooks/:hookId/rule-packs", h.ListInstallsForHook, iamMW(iam.ResourceHook.Action(iam.VerbRead)))
	g.PATCH("/rule-pack-installs/:installId", h.PatchInstall, iamMW(iam.ResourceHook.Action(iam.VerbUpdate)))
	g.POST("/rule-pack-installs/:installId/upgrade", h.UpgradeInstall, iamMW(iam.ResourceHook.Action(iam.VerbUpdate)))
	g.DELETE("/rule-pack-installs/:installId", h.UninstallByID, iamMW(iam.ResourceHook.Action(iam.VerbUpdate)))
	g.PATCH("/rule-pack-installs/:installId/overrides", h.UpsertOverrides, iamMW(iam.ResourceHook.Action(iam.VerbUpdate)))
	g.GET("/rule-pack-installs/:installId/effective-rules", h.EffectiveRules, iamMW(iam.ResourceHook.Action(iam.VerbRead)))
}

// List returns every pack metadata row sorted by (name, version desc).
// Returns [] (not null) on empty so the UI can render "no packs" without
// a null-check.
func (h *Handler) List(c echo.Context) error {
	packs, err := h.store.ListPacks(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
	if packs == nil {
		packs = []rulepack.Pack{}
	}
	return c.JSON(http.StatusOK, packs)
}

// Get returns a pack with its full rule list. 404 when the pack does not
// exist; 400 when :id is missing (Echo routing should prevent this but
// the explicit guard makes the contract clear).
func (h *Handler) Get(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "id required"})
	}
	p, err := h.store.GetPack(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]any{"error": "pack not found"})
	}
	return c.JSON(http.StatusOK, p)
}

// Preview dry-parses a YAML pack body without inserting. Always 200 — the
// UI renders {pack, warnings, errors} side-by-side, so a parse failure is
// payload, not transport. errors[] is empty on success.
func (h *Handler) Preview(c echo.Context) error {
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "read body failed"})
	}
	pack, warnings, parseErr := rulepack.LoadYAML(body)
	errs := []string{}
	if parseErr != nil {
		errs = append(errs, parseErr.Error())
	}
	if warnings == nil {
		warnings = []string{}
	}
	return c.JSON(http.StatusOK, map[string]any{
		"pack":     pack,
		"warnings": warnings,
		"errors":   errs,
		// Per-rule Vectorscan-compat + performance advisories (non-blocking);
		// the UI surfaces these so authors can fix slow/divergent patterns
		// before importing.
		"lint": rulepack.LintPack(pack),
	})
}

// Import parses a YAML pack body and inserts it transactionally. Parse
// failures are 400; duplicate (name, version) imports return 409.
func (h *Handler) Import(c echo.Context) error {
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "read body failed"})
	}
	pack, warnings, parseErr := rulepack.LoadYAML(body)
	if parseErr != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{
			"error":  "parse_failed",
			"detail": parseErr.Error(),
		})
	}
	saved, err := h.store.ImportPack(c.Request().Context(), pack)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, rulepack.ErrDuplicatePackVersion) {
			status = http.StatusConflict
		}
		return c.JSON(status, map[string]any{"error": err.Error()})
	}
	if warnings == nil {
		warnings = []string{}
	}

	ae := audit.EntryFor(c, iam.ResourceRulePack, iam.VerbImport)
	ae.EntityID = saved.ID
	ae.AfterState = map[string]any{
		"name":      saved.Name,
		"version":   saved.Version,
		"ruleCount": len(saved.Rules),
	}
	h.emitAudit(c, ae)
	h.invalidateHookConfig(c.Request().Context())

	return c.JSON(http.StatusOK, map[string]any{
		"packId":    saved.ID,
		"ruleCount": len(saved.Rules),
		"warnings":  warnings,
		"lint":      rulepack.LintPack(pack),
	})
}

// DryRun evaluates a caller-provided content string against a stored pack.
func (h *Handler) DryRun(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "id required"})
	}
	pack, err := h.store.GetPack(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]any{"error": "pack not found"})
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "malformed_json", "detail": err.Error()})
	}
	if body.Content == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "content required"})
	}
	matches := rulepack.Evaluate(*pack, pack.Rules, []hookcore.ContentBlock{{Type: "text", Text: body.Content}})
	if matches == nil {
		matches = []rulepack.Match{}
	}
	return c.JSON(http.StatusOK, map[string]any{"matches": matches})
}

// Install binds a specific pack version to a hook.
func (h *Handler) Install(c echo.Context) error {
	hookID := c.Param("hookId")
	if hookID == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "hookId required"})
	}
	var body struct {
		PackID     string `json:"packId"`
		PinVersion string `json:"pinVersion"`
		Enabled    *bool  `json:"enabled"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "malformed_json", "detail": err.Error()})
	}
	if body.PackID == "" || body.PinVersion == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "packId and pinVersion required"})
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	inst, err := h.store.Install(c.Request().Context(), rulepack.Install{
		PackID:      body.PackID,
		PinVersion:  body.PinVersion,
		BoundHookID: hookID,
		Enabled:     enabled,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}

	ae := audit.EntryFor(c, iam.ResourceRulePack, iam.VerbCreate)
	ae.EntityID = inst.ID
	ae.AfterState = map[string]any{
		"packId":     inst.PackID,
		"pinVersion": inst.PinVersion,
		"hookId":     inst.BoundHookID,
		"enabled":    inst.Enabled,
	}
	h.emitAudit(c, ae)
	h.invalidateHookConfig(c.Request().Context())

	return c.JSON(http.StatusOK, inst)
}

// UpsertOverrides saves per-rule override rows for a given install.
func (h *Handler) UpsertOverrides(c echo.Context) error {
	installID := c.Param("installId")
	if installID == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "installId required"})
	}
	var body struct {
		Overrides []rulepack.Override `json:"overrides"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "malformed_json", "detail": err.Error()})
	}
	if err := h.store.UpsertOverrides(c.Request().Context(), installID, body.Overrides); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}

	ae := audit.EntryFor(c, iam.ResourceRulePack, iam.VerbUpdate)
	ae.EntityID = installID
	ae.AfterState = map[string]any{"overrideCount": len(body.Overrides)}
	h.emitAudit(c, ae)
	h.invalidateHookConfig(c.Request().Context())

	return c.JSON(http.StatusOK, map[string]any{
		"installId":      installID,
		"overridesSaved": len(body.Overrides),
	})
}

// Create inserts a Pack authored via the JSON admin form (not a YAML
// upload). The request body mirrors the YAML shape but is validated by
// structural JSON binding. Duplicate (name, version) returns 409.
func (h *Handler) Create(c echo.Context) error {
	var body rulepack.Pack
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "malformed_json", "detail": err.Error()})
	}
	// Route through the SAME validator as YAML Import — compile every pattern,
	// check severity ∈ {hard,soft,warn}, require a category, enforce name/
	// version/maintainer shape. Previously Create checked only non-empty
	// ruleId/pattern/severity and called ImportPack directly, so a typo'd
	// regex authored via the JSON form returned 201 and that rule never fired
	// (the evaluator silently skips uncompilable patterns).
	if len(body.Rules) == 0 {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "at least one rule required"})
	}
	if _, err := rulepack.ValidatePack(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{
			"error":  "validation_failed",
			"detail": err.Error(),
		})
	}
	saved, err := h.store.ImportPack(c.Request().Context(), &body)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, rulepack.ErrDuplicatePackVersion) {
			status = http.StatusConflict
		}
		return c.JSON(status, map[string]any{"error": err.Error()})
	}

	ae := audit.EntryFor(c, iam.ResourceRulePack, iam.VerbCreate)
	ae.EntityID = saved.ID
	ae.AfterState = map[string]any{
		"name":      saved.Name,
		"version":   saved.Version,
		"ruleCount": len(saved.Rules),
	}
	h.emitAudit(c, ae)
	h.invalidateHookConfig(c.Request().Context())

	return c.JSON(http.StatusCreated, saved)
}

// Update applies a partial metadata update and (optionally) a full rule
// replacement to a pack. The request body uses pointer fields so callers
// can distinguish "leave unchanged" (absent) from "clear this field"
// (empty string in the pointed value).
func (h *Handler) Update(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "id required"})
	}
	var body struct {
		Maintainer  *string          `json:"maintainer,omitempty"`
		Description *string          `json:"description,omitempty"`
		Rules       *[]rulepack.Rule `json:"rules,omitempty"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "malformed_json", "detail": err.Error()})
	}
	if body.Maintainer == nil && body.Description == nil && body.Rules == nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "no fields to update"})
	}
	before, _ := h.store.GetPack(c.Request().Context(), id)
	err := h.store.UpdatePack(c.Request().Context(), id, rulepack.PackUpdate{
		Maintainer:  body.Maintainer,
		Description: body.Description,
		Rules:       body.Rules,
	})
	if err != nil {
		if errors.Is(err, rulepack.ErrPackNotFound) {
			return c.JSON(http.StatusNotFound, map[string]any{"error": "pack not found"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
	pack, err := h.store.GetPack(c.Request().Context(), id)

	ae := audit.EntryFor(c, iam.ResourceRulePack, iam.VerbUpdate)
	ae.EntityID = id
	if before != nil {
		ae.BeforeState = rulePackAuditSummary(before)
	}
	if pack != nil {
		ae.AfterState = rulePackAuditSummary(pack)
	}
	h.emitAudit(c, ae)
	h.invalidateHookConfig(c.Request().Context())

	if err != nil {
		return c.JSON(http.StatusOK, map[string]any{"id": id})
	}
	return c.JSON(http.StatusOK, pack)
}

// Delete removes a pack and its rules. Returns 409 when installs still
// reference the pack (the FK violation surfaces as a generic error from
// the store, so we detect it heuristically).
func (h *Handler) Delete(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "id required"})
	}
	before, _ := h.store.GetPack(c.Request().Context(), id)
	err := h.store.DeletePack(c.Request().Context(), id)
	if err != nil {
		if errors.Is(err, rulepack.ErrPackNotFound) {
			return c.JSON(http.StatusNotFound, map[string]any{"error": "pack not found"})
		}
		return c.JSON(http.StatusConflict, map[string]any{"error": err.Error()})
	}

	ae := audit.EntryFor(c, iam.ResourceRulePack, iam.VerbDelete)
	ae.EntityID = id
	if before != nil {
		ae.BeforeState = rulePackAuditSummary(before)
	}
	h.emitAudit(c, ae)
	h.invalidateHookConfig(c.Request().Context())

	return c.JSON(http.StatusOK, map[string]any{"deleted": id})
}

// ListInstallsForHook returns every rule-pack install bound to a hook.
// Disabled installs are included so the UI can render toggle state.
func (h *Handler) ListInstallsForHook(c echo.Context) error {
	hookID := c.Param("hookId")
	if hookID == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "hookId required"})
	}
	installs, err := h.store.ListInstallsForHook(c.Request().Context(), hookID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
	if installs == nil {
		installs = []rulepack.Install{}
	}
	return c.JSON(http.StatusOK, installs)
}

// PatchInstall toggles the `enabled` flag on a rule-pack install. Pin
// version changes require uninstall + reinstall so operators are never
// surprised by a silent binding swap.
func (h *Handler) PatchInstall(c echo.Context) error {
	installID := c.Param("installId")
	if installID == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "installId required"})
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "malformed_json", "detail": err.Error()})
	}
	if body.Enabled == nil {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "enabled required"})
	}
	if err := h.store.UpdateInstall(c.Request().Context(), installID, *body.Enabled); err != nil {
		if errors.Is(err, rulepack.ErrInstallNotFound) {
			return c.JSON(http.StatusNotFound, map[string]any{"error": "install not found"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}

	ae := audit.EntryFor(c, iam.ResourceRulePack, iam.VerbUpdate)
	ae.EntityID = installID
	ae.AfterState = map[string]any{"enabled": *body.Enabled}
	h.emitAudit(c, ae)
	h.invalidateHookConfig(c.Request().Context())

	return c.JSON(http.StatusOK, map[string]any{
		"installId": installID,
		"enabled":   *body.Enabled,
	})
}

// UpgradeInstall advances an install's pinned pack version to the latest
// available version of the same pack family. Installs pin to an exact version
// row via packId, so this is a packId + pinVersion swap; the bound hook,
// enabled flag, and per-rule overrides survive. A no-op (already-latest)
// returns 200 with upgraded=false so the UI can render "up to date".
func (h *Handler) UpgradeInstall(c echo.Context) error {
	installID := c.Param("installId")
	if installID == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "installId required"})
	}
	res, err := h.store.UpgradeInstallToLatest(c.Request().Context(), installID)
	if err != nil {
		if errors.Is(err, rulepack.ErrInstallNotFound) {
			return c.JSON(http.StatusNotFound, map[string]any{"error": "install not found"})
		}
		if errors.Is(err, rulepack.ErrPackNotFound) {
			return c.JSON(http.StatusNotFound, map[string]any{"error": "pack not found"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}

	if res.Upgraded {
		ae := audit.EntryFor(c, iam.ResourceRulePack, iam.VerbUpdate)
		ae.EntityID = installID
		ae.BeforeState = map[string]any{"pinVersion": res.FromVersion}
		ae.AfterState = map[string]any{"pinVersion": res.ToVersion}
		h.emitAudit(c, ae)
		// Effective rule set changed — fan out hook invalidation so the
		// three data-plane services rebuild the bound rule-pack engine.
		h.invalidateHookConfig(c.Request().Context())
	}

	return c.JSON(http.StatusOK, res)
}

// UninstallByID removes a rule-pack install (and its overrides via
// cascade). Idempotent — 404 when the install is already gone.
func (h *Handler) UninstallByID(c echo.Context) error {
	installID := c.Param("installId")
	if installID == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "installId required"})
	}
	if err := h.store.DeleteInstall(c.Request().Context(), installID); err != nil {
		if errors.Is(err, rulepack.ErrInstallNotFound) {
			return c.JSON(http.StatusNotFound, map[string]any{"error": "install not found"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}

	ae := audit.EntryFor(c, iam.ResourceRulePack, iam.VerbDelete)
	ae.EntityID = installID
	h.emitAudit(c, ae)
	h.invalidateHookConfig(c.Request().Context())

	return c.JSON(http.StatusOK, map[string]any{"uninstalled": installID})
}

// EffectiveRules returns the post-override rule set a hook would see.
func (h *Handler) EffectiveRules(c echo.Context) error {
	installID := c.Param("installId")
	if installID == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{"error": "installId required"})
	}
	ers, err := h.store.LoadForInstall(c.Request().Context(), installID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]any{"error": "install not found"})
	}
	return c.JSON(http.StatusOK, ers)
}

// rulePackAuditSummary returns a compact projection of a Pack suitable for
// admin-audit BeforeState / AfterState payloads. Rule bodies (patterns,
// descriptions) are intentionally omitted so the audit log stays bounded
// even when a pack carries hundreds of rules.
func rulePackAuditSummary(p *rulepack.Pack) map[string]any {
	if p == nil {
		return nil
	}
	return map[string]any{
		"id":          p.ID,
		"name":        p.Name,
		"version":     p.Version,
		"maintainer":  p.Maintainer,
		"description": p.Description,
		"ruleCount":   len(p.Rules),
	}
}
