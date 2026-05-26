// Package dsar owns the Control Plane admin API for Data Subject
// Access Requests (DSAR / GDPR). R8-B4 — small leaf extraction.
package dsar

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/governance/dsar/dsarstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
)

// dsarDB is the narrow persistence seam used by Handler. In production
// *dsarstore.Store satisfies this; tests supply an in-memory double.
type dsarDB interface {
	ListDSARRequests(ctx context.Context, status string, limit, offset int) ([]dsarstore.DSARRequest, int, error)
	GetDSARRequest(ctx context.Context, id string) (*dsarstore.DSARRequest, error)
	CreateDSARRequest(ctx context.Context, p dsarstore.CreateDSARRequestParams) (*dsarstore.DSARRequest, error)
	UpdateDSARRequest(ctx context.Context, id string, p dsarstore.UpdateDSARParams) (*dsarstore.DSARRequest, error)
	FulfillDSARAccess(ctx context.Context, subjectID string) (*dsarstore.DSARAccessExport, error)
	FulfillDSARErasure(ctx context.Context, subjectID string) (*dsarstore.DSARErasureResult, error)
}

// Deps is the construction-time arg shape.
type Deps struct {
	Pool   *pgxpool.Pool
	Audit  *audit.Writer
	Logger *slog.Logger
}

// Handler owns the DSAR admin API surface.
type Handler struct {
	db     dsarDB
	audit  *audit.Writer
	logger *slog.Logger
}

// New constructs a Handler.
func New(d Deps) *Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{db: dsarstore.New(d.Pool), audit: d.Audit, logger: logger}
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

type Actor struct{ UserID, Name string }

func actorFromContext(c echo.Context) Actor {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return Actor{}
	}
	return Actor{UserID: aa.KeyID, Name: aa.KeyName}
}

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
