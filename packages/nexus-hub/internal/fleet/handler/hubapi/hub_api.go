package hubapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/enrollment"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/jobs/scheduler"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// HubAPI implements /api/hub/* endpoints called by the Control Plane.
type HubAPI struct {
	Mgr        *manager.Manager
	Scheduler  *scheduler.Scheduler
	Enrollment *enrollment.Service
	// DLQPool is the pgx pool the dead-letter-queue admin endpoints
	// (List/Retry) read from and write to. Optional — when nil the DLQ
	// endpoints return 503; production wires this from the same
	// *pgxpool.Pool the consumer uses.
	DLQPool dlqPool
	// DLQProducer publishes retry-requested messages back to their
	// original MQ subject. Optional — when nil the Retry endpoint
	// returns 503.
	DLQProducer mq.Producer
	// Logger is used by the override-projection path to surface defensive
	// branches (e.g. unexpected empty state). nil falls back to slog.Default.
	Logger *slog.Logger
}

// logger returns h.Logger or slog.Default when unset, so call sites can
// log without nil-checking.
func (h *HubAPI) logger() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// ConfigUpdate handles POST /api/hub/config/update.
//
// `state` is intentionally nullable: Category A (runtime-only) configs carry
// the full state in `state`, while Category B (DB-backed: hooks, routing
// rules, credentials, ...) send a nil `state` — the call only bumps the
// shadow version so connected Things reload from DB. See
// docs/users/product/architecture.md §Category A/B.
//
// Actor identity (ActorID/ActorName/SourceIP) is treated as ambient audit
// metadata, not part of the business payload. Control Plane's admin proxy
// injects it via X-Nexus-Actor-Id / X-Nexus-Actor-Name headers and relies on
// Echo's RealIP resolution; we fall back to those headers when the body omits
// the fields so upstream callers don't have to leak session identity into the
// JSON contract. Body fields still win if present, to keep scripted / internal
// callers able to forge the actor for tests and migrations.
func (h *HubAPI) ConfigUpdate(c echo.Context) error {
	var req manager.UpdateConfigRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.ThingType == "" || req.ConfigKey == "" {
		return badRequest(c, "thingType and configKey are required")
	}
	if req.Action == "" {
		req.Action = "update"
	}
	applyActorFromHeaders(c, &req)

	resp, err := h.Mgr.UpdateConfig(c.Request().Context(), req)
	if err != nil {
		return internalError(c, "config update failed")
	}
	return c.JSON(http.StatusOK, resp)
}

// applyActorFromHeaders backfills ActorID / ActorName / SourceIP on an
// UpdateConfigRequest from request headers when the body left them empty.
// Populated body fields are preserved so admin scripts and test harnesses can
// still forge arbitrary actors.
func applyActorFromHeaders(c echo.Context, req *manager.UpdateConfigRequest) {
	if req.ActorID == "" {
		req.ActorID = c.Request().Header.Get("X-Nexus-Actor-Id")
	}
	if req.ActorName == "" {
		req.ActorName = c.Request().Header.Get("X-Nexus-Actor-Name")
	}
	if req.SourceIP == "" {
		req.SourceIP = c.RealIP()
	}
}

// ListThings handles GET /api/hub/things.
//
// Accepts an optional `hasOverrides=true|false` query param to filter by
// presence of active rows in thing_config_override. Anything other than the
// two literal strings is treated as "no filter" so a misspelled value
// degrades to the unfiltered list rather than 400-ing.
func (h *HubAPI) ListThings(c echo.Context) error {
	params := store.ListThingsParams{
		Type:     c.QueryParam("type"),
		Status:   c.QueryParam("status"),
		Search:   c.QueryParam("search"),
		Page:     parseIntDefault(c.QueryParam("page"), 1),
		PageSize: clamp(parseIntDefault(c.QueryParam("pageSize"), 50), 1, 200),
	}
	switch c.QueryParam("hasOverrides") {
	case "true":
		t := true
		params.HasOverrides = &t
	case "false":
		f := false
		params.HasOverrides = &f
	}

	result, err := h.Mgr.ListThings(c.Request().Context(), params)
	if err != nil {
		return handleErr(c, err)
	}

	return c.JSON(http.StatusOK, map[string]any{
		"things":   result.Things,
		"total":    result.Total,
		"page":     params.Page,
		"pageSize": params.PageSize,
	})
}

// GetThing handles GET /api/hub/things/:id.
func (h *HubAPI) GetThing(c echo.Context) error {
	id := c.Param("id")
	thing, err := h.Mgr.GetThingDetail(c.Request().Context(), id)
	if err != nil {
		return handleErr(c, err)
	}
	return c.JSON(http.StatusOK, thing)
}

// ResyncThing handles POST /api/hub/things/:id/resync.
//
// Replays desired state to the Thing via WebSocket (local) or nexus.hub.signal
// MQ (peer Hub). Does not bump config template versions, does not write a
// config_change_event row — this is a pure redelivery of state the Thing
// should already know about.
//
// Two modes, switched by request body:
//   - {"configKey": "<key>"} — single-key replay (admin "Re-sync this key"
//     action on the Node Detail page). Returns {ok, thingId, configKey}.
//   - {} (or empty body) — whole-Thing replay: every key in thing.desired is
//     re-pushed with Force=true. Drives the admin "Force resync all" action.
//     Returns {ok, thingId, keyCount} where keyCount is the number of keys
//     that were actually pushed. A freshly-enrolled Thing with no template
//     yet returns keyCount=0 (not an error).
func (h *HubAPI) ResyncThing(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return badRequest(c, "thing id is required")
	}
	var req struct {
		ConfigKey string `json:"configKey"`
	}
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}

	if req.ConfigKey == "" {
		res, err := h.Mgr.ForceResyncAll(c.Request().Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return notFound(c, "thing not found")
			}
			return internalError(c, "resync failed")
		}
		// keyCount = number of keys actually pushed; failed (optional) lets
		// the admin UI render which keys did not deliver. ok=true even when
		// some keys failed: the Thing-level call succeeded; per-key
		// degradation is reported, not 5xx'd.
		body := map[string]any{
			"ok":       true,
			"thingId":  id,
			"keyCount": res.Pushed,
		}
		if len(res.Failed) > 0 {
			body["failed"] = res.Failed
		}
		return c.JSON(http.StatusOK, body)
	}

	if err := h.Mgr.ForceResyncKey(c.Request().Context(), id, req.ConfigKey); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			return notFound(c, "thing not found")
		case errors.Is(err, manager.ErrConfigKeyNotInDesired):
			return notFound(c, "config key not present in thing desired state")
		default:
			return internalError(c, "resync failed")
		}
	}

	return c.JSON(http.StatusOK, map[string]any{
		"ok":        true,
		"thingId":   id,
		"configKey": req.ConfigKey,
	})
}

// GetThingServiceMeta handles GET /api/hub/things/:id/service-meta.
// Returns the thing_service row fields (managementUrl, metricsUrl) for the
// given thingId. Used by Control Plane setup relay APIs to discover where to
// forward management requests (e.g. /management/ca-cert).
func (h *HubAPI) GetThingServiceMeta(c echo.Context) error {
	id := c.Param("id")
	managementURL, err := h.Mgr.GetThingManagementURL(c.Request().Context(), id)
	if err != nil {
		return handleErr(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"thingId":       id,
		"managementUrl": managementURL,
	})
}

// GetThingShadow handles GET /api/hub/things/:id/shadow.
func (h *HubAPI) GetThingShadow(c echo.Context) error {
	id := c.Param("id")
	comp, err := h.Mgr.GetShadowComparison(c.Request().Context(), id)
	if err != nil {
		return handleErr(c, err)
	}
	return c.JSON(http.StatusOK, comp)
}

// ListDrift handles GET /api/hub/drift.
func (h *HubAPI) ListDrift(c echo.Context) error {
	drifted, err := h.Mgr.GetDriftedThings(c.Request().Context())
	if err != nil {
		return handleErr(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"drifted": drifted,
		"total":   len(drifted),
	})
}

// ListConfigHistory handles GET /api/hub/config/history.
func (h *HubAPI) ListConfigHistory(c echo.Context) error {
	params := store.ListConfigHistoryParams{
		ThingType: c.QueryParam("thingType"),
		ConfigKey: c.QueryParam("configKey"),
		ActorID:   c.QueryParam("actorId"),
		From:      parseTimeOrNil(c.QueryParam("from")),
		To:        parseTimeOrNil(c.QueryParam("to")),
		Page:      parseIntDefault(c.QueryParam("page"), 1),
		PageSize:  clamp(parseIntDefault(c.QueryParam("pageSize"), 50), 1, 200),
	}

	result, err := h.Mgr.Store().ConfigStore().ListConfigHistory(c.Request().Context(), params)
	if err != nil {
		return handleErr(c, err)
	}

	return c.JSON(http.StatusOK, map[string]any{
		"events":   result.Events,
		"total":    result.Total,
		"page":     params.Page,
		"pageSize": params.PageSize,
	})
}

// ListConfigCatalog handles GET /api/hub/config/catalog.
//
// Returns every (thingType, configKey) pair currently stored in
// thing_config_template, grouped by thingType. The admin Config Sync
// history filter consumes this so the Type / Config Key selects render
// only options that can actually produce results — replacing the earlier
// hardcoded allow-list that included stale keys (e.g. "bump-mode") and
// missed live ones (e.g. "observability", "hooks").
func (h *HubAPI) ListConfigCatalog(c echo.Context) error {
	entries, err := h.Mgr.Store().ConfigStore().ListConfigTemplateCatalog(c.Request().Context())
	if err != nil {
		return handleErr(c, err)
	}
	if entries == nil {
		entries = []store.ConfigTemplateCatalogEntry{}
	}
	return c.JSON(http.StatusOK, map[string]any{"entries": entries})
}

// ListJobs handles GET /api/hub/jobs with limit/offset pagination.
func (h *HubAPI) ListJobs(c echo.Context) error {
	if h.Scheduler == nil {
		return c.JSON(http.StatusOK, map[string]any{"jobs": []any{}, "total": 0, "limit": 20, "offset": 0})
	}
	limit := 20
	offset := 0
	if s := c.QueryParam("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			limit = n
		}
	}
	if s := c.QueryParam("offset"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			offset = n
		}
	}
	all, err := h.Scheduler.ListJobs(c.Request().Context())
	if err != nil {
		return internalError(c, "list jobs failed")
	}
	if search := c.QueryParam("search"); search != "" {
		lower := strings.ToLower(search)
		var filtered []scheduler.JobStatus
		for _, j := range all {
			if strings.Contains(strings.ToLower(j.Name), lower) ||
				strings.Contains(strings.ToLower(j.Description), lower) {
				filtered = append(filtered, j)
			}
		}
		all = filtered
	}
	if enabled := c.QueryParam("enabled"); enabled == "true" || enabled == "false" {
		wantEnabled := enabled == "true"
		var filtered []scheduler.JobStatus
		for _, j := range all {
			if j.Enabled == wantEnabled {
				filtered = append(filtered, j)
			}
		}
		all = filtered
	}
	total := len(all)
	if offset >= total {
		return c.JSON(http.StatusOK, map[string]any{"jobs": []any{}, "total": total, "limit": limit, "offset": offset})
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return c.JSON(http.StatusOK, map[string]any{"jobs": all[offset:end], "total": total, "limit": limit, "offset": offset})
}

// GetJob handles GET /api/hub/jobs/:id.
func (h *HubAPI) GetJob(c echo.Context) error {
	if h.Scheduler == nil {
		return notFound(c, "scheduler not enabled")
	}
	id := c.Param("id")
	status, err := h.Scheduler.GetJob(c.Request().Context(), id)
	if err != nil {
		return notFound(c, "job not found")
	}
	return c.JSON(http.StatusOK, status)
}

// ListJobRuns handles GET /api/hub/jobs/:id/runs.
func (h *HubAPI) ListJobRuns(c echo.Context) error {
	limit := clamp(parseIntDefault(c.QueryParam("limit"), 100), 1, 500)
	offset := parseIntDefault(c.QueryParam("offset"), 0)
	if offset < 0 {
		offset = 0
	}
	if h.Scheduler == nil {
		return c.JSON(http.StatusOK, map[string]any{
			"runs":   []any{},
			"total":  0,
			"limit":  limit,
			"offset": offset,
		})
	}
	id := c.Param("id")
	runs, total, err := h.Scheduler.ListRuns(c.Request().Context(), id, limit, offset)
	if err != nil {
		return internalError(c, "list runs failed")
	}
	return c.JSON(http.StatusOK, map[string]any{
		"runs":   runs,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// TriggerJob handles POST /api/hub/jobs/:id/trigger.
func (h *HubAPI) TriggerJob(c echo.Context) error {
	id := c.Param("id")
	if h.Scheduler == nil {
		return notFound(c, "scheduler not enabled")
	}
	if err := h.Scheduler.Trigger(c.Request().Context(), id); err != nil {
		return notFound(c, "job not found")
	}
	now := time.Now()
	return c.JSON(http.StatusOK, map[string]any{
		"ok":          true,
		"jobId":       id,
		"triggeredAt": now.Format(time.RFC3339),
	})
}

// UpdateJob handles PUT /api/hub/jobs/:id. Only the `enabled` field is
// mutable from the admin API; metadata is owned by the code registration
// path.
func (h *HubAPI) UpdateJob(c echo.Context) error {
	if h.Scheduler == nil {
		return notFound(c, "scheduler not enabled")
	}
	id := c.Param("id")
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.Bind(&req); err != nil || req.Enabled == nil {
		return badRequest(c, "enabled field is required")
	}
	if err := h.Scheduler.SetEnabled(c.Request().Context(), id, *req.Enabled); err != nil {
		return notFound(c, "job not found")
	}
	return c.JSON(http.StatusOK, map[string]any{
		"ok":      true,
		"jobId":   id,
		"enabled": *req.Enabled,
	})
}

// GenerateEnrollmentToken handles POST /api/hub/enrollment/token.
func (h *HubAPI) GenerateEnrollmentToken(c echo.Context) error {
	var req enrollment.GenerateRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.Label == "" {
		return badRequest(c, "label is required")
	}

	tok, err := h.Enrollment.GenerateToken(c.Request().Context(), req)
	if err != nil {
		return internalError(c, "token generation failed")
	}
	return c.JSON(http.StatusCreated, tok)
}

// ListEnrollmentTokens handles GET /api/hub/enrollment/tokens.
func (h *HubAPI) ListEnrollmentTokens(c echo.Context) error {
	tokens, err := h.Enrollment.ListTokens(c.Request().Context())
	if err != nil {
		return internalError(c, "list tokens failed")
	}
	return c.JSON(http.StatusOK, map[string]any{
		"tokens": tokens,
		"total":  len(tokens),
	})
}
