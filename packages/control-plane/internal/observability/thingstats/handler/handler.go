// Package thingstats owns the Control Plane admin API for per-Thing
// metric rollup stats. R8-B16 leaf extraction.
package thingstats

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/thingstats/thingstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
)

// thingOperations is the minimal interface from thingstore.Store that the
// handler needs, allowing test doubles to inject controlled responses
// without a real database connection.
type thingOperations interface {
	GetThing(ctx context.Context, id string) (*thingstore.ThingRegistry, error)
	QueryThingRollup(ctx context.Context, q thingstore.ThingMetricsQuery) ([]metrics.ThingRollupRow, error)
	ThingRollupHasAnyRecent(ctx context.Context, thingID string, start, end time.Time) (bool, error)
}

// queryPool is the minimal interface from pgxpool.Pool that resolveDimensionNames
// needs, allowing test doubles to inject controlled query errors without a
// real database connection.
type queryPool interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type Deps struct {
	Pool   *pgxpool.Pool
	Audit  *audit.Writer
	Logger *slog.Logger
}

type Handler struct {
	thing  thingOperations
	pool   queryPool // kept for Pool.Query in resolveDimensionNames
	audit  *audit.Writer
	logger *slog.Logger
}

func New(d Deps) *Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		thing:  thingstore.New(d.Pool),
		pool:   d.Pool,
		audit:  d.Audit,
		logger: logger,
	}
}

func errJSON(message, errType, code string) map[string]any {
	return map[string]any{
		"error": map[string]any{"message": message, "type": errType, "code": code},
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

// parseRFC3339Flexible parses an RFC3339 timestamp string, accepting both
// nanosecond-precision (RFC3339Nano) and second-precision (RFC3339) forms.
// Only the RFC3339Nano branch is reachable: Go's time.RFC3339Nano layout is
// a superset of time.RFC3339 (RFC3339Nano = "2006-01-02T15:04:05.999999999Z07:00",
// RFC3339 = "2006-01-02T15:04:05Z07:00") — any string matched by RFC3339 is
// also matched by RFC3339Nano. The RFC3339 fallback is retained as a
// defensive guard against future stdlib changes but cannot be reached today.
func parseRFC3339Flexible(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func parseTimeRange(c echo.Context) (start, end *time.Time) {
	if v := c.QueryParam("startTime"); v != "" {
		if t, ok := parseRFC3339Flexible(v); ok {
			start = &t
		}
	}
	if v := c.QueryParam("endTime"); v != "" {
		if t, ok := parseRFC3339Flexible(v); ok {
			end = &t
		}
	}
	return
}
