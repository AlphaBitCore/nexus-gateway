// Package cache owns the Control Plane admin API for the 3-tier
// prompt cache configuration surface: GET/PUT global, GET/list/PUT
// adapter, GET/PUT/DELETE provider, plus effective + overrides views.
package cache

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/cache/cachestore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
)

// HubConfigChanger is the narrow Hub surface cache/ needs:
// NotifyConfigChange to push the assembled 3-tier blob under the
// `cache` shadow key.
type HubConfigChanger interface {
	NotifyConfigChange(ctx context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error)
}

// Deps is the construction-time arg shape.
type Deps struct {
	Pool   *pgxpool.Pool
	Hub    HubConfigChanger // may be nil — handlers tolerate it
	Audit  *audit.Writer
	Logger *slog.Logger
}

// Handler is the per-domain admin handler for /api/admin/cache/*
// endpoints.
type Handler struct {
	pool   cachestore.PgxPool // interface — accepts *pgxpool.Pool in prod, pgxmock in tests
	cache  *cachestore.Store
	hub    HubConfigChanger
	audit  *audit.Writer
	logger *slog.Logger
	// tsStore provides DB persistence for time-sensitive rule overrides.
	// Nil when no DB pool is available (test/dev mode without DB).
	tsStore TimeSensitiveStore
}

// New constructs a cache Handler from its narrow Deps.
func New(d Deps) *Handler {
	h := &Handler{hub: d.Hub, audit: d.Audit, logger: d.Logger}
	if d.Pool != nil {
		h.pool = d.Pool
		h.cache = cachestore.New(d.Pool)
		h.tsStore = configstore.NewSemanticCacheStore(d.Pool)
	}
	return h
}

// newWithPool is the test seam for injecting a pgxmock-backed pool into
// both the cachestore and the preview QueryRow surface. tsStore is nil
// when pool is nil (tests that do not exercise DB-persistence paths).
func newWithPool(pool cachestore.PgxPool, hub HubConfigChanger, aw *audit.Writer, logger *slog.Logger) *Handler {
	h := &Handler{pool: pool, hub: hub, audit: aw, logger: logger}
	if pool != nil {
		h.cache = cachestore.New(pool)
	}
	return h
}

// RegisterRoutes mounts the prompt-cache admin endpoints under the
// caller-supplied admin group, gated by the prompt-cache IAM resource.
func (h *Handler) RegisterRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/cache/global", h.CacheGetGlobal, iamMW(iam.ResourcePromptCache.Action(iam.VerbRead)))
	g.PUT("/cache/global", h.CachePutGlobal, iamMW(iam.ResourcePromptCache.Action(iam.VerbUpdate)))

	g.GET("/cache/adapters", h.CacheListAdapters, iamMW(iam.ResourcePromptCache.Action(iam.VerbRead)))
	g.GET("/cache/adapter/:adapter_type", h.CacheGetAdapter, iamMW(iam.ResourcePromptCache.Action(iam.VerbRead)))
	g.PUT("/cache/adapter/:adapter_type", h.CachePutAdapter, iamMW(iam.ResourcePromptCache.Action(iam.VerbUpdate)))

	g.GET("/cache/provider/:provider_id", h.CacheGetProvider, iamMW(iam.ResourcePromptCache.Action(iam.VerbRead)))
	g.PUT("/cache/provider/:provider_id", h.CachePutProvider, iamMW(iam.ResourcePromptCache.Action(iam.VerbUpdate)))
	g.DELETE("/cache/provider/:provider_id", h.CacheDeleteProvider, iamMW(iam.ResourcePromptCache.Action(iam.VerbUpdate)))

	g.GET("/cache/effective", h.CacheGetEffective, iamMW(iam.ResourcePromptCache.Action(iam.VerbRead)))
	g.GET("/cache/overrides", h.CacheListOverrides, iamMW(iam.ResourcePromptCache.Action(iam.VerbRead)))
}

// errJSON builds a canonical JSON error envelope used across admin handlers.
func errJSON(message, errType, code string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	}
}

// actor is the per-request identity.
type actor struct {
	UserID string
	Name   string
}

// actorFromContext extracts the caller identity attached by the admin
// auth middleware.
func actorFromContext(c echo.Context) actor {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return actor{}
	}
	return actor{UserID: aa.KeyID, Name: aa.KeyName}
}

// propagateCacheConfig assembles the full 3-tier blob and pushes it to
// Hub via NotifyConfigChange under the shadow key `cache`.
// On Hub failure the caller should respond 502 with a structured propagation_error.
func (h *Handler) propagateCacheConfig(ctx context.Context, actorID, actorName string) error {
	if h.hub == nil {
		return nil // Hub not wired (test/dev mode); silent no-op
	}
	blob, err := h.cache.AssembleCacheConfigBlob(ctx)
	if err != nil {
		return wrapErr("assemble cache blob", err)
	}
	_, err = h.hub.NotifyConfigChange(ctx, hub.ConfigChangeRequest{
		ThingType: "ai-gateway",
		ConfigKey: configkey.Cache,
		State:     blob,
		ActorID:   actorID,
		ActorName: actorName,
	})
	return err
}

// hubPropagationErrorJSON is the structured body returned on HTTP 502
// when the DB write succeeded but Hub propagation failed (per ADR-4).
func hubPropagationErrorJSON(detail error) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": "Config saved locally but propagation to gateway failed; verify Hub health and retry, or wait up to 60s for the reconcile job to recover.",
			"type":    "propagation_error",
			"code":    "",
			"detail":  detail.Error(),
		},
	}
}

// internalServerError is the canonical 500 used across this domain.
func internalServerError(c echo.Context, msg string) error {
	return c.JSON(http.StatusInternalServerError, errJSON(msg, "server_error", ""))
}

// wrapErr formats an error with a context prefix.
func wrapErr(ctx string, err error) error {
	return &wrappedErr{ctx: ctx, err: err}
}

type wrappedErr struct {
	ctx string
	err error
}

func (w *wrappedErr) Error() string { return w.ctx + ": " + w.err.Error() }
func (w *wrappedErr) Unwrap() error { return w.err }
