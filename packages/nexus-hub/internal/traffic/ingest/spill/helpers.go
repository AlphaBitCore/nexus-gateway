package spill

import (
	"net/http"

	"github.com/labstack/echo/v4"
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

func internalError(c echo.Context, msg string) error {
	return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: msg, Code: "INTERNAL_ERROR"})
}

func serviceUnavailable(c echo.Context, msg string) error {
	return c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: msg, Code: "SERVICE_UNAVAILABLE"})
}
