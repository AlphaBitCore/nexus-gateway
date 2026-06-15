// Helper copies for the per-domain sub-package.
package diag

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	nexushttperr "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/httperr"
)

func badRequest(c echo.Context, msg string) error {
	return c.JSON(http.StatusBadRequest, nexushttperr.ErrJSON(msg, "validation_error", "INVALID_REQUEST"))
}

func unauthorized(c echo.Context, msg string) error {
	return c.JSON(http.StatusUnauthorized, nexushttperr.ErrJSON(msg, "auth_error", "UNAUTHORIZED"))
}

func forbidden(c echo.Context, msg string) error {
	return c.JSON(http.StatusForbidden, nexushttperr.ErrJSON(msg, "auth_error", "FORBIDDEN"))
}

func notFound(c echo.Context, msg string) error {
	return c.JSON(http.StatusNotFound, nexushttperr.ErrJSON(msg, "not_found", "NOT_FOUND"))
}

func internalError(c echo.Context, msg string) error {
	return c.JSON(http.StatusInternalServerError, nexushttperr.ErrJSON(msg, "internal_error", "INTERNAL_ERROR"))
}

func serviceUnavailable(c echo.Context, msg string) error {
	return c.JSON(http.StatusServiceUnavailable, nexushttperr.ErrJSON(msg, "service_unavailable", "SERVICE_UNAVAILABLE"))
}

func handleErr(c echo.Context, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return notFound(c, "not found")
	}
	return internalError(c, "internal server error")
}

func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 1 {
		return def
	}
	return v
}

func clamp(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func parseTimeOrNil(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

const thingContextKey = "thing"

// ThingFromContext retrieves the validated Thing from the Echo context.
func ThingFromContext(c echo.Context) *store.Thing {
	v := c.Get(thingContextKey)
	if v == nil {
		return nil
	}
	t, _ := v.(*store.Thing)
	return t
}
