// Helper copies for the per-domain sub-package.
package enroll

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// ErrorResponse matches the OpenAPI ErrorResponse schema.
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func badRequest(c echo.Context, msg string) error {
	return c.JSON(http.StatusBadRequest, ErrorResponse{Error: msg, Code: "INVALID_REQUEST"})
}

func unauthorized(c echo.Context, msg string) error {
	return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: msg, Code: "UNAUTHORIZED"})
}

func forbidden(c echo.Context, msg string) error {
	return c.JSON(http.StatusForbidden, ErrorResponse{Error: msg, Code: "FORBIDDEN"})
}

func notFound(c echo.Context, msg string) error {
	return c.JSON(http.StatusNotFound, ErrorResponse{Error: msg, Code: "NOT_FOUND"})
}

func internalError(c echo.Context, msg string) error {
	return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: msg, Code: "INTERNAL_ERROR"})
}

func serviceUnavailable(c echo.Context, msg string) error {
	return c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: msg, Code: "SERVICE_UNAVAILABLE"})
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
