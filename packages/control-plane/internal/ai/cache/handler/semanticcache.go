// Package cache: semanticcache.go — admin API for the fleet-wide semantic
// cache embedding singleton config.
//
// GET  /api/admin/semantic-cache/config — returns the singleton row
// PUT  /api/admin/semantic-cache/config — validates + saves + Hub push
//
// IAM: iam.ResourceSemanticCache.{Read, Update}
// Hub: NotifyConfigChange with full row state — the gateway IndexLifecycle
//      requires the snapshot blob to make EnsureIndex decisions.
//      The reconcile job recovers within 60s on transient Hub outages.

package cache

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
)

// SemanticCacheStore is the narrow seam the semantic-cache handler needs.
// configstore.SemanticCacheStore satisfies it in production; in-memory
// doubles satisfy it in tests. Defined here (not in configstore) to keep
// the dependency direction correct (handler depends on store, not reverse).
//
// Nexus is single-tenant by design — there is no per-org or per-tenant
// variation of this row, so the signatures do not carry an org identifier.
type SemanticCacheStore interface {
	Get(ctx context.Context) (*configstore.SemanticCacheConfigRow, error)
	Save(ctx context.Context, in configstore.SaveInput) (*configstore.SemanticCacheConfigRow, error)
}

// SemanticCacheHubInvalidator is the narrow Hub surface the semantic-cache
// handler needs. semantic_cache.config is a Category A config — the gateway
// IndexLifecycle requires the full snapshot in the broadcast payload so it
// can decide whether the embedding fingerprint changed and a RediSearch
// FT.CREATE is needed. A bare version-bump (InvalidateConfig) leaves the
// thing_config_template.state at null, the gateway sees an empty payload
// and disables L2 silently. Mirrors the time_sensitive_patterns push at
// packages/control-plane/internal/ai/cache/handler/time_sensitive.go:301.
type SemanticCacheHubInvalidator interface {
	NotifyConfigChange(ctx context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error)
}

// SemanticCacheHandlerDeps is the construction-time argument shape for
// NewSemanticCacheHandler.
type SemanticCacheHandlerDeps struct {
	Store  SemanticCacheStore
	Hub    SemanticCacheHubInvalidator // may be nil — invalidation skipped, reconcile recovers within 60s
	Audit  *audit.Writer               // may be nil (tests) — audit emission skipped silently
	Logger *slog.Logger
	// AIGatewayURL is the internal base URL of the AI Gateway service.
	// Required for the prewarm endpoint which forwards embedding+write to the
	// AI GW's /internal/semantic-prewarm handler. Empty string disables
	// prewarm forwarding (returns 503 with a clear error message).
	AIGatewayURL string
	// Poison is the negative-feedback poison list backend.
	// When nil, the feedback POST endpoint returns 503 with a descriptive message.
	Poison PoisonAdder
}

// SemanticCacheHandler owns /api/admin/semantic-cache/* routes.
// Structural mirror of the AI Guard handler (governance/aiguard/handler/).
type SemanticCacheHandler struct {
	store        SemanticCacheStore
	hub          SemanticCacheHubInvalidator
	audit        *audit.Writer
	logger       *slog.Logger
	aiGatewayURL string      // internal AI GW base URL for prewarm forwarding
	poison       PoisonAdder // may be nil when Redis unavailable
}

// NewSemanticCacheHandler constructs the handler from its narrow deps.
// hub may be nil (e.g., when Hub coordination is not configured); the
// invalidation push becomes a no-op and the data plane converges on the
// next ConfigCache TTL expiry or reconcile job. Audit may be nil (tests);
// when nil, mutating endpoints skip audit emission silently.
func NewSemanticCacheHandler(d SemanticCacheHandlerDeps) *SemanticCacheHandler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &SemanticCacheHandler{
		store:        d.Store,
		hub:          d.Hub,
		audit:        d.Audit,
		logger:       logger,
		aiGatewayURL: d.AIGatewayURL,
		poison:       d.Poison,
	}
}

// RegisterSemanticCacheRoutes mounts the semantic-cache admin endpoints
// under the caller-supplied admin group. iamMW gates each route per the
// SemanticCache resource verb taxonomy.
//
// Registered routes:
//
//	GET  /semantic-cache/config    — read singleton config
//	PUT  /semantic-cache/config    — save singleton config + Hub invalidate
//	POST /semantic-cache/prewarm   — bulk-embed + HSET FAQ corpus
//	POST /cache/semantic-feedback  — negative-feedback poison an entry
//	GET  /cache/semantic-feedback  — list recent feedback entries
func (h *SemanticCacheHandler) RegisterSemanticCacheRoutes(
	g *echo.Group,
	iamMW func(action string) echo.MiddlewareFunc,
) {
	g.GET("/semantic-cache/config", h.GetConfig, iamMW(iam.ResourceSemanticCache.Action(iam.VerbRead)))
	g.PUT("/semantic-cache/config", h.PutConfig, iamMW(iam.ResourceSemanticCache.Action(iam.VerbUpdate)))
	g.POST("/semantic-cache/prewarm", h.PrewarmCache, iamMW(iam.ResourceSemanticCache.Action(iam.VerbUpdate)))
	h.RegisterSemanticFeedbackRoutes(g, iamMW)
}

// semanticCacheUpdateRequest is the JSON body shape for PUT
// /api/admin/semantic-cache/config. Mirrors SemanticCacheConfigUpdate from
// the OpenAPI spec (docs/openapi/e61-s6-cache-admin.yaml).
type semanticCacheUpdateRequest struct {
	EmbeddingProviderID *string `json:"embeddingProviderId"`
	EmbeddingModelID    *string `json:"embeddingModelId"`
	EmbeddingDimension  *int    `json:"embeddingDimension"`
	Enabled             bool    `json:"enabled"`
	// The four fleet-wide L2 tuning knobs surfaced after the per-route policy
	// retirement. All optional — store.Save normalizes out-of-range / unknown
	// enum values back to schema defaults (0.96 / "vk" /
	// "system_plus_last_user" / false) so a partial payload from an older
	// client behaves as a no-op for these fields.
	Threshold       *float64 `json:"threshold,omitempty"`
	VaryBy          *string  `json:"varyBy,omitempty"`
	EmbedStrategy   *string  `json:"embedStrategy,omitempty"`
	AllowCrossModel *bool    `json:"allowCrossModel,omitempty"`
}

// GetConfig returns the current singleton SemanticCacheConfigRow as JSON.
// Mirror of aiguard.Handler.GetConfig.
func (h *SemanticCacheHandler) GetConfig(c echo.Context) error {
	row, err := h.store.Get(c.Request().Context())
	if err != nil {
		h.logger.Error("semantic-cache: get config", "error", err)
		return c.JSON(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, row)
}

// PutConfig validates + saves the singleton row, fires a Hub Category B
// invalidation so ai-gateway Things reload their in-process L1 snapshot,
// and emits an admin audit entry on success.
//
// Validation:
//   - If enabled=true and no (providerID, modelID) pair is set → 400
//   - EmbeddingDimension must be > 0 when providerID+modelID are set
//
// Fingerprint recomputation and index-name bumping happen inside
// configstore.SemanticCacheStore.Save.
func (h *SemanticCacheHandler) PutConfig(c echo.Context) error {
	var req semanticCacheUpdateRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{
			"error": "malformed_json", "detail": err.Error(),
		})
	}

	// Validation: enabling without a model is an operator error.
	hasModel := req.EmbeddingProviderID != nil && *req.EmbeddingProviderID != "" &&
		req.EmbeddingModelID != nil && *req.EmbeddingModelID != ""
	if req.Enabled && !hasModel {
		return c.JSON(http.StatusBadRequest, map[string]any{
			"error": "enabling semantic cache requires embeddingProviderId and embeddingModelId",
		})
	}

	// A supplied dimension must be positive. nil is allowed when a model is set:
	// the store derives the model's default_dimension from its capabilityJson.
	// Compatibility of a supplied dimension with the model's supported_dimensions
	// is validated in the store (ErrUnsupportedEmbeddingDimension) so an operator
	// cannot persist a value the gateway would 400 on for every embed call.
	if hasModel && req.EmbeddingDimension != nil && *req.EmbeddingDimension <= 0 {
		return c.JSON(http.StatusBadRequest, map[string]any{
			"error": "embeddingDimension must be a positive integer",
		})
	}

	actor := actorFromContext(c)

	// Preserve current tuning fields when the client omits them so a PUT
	// that only flips Enabled doesn't reset the tuning to schema defaults.
	current, err := h.store.Get(c.Request().Context())
	if err != nil {
		h.logger.Error("semantic-cache: load current for tuning preserve", "error", err)
		return c.JSON(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
	threshold := current.Threshold
	if req.Threshold != nil {
		threshold = *req.Threshold
	}
	varyBy := current.VaryBy
	if req.VaryBy != nil {
		varyBy = *req.VaryBy
	}
	embedStrategy := current.EmbedStrategy
	if req.EmbedStrategy != nil {
		embedStrategy = *req.EmbedStrategy
	}
	allowCrossModel := current.AllowCrossModel
	if req.AllowCrossModel != nil {
		allowCrossModel = *req.AllowCrossModel
	}

	saved, err := h.store.Save(c.Request().Context(), configstore.SaveInput{
		EmbeddingProviderID: req.EmbeddingProviderID,
		EmbeddingModelID:    req.EmbeddingModelID,
		EmbeddingDimension:  req.EmbeddingDimension,
		Enabled:             req.Enabled,
		Threshold:           threshold,
		VaryBy:              varyBy,
		EmbedStrategy:       embedStrategy,
		AllowCrossModel:     allowCrossModel,
		UpdatedBy:           actor.UserID,
	})
	if err != nil {
		// Capability validation failures are operator errors → 400, not 500.
		if errors.Is(err, configstore.ErrUnsupportedEmbeddingDimension) ||
			errors.Is(err, configstore.ErrEmbeddingDimensionRequired) {
			return c.JSON(http.StatusBadRequest, map[string]any{"error": err.Error()})
		}
		h.logger.Error("semantic-cache: save config", "error", err)
		return c.JSON(http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}

	// Push the full saved row to the Hub shadow so ai-gateway IndexLifecycle
	// observes the new embedding fingerprint and ensures the RediSearch
	// FT index (semantic.IndexLifecycle.OnConfigSnapshot at
	// packages/ai-gateway/internal/cache/semantic/index_lifecycle.go:47).
	// Without the full state in the broadcast, the gateway's
	// registerAGSemanticCacheConfig handler sees an empty blob, calls
	// IndexLifecycle.OnConfigSnapshot with Enabled=false, and L2 stays
	// disabled fleet-wide — even when this admin row has enabled=true.
	// Propagation failure is logged but not escalated to 502 — the DB
	// write succeeded and the reconcile job recovers within 60s.
	// Mirrors time_sensitive.go:301 (pushTimeSensitiveToHub).
	if h.hub != nil {
		if _, hubErr := h.hub.NotifyConfigChange(c.Request().Context(), hub.ConfigChangeRequest{
			ThingType: "ai-gateway",
			ConfigKey: configkey.SemanticCacheConfig,
			State:     saved,
			ActorID:   actor.UserID,
			ActorName: actor.Name,
		}); hubErr != nil {
			h.logger.Warn("semantic-cache: hub push failed (reconcile job will recover)",
				"error", hubErr)
		}
	}

	h.emitAudit(c, audit.EntryFor(c, iam.ResourceSemanticCache, iam.VerbUpdate), saved)

	return c.JSON(http.StatusOK, saved)
}

// emitAudit publishes an admin-audit entry on successful mutation.
// nil-safe on the writer pointer so test construction stays ergonomic.
func (h *SemanticCacheHandler) emitAudit(c echo.Context, e audit.Entry, saved *configstore.SemanticCacheConfigRow) {
	if h.audit == nil {
		return
	}
	e.EntityID = "singleton"
	e.AfterState = semanticCacheAuditSummary(saved)
	h.audit.LogObserved(c.Request().Context(), e)
}

// semanticCacheAuditSummary returns a compact projection of the saved row
// suitable for admin-audit AfterState payloads. row is guaranteed non-nil
// at all call sites (always called right after a successful Save).
func semanticCacheAuditSummary(row *configstore.SemanticCacheConfigRow) map[string]any {
	providerID := ""
	if row.EmbeddingProviderID != nil {
		providerID = *row.EmbeddingProviderID
	}
	modelID := ""
	if row.EmbeddingModelID != nil {
		modelID = *row.EmbeddingModelID
	}
	dim := 0
	if row.EmbeddingDimension != nil {
		dim = *row.EmbeddingDimension
	}
	return map[string]any{
		"embeddingProviderId":  providerID,
		"embeddingModelId":     modelID,
		"embeddingDimension":   dim,
		"embeddingFingerprint": row.EmbeddingFingerprint,
		"redisIndexName":       row.RedisIndexName,
		"enabled":              row.Enabled,
	}
}
