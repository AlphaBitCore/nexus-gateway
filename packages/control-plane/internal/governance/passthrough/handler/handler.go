// Package passthrough owns the Control Plane admin API for the
// emergency-passthrough surface — 3-tier (global / adapter / per-provider)
// bypass config for the ai-gateway data plane. Writes go through Hub which
// UPSERTs the gateway_passthrough shadow blob and broadcasts to ai-gateway.
package passthrough

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// pgxQueryer is the minimal pgx pool surface this handler exercises.
// *pgxpool.Pool satisfies it in production; pgxmock's PgxPoolIface
// satisfies it in tests. The seam exists because passthrough's blob
// SQL is composed inline (not extracted to store/) — same pattern as
// sibling handler/{cache,virtualkey,routing} for direct-pool sites.
type pgxQueryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Emergency-passthrough handler constants.
const (
	shadowKey    = "gateway_passthrough"
	maxExpiry    = 8 * time.Hour
	minReasonLen = 20
)

// HubConfigChanger is the narrow Hub surface passthrough/ needs:
// NotifyConfigChange pushes the assembled blob under the
// gateway_passthrough shadow key. Same shape as cache.HubConfigChanger.
type HubConfigChanger interface {
	NotifyConfigChange(ctx context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error)
}

// Deps is the construction-time arg shape.
type Deps struct {
	Pool   *pgxpool.Pool    // concrete pool for SQL; nil allowed in tests that inject via newWithPool
	Hub    HubConfigChanger // may be nil — propagation skipped
	Audit  *audit.Writer    // may be nil — audit emits become no-ops
	Logger *slog.Logger
}

// Handler is the per-domain admin handler for /api/admin/passthrough*
// endpoints.
type Handler struct {
	hub    HubConfigChanger
	audit  *audit.Writer
	logger *slog.Logger
	// pool is the SQL surface — set from Deps.Pool in production and
	// overridden in tests via newWithPool. Keeps every SQL site
	// behind one indirection so pgxmock can stand in.
	pool pgxQueryer
}

// New constructs a passthrough Handler from its narrow Deps.
func New(d Deps) *Handler {
	h := &Handler{hub: d.Hub, audit: d.Audit, logger: d.Logger}
	if d.Pool != nil {
		h.pool = d.Pool
	}
	return h
}

// RegisterRoutes mounts the passthrough admin endpoints. IAM gating:
// read is wide (any admin can inspect state); write is narrow
// (Provider Admin + Compliance Admin); emergency-enable is the most
// restricted (Incident Response role only).
func (h *Handler) RegisterRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	read := iam.ResourcePassthrough.Action(iam.VerbRead)
	write := iam.ResourcePassthrough.Action(iam.VerbWrite)
	emergency := iam.ResourcePassthrough.Action(iam.VerbEmergencyEnable)

	g.GET("/passthrough/global", h.GetGlobal, iamMW(read))
	g.PUT("/passthrough/global", h.PutGlobal, iamMW(emergency))

	g.GET("/passthrough/adapter/:adapter_type", h.GetAdapter, iamMW(read))
	g.PUT("/passthrough/adapter/:adapter_type", h.PutAdapter, iamMW(emergency))
	g.DELETE("/passthrough/adapter/:adapter_type", h.DeleteAdapter, iamMW(write))

	g.GET("/passthrough/provider/:provider_id", h.GetProvider, iamMW(read))
	g.PUT("/passthrough/provider/:provider_id", h.PutProvider, iamMW(emergency))
	g.DELETE("/passthrough/provider/:provider_id", h.DeleteProvider, iamMW(write))

	g.GET("/passthrough/effective/:provider_id", h.GetEffective, iamMW(read))
	g.GET("/passthrough/snapshot", h.GetSnapshot, iamMW(read))
}

// payload is the JSON shape every write endpoint accepts.
type payload struct {
	Enabled         bool       `json:"enabled"`
	BypassHooks     bool       `json:"bypassHooks"`
	BypassCache     bool       `json:"bypassCache"`
	BypassNormalize bool       `json:"bypassNormalize"`
	ExpiresAt       *time.Time `json:"expiresAt,omitempty"`
	Reason          string     `json:"reason,omitempty"`
}

// validate enforces the passthrough invariants on every write call.
func validate(p payload) (msg, code string, ok bool) {
	if !p.Enabled {
		return "", "", true
	}
	if !p.BypassHooks && !p.BypassCache && !p.BypassNormalize {
		return `at least one bypass flag (bypassHooks / bypassCache / bypassNormalize) must be set when enabled=true`,
			"passthrough_no_bypass_selected", false
	}
	if p.BypassNormalize && !p.BypassCache {
		return `bypassNormalize=true requires bypassCache=true — the cache key derives from the canonical normalized payload; see docs/operators/ops/runbooks/r-emergency-passthrough.md`,
			"passthrough_normalize_requires_cache_bypass", false
	}
	if p.ExpiresAt == nil {
		return `expiresAt is required when enabled=true`, "passthrough_invalid_expiry", false
	}
	if p.ExpiresAt.After(time.Now().Add(maxExpiry)) {
		return fmt.Sprintf(`expiresAt cannot exceed now() + %s; re-enable for a longer window via a fresh write`, maxExpiry),
			"passthrough_invalid_expiry", false
	}
	if p.ExpiresAt.Before(time.Now()) {
		return `expiresAt is already in the past`, "passthrough_invalid_expiry", false
	}
	if len(p.Reason) < minReasonLen {
		return fmt.Sprintf(`reason must be at least %d characters explaining why passthrough is needed`, minReasonLen),
			"passthrough_invalid_reason", false
	}
	return "", "", true
}

func (p payload) configJSON() ([]byte, error) {
	return json.Marshal(map[string]bool{
		"bypassHooks":     p.BypassHooks,
		"bypassCache":     p.BypassCache,
		"bypassNormalize": p.BypassNormalize,
	})
}

// fillFromJSONB parses a JSONB config blob into the payload's three
// bypass flags.
func (p *payload) fillFromJSONB(raw []byte) {
	if len(raw) == 0 {
		return
	}
	var m map[string]bool
	if err := json.Unmarshal(raw, &m); err == nil {
		p.BypassHooks = m["bypassHooks"]
		p.BypassCache = m["bypassCache"]
		p.BypassNormalize = m["bypassNormalize"]
	}
}

// tierBlob is the per-tier JSON shape that lands on the Hub-pushed
// `gateway_passthrough` shadow blob. Matches ai-gateway
// passthrough.TierEntry in the JSON tags.
type tierBlob struct {
	Enabled         bool       `json:"enabled"`
	BypassHooks     bool       `json:"bypassHooks"`
	BypassCache     bool       `json:"bypassCache"`
	BypassNormalize bool       `json:"bypassNormalize"`
	ExpiresAt       *time.Time `json:"expiresAt,omitempty"`
	EnabledBy       string     `json:"enabledBy,omitempty"`
	Reason          string     `json:"reason,omitempty"`
}

// blob is the typed Hub-wire shape. hubclient marshals the struct
// itself (State is `any`), so we must NOT pre-marshal — returning
// []byte would land double-encoded on the receiver and ai-gateway's
// json.Unmarshal would fail.
type blob struct {
	Global    tierBlob            `json:"global"`
	Adapters  map[string]tierBlob `json:"adapters"`
	Providers map[string]tierBlob `json:"providers"`
}

// propagateConfig assembles the full {global, adapters, providers}
// blob and pushes via Hub. nil-Hub is a no-op.
func (h *Handler) propagateConfig(ctx context.Context, actorID, actorName string) error {
	if h.hub == nil {
		return nil
	}
	b, err := h.assembleBlob(ctx)
	if err != nil {
		return fmt.Errorf("assemble passthrough blob: %w", err)
	}
	_, err = h.hub.NotifyConfigChange(ctx, hub.ConfigChangeRequest{
		ThingType: "ai-gateway",
		ConfigKey: shadowKey,
		State:     b,
		ActorID:   actorID,
		ActorName: actorName,
	})
	return err
}

func (h *Handler) assembleBlob(ctx context.Context) (blob, error) {
	b := blob{
		Adapters:  map[string]tierBlob{},
		Providers: map[string]tierBlob{},
	}

	// Global singleton
	var gCfg []byte
	g := tierBlob{}
	row := h.pool.QueryRow(ctx,
		`SELECT enabled, config, expires_at, COALESCE(enabled_by, ''), COALESCE(reason, '')
		   FROM gateway_passthrough_config_global WHERE id = 'singleton'`)
	if err := row.Scan(&g.Enabled, &gCfg, &g.ExpiresAt, &g.EnabledBy, &g.Reason); err == nil {
		applyTierBypass(gCfg, &g)
		b.Global = g
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return b, fmt.Errorf("read global: %w", err)
	}

	// Adapters
	rows, err := h.pool.Query(ctx,
		`SELECT adapter_type, enabled, config, expires_at, COALESCE(enabled_by, ''), COALESCE(reason, '')
		   FROM gateway_passthrough_config_adapter`)
	if err != nil {
		return b, fmt.Errorf("read adapters: %w", err)
	}
	for rows.Next() {
		var aType string
		var t tierBlob
		var cfg []byte
		if err := rows.Scan(&aType, &t.Enabled, &cfg, &t.ExpiresAt, &t.EnabledBy, &t.Reason); err != nil {
			rows.Close()
			return b, err
		}
		applyTierBypass(cfg, &t)
		b.Adapters[aType] = t
	}
	rows.Close()

	// Providers
	prows, err := h.pool.Query(ctx,
		`SELECT provider_id, enabled, config, expires_at, COALESCE(enabled_by, ''), COALESCE(reason, '')
		   FROM gateway_passthrough_config_provider`)
	if err != nil {
		return b, fmt.Errorf("read providers: %w", err)
	}
	for prows.Next() {
		var pID string
		var t tierBlob
		var cfg []byte
		if err := prows.Scan(&pID, &t.Enabled, &cfg, &t.ExpiresAt, &t.EnabledBy, &t.Reason); err != nil {
			prows.Close()
			return b, err
		}
		applyTierBypass(cfg, &t)
		b.Providers[pID] = t
	}
	prows.Close()

	return b, nil
}

func applyTierBypass(raw []byte, t *tierBlob) {
	if len(raw) == 0 {
		return
	}
	var m map[string]bool
	if err := json.Unmarshal(raw, &m); err == nil {
		t.BypassHooks = m["bypassHooks"]
		t.BypassCache = m["bypassCache"]
		t.BypassNormalize = m["bypassNormalize"]
	}
}

func hubPropagationErrorJSON(detail error) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": "Passthrough config saved locally but propagation to ai-gateway failed; verify Hub health and retry, or wait up to 60s for the reconcile job to recover.",
			"type":    "propagation_error",
			"detail":  detail.Error(),
		},
	}
}

// actor reads the JWT-auth-populated "user" context key. Mirrors the
// original handler — different from killswitch's actorFromContext
// which reads middleware.AdminAuth.
func actor(c echo.Context) (string, string) {
	if u, ok := c.Get("user").(map[string]any); ok {
		id, _ := u["id"].(string)
		name, _ := u["display_name"].(string)
		return id, name
	}
	return "", ""
}

// ── Endpoints ─────────────────────────────────────────────────────────────

func (h *Handler) GetGlobal(c echo.Context) error {
	out := payload{}
	var cfg []byte
	var reason *string
	err := h.pool.QueryRow(c.Request().Context(),
		`SELECT enabled, config, expires_at, reason
		   FROM gateway_passthrough_config_global WHERE id = 'singleton'`,
	).Scan(&out.Enabled, &cfg, &out.ExpiresAt, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusOK, payload{})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("read global passthrough: "+err.Error(), "server_error", ""))
	}
	if reason != nil {
		out.Reason = *reason
	}
	out.fillFromJSONB(cfg)
	return c.JSON(http.StatusOK, out)
}

func (h *Handler) PutGlobal(c echo.Context) error {
	var body payload
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if msg, code, ok := validate(body); !ok {
		return c.JSON(http.StatusBadRequest, errJSON(msg, code, ""))
	}
	cfg, _ := body.configJSON()
	actorID, _ := actor(c)
	ctx := c.Request().Context()
	// Snapshot the prior tier row before the upsert so the audit emit can
	// emit a BeforeState diff alongside the AfterState. A read error here
	// is non-fatal — we still apply the mutation and emit audit with a
	// nil BeforeState (logged separately).
	before, beforeErr := h.readTierState(ctx, "global", "singleton")
	if beforeErr != nil {
		h.logger.Warn("passthrough: read prior state failed (audit BeforeState will be omitted)",
			"tier", "global", "error", beforeErr)
	}
	_, err := h.pool.Exec(ctx,
		`INSERT INTO gateway_passthrough_config_global (id, enabled, config, expires_at, enabled_by, reason, updated_at)
		 VALUES ('singleton', $1, $2, $3, $4, $5, NOW())
		 ON CONFLICT (id) DO UPDATE SET
		   enabled = EXCLUDED.enabled, config = EXCLUDED.config,
		   expires_at = EXCLUDED.expires_at, enabled_by = EXCLUDED.enabled_by,
		   reason = EXCLUDED.reason, updated_at = NOW()`,
		body.Enabled, cfg, body.ExpiresAt, actorID, nullableString(body.Reason))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("write global passthrough: "+err.Error(), "server_error", ""))
	}
	if err := h.propagateConfig(ctx, actorID, ""); err != nil {
		return c.JSON(http.StatusBadGateway, hubPropagationErrorJSON(err))
	}
	// Emit audit after both the DB upsert and the Hub propagation have
	// committed; matches the killswitch handler's ordering so the audit
	// row only lands when the user-visible mutation is fully durable.
	h.emitAudit(c, iam.VerbEmergencyEnable, "global", before, payloadAuditState(body))
	return c.JSON(http.StatusOK, body)
}

func (h *Handler) GetAdapter(c echo.Context) error {
	aType := c.Param("adapter_type")
	out := payload{}
	var cfg []byte
	var reason *string
	err := h.pool.QueryRow(c.Request().Context(),
		`SELECT enabled, config, expires_at, reason
		   FROM gateway_passthrough_config_adapter WHERE adapter_type = $1`, aType,
	).Scan(&out.Enabled, &cfg, &out.ExpiresAt, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusNotFound, errJSON("no passthrough config for adapter "+aType, "not_found", ""))
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("read adapter passthrough: "+err.Error(), "server_error", ""))
	}
	if reason != nil {
		out.Reason = *reason
	}
	out.fillFromJSONB(cfg)
	return c.JSON(http.StatusOK, out)
}

func (h *Handler) PutAdapter(c echo.Context) error {
	aType := c.Param("adapter_type")
	var body payload
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if msg, code, ok := validate(body); !ok {
		return c.JSON(http.StatusBadRequest, errJSON(msg, code, ""))
	}
	cfg, _ := body.configJSON()
	actorID, _ := actor(c)
	ctx := c.Request().Context()
	before, beforeErr := h.readTierState(ctx, "adapter", aType)
	if beforeErr != nil {
		h.logger.Warn("passthrough: read prior state failed (audit BeforeState will be omitted)",
			"tier", "adapter", "adapter_type", aType, "error", beforeErr)
	}
	_, err := h.pool.Exec(ctx,
		`INSERT INTO gateway_passthrough_config_adapter (adapter_type, enabled, config, expires_at, enabled_by, reason, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NOW())
		 ON CONFLICT (adapter_type) DO UPDATE SET
		   enabled = EXCLUDED.enabled, config = EXCLUDED.config,
		   expires_at = EXCLUDED.expires_at, enabled_by = EXCLUDED.enabled_by,
		   reason = EXCLUDED.reason, updated_at = NOW()`,
		aType, body.Enabled, cfg, body.ExpiresAt, actorID, nullableString(body.Reason))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("write adapter passthrough: "+err.Error(), "server_error", ""))
	}
	if err := h.propagateConfig(ctx, actorID, ""); err != nil {
		return c.JSON(http.StatusBadGateway, hubPropagationErrorJSON(err))
	}
	h.emitAudit(c, iam.VerbEmergencyEnable, aType, before, payloadAuditState(body))
	return c.JSON(http.StatusOK, body)
}

func (h *Handler) DeleteAdapter(c echo.Context) error {
	aType := c.Param("adapter_type")
	ctx := c.Request().Context()
	before, beforeErr := h.readTierState(ctx, "adapter", aType)
	if beforeErr != nil {
		h.logger.Warn("passthrough: read prior state failed (audit BeforeState will be omitted)",
			"tier", "adapter", "adapter_type", aType, "error", beforeErr)
	}
	if _, err := h.pool.Exec(ctx,
		`DELETE FROM gateway_passthrough_config_adapter WHERE adapter_type = $1`, aType); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("delete adapter passthrough: "+err.Error(), "server_error", ""))
	}
	actorID, _ := actor(c)
	if err := h.propagateConfig(ctx, actorID, ""); err != nil {
		return c.JSON(http.StatusBadGateway, hubPropagationErrorJSON(err))
	}
	h.emitAudit(c, iam.VerbWrite, aType, before, nil)
	return c.NoContent(http.StatusNoContent)
}

func (h *Handler) GetProvider(c echo.Context) error {
	pID := c.Param("provider_id")
	out := payload{}
	var cfg []byte
	var reason *string
	err := h.pool.QueryRow(c.Request().Context(),
		`SELECT enabled, config, expires_at, reason
		   FROM gateway_passthrough_config_provider WHERE provider_id = $1`, pID,
	).Scan(&out.Enabled, &cfg, &out.ExpiresAt, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusNotFound, errJSON("no passthrough config for provider "+pID, "not_found", ""))
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("read provider passthrough: "+err.Error(), "server_error", ""))
	}
	if reason != nil {
		out.Reason = *reason
	}
	out.fillFromJSONB(cfg)
	return c.JSON(http.StatusOK, out)
}

func (h *Handler) PutProvider(c echo.Context) error {
	pID := c.Param("provider_id")
	var body payload
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	if msg, code, ok := validate(body); !ok {
		return c.JSON(http.StatusBadRequest, errJSON(msg, code, ""))
	}
	cfg, _ := body.configJSON()
	actorID, _ := actor(c)
	ctx := c.Request().Context()
	before, beforeErr := h.readTierState(ctx, "provider", pID)
	if beforeErr != nil {
		h.logger.Warn("passthrough: read prior state failed (audit BeforeState will be omitted)",
			"tier", "provider", "provider_id", pID, "error", beforeErr)
	}
	_, err := h.pool.Exec(ctx,
		`INSERT INTO gateway_passthrough_config_provider (provider_id, enabled, config, expires_at, enabled_by, reason, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NOW())
		 ON CONFLICT (provider_id) DO UPDATE SET
		   enabled = EXCLUDED.enabled, config = EXCLUDED.config,
		   expires_at = EXCLUDED.expires_at, enabled_by = EXCLUDED.enabled_by,
		   reason = EXCLUDED.reason, updated_at = NOW()`,
		pID, body.Enabled, cfg, body.ExpiresAt, actorID, nullableString(body.Reason))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("write provider passthrough: "+err.Error(), "server_error", ""))
	}
	if err := h.propagateConfig(ctx, actorID, ""); err != nil {
		return c.JSON(http.StatusBadGateway, hubPropagationErrorJSON(err))
	}
	h.emitAudit(c, iam.VerbEmergencyEnable, pID, before, payloadAuditState(body))
	return c.JSON(http.StatusOK, body)
}

func (h *Handler) DeleteProvider(c echo.Context) error {
	pID := c.Param("provider_id")
	ctx := c.Request().Context()
	before, beforeErr := h.readTierState(ctx, "provider", pID)
	if beforeErr != nil {
		h.logger.Warn("passthrough: read prior state failed (audit BeforeState will be omitted)",
			"tier", "provider", "provider_id", pID, "error", beforeErr)
	}
	if _, err := h.pool.Exec(ctx,
		`DELETE FROM gateway_passthrough_config_provider WHERE provider_id = $1`, pID); err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("delete provider passthrough: "+err.Error(), "server_error", ""))
	}
	actorID, _ := actor(c)
	if err := h.propagateConfig(ctx, actorID, ""); err != nil {
		return c.JSON(http.StatusBadGateway, hubPropagationErrorJSON(err))
	}
	h.emitAudit(c, iam.VerbWrite, pID, before, nil)
	return c.NoContent(http.StatusNoContent)
}

func (h *Handler) GetEffective(c echo.Context) error {
	pID := c.Param("provider_id")
	out := payload{}
	var cfg []byte
	var reason *string
	err := h.pool.QueryRow(c.Request().Context(),
		`SELECT enabled, config, expires_at, reason
		   FROM gateway_passthrough_config_effective WHERE provider_id = $1`, pID,
	).Scan(&out.Enabled, &cfg, &out.ExpiresAt, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(http.StatusNotFound, errJSON("provider not found", "not_found", ""))
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("read effective passthrough: "+err.Error(), "server_error", ""))
	}
	if reason != nil {
		out.Reason = *reason
	}
	out.fillFromJSONB(cfg)
	return c.JSON(http.StatusOK, out)
}

// nullableString turns "" into nil to land NULL in the column.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// snapshotResponse is the bulk-read shape returned by
// GET /api/admin/passthrough/snapshot.
type snapshotResponse struct {
	Global        tierBlob            `json:"global"`
	Adapters      map[string]tierBlob `json:"adapters"`
	Providers     map[string]tierBlob `json:"providers"`
	ProviderNames map[string]string   `json:"providerNames,omitempty"`
}

func (h *Handler) GetSnapshot(c echo.Context) error {
	ctx := c.Request().Context()
	b, err := h.assembleBlob(ctx)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("assemble snapshot: "+err.Error(), "server_error", ""))
	}
	// Resolve provider names for whatever provider IDs landed in the
	// blob. A missing row just doesn't appear in the lookup; the UI
	// falls back to displaying the raw ID.
	names := map[string]string{}
	if len(b.Providers) > 0 {
		ids := make([]string, 0, len(b.Providers))
		for id := range b.Providers {
			ids = append(ids, id)
		}
		rows, err := h.pool.Query(ctx,
			`SELECT id, name FROM "Provider" WHERE id = ANY($1::uuid[])`, ids)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id, name string
				if err := rows.Scan(&id, &name); err == nil {
					names[id] = name
				}
			}
		}
	}
	return c.JSON(http.StatusOK, snapshotResponse{
		Global:        b.Global,
		Adapters:      b.Adapters,
		Providers:     b.Providers,
		ProviderNames: names,
	})
}

func errJSON(message, errType, code string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	}
}

// readTierState is the small per-tier helper that snapshots
// {enabled, bypassHooks, bypassCache, bypassNormalize, expiresAt, reason}
// from the matching tier table for use as audit BeforeState / AfterState.
// Returns (nil, nil) when the row does not exist — the caller renders that
// as "no prior state" (audit entry omits BeforeState) or "row gone"
// (audit entry omits AfterState on delete) as appropriate.
//
// tier is "global", "adapter", or "provider"; key is the tier identifier
// ("singleton" / adapterType / providerID). Errors are returned to the
// caller so the audit BeforeState capture failure can be logged without
// short-circuiting the user-visible mutation — the upstream operation has
// already committed by the time the AfterState snapshot runs.
func (h *Handler) readTierState(ctx context.Context, tier, key string) (map[string]any, error) {
	var (
		enabled   bool
		cfg       []byte
		expiresAt *time.Time
		reason    *string
		row       pgx.Row
	)
	switch tier {
	case "global":
		row = h.pool.QueryRow(ctx,
			`SELECT enabled, config, expires_at, reason
			   FROM gateway_passthrough_config_global WHERE id = $1`, key)
	case "adapter":
		row = h.pool.QueryRow(ctx,
			`SELECT enabled, config, expires_at, reason
			   FROM gateway_passthrough_config_adapter WHERE adapter_type = $1`, key)
	case "provider":
		row = h.pool.QueryRow(ctx,
			`SELECT enabled, config, expires_at, reason
			   FROM gateway_passthrough_config_provider WHERE provider_id = $1`, key)
	default:
		return nil, fmt.Errorf("readTierState: unknown tier %q", tier)
	}
	if err := row.Scan(&enabled, &cfg, &expiresAt, &reason); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	state := map[string]any{
		"enabled":         enabled,
		"bypassHooks":     false,
		"bypassCache":     false,
		"bypassNormalize": false,
	}
	if len(cfg) > 0 {
		var m map[string]bool
		if err := json.Unmarshal(cfg, &m); err == nil {
			state["bypassHooks"] = m["bypassHooks"]
			state["bypassCache"] = m["bypassCache"]
			state["bypassNormalize"] = m["bypassNormalize"]
		}
	}
	if expiresAt != nil {
		state["expiresAt"] = expiresAt.UTC().Format(time.RFC3339Nano)
	}
	if reason != nil {
		state["reason"] = *reason
	}
	return state, nil
}

// payloadAuditState renders the incoming write payload as an audit
// AfterState map matching the readTierState shape. Keeps the
// before/after snapshots structurally identical so SIEM consumers can
// diff them mechanically.
func payloadAuditState(p payload) map[string]any {
	m := map[string]any{
		"enabled":         p.Enabled,
		"bypassHooks":     p.BypassHooks,
		"bypassCache":     p.BypassCache,
		"bypassNormalize": p.BypassNormalize,
	}
	if p.ExpiresAt != nil {
		m["expiresAt"] = p.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	if p.Reason != "" {
		m["reason"] = p.Reason
	}
	return m
}

// emitAudit publishes a single audit entry. nil-Writer is a no-op; the
// writer itself swallows MQ failures (LogObserved). Callers MUST have
// already committed the upstream mutation before calling — the audit
// row is fire-and-forget.
func (h *Handler) emitAudit(c echo.Context, verb iam.Verb, entityID string, before, after map[string]any) {
	if h.audit == nil {
		return
	}
	ae := audit.EntryFor(c, iam.ResourcePassthrough, verb)
	ae.EntityID = entityID
	if before != nil {
		ae.BeforeState = before
	}
	if after != nil {
		ae.AfterState = after
	}
	h.audit.LogObserved(c.Request().Context(), ae)
}
