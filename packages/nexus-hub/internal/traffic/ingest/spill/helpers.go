package spill

import (
	"net/http"

	"github.com/labstack/echo/v4"

	nexushttperr "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/httperr"
)

func badRequest(c echo.Context, msg string) error {
	return c.JSON(http.StatusBadRequest, nexushttperr.ErrJSON(msg, "validation_error", "INVALID_REQUEST"))
}

func unauthorized(c echo.Context, msg string) error {
	return c.JSON(http.StatusUnauthorized, nexushttperr.ErrJSON(msg, "auth_error", "UNAUTHORIZED"))
}

func internalError(c echo.Context, msg string) error {
	return c.JSON(http.StatusInternalServerError, nexushttperr.ErrJSON(msg, "internal_error", "INTERNAL_ERROR"))
}

func serviceUnavailable(c echo.Context, msg string) error {
	return c.JSON(http.StatusServiceUnavailable, nexushttperr.ErrJSON(msg, "service_unavailable", "SERVICE_UNAVAILABLE"))
}
