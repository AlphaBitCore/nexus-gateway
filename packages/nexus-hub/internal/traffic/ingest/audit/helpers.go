package audit

import (
	"net/http"

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

func serviceUnavailable(c echo.Context, msg string) error {
	return c.JSON(http.StatusServiceUnavailable, ErrorResponse{Error: msg, Code: "SERVICE_UNAVAILABLE"})
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
