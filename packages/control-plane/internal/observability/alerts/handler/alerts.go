package alerts

import (
	"net/http"
	"net/url"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

func (h *Handler) ListAlerts(c echo.Context) error {
	return h.hubAlertForward(c, http.MethodGet, "/api/v1/admin/alerts")
}

func (h *Handler) GetAlert(c echo.Context) error {
	return h.hubAlertForward(c, http.MethodGet, "/api/v1/admin/alerts/"+url.PathEscape(c.Param("id")))
}

func (h *Handler) AckAlert(c echo.Context) error {
	id := c.Param("id")
	return h.hubAlertForwardMutating(c, http.MethodPost, "/api/v1/admin/alerts/"+url.PathEscape(id)+"/ack", iam.VerbAcknowledge, "alert", id)
}

func (h *Handler) ResolveAlert(c echo.Context) error {
	id := c.Param("id")
	return h.hubAlertForwardMutating(c, http.MethodPost, "/api/v1/admin/alerts/"+url.PathEscape(id)+"/resolve", iam.VerbAcknowledge, "alert", id)
}

func (h *Handler) ListAlertRules(c echo.Context) error {
	return h.hubAlertForward(c, http.MethodGet, "/api/v1/admin/alerts/rules")
}

func (h *Handler) GetAlertRule(c echo.Context) error {
	return h.hubAlertForward(c, http.MethodGet, "/api/v1/admin/alerts/rules/"+url.PathEscape(c.Param("id")))
}

func (h *Handler) UpdateAlertRule(c echo.Context) error {
	id := c.Param("id")
	return h.hubAlertForwardMutating(c, http.MethodPut, "/api/v1/admin/alerts/rules/"+url.PathEscape(id), iam.VerbUpdate, "alertRule", id)
}

func (h *Handler) ResetAlertRule(c echo.Context) error {
	id := c.Param("id")
	return h.hubAlertForwardMutating(c, http.MethodPost, "/api/v1/admin/alerts/rules/"+url.PathEscape(id)+"/reset", iam.VerbUpdate, "alertRule", id)
}

func (h *Handler) ListAlertChannels(c echo.Context) error {
	return h.hubAlertForward(c, http.MethodGet, "/api/v1/admin/alerts/channels")
}

func (h *Handler) CreateAlertChannel(c echo.Context) error {
	return h.hubAlertForwardMutating(c, http.MethodPost, "/api/v1/admin/alerts/channels", iam.VerbCreate, "alertChannel", "")
}

func (h *Handler) GetAlertChannel(c echo.Context) error {
	return h.hubAlertForward(c, http.MethodGet, "/api/v1/admin/alerts/channels/"+url.PathEscape(c.Param("id")))
}

func (h *Handler) UpdateAlertChannel(c echo.Context) error {
	id := c.Param("id")
	return h.hubAlertForwardMutating(c, http.MethodPut, "/api/v1/admin/alerts/channels/"+url.PathEscape(id), iam.VerbUpdate, "alertChannel", id)
}

func (h *Handler) DeleteAlertChannel(c echo.Context) error {
	id := c.Param("id")
	return h.hubAlertForwardMutating(c, http.MethodDelete, "/api/v1/admin/alerts/channels/"+url.PathEscape(id), iam.VerbDelete, "alertChannel", id)
}

func (h *Handler) TestAlertChannel(c echo.Context) error {
	id := c.Param("id")
	return h.hubAlertForwardMutating(c, http.MethodPost, "/api/v1/admin/alerts/channels/"+url.PathEscape(id)+"/test", iam.VerbUpdate, "alertChannel", id)
}
