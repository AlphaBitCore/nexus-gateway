// Package exemption owns the Control Plane admin API for the unified
// compliance exemption surface:
//   - exemption-grants CRUD (/compliance/exemption-grants*)
//   - request approve/reject (/compliance/exemptions/:id/(approve|reject))
//   - employee request submit (/exemption-requests)
//
// Hub coupling is Cat B-only: every grant write fires
// InvalidateConfig for both receivers — compliance-proxy and agent.
// Hub broadcasts a stateless WS signal that triggers each receiver's
// own DB-backed reload (proxy reads compliance_exemption_grant
// directly via pgx; agent pulls Hub's AgentExemptionsLoader
// projection, which also reads compliance_exemption_grant). Both legs
// see the same grant table — there is no separate agent-side store.
package exemption

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
)

const (
	thingTypeComplianceProxy = "compliance-proxy"
	thingTypeAgent           = "agent"
)

// DataLayer is the store seam for exemption requests and durable
// grants. *store.DB satisfies it in production; unit tests inject a
// stub.
type DataLayer interface {
	CreateExemptionRequest(ctx context.Context, p map[string]any) (*store.ExemptionRequest, error)
	GetExemptionRequest(ctx context.Context, id string) (*store.ExemptionRequest, error)
	MarkExemptionRequestRejected(ctx context.Context, id, reviewerID string) error
	ApproveExemptionRequestWithGrant(ctx context.Context, reqID, reviewerUserID, approverDisplayName string) (*store.ComplianceExemptionGrant, error)
	GetComplianceExemptionGrant(ctx context.Context, id string) (*store.ComplianceExemptionGrant, error)
	GetComplianceExemptionGrantByExemptionRequestID(ctx context.Context, exemptionRequestID string) (*store.ComplianceExemptionGrant, error)
	InsertComplianceExemptionGrant(ctx context.Context, p store.ComplianceExemptionGrantInsert) (*store.ComplianceExemptionGrant, error)
	UpdateComplianceExemptionGrantInactive(ctx context.Context, id string, inactive bool) error
	DeleteComplianceExemptionGrantIfPreActivation(ctx context.Context, id string) (bool, error)
	ListUnifiedExemptionsPage(ctx context.Context, tab string, now time.Time, limit, offset int) ([]store.UnifiedExemptionRow, int, error)
	GetUnifiedExemptionByID(ctx context.Context, id string, now time.Time) (*store.UnifiedExemptionRow, error)
}

// HubAPI is the narrow Hub surface this handler needs. Cat B-only:
// every mutation fires InvalidateConfig and Hub broadcasts a stateless
// WS signal that triggers each receiver's own DB-backed reload.
type HubAPI interface {
	InvalidateConfig(ctx context.Context, thingType, configKey string)
}

// Deps is the construction-time arg shape.
type Deps struct {
	DataLayer DataLayer // production: *store.DB; tests: stub
	Hub       HubAPI    // may be nil — write endpoints return 503
	Audit     *audit.Writer
	Logger    *slog.Logger
}

// Handler owns the exemption admin API surface.
type Handler struct {
	dataLayer DataLayer
	hub       HubAPI
	audit     *audit.Writer
	logger    *slog.Logger
}

// New constructs a Handler from its narrow Deps.
func New(d Deps) *Handler {
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		dataLayer: d.DataLayer,
		hub:       d.Hub,
		audit:     d.Audit,
		logger:    logger,
	}
}

// data returns the active DataLayer.
func (h *Handler) data() DataLayer {
	return h.dataLayer
}

// RegisterRoutes mounts every exemption admin route.
//   - exemption-grants CRUD: /compliance/exemption-grants*, /compliance/exemptions/:id/(approve|reject)
//   - employee submit: /exemption-requests
func (h *Handler) RegisterRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	// Compliance exemption grants + request approve/reject.
	g.GET("/compliance/exemption-grants", h.ListGrants, iamMW(iam.ResourceComplianceExemption.Action(iam.VerbRead)))
	g.POST("/compliance/exemption-grants", h.PostGrant, iamMW(iam.ResourceComplianceExemption.Action(iam.VerbUpdate)))
	g.PATCH("/compliance/exemption-grants/:id", h.PatchGrant, iamMW(iam.ResourceComplianceExemption.Action(iam.VerbUpdate)))
	g.DELETE("/compliance/exemption-grants/:id", h.DeleteGrant, iamMW(iam.ResourceComplianceExemption.Action(iam.VerbDelete)))
	g.GET("/compliance/exemptions/:id", h.GetUnified, iamMW(iam.ResourceComplianceExemption.Action(iam.VerbRead)))
	g.POST("/compliance/exemptions/:id/approve", h.ApproveRequest, iamMW(iam.ResourceComplianceExemption.Action(iam.VerbUpdate)))
	g.POST("/compliance/exemptions/:id/reject", h.RejectRequest, iamMW(iam.ResourceComplianceExemption.Action(iam.VerbReject)))

	// Employee-facing submit.
	g.POST("/exemption-requests", h.CreateRequest, iamMW(iam.ResourceComplianceExemption.Action(iam.VerbUpdate)))
}

// Compliance exemption grants

func (h *Handler) ListGrants(c echo.Context) error {
	tab := strings.ToLower(strings.TrimSpace(c.QueryParam("tab")))
	pg := parsePagination(c)
	now := time.Now().UTC()
	rows, total, err := h.data().ListUnifiedExemptionsPage(c.Request().Context(), tab, now, pg.Limit, pg.Offset)
	if err != nil {
		h.logger.Error("list unified exemptions", "error", err, "tab", tab)
		return c.JSON(http.StatusBadRequest, errJSON(err.Error(), "validation_error", "VALIDATION_ERROR"))
	}
	return c.JSON(http.StatusOK, map[string]any{"rows": rows, "total": total})
}

func (h *Handler) PostGrant(c echo.Context) error {
	var req struct {
		SourceIP        string `json:"sourceIP"`
		TargetHost      string `json:"targetHost"`
		DurationMinutes int    `json:"durationMinutes"`
		Reason          string `json:"reason"`
		EffectiveFrom   string `json:"effectiveFrom"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid body", "validation_error", "VALIDATION_ERROR"))
	}
	if req.SourceIP == "" || req.TargetHost == "" {
		return c.JSON(http.StatusBadRequest, errJSON("sourceIP and targetHost are required", "validation_error", "VALIDATION_ERROR"))
	}
	if req.DurationMinutes <= 0 || req.DurationMinutes > 7*24*60 {
		return c.JSON(http.StatusBadRequest, errJSON("durationMinutes must be in (0, 10080]", "validation_error", "VALIDATION_ERROR"))
	}
	if len(req.Reason) < 4 || len(req.Reason) > 500 {
		return c.JSON(http.StatusBadRequest, errJSON("reason must be 4..500 chars", "validation_error", "VALIDATION_ERROR"))
	}
	if h.hub == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Hub is not configured", "hub_error", "HUB_UNAVAILABLE"))
	}
	a := actorFromContext(c)
	now := time.Now().UTC()
	effectiveFrom := now
	if req.EffectiveFrom != "" {
		t, err := time.Parse(time.RFC3339, req.EffectiveFrom)
		if err != nil {
			return c.JSON(http.StatusBadRequest, errJSON("effectiveFrom must be RFC3339", "validation_error", "VALIDATION_ERROR"))
		}
		effectiveFrom = t.UTC()
	}
	expiresAt := effectiveFrom.Add(time.Duration(req.DurationMinutes) * time.Minute)
	if !expiresAt.After(now) {
		return c.JSON(http.StatusBadRequest, errJSON("computed expiresAt must be in the future", "validation_error", "VALIDATION_ERROR"))
	}
	if !expiresAt.After(effectiveFrom) {
		return c.JSON(http.StatusBadRequest, errJSON("duration must yield expiresAt after effectiveFrom", "validation_error", "VALIDATION_ERROR"))
	}

	ctx := c.Request().Context()
	g, err := h.data().InsertComplianceExemptionGrant(ctx, store.ComplianceExemptionGrantInsert{
		SourceIP:        req.SourceIP,
		TargetHost:      req.TargetHost,
		Reason:          req.Reason,
		DurationMinutes: req.DurationMinutes,
		EffectiveFrom:   effectiveFrom,
		ExpiresAt:       expiresAt,
		RequestedBy:     stringPtr(a.Name),
		ApprovedBy:      a.Name,
	})
	if err != nil {
		h.logger.Error("insert compliance exemption grant", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to create grant", "server_error", "INTERNAL_ERROR"))
	}

	h.invalidateExemptions(ctx, "create", a, c)
	return c.JSON(http.StatusOK, map[string]any{"grant": g})
}

func (h *Handler) PatchGrant(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("id is required", "validation_error", "VALIDATION_ERROR"))
	}
	var body struct {
		Inactive bool `json:"inactive"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid body", "validation_error", "VALIDATION_ERROR"))
	}
	if h.hub == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Hub is not configured", "hub_error", "HUB_UNAVAILABLE"))
	}
	ctx := c.Request().Context()
	if err := h.data().UpdateComplianceExemptionGrantInactive(ctx, id, body.Inactive); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errJSON("Grant not found", "not_found", "NOT_FOUND"))
		}
		h.logger.Error("update grant inactive", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to update grant", "server_error", "INTERNAL_ERROR"))
	}
	a := actorFromContext(c)
	h.invalidateExemptions(ctx, "update", a, c)
	return c.JSON(http.StatusOK, map[string]any{"id": id, "inactive": body.Inactive})
}

func (h *Handler) DeleteGrant(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("id is required", "validation_error", "VALIDATION_ERROR"))
	}
	if h.hub == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Hub is not configured", "hub_error", "HUB_UNAVAILABLE"))
	}
	ctx := c.Request().Context()
	ds := h.data()
	deleted, err := ds.DeleteComplianceExemptionGrantIfPreActivation(ctx, id)
	if err != nil {
		h.logger.Error("delete compliance exemption grant", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to delete grant", "server_error", "INTERNAL_ERROR"))
	}
	if !deleted {
		g, err := ds.GetComplianceExemptionGrant(ctx, id)
		if err != nil {
			h.logger.Error("get grant after delete miss", "error", err)
			return c.JSON(http.StatusInternalServerError, errJSON("Failed to read grant", "server_error", "INTERNAL_ERROR"))
		}
		if g == nil {
			return c.JSON(http.StatusNotFound, errJSON("Grant not found", "not_found", "NOT_FOUND"))
		}
		return c.JSON(http.StatusForbidden, errJSON("Cannot delete a grant that has reached its effective window; disable it instead", "forbidden", "GRANT_ACTIVATED"))
	}
	a := actorFromContext(c)
	h.invalidateExemptions(ctx, "delete", a, c)
	return c.JSON(http.StatusOK, map[string]any{"id": id, "deleted": true})
}

func (h *Handler) GetUnified(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("id is required", "validation_error", "VALIDATION_ERROR"))
	}
	row, err := h.data().GetUnifiedExemptionByID(c.Request().Context(), id, time.Now().UTC())
	if err != nil {
		h.logger.Error("get unified exemption", "error", err, "id", id)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to read exemption", "server_error", "INTERNAL_ERROR"))
	}
	if row == nil {
		return c.JSON(http.StatusNotFound, errJSON("Exemption not found", "not_found", "NOT_FOUND"))
	}
	return c.JSON(http.StatusOK, row)
}

func (h *Handler) ApproveRequest(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("id is required", "validation_error", "VALIDATION_ERROR"))
	}
	if h.hub == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("Hub is not configured", "hub_error", "HUB_UNAVAILABLE"))
	}
	ctx := c.Request().Context()
	ds := h.data()
	a := actorFromContext(c)

	reqRow, err := ds.GetExemptionRequest(ctx, id)
	if err != nil {
		h.logger.Error("get exemption request", "error", err, "id", id)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to read exemption request", "server_error", "INTERNAL_ERROR"))
	}
	if reqRow == nil {
		return c.JSON(http.StatusNotFound, errJSON("Exemption request not found", "not_found", "NOT_FOUND"))
	}
	if reqRow.Status == "APPROVED" {
		g, err := ds.GetComplianceExemptionGrantByExemptionRequestID(ctx, id)
		if err != nil {
			h.logger.Error("get grant by exemption request", "error", err, "id", id)
			return c.JSON(http.StatusInternalServerError, errJSON("Failed to read grant", "server_error", "INTERNAL_ERROR"))
		}
		if g != nil {
			h.invalidateExemptions(ctx, "reconcile", a, c)
			return c.JSON(http.StatusOK, map[string]any{
				"id":        id,
				"status":    "APPROVED",
				"grantId":   g.ID,
				"reapplied": true,
			})
		}
	}
	if reqRow.Status != "PENDING" {
		return c.JSON(http.StatusConflict, errJSON("Exemption request is not pending", "conflict", "CONFLICT"))
	}

	g, err := ds.ApproveExemptionRequestWithGrant(ctx, id, a.UserID, a.Name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusConflict, errJSON("Exemption request is not pending", "conflict", "CONFLICT"))
		}
		h.logger.Error("approve exemption request with grant", "error", err, "id", id)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to approve exemption request", "server_error", "INTERNAL_ERROR"))
	}
	if g == nil {
		return c.JSON(http.StatusNotFound, errJSON("Exemption request not found", "not_found", "NOT_FOUND"))
	}

	h.invalidateExemptions(ctx, "approve", a, c)
	return c.JSON(http.StatusOK, map[string]any{
		"id":      id,
		"status":  "APPROVED",
		"grantId": g.ID,
	})
}

func (h *Handler) RejectRequest(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("id is required", "validation_error", "VALIDATION_ERROR"))
	}
	ctx := c.Request().Context()
	ds := h.data()
	a := actorFromContext(c)

	reqRow, err := ds.GetExemptionRequest(ctx, id)
	if err != nil {
		h.logger.Error("get exemption request", "error", err, "id", id)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to read exemption request", "server_error", "INTERNAL_ERROR"))
	}
	if reqRow == nil {
		return c.JSON(http.StatusNotFound, errJSON("Exemption request not found", "not_found", "NOT_FOUND"))
	}
	if reqRow.Status != "PENDING" {
		return c.JSON(http.StatusConflict, errJSON("Exemption request is not pending", "conflict", "CONFLICT"))
	}
	if err := ds.MarkExemptionRequestRejected(ctx, id, a.UserID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusConflict, errJSON("Exemption request is not pending", "conflict", "CONFLICT"))
		}
		h.logger.Error("mark exemption rejected", "error", err, "id", id)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to reject exemption request", "server_error", "INTERNAL_ERROR"))
	}

	rae := audit.EntryFor(c, iam.ResourceComplianceExemption, iam.VerbReject)
	rae.EntityID = id
	if h.audit != nil {
		h.audit.LogObserved(ctx, rae)
	}
	return c.JSON(http.StatusOK, map[string]any{"id": id, "status": "REJECTED"})
}

// Employee-facing submit

func (h *Handler) CreateRequest(c echo.Context) error {
	var body map[string]any
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}
	txID, _ := body["transactionId"].(string)
	srcIP, _ := body["sourceIp"].(string)
	targetHost, _ := body["targetHost"].(string)
	reason, _ := body["reason"].(string)
	if txID == "" || srcIP == "" || targetHost == "" || reason == "" {
		return c.JSON(http.StatusBadRequest, errJSON("transactionId, sourceIp, targetHost, and reason are required", "validation_error", ""))
	}
	durMin := 240.0
	if v, ok := body["durationMinutes"].(float64); ok {
		durMin = v
	}
	requestedBy := "employee"
	if v, ok := body["requestedBy"].(string); ok {
		requestedBy = v
	}

	req, err := h.data().CreateExemptionRequest(c.Request().Context(), map[string]any{
		"transactionId":   txID,
		"sourceIp":        srcIP,
		"targetHost":      targetHost,
		"reason":          reason,
		"durationMinutes": int(durMin),
		"requestedBy":     requestedBy,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceComplianceExemption, iam.VerbCreate)
	ae.EntityID = txID
	ae.AfterState = map[string]any{"targetHost": targetHost, "reason": reason, "durationMinutes": int(durMin)}
	if h.audit != nil {
		h.audit.LogObserved(c.Request().Context(), ae)
	}
	return c.JSON(http.StatusCreated, req)
}

// Hub Cat B invalidate signal

// invalidateExemptions fires a Cat B InvalidateConfig signal for both
// receivers of compliance_exemption_grant — compliance-proxy and
// agent — so each Thing of those types re-pulls the projection on the
// next Hub WS push. Then, if an Echo context + audit writer are
// wired, records a single admin-audit row mapping the fine-grained
// `action` string to a canonical iam.Verb. The signal is
// fire-and-forget; the Hub client retries internally and logs
// failures at warn — there is no per-call error to surface because
// each receiver is the source of truth from this point onward.
func (h *Handler) invalidateExemptions(ctx context.Context, action string, a Actor, c echo.Context) {
	if h.hub != nil {
		h.hub.InvalidateConfig(ctx, thingTypeComplianceProxy, configkey.Exemptions)
		h.hub.InvalidateConfig(ctx, thingTypeAgent, configkey.Exemptions)
	}
	if c == nil || h.audit == nil {
		return
	}
	// Map the fine-grained action label onto the closed iam verb set
	// so SIEM consumers see a stable taxonomy. "delete" → VerbDelete;
	// every other action (create / update / approve / reconcile) is a
	// state mutation → VerbUpdate.
	verb := iam.VerbUpdate
	if action == "delete" {
		verb = iam.VerbDelete
	}
	ae := audit.EntryFor(c, iam.ResourceComplianceExemption, verb)
	// The actor identity (a.UserID / a.Name) is already attached by
	// audit.EntryFor via the AdminAuth middleware in c; AfterState
	// captures the fine-grained action label for diffability.
	ae.AfterState = map[string]any{"action": action, "actorName": a.Name}
	h.audit.LogObserved(ctx, ae)
}

// Helper-copies (R6 runbook §4.2)

// Actor mirrors handler.Actor to avoid the cross-package back-import.
type Actor struct {
	UserID string
	Name   string
}

func actorFromContext(c echo.Context) Actor {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return Actor{}
	}
	return Actor{UserID: aa.KeyID, Name: aa.KeyName}
}

func errJSON(message, errType, code string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
		},
	}
}

func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type pagination struct{ Limit, Offset int }

func parsePagination(c echo.Context) pagination {
	limit := 50
	offset := 0
	if v := c.QueryParam("limit"); v != "" {
		if n, err := parseInt(v); err == nil && n > 0 {
			limit = n
			if limit > 1000 {
				limit = 1000
			}
		}
	}
	if v := c.QueryParam("offset"); v != "" {
		if n, err := parseInt(v); err == nil && n >= 0 {
			offset = n
		}
	}
	return pagination{Limit: limit, Offset: offset}
}

// parseInt is a tiny local copy to avoid pulling strconv into the
// helper-copy section's import list bloat.
func parseInt(s string) (int, error) {
	n := 0
	sign := 1
	for i, r := range s {
		if i == 0 && r == '-' {
			sign = -1
			continue
		}
		if r < '0' || r > '9' {
			return 0, errParseInt
		}
		n = n*10 + int(r-'0')
	}
	return sign * n, nil
}

var errParseInt = errors.New("not an int")
