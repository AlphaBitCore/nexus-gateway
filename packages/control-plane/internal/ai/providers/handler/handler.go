// Package providers owns the Control Plane admin API for the
// provider/model/credential cluster: provider CRUD, model CRUD,
// credential CRUD + rotation, reliability config. R6 sixth (and
// largest) domain extracted from the flat handler/ package; recipe
// in docs/_archive/2026-q2/programs/r6-handler-decomp-runbook.md.
package providers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/credstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/modelstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/providerstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/crypto"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	cpgx "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/pgx"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
)

// HubInvalidator is the narrow Hub surface providers/ needs:
// fire-and-forget InvalidateConfig (every CUD path fan-outs to
// ai-gateway's provider/model/credential caches). Same shape as
// hooks/HubInvalidator — could converge with that pattern in a
// future runbook follow-up.
type HubInvalidator interface {
	InvalidateConfig(ctx context.Context, thingType, configKey string)
}

// ProxyConfig is the BFF proxy snapshot providers/ needs to call
// out to ai-gateway for credential probe + reliability tests.
type ProxyConfig struct {
	ComplianceProxyRuntimeURL string
	ComplianceProxyAPIToken   string
	AIGatewayURL              string
}

// Deps is the construction-time arg shape.
type Deps struct {
	Pool       cpgx.PgxPool
	Hub        HubInvalidator
	Audit      *audit.Writer
	Logger     *slog.Logger
	Vault      *crypto.Vault      // legacy single-key encryption; nil if disabled
	MultiVault *crypto.MultiVault // multi-key encryption; takes precedence
	Proxy      ProxyConfig
	Redis      redis.UniversalClient // circuit-reset writes
}

// Handler is the per-domain admin handler for /api/admin/{providers,
// models, credentials}* endpoints.
type Handler struct {
	providers  *providerstore.Store
	meta       *systemmetastore.Store
	creds      *credstore.Store
	models     *modelstore.Store
	hub        HubInvalidator
	audit      *audit.Writer
	logger     *slog.Logger
	vault      *crypto.Vault
	multiVault *crypto.MultiVault
	proxy      ProxyConfig
	redis      redis.UniversalClient
}

// New constructs a providers Handler from its narrow Deps.
func New(d Deps) *Handler {
	h := &Handler{
		hub:        d.Hub,
		audit:      d.Audit,
		logger:     d.Logger,
		vault:      d.Vault,
		multiVault: d.MultiVault,
		proxy:      d.Proxy,
		redis:      d.Redis,
	}
	if d.Pool != nil {
		h.providers = providerstore.New(d.Pool)
		h.meta = systemmetastore.New(d.Pool)
		h.creds = credstore.New(d.Pool)
		h.models = modelstore.New(d.Pool)
	}
	return h
}

// RegisterRoutes funcs (RegisterProviderRoutes, RegisterModelRoutes,
// RegisterCredentialRoutes, RegisterReliabilitySettingsRoutes) live
// in their respective per-domain files (providers.go, models.go,
// credentials.go, credential_reliability.go) — sed-ported verbatim
// from the pre-extraction admin_X.go files so the canonical IAM
// strings (which differ per-route in surprising ways, e.g. some
// /providers/:id/models routes gate on iam.ResourceModel not
// iam.ResourceProvider) survive the move unchanged.

func errJSON(message, errType, code string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	}
}

func internalServerError(c echo.Context, msg string) error {
	return c.JSON(http.StatusInternalServerError, errJSON(msg, "server_error", ""))
}

type actor struct {
	UserID string
	Name   string
}

func actorFromContext(c echo.Context) actor {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return actor{}
	}
	return actor{UserID: aa.KeyID, Name: aa.KeyName}
}

type pagination struct {
	Limit  int
	Offset int
}

func parsePagination(c echo.Context) pagination {
	limit := 50
	offset := 0
	if v := c.QueryParam("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
			if limit > 1000 {
				limit = 1000
			}
		}
	}
	if v := c.QueryParam("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return pagination{Limit: limit, Offset: offset}
}

// incrementConfigVersion atomically increments the agent config
// version. Local copy of *AdminHandler.incrementConfigVersion.
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
