// Package analytics owns the Control Plane admin API for read-only
// analytics surfaces: /analytics/* (provider/user/cost/routing/
// quality/cache-roi/cost-summary) and /metrics/* (per-Thing rollup
// aggregates + latency phase percentiles). R6 eighth domain extracted
// from the flat handler/ package; recipe documented in
// docs/_archive/2026-q2/programs/r6-handler-decomp-runbook.md.
//
// Read-only domain: no Hub, no Audit (no mutations). Only DB + Logger.
package analytics

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	cpgx "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/pgx"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/settings/store/metricsstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/analytics/analyticsstore"
)

// pgxQueryer is the minimal pgx pool surface this handler exercises for
// direct SQL sites (queries composed inline rather than extracted to
// store/). *pgxpool.Pool satisfies it in production; pgxmock's
// PgxPoolIface satisfies it in tests. Same seam used by sibling
// handler/passthrough.
type pgxQueryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Deps is the construction-time arg shape.
type Deps struct {
	Pool   cpgx.PgxPool
	Logger *slog.Logger
	// ExcludeInternalOpsFromBilledCost mirrors the Hub yaml flag. Surfaced on
	// cost-summary so the UI can render the correct hint label per row.
	ExcludeInternalOpsFromBilledCost bool
}

// Handler is the per-domain admin handler for /api/admin/{analytics,
// metrics}* endpoints.
type Handler struct {
	metrics   *metricsstore.Store
	analytics *analyticsstore.Store
	logger    *slog.Logger
	// pool is the direct SQL surface — satisfies pgxQueryer in production
	// and is overridden in tests via a pgxmock pool. Keeps every direct
	// SQL site behind one indirection so pgxmock can stand in.
	pool                             pgxQueryer
	excludeInternalOpsFromBilledCost bool
}

// New constructs an analytics Handler from its narrow Deps.
func New(d Deps) *Handler {
	h := &Handler{
		logger:                           d.Logger,
		excludeInternalOpsFromBilledCost: d.ExcludeInternalOpsFromBilledCost,
	}
	if d.Pool != nil {
		h.pool = d.Pool
		h.metrics = metricsstore.New(d.Pool)
		h.analytics = analyticsstore.New(d.Pool)
	}
	return h
}

// --- Helper-copies (R6 runbook §4.2 option 1) ---

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

// _ keeps middleware imported even if no path in this file calls it
// directly — analytics methods reference middleware.AdminAuthFromContext
// indirectly via Echo helpers.
var _ = middleware.AdminAuthFromContext

type pagination struct {
	Limit  int
	Offset int
}

// parseRFC3339Flexible parses a time string in either RFC3339Nano or
// plain RFC3339. Returns (zero, false) on failure. Local copy of
// handler/helpers.go's parseRFC3339Flexible (R6 helper-copy strategy).
func parseRFC3339Flexible(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
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
