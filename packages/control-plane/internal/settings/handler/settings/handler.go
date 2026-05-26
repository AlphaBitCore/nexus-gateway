// Package settings owns the Control Plane admin API for platform-wide
// gateway settings, agent (device-defaults) settings, and the
// device-auth mode toggle. First R6 domain extracted from the flat
// handler/ package; canonical pattern recorded in
// docs/_archive/2026-q2/programs/r6-handler-decomp-runbook.md.
package settings

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	authserver_store "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// HubInvalidator is the narrow Hub surface settings/ needs: fire-and-forget
// per-(thingType, configKey) config invalidation.
type HubInvalidator interface {
	InvalidateConfig(ctx context.Context, thingType, configKey string)
}

// payloadCaptureMetadataStore is the narrow view of system_metadata r/w
// used by GetPayloadCaptureConfig / UpdatePayloadCaptureConfig.
// Tests inject an in-memory double via Handler.payloadCaptureMetaStore;
// production falls back to h.db which satisfies this interface.
type payloadCaptureMetadataStore interface {
	GetSystemMetadata(ctx context.Context, key string) (json.RawMessage, error)
	SetSystemMetadata(ctx context.Context, key string, value any, updatedBy string) error
}

// Deps is the construction-time arg shape. main.go (or admin_routes.go)
// assembles the parent AdminHandler-equivalent value and passes the
// smaller Deps subset the settings methods need.
type Deps struct {
	Pool   *pgxpool.Pool          // concrete pool for authserver_store.NewIdPStore
	Meta   *systemmetastore.Store // for GetSystemMetadata/SetSystemMetadata
	Hub    HubInvalidator         // may be nil — handlers tolerate it
	Audit  *audit.Writer
	Logger *slog.Logger
}

// Handler is the per-domain admin handler for /api/admin/settings/*
// endpoints. Holds only the dependencies its methods touch — no
// cross-domain bleed from the original AdminHandler god-object.
type Handler struct {
	pool   *pgxpool.Pool          // concrete pool for authserver_store.NewIdPStore
	meta   *systemmetastore.Store // for GetSystemMetadata/SetSystemMetadata
	hub    HubInvalidator
	audit  *audit.Writer
	logger *slog.Logger

	// listIdPsFn is the unit-test seam for device-auth IdP enumeration.
	// Default uses authserver_store.NewIdPStore(h.pool) which requires
	// a concrete *pgxpool.Pool; tests inject a fake to avoid standing up
	// a real Postgres just to verify the categorisation + mode-validator
	// branches. Production callers (cmd/control-plane/main.go) never set
	// this field — they go through New(), which leaves it nil so the
	// helper falls back to the real store.
	listIdPsFn func(ctx context.Context) ([]authserver_store.IdentityProvider, error)

	// payloadCaptureMetaStore overrides the payload-capture settings
	// handler's system_metadata reads/writes in tests. Nil in production:
	// the handler falls back to h.meta.
	payloadCaptureMetaStore payloadCaptureMetadataStore
}

// New constructs a settings Handler from its narrow Deps.
func New(d Deps) *Handler {
	return &Handler{pool: d.Pool, meta: d.Meta, hub: d.Hub, audit: d.Audit, logger: d.Logger}
}

// RegisterRoutes registers system settings routes.
//
// The /settings/sso* routes are gone — SSO configuration lives on per-IdP
// rows under /api/admin/identity-providers (handled by
// admin_identity_provider.go). The SystemMetadata["sso.config"] blob is
// not read here; existing blobs were converted to IdentityProvider rows.
func (h *Handler) RegisterRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/settings", h.GetSettings, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
	g.PUT("/settings", h.UpdateSettings, iamMW(iam.ResourceSettings.Action(iam.VerbUpdate)))
	g.GET("/settings/device-auth", h.GetDeviceAuthSettings, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
	g.PUT("/settings/device-auth", h.UpdateDeviceAuthSettings, iamMW(iam.ResourceSettings.Action(iam.VerbUpdate)))
	// Device-fleet runtime defaults (audit policy, forensics, shutdown
	// warning copy). Carved into ResourceDeviceDefaults so the compliance
	// team can manage agent audit behavior without holding write on every
	// platform setting.
	g.GET("/settings/device-defaults", h.GetAgentSettings, iamMW(iam.ResourceDeviceDefaults.Action(iam.VerbRead)))
	g.PUT("/settings/device-defaults", h.UpdateAgentSettings, iamMW(iam.ResourceDeviceDefaults.Action(iam.VerbUpdate)))

	// Setup wizard state
	g.GET("/setup-state", h.GetSetupState, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
	g.PUT("/setup-state", h.UpdateSetupState, iamMW(iam.ResourceSettings.Action(iam.VerbUpdate)))

	// Cache management (flush all shadow config + IAM cache)
	g.GET("/cache/stats", h.CacheStats, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
	g.POST("/cache/flush", h.CacheFlush, iamMW(iam.ResourceSettings.Action(iam.VerbUpdate)))

	// Observability settings: separate resource so granting observability
	// read does not imply read on SIEM / payload-capture.
	g.GET("/settings/observability", h.GetObservability, iamMW(iam.ResourceObservability.Action(iam.VerbRead)))
	g.PUT("/settings/observability", h.UpdateObservability, iamMW(iam.ResourceObservability.Action(iam.VerbWrite)))

	// Payload capture settings. Cross-service data-retention / privacy
	// decision; carved into its own resource.
	g.GET("/settings/payload-capture", h.GetPayloadCaptureConfig, iamMW(iam.ResourcePayloadCapture.Action(iam.VerbRead)))
	g.PUT("/settings/payload-capture", h.UpdatePayloadCaptureConfig, iamMW(iam.ResourcePayloadCapture.Action(iam.VerbUpdate)))

	// Global StreamingPolicy default. Per-resource overrides live on
	// interception_domain and Provider; only the global default needs its
	// own admin route.
	g.GET("/settings/streaming-compliance", h.GetStreamingComplianceConfig, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
	g.PUT("/settings/streaming-compliance", h.UpdateStreamingComplianceConfig, iamMW(iam.ResourceSettings.Action(iam.VerbUpdate)))

	// Note: /instances and ReadinessCheck are registered in
	// infrastructure/infra/readiness.go via infraHandler.RegisterReadinessRoutes.
	// The public /ready endpoint is registered in cmd/control-plane/main.go.
}

// errJSON builds a canonical JSON error envelope used across admin
// handlers.
func errJSON(message, errType, code string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	}
}

// incrementConfigVersion atomically increments the agent config version
// stored in system_metadata so that agents can detect configuration
// changes. Errors are logged but not propagated — a missed increment is
// non-fatal; the agent will pick up changes on the next full poll.
// Local copy of *AdminHandler.incrementConfigVersion per the R6 runbook
// helper-copy strategy.
func (h *Handler) incrementConfigVersion(ctx context.Context) {
	const key = "agent.config.version"
	version := 0
	raw, err := h.meta.GetSystemMetadata(ctx, key)
	if err == nil && raw != nil {
		var v int
		if json.Unmarshal(raw, &v) == nil {
			version = v
		}
	}
	version++
	if err := h.meta.SetSystemMetadata(ctx, key, version, "system"); err != nil {
		h.logger.Error("increment agent config version", "error", err)
	}
}

// internalServerError is the canonical 500 used across this domain.
// Wraps errJSON so each handler doesn't repeat the same boilerplate.
func internalServerError(c echo.Context, msg string) error {
	return c.JSON(http.StatusInternalServerError, errJSON(msg, "server_error", ""))
}
