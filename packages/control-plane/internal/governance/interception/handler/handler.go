// Package interception owns the Control Plane admin API for the
// compliance-proxy interception-domain catalog. R8-B16 leaf extraction.
package interception

import (
	"context"
	jsonImpl "encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/interception/interceptionstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store/systemmetastore"
)

type HubAPI interface {
	NotifyConfigChange(ctx context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error)
	InvalidateConfig(ctx context.Context, thingType, configKey string)
}

// interceptionDB is the narrow persistence seam for the interception handler.
// *interceptionstore.Store satisfies this in production; tests supply an
// in-memory double without standing up a real DB.
type interceptionDB interface {
	ListInterceptionDomains(ctx context.Context, p interceptionstore.InterceptionDomainListParams) (*interceptionstore.ListInterceptionDomainsResult, error)
	GetInterceptionDomain(ctx context.Context, id string) (*interceptionstore.InterceptionDomainRow, error)
	CreateInterceptionDomain(ctx context.Context, in interceptionstore.CreateInterceptionDomainInput) (*interceptionstore.InterceptionDomainRow, error)
	UpdateInterceptionDomain(ctx context.Context, id string, in interceptionstore.UpdateInterceptionDomainInput) (*interceptionstore.InterceptionDomainRow, error)
	DeleteInterceptionDomain(ctx context.Context, id string) error
	GetInterceptionPath(ctx context.Context, id string) (*interceptionstore.InterceptionPathRow, error)
	CreateInterceptionPath(ctx context.Context, domainID string, in interceptionstore.CreateInterceptionPathInput) (*interceptionstore.InterceptionPathRow, error)
	UpdateInterceptionPath(ctx context.Context, id string, in interceptionstore.UpdateInterceptionPathInput) (*interceptionstore.InterceptionPathRow, error)
	DeleteInterceptionPath(ctx context.Context, id string) error
}

type Deps struct {
	Pool   interceptionstore.PgxPool
	Meta   *systemmetastore.Store
	Hub    HubAPI
	Audit  *audit.Writer
	Logger *slog.Logger
}

type Handler struct {
	store  interceptionDB
	meta   *systemmetastore.Store
	hub    HubAPI
	audit  *audit.Writer
	logger *slog.Logger
}

func New(d Deps) *Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		store:  interceptionstore.New(d.Pool),
		meta:   d.Meta,
		hub:    d.Hub,
		audit:  d.Audit,
		logger: logger,
	}
}

func errJSON(message, errType, code string) map[string]any {
	return map[string]any{"error": map[string]any{"message": message, "type": errType, "code": code}}
}

type Actor struct{ UserID, Name string }

func actorFromContext(c echo.Context) Actor {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return Actor{}
	}
	return Actor{UserID: aa.KeyID, Name: aa.KeyName}
}

func sourceIP(c echo.Context) string { return c.RealIP() }

type pagination struct{ Limit, Offset int }

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

func internalServerError(c echo.Context, msg string) error {
	return c.JSON(http.StatusInternalServerError, errJSON(msg, "server_error", ""))
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// incrementConfigVersion mirrors handler.incrementConfigVersion.
func (h *Handler) incrementConfigVersion(ctx context.Context) {
	if h.meta == nil {
		return
	}
	const key = "agent.config.version"
	version := 0
	raw, err := h.meta.GetSystemMetadata(ctx, key)
	if err == nil && raw != nil {
		var v int
		if err := jsonUnmarshal(raw, &v); err == nil {
			version = v
		}
	}
	version++
	if err := h.meta.SetSystemMetadata(ctx, key, version, "system"); err != nil {
		h.logger.Error("increment agent config version", "error", err)
	}
}

// jsonUnmarshal is a tiny alias to avoid importing encoding/json at
// every helper file.
var jsonUnmarshal = jsonImpl.Unmarshal
