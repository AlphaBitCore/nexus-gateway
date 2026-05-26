package iam

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/revocation"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// revocationRevoke is a package-level indirection point so the admin auth-
// session handlers can be unit-tested without a real revocation.Service
// (which needs MQ + DB). Production wiring goes straight to h.revocation;
// tests swap it via SetRevokeFnForTest in auth_sessions_export_test.go.
var revocationRevoke = func(h *Handler, ctx context.Context, req revocation.Request) error {
	return h.revocation.Revoke(ctx, req)
}

// revokeUserScope mints a scope=user revocation and purges matching refresh
// rows. Safe no-op when the revocation service is not wired (test builds, or
// boots without MQ). Errors are logged but do not fail the caller: the
// underlying admin mutation already succeeded and must not be rolled back
// because a best-effort revocation fan-out failed -- the RS-side checker
// still denies tokens once the row is durable, and reconnect replay covers
// any event lost in transit.
func (h *Handler) revokeUserScope(ctx context.Context, userID, reason string) {
	if h.revocation == nil {
		return
	}
	if err := revocationRevoke(h, ctx, revocation.Request{
		Scope:        revocation.ScopeUser,
		TargetUserID: &userID,
		ExpiresAt:    time.Now().Add(h.authRefreshTTL).UTC(),
		Reason:       reason,
	}); err != nil {
		h.logger.Error("admin revoke", "userId", userID, "reason", reason, "error", err)
	}
	if h.pool == nil {
		return
	}
	// RefreshToken.userId is text (NexusUser.id is a String, not @db.Uuid),
	// so no ::uuid cast is used here; see ListAuthSessions for the same
	// note. A no-op delete is fine: the revocation event is already durable
	// and fans out independently.
	if _, err := h.pool.Exec(ctx, `DELETE FROM "RefreshToken" WHERE "userId" = $1`, userID); err != nil {
		h.logger.Error("admin revoke: delete refresh rows", "userId", userID, "error", err)
	}
}

// RegisterAuthSessionRoutes wires the admin force-logout + replay endpoints.
// DELETE /auth/sessions gates on admin:nexus-session.revoke — aligned with
// the audit Entry emitted by DeleteAuthSessions (auth_sessions.go L220) which
// already uses ResourceNexusSession + VerbRevoke. GET /auth/sessions still
// gates on admin:user.update because the list reveals every admin session
// across the tenant, which is operationally privileged data.
// The replay endpoint gates on admin:revocation.read.
func (h *Handler) RegisterAuthSessionRoutes(g *echo.Group, iamMW func(string) echo.MiddlewareFunc) {
	g.GET("/auth/sessions", h.ListAuthSessions, iamMW(iam.ResourceUser.Action(iam.VerbUpdate)))
	g.DELETE("/auth/sessions", h.DeleteAuthSessions, iamMW(iam.ResourceNexusSession.Action(iam.VerbRevoke)))
	g.GET("/revocations", h.ListRevocations, iamMW(iam.ResourceRevocation.Action(iam.VerbRead)))
}

// RegisterInternalAuthRoutes wires the Hub -> CP internal revoke-device route.
// The caller's group MUST already be gated by rstokenauth.Middleware; this
// method does not add its own auth.
func (h *Handler) RegisterInternalAuthRoutes(g *echo.Group) {
	g.POST("/auth/revoke-device", h.RevokeDeviceInternal)
}

// sessionRow is the JSON wire shape returned by ListAuthSessions. CreatedAt
// is the session's first-mint time (MIN over the sessionId partition);
// LastRefreshedAt is the most recent rotation's createdAt.
type sessionRow struct {
	SessionID       string     `json:"sessionId"`
	UserID          string     `json:"userId"`
	ClientID        string     `json:"clientId"`
	DeviceID        *string    `json:"deviceId,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	LastRefreshedAt *time.Time `json:"lastRefreshedAt,omitempty"`
	ExpiresAt       time.Time  `json:"expiresAt"`
}

// ListAuthSessions returns one row per sessionId from the RefreshToken table,
// optionally filtered by user_id / device_id / session_id. DISTINCT ON keeps
// the newest rotation; the MIN window function surfaces the first-mint time
// as the session's CreatedAt.
func (h *Handler) ListAuthSessions(c echo.Context) error {
	if h.pool == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("database not available", "server_error", "DB_UNAVAILABLE"))
	}
	ctx := c.Request().Context()
	userID := c.QueryParam("user_id")
	deviceID := c.QueryParam("device_id")
	sessionID := c.QueryParam("session_id")

	// The plan specifies a $1::uuid cast on userId, but the underlying
	// RefreshToken.userId column is text (NexusUser.id is a String, not a
	// @db.Uuid field, so FK-referenced values arrive as text). Compare
	// userId as text; sessionId is genuinely uuid in the schema so keep
	// the $3::uuid cast there.
	const q = `SELECT DISTINCT ON ("sessionId")
		"sessionId","userId","clientId","deviceId",
		MIN("createdAt") OVER (PARTITION BY "sessionId") AS created_at,
		"createdAt" AS last_refreshed_at,
		"expiresAt"
	FROM "RefreshToken"
	WHERE ($1='' OR "userId"=$1)
	  AND ($2='' OR "deviceId"=$2)
	  AND ($3='' OR "sessionId"=$3::uuid)
	ORDER BY "sessionId","createdAt" DESC
	LIMIT 500`

	rows, err := h.pool.Query(ctx, q, userID, deviceID, sessionID)
	if err != nil {
		h.logger.Error("list auth sessions", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("failed to list sessions", "server_error", ""))
	}
	defer rows.Close()

	out := make([]sessionRow, 0, 16)
	for rows.Next() {
		var r sessionRow
		var lastRefreshed sql.NullTime
		if err := rows.Scan(&r.SessionID, &r.UserID, &r.ClientID, &r.DeviceID, &r.CreatedAt, &lastRefreshed, &r.ExpiresAt); err != nil {
			h.logger.Error("scan auth sessions", "error", err)
			return c.JSON(http.StatusInternalServerError, errJSON("failed to list sessions", "server_error", ""))
		}
		if lastRefreshed.Valid {
			t := lastRefreshed.Time
			r.LastRefreshedAt = &t
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		h.logger.Error("iterate auth sessions", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("failed to list sessions", "server_error", ""))
	}
	return c.JSON(http.StatusOK, map[string]any{"sessions": out})
}

// DeleteAuthSessions force-logs-out a user, device, or session. Exactly one
// filter must be supplied; zero or more than one returns 400 so callers
// cannot accidentally revoke a broader blast radius than they intended.
func (h *Handler) DeleteAuthSessions(c echo.Context) error {
	userID := c.QueryParam("user_id")
	deviceID := c.QueryParam("device_id")
	sessionID := c.QueryParam("session_id")
	filterCount := 0
	for _, v := range []string{userID, deviceID, sessionID} {
		if v != "" {
			filterCount++
		}
	}
	if filterCount != 1 {
		return c.JSON(http.StatusBadRequest, errJSON(
			"exactly one of user_id, device_id, session_id required",
			"validation_error",
			"FILTER_REQUIRED",
		))
	}
	if h.revocation == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON(
			"revocation service not configured",
			"server_error",
			"REVOCATION_UNAVAILABLE",
		))
	}
	if h.pool == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("database not available", "server_error", "DB_UNAVAILABLE"))
	}

	ctx := c.Request().Context()
	req := revocation.Request{
		ExpiresAt: time.Now().Add(h.authRefreshTTL).UTC(),
		Reason:    revocation.ReasonAdminDisable,
	}
	var entityID, whereCol string
	switch {
	case userID != "":
		req.Scope = revocation.ScopeUser
		req.TargetUserID = &userID
		entityID = userID
		whereCol = `"userId"`
	case deviceID != "":
		req.Scope = revocation.ScopeDevice
		req.TargetDeviceID = &deviceID
		entityID = deviceID
		whereCol = `"deviceId"`
	case sessionID != "":
		req.Scope = revocation.ScopeSession
		req.TargetSessionID = &sessionID
		entityID = sessionID
		whereCol = `"sessionId"`
	}

	if err := revocationRevoke(h, ctx, req); err != nil {
		h.logger.Error("force-logout: revoke", "error", err, "scope", string(req.Scope))
		return c.JSON(http.StatusInternalServerError, errJSON("failed to revoke", "server_error", ""))
	}
	// Hard-delete matching refresh rows so the session cannot rotate.
	// A no-op (zero rows) is not an error: the revocation event is already
	// durable and fans out independently.
	if _, err := h.pool.Exec(ctx, `DELETE FROM "RefreshToken" WHERE `+whereCol+` = $1`, entityID); err != nil {
		h.logger.Error("force-logout: delete refresh rows", "error", err, "scope", string(req.Scope))
		// Intentional: we do not roll back the revocation. Leaving the row in
		// place means the RS-side checker will still deny the tokens; surface
		// a 500 so operators notice the DB delete failed.
		return c.JSON(http.StatusInternalServerError, errJSON("failed to purge refresh rows", "server_error", ""))
	}

	ae := audit.EntryFor(c, iam.ResourceNexusSession, iam.VerbRevoke)
	ae.EntityID = entityID
	ae.AfterState = map[string]any{"scope": req.Scope, "reason": req.Reason}
	h.audit.LogObserved(ctx, ae)

	return c.JSON(http.StatusOK, map[string]any{"ok": true})
}

// revocationEvent is the wire shape returned by ListRevocations. Field names
// mirror revocation.Event so downstream RS-side checkers can decode a replay
// response with the same struct they use for live MQ messages.
type revocationEvent struct {
	EventID         string    `json:"event_id"`
	RevokedAt       time.Time `json:"revoked_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	Scope           string    `json:"scope"`
	TargetJTI       string    `json:"target_jti,omitempty"`
	TargetUserID    string    `json:"target_user_id,omitempty"`
	TargetDeviceID  string    `json:"target_device_id,omitempty"`
	TargetSessionID string    `json:"target_session_id,omitempty"`
	Reason          string    `json:"reason"`
}

// ListRevocations returns revoked_token rows with id > since, ascending. The
// response includes a lastId cursor callers pipeline into the next request.
// Task 4.5 consumes this endpoint on RS-side reconnect catch-up.
func (h *Handler) ListRevocations(c echo.Context) error {
	if h.revocationStore == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON(
			"revocation store not configured",
			"server_error",
			"REVOCATION_STORE_UNAVAILABLE",
		))
	}
	since := parseInt64(c.QueryParam("since"))
	limit := parseIntDefault(c.QueryParam("limit"), 500, 1000)

	rows, last, err := h.revocationStore.ListSince(c.Request().Context(), since, limit)
	if err != nil {
		h.logger.Error("list revocations", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("failed to list revocations", "server_error", ""))
	}
	events := make([]revocationEvent, 0, len(rows))
	for _, r := range rows {
		events = append(events, revocationEvent{
			EventID:         "evt_replay_" + strconv.FormatInt(r.ID, 10),
			RevokedAt:       r.RevokedAt,
			ExpiresAt:       r.ExpiresAt,
			Scope:           string(r.Scope),
			TargetJTI:       ptrToString(r.TargetJTI),
			TargetUserID:    ptrToString(r.TargetUserID),
			TargetDeviceID:  ptrToString(r.TargetDeviceID),
			TargetSessionID: ptrToString(r.TargetSessionID),
			Reason:          r.Reason,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"events": events,
		"lastId": last,
	})
}

// revokeDeviceRequest mirrors the Hub -> CP internal request body.
type revokeDeviceRequest struct {
	DeviceID  string     `json:"deviceId"`
	Reason    string     `json:"reason,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// RevokeDeviceInternal is the Hub -> CP revocation entry point used when Hub
// unenrolls a device. It MUST sit behind rstokenauth.Middleware; this handler
// does not re-verify the caller.
func (h *Handler) RevokeDeviceInternal(c echo.Context) error {
	if h.pool == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON("database not available", "server_error", "DB_UNAVAILABLE"))
	}
	if h.revocation == nil {
		return c.JSON(http.StatusServiceUnavailable, errJSON(
			"revocation service not configured",
			"server_error",
			"REVOCATION_UNAVAILABLE",
		))
	}

	var body revokeDeviceRequest
	dec := json.NewDecoder(c.Request().Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("invalid JSON body", "validation_error", "INVALID_BODY"))
	}
	if body.DeviceID == "" {
		return c.JSON(http.StatusBadRequest, errJSON("deviceId is required", "validation_error", "DEVICE_ID_REQUIRED"))
	}
	reason := body.Reason
	if reason == "" {
		reason = revocation.ReasonUnenroll
	}
	var exp time.Time
	if body.ExpiresAt != nil && !body.ExpiresAt.IsZero() {
		exp = body.ExpiresAt.UTC()
	} else {
		exp = time.Now().Add(h.authRefreshTTL).UTC()
	}

	ctx := c.Request().Context()
	deviceID := body.DeviceID
	if err := revocationRevoke(h, ctx, revocation.Request{
		Scope:          revocation.ScopeDevice,
		TargetDeviceID: &deviceID,
		ExpiresAt:      exp,
		Reason:         reason,
	}); err != nil {
		h.logger.Error("internal revoke-device: revoke", "error", err, "deviceId", deviceID)
		return c.JSON(http.StatusInternalServerError, errJSON("failed to revoke device", "server_error", ""))
	}
	if _, err := h.pool.Exec(ctx, `DELETE FROM "RefreshToken" WHERE "deviceId" = $1`, deviceID); err != nil {
		h.logger.Error("internal revoke-device: delete refresh rows", "error", err, "deviceId", deviceID)
		return c.JSON(http.StatusInternalServerError, errJSON("failed to purge refresh rows", "server_error", ""))
	}

	if h.audit != nil {
		// Internal Hub-driven path — ActorID/Label are synthetic, so
		// audit.EntryFor (which reads admin auth from c) is not used here.
		// EntityType + Action follow the canonical catalog directly:
		// nexus-session.revoke covers both forced-logout and device-revoke.
		e := audit.Entry{
			ActorID:        "internal",
			ActorLabel:     "nexus-hub",
			SourceIP:       c.RealIP(),
			Action:         string(iam.VerbRevoke),
			EntityType:     iam.ResourceNexusSession.Name,
			EntityID:       deviceID,
			AfterState:     map[string]any{"reason": reason, "via": "revoke-device"},
			NexusRequestID: middleware.NexusRequestIDFromContext(c),
		}
		h.audit.LogObserved(ctx, e)
	}
	return c.NoContent(http.StatusNoContent)
}

// parseInt64 returns 0 on empty / parse error. Negative values are clamped to
// 0 because the store's ListSince uses id > since and negative cursors are a
// client bug.
func parseInt64(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// parseIntDefault returns def on empty / parse error, clamps to [1, max].
func parseIntDefault(s string, def, max int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	if n < 1 {
		return 1
	}
	if n > max {
		return max
	}
	return n
}

// ptrToString dereferences a *string or returns "". The sibling derefStr in
// admin_extras.go has the same semantics; keeping a dedicated local name here
// makes the intent obvious at each revocation call site.
func ptrToString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
