// Package dlq owns the CP admin API for the traffic_event_dlq dead-letter
// queue admin surface. List + Retry endpoints proxy to the Hub's
// /api/hub/dlq routes (which own the table + MQ producer); CP adds the
// JWT-auth + IAM scope + AdminAuditLog wrap that the Hub deliberately
// does not.
package dlq

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v4"

	cpaudit "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// HubClient is the narrow surface the DLQ handler needs from
// platform/hub.Client. Declared here so the test suite can substitute a
// stub without dragging in nexushttp and net/http machinery.
type HubClient interface {
	ListDLQ(ctx context.Context, subject, limit, cursor string) ([]byte, int, error)
	RetryDLQ(ctx context.Context, id string) ([]byte, int, error)
}

// Deps wires the handler. Audit may be nil in tests (LogObserved is a
// no-op then); Hub must be non-nil for the routes to function.
type Deps struct {
	Hub    HubClient
	Audit  *cpaudit.Writer
	Logger *slog.Logger
}

// Handler implements the two admin DLQ endpoints.
type Handler struct {
	hub    HubClient
	audit  *cpaudit.Writer
	logger *slog.Logger
}

// New returns a Handler. nil Logger falls back to slog.Default so callers
// can omit it in tests.
func New(d Deps) *Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{hub: d.Hub, audit: d.Audit, logger: logger}
}

// RegisterDLQRoutes wires the GET /observability/dlq + POST
// /observability/dlq/:id/retry endpoints onto the supplied admin group.
//
// IAM:
//   - GET   → observability-dlq.read   (non-destructive: list rows + payload sizes)
//   - POST  → observability-dlq.manage (destructive: republishes raw bytes
//     back to MQ + deletes the DLQ row)
//
// The two distinct verbs let admins grant "see what's in DLQ" without
// granting "re-inject arbitrary captured payloads" — the retry verb is
// equivalent to "publish to MQ on behalf of the original producer".
func (h *Handler) RegisterDLQRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/observability/dlq", h.ListDLQ, iamMW(iam.ResourceObservabilityDLQ.Action(iam.VerbRead)))
	g.POST("/observability/dlq/:id/retry", h.RetryDLQ, iamMW(iam.ResourceObservabilityDLQ.Action(iam.VerbManage)))
}

// ListDLQ proxies GET /admin/observability/dlq to Hub. Query params
// (subject / limit / cursor) flow through unchanged; the response body
// is forwarded verbatim so the UI binds to the same shape Hub emits.
func (h *Handler) ListDLQ(c echo.Context) error {
	subject := c.QueryParam("subject")
	limit := c.QueryParam("limit")
	cursor := c.QueryParam("cursor")

	body, status, err := h.hub.ListDLQ(c.Request().Context(), subject, limit, cursor)
	if err != nil {
		h.logger.Error("dlq list: hub call failed", "error", err)
		return c.JSON(http.StatusBadGateway, errJSON("Hub unreachable", "hub_unavailable", ""))
	}
	return c.Blob(status, "application/json", body)
}

// RetryDLQ proxies POST /admin/observability/dlq/:id/retry to Hub, then
// stamps an AdminAuditLog entry on success so the retry is durably
// attributed to the operator (Hub deliberately does NOT write to audit
// itself — CP owns the admin-side audit trail).
func (h *Handler) RetryDLQ(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("id is required", "validation_error", ""))
	}

	body, status, err := h.hub.RetryDLQ(c.Request().Context(), id)
	if err != nil {
		h.logger.Error("dlq retry: hub call failed", "error", err, "id", id)
		return c.JSON(http.StatusBadGateway, errJSON("Hub unreachable", "hub_unavailable", ""))
	}

	// Only audit when Hub reports success — a 404 / 503 path is not a
	// real retry and should not pollute the operator audit trail.
	if status >= 200 && status < 300 && h.audit != nil {
		ae := cpaudit.EntryFor(c, iam.ResourceObservabilityDLQ, iam.VerbManage)
		ae.EntityID = id
		ae.AfterState = map[string]any{"action": "retry"}
		h.audit.LogObserved(c.Request().Context(), ae)
	}

	return c.Blob(status, "application/json", body)
}

// errJSON mirrors the SIEM handler's helper for a consistent admin-API
// error envelope. Duplicated rather than imported because the handler
// stays standalone (no cross-handler runtime coupling).
func errJSON(message, errType, code string) map[string]any {
	return map[string]any{"error": map[string]any{"message": message, "type": errType, "code": code}}
}
