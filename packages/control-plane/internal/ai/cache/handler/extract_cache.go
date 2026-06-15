// Package cache: extract_cache.go — admin API for the fleet-wide L1 extract
// (exact-match) response cache singleton.
//
// GET  /api/admin/extract-cache/config — returns the singleton row
// PUT  /api/admin/extract-cache/config — validates + saves + Hub-invalidates
//
// IAM: iam.ResourceExtractCache.{Read, Update}
// Hub: NotifyConfigChange pushes the full state under
//      response_cache.extract_config. A dropped push is escalated to HTTP 502
//      AND healed within one reconcile cycle by the configreconcile content-diff
//      watch for this key: the push and the reconcile loader
//      share the ExtractCacheConfigRow.WireState projection. Mirror of the
//      semantic-cache handler shape.

package cache

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	hubclient "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
)

// ExtractCacheStore is the narrow seam the extract-cache handler needs.
// configstore.ExtractCacheStore satisfies it in production; in-memory
// doubles satisfy it in tests.
type ExtractCacheStore interface {
	Get(ctx context.Context) (*configstore.ExtractCacheConfigRow, error)
	Save(ctx context.Context, in configstore.ExtractCacheSaveInput) (*configstore.ExtractCacheConfigRow, error)
}

// ExtractCacheHubPusher is the narrow Hub seam the extract-cache handler
// needs. Unlike semantic-cache (which calls InvalidateConfig + relies on the
// receiver to pull), this handler PUSHES the full state because the AI
// Gateway receiver atomically swaps cache.Cache fields from the broadcast
// payload — Hub.thing_config_template stores the state JSON, and the
// receiver decodes those bytes on every config_changed broadcast.
type ExtractCacheHubPusher interface {
	NotifyConfigChange(ctx context.Context, req hubclient.ConfigChangeRequest) (*hubclient.ConfigChangeResponse, error)
}

// ExtractCacheHandlerDeps mirrors SemanticCacheHandlerDeps.
type ExtractCacheHandlerDeps struct {
	Store  ExtractCacheStore
	Hub    ExtractCacheHubPusher // may be nil — push skipped (test/dev mode)
	Audit  *audit.Writer         // may be nil (tests)
	Logger *slog.Logger
}

// ExtractCacheHandler owns /api/admin/extract-cache/* routes.
type ExtractCacheHandler struct {
	store  ExtractCacheStore
	hub    ExtractCacheHubPusher
	audit  *audit.Writer
	logger *slog.Logger
}

func NewExtractCacheHandler(d ExtractCacheHandlerDeps) *ExtractCacheHandler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &ExtractCacheHandler{
		store:  d.Store,
		hub:    d.Hub,
		audit:  d.Audit,
		logger: logger,
	}
}

// RegisterExtractCacheRoutes mounts the extract-cache admin endpoints under
// the caller-supplied admin group. iamMW gates each route per the
// ExtractCache resource verb taxonomy.
func (h *ExtractCacheHandler) RegisterExtractCacheRoutes(
	g *echo.Group,
	iamMW func(action string) echo.MiddlewareFunc,
) {
	g.GET("/extract-cache/config", h.GetConfig, iamMW(iam.ResourceExtractCache.Action(iam.VerbRead)))
	g.PUT("/extract-cache/config", h.PutConfig, iamMW(iam.ResourceExtractCache.Action(iam.VerbUpdate)))
}

// extractCacheUpdateRequest is the JSON body for PUT.
//
// All three fields are required; the handler validates them and either accepts
// the whole payload or returns 400. Unlike semantic-cache, there is no
// partial-update pattern because the field count is small (3) and admins
// always send the full row from the UI.
type extractCacheUpdateRequest struct {
	Enabled             bool `json:"enabled"`
	TTLSeconds          int  `json:"ttlSeconds"`
	ApplyFreshnessRules bool `json:"applyFreshnessRules"`
}

// GetConfig returns the singleton ExtractCacheConfigRow as JSON.
func (h *ExtractCacheHandler) GetConfig(c echo.Context) error {
	row, err := h.store.Get(c.Request().Context())
	if err != nil {
		h.logger.Error("extract-cache: get config", "error", err)
		return c.JSON(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, row)
}

// PutConfig validates and saves the singleton row, fires a Hub Category B
// invalidation so the ai-gateway atomically swaps its in-process cache config,
// and emits an admin audit entry on success.
//
// Validation: ttlSeconds must be in [60, 604800] when enabled=true.
func (h *ExtractCacheHandler) PutConfig(c echo.Context) error {
	var req extractCacheUpdateRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{
			"error": "malformed_json", "detail": err.Error(),
		})
	}

	if req.Enabled && (req.TTLSeconds < 60 || req.TTLSeconds > 7*86400) {
		return c.JSON(http.StatusBadRequest, map[string]any{
			"error": "ttlSeconds must be in [60, 604800] when enabled=true",
		})
	}

	actor := actorFromContext(c)

	saved, err := h.store.Save(c.Request().Context(), configstore.ExtractCacheSaveInput{
		Enabled:             req.Enabled,
		TTLSeconds:          req.TTLSeconds,
		ApplyFreshnessRules: req.ApplyFreshnessRules,
		UpdatedBy:           actor.UserID,
	})
	if err != nil {
		h.logger.Error("extract-cache: save config", "error", err)
		return c.JSON(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}

	// Push the full state to Hub (Type A pattern) so the broadcast payload
	// carries the new config bytes. The AI Gateway receiver atomically swaps
	// its in-process cache config via SetConfig. The same projection
	// (ExtractCacheConfigRow.WireState) feeds the configreconcile watch for
	// this key, so a dropped push is BOTH escalated to HTTP
	// 502 here AND healed within one reconcile cycle if the admin does not
	// retry — identical to cache.go.
	state := saved.WireState()
	if _, hubErr := hubclient.PushTypeA(c.Request().Context(), h.hub, "ai-gateway", configkey.ResponseCacheExtractConfig, state, hubclient.Actor{ID: actor.UserID, Name: actor.Name}); hubErr != nil {
		h.logger.Error("extract-cache: hub push failed", "error", hubErr)
		return hubclient.RespondPropagationFailure(c, hubErr)
	}

	if h.audit != nil {
		e := audit.EntryFor(c, iam.ResourceExtractCache, iam.VerbUpdate)
		e.EntityID = "singleton"
		e.AfterState = map[string]any{
			"enabled":             saved.Enabled,
			"ttlSeconds":          saved.TTLSeconds,
			"applyFreshnessRules": saved.ApplyFreshnessRules,
		}
		h.audit.LogObserved(c.Request().Context(), e)
	}

	return c.JSON(http.StatusOK, saved)
}
