// Package opsmetrics owns the CP admin API for ops-metrics queries.
package opsmetrics

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/httperr"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/opsmetrics/opsstore"
)

type Deps struct {
	Pool   opsstore.PgxPool
	Logger *slog.Logger
}

type Handler struct {
	ops    *opsstore.Store
	logger *slog.Logger
}

func New(d Deps) *Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{ops: opsstore.New(d.Pool), logger: logger}
}

// errJSON is the canonical admin error envelope helper (see internal/platform/httperr).
var errJSON = httperr.ErrJSON

func internalServerError(c echo.Context, msg string) error {
	return c.JSON(http.StatusInternalServerError, errJSON(msg, "server_error", ""))
}

func parseRFC3339Flexible(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// _ keeps strings imported for the file-level reference in opsmetrics.go.
var _ = strings.TrimSpace
