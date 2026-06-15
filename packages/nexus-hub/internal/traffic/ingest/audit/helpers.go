package audit

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	nexushttperr "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/httperr"
)

func badRequest(c echo.Context, msg string) error {
	return c.JSON(http.StatusBadRequest, nexushttperr.ErrJSON(msg, "validation_error", "INVALID_REQUEST"))
}

func serviceUnavailable(c echo.Context, msg string) error {
	return c.JSON(http.StatusServiceUnavailable, nexushttperr.ErrJSON(msg, "service_unavailable", "SERVICE_UNAVAILABLE"))
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
