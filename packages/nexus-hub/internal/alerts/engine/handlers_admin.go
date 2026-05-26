// Package alerting admin HTTP handlers — implements the 14 endpoints under
// /api/v1/admin/alerts/* consumed by the Control Plane admin UI. Echo is the
// outer router; handlers here are echo.HandlerFunc and registered directly
// on an Echo route group (see internal/handler/routes.go).
package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

// adminStoreAPI is the subset of *Store the admin handlers need. Defined as
// interface so tests can inject a fake.
type adminStoreAPI interface {
	ListAlerts(ctx context.Context, f ListFilter) ([]Alert, int, error)
	GetAlert(ctx context.Context, id string) (*Alert, error)
	ListDispatchesByAlert(ctx context.Context, alertID string) ([]Dispatch, error)
	AcknowledgeAlert(ctx context.Context, id, by, reason string) error
	ResolveAlert(ctx context.Context, id, by, reason string) error
	ListRules(ctx context.Context, p ListRulesParams) ([]AlertRule, int, error)
	GetRule(ctx context.Context, id string) (*AlertRule, error)
	UpdateRule(ctx context.Context, r AlertRule) error
	InsertChannel(ctx context.Context, c Channel) (string, error)
	GetChannel(ctx context.Context, id string) (*Channel, error)
	ListChannels(ctx context.Context) ([]Channel, error)
	UpdateChannel(ctx context.Context, c Channel) error
	DeleteChannel(ctx context.Context, id string) error
	InsertAlert(ctx context.Context, a Alert) (string, error)
	InsertDispatch(ctx context.Context, d Dispatch) (string, error)
}

// RuleDefault captures the code-owned defaults the rule-reset endpoint writes
// back to the DB. It is populated from rules.RuleDef in main.go via a tiny
// adapter — defined here to avoid importing the rules subpackage (which
// imports this package for Severity, creating a cycle).
type RuleDefault struct {
	ID              string
	DisplayName     string
	DefaultSeverity Severity
	RequiresAck     bool
	Enabled         bool
	CooldownSec     int
	Params          json.RawMessage
	ParamsSchema    json.RawMessage
}

// RuleRegistry is the registry surface the admin-reset handler needs.
// *rules.Registry satisfies this via a small adapter in main.go.
type RuleRegistry interface {
	Lookup(id string) (RuleDefault, bool)
}

// AdminHandlers groups the admin endpoint implementations.
type AdminHandlers struct {
	Store   adminStoreAPI
	Rules   RuleRegistry
	Senders SenderRegistry
	Logger  *slog.Logger
}

// logger returns the handler's logger, defaulting to slog.Default if nil.
func (h *AdminHandlers) logger() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// ListAlerts handles GET /api/v1/admin/alerts.
//
// Categorical filters (state, severity, sourceType, ruleId) accept repeated
// query-string values — e.g. `?state=firing&state=acknowledged` narrows to
// rows in either state. A missing key is read as an empty slice, which the
// store interprets as "do not filter on this dimension".
func (h *AdminHandlers) ListAlerts(c echo.Context) error {
	qp := c.QueryParams()
	// Reject unknown severity query-string values rather than silently
	// treating them as "no rows" (they would still pass through to
	// buildListWhere as an uppercased string that fails the
	// AlertSeverity[] cast — clearer to 400 here with a useful message).
	for _, s := range qp["severity"] {
		if _, err := ParseLoose(s); err != nil {
			return echoErr(c, http.StatusBadRequest, "invalid severity filter: "+err.Error())
		}
	}
	f := ListFilter{
		State:      qp["state"],
		Severity:   qp["severity"],
		SourceType: qp["sourceType"],
		RuleID:     qp["ruleId"],
	}
	if s := c.QueryParam("since"); s != "" {
		t, ok := parseFlexibleTime(s)
		if !ok {
			return echoErr(c, http.StatusBadRequest, "invalid since: must be RFC3339 (e.g. 2006-01-02T15:04:05Z)")
		}
		f.Since = &t
	}
	if s := c.QueryParam("until"); s != "" {
		t, ok := parseFlexibleTime(s)
		if !ok {
			return echoErr(c, http.StatusBadRequest, "invalid until: must be RFC3339 (e.g. 2006-01-02T15:04:05Z)")
		}
		f.Until = &t
	}
	if s := c.QueryParam("offset"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return echoErr(c, http.StatusBadRequest, "invalid offset")
		}
		f.Offset = n
	}
	if s := c.QueryParam("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return echoErr(c, http.StatusBadRequest, "invalid limit")
		}
		f.Limit = n
	}

	alerts, total, err := h.Store.ListAlerts(c.Request().Context(), f)
	if err != nil {
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}
	if alerts == nil {
		alerts = []Alert{}
	}
	return c.JSON(http.StatusOK, map[string]any{
		"alerts": alerts,
		"total":  total,
	})
}

// alertDetailResponse is the flat JSON shape returned by GetAlert. Alert's
// fields are embedded at the top level alongside `dispatches` so the admin UI
// can treat the detail response as an Alert with one extra field — matching
// `AlertDetailResponse extends Alert { dispatches }` on the TS side.
type alertDetailResponse struct {
	Alert
	Dispatches []Dispatch `json:"dispatches"`
}

// GetAlert handles GET /api/v1/admin/alerts/:id.
func (h *AdminHandlers) GetAlert(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return echoErr(c, http.StatusBadRequest, "id required")
	}
	ctx := c.Request().Context()
	alert, err := h.Store.GetAlert(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return echoErr(c, http.StatusNotFound, "alert not found")
		}
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}
	dispatches, err := h.Store.ListDispatchesByAlert(ctx, id)
	if err != nil {
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}
	if dispatches == nil {
		dispatches = []Dispatch{}
	}
	return c.JSON(http.StatusOK, alertDetailResponse{
		Alert:      *alert,
		Dispatches: dispatches,
	})
}

// ackResolveBody is the request body for ack/resolve endpoints.
type ackResolveBody struct {
	Reason string `json:"reason"`
}

// actorFromHeader pulls the actor from X-Nexus-Actor-User-Id, falling back to
// "system" when absent. Control Plane injects this header on admin proxies.
func actorFromHeader(c echo.Context) string {
	if v := c.Request().Header.Get("X-Nexus-Actor-User-Id"); v != "" {
		return v
	}
	return "system"
}

// AckAlert handles POST /api/v1/admin/alerts/:id/ack.
func (h *AdminHandlers) AckAlert(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return echoErr(c, http.StatusBadRequest, "id required")
	}
	body := ackResolveBody{}
	// Body is optional; decode only if present.
	if c.Request().ContentLength > 0 {
		if err := json.NewDecoder(c.Request().Body).Decode(&body); err != nil && !errors.Is(err, errEmptyBody) {
			return echoErr(c, http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
	}
	actor := actorFromHeader(c)
	if err := h.Store.AcknowledgeAlert(c.Request().Context(), id, actor, body.Reason); err != nil {
		if errors.Is(err, ErrNotFound) {
			return echoErr(c, http.StatusNotFound, "alert not found or not firing")
		}
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

// ResolveAlert handles POST /api/v1/admin/alerts/:id/resolve.
func (h *AdminHandlers) ResolveAlert(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return echoErr(c, http.StatusBadRequest, "id required")
	}
	body := ackResolveBody{}
	if c.Request().ContentLength > 0 {
		if err := json.NewDecoder(c.Request().Body).Decode(&body); err != nil && !errors.Is(err, errEmptyBody) {
			return echoErr(c, http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
	}
	actor := actorFromHeader(c)
	if err := h.Store.ResolveAlert(c.Request().Context(), id, actor, body.Reason); err != nil {
		if errors.Is(err, ErrNotFound) {
			return echoErr(c, http.StatusNotFound, "alert not found or already resolved")
		}
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

// ListRules handles GET /api/v1/admin/alerts/rules.
//
// Query params (all optional):
//
//	search       free-text match on id or displayName (case-insensitive)
//	enabled      "true" / "false" — restrict to the matching boolean
//	severity     one of info / low / medium / high / critical
//	sourceType   matches the rule's sourceType exactly
//	limit        page size (default 50)
//	offset       page offset
func (h *AdminHandlers) ListRules(c echo.Context) error {
	p := ListRulesParams{
		Limit:      50,
		Search:     c.QueryParam("search"),
		Severity:   c.QueryParam("severity"),
		SourceType: c.QueryParam("sourceType"),
	}
	if s := c.QueryParam("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			p.Limit = n
		}
	}
	if s := c.QueryParam("offset"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			p.Offset = n
		}
	}
	if s := c.QueryParam("enabled"); s == "true" {
		v := true
		p.Enabled = &v
	} else if s == "false" {
		v := false
		p.Enabled = &v
	}

	ruleList, total, err := h.Store.ListRules(c.Request().Context(), p)
	if err != nil {
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}
	if ruleList == nil {
		ruleList = []AlertRule{}
	}
	return c.JSON(http.StatusOK, map[string]any{
		"rules":  ruleList,
		"total":  total,
		"limit":  p.Limit,
		"offset": p.Offset,
	})
}

// GetRule handles GET /api/v1/admin/alerts/rules/:id.
func (h *AdminHandlers) GetRule(c echo.Context) error {
	id := c.Param("id")
	r, err := h.Store.GetRule(c.Request().Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return echoErr(c, http.StatusNotFound, "rule not found")
		}
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, r)
}

// updateRuleBody captures the allowed mutable fields for a rule update. Any
// field left nil is preserved from the existing DB row.
type updateRuleBody struct {
	Enabled         *bool            `json:"enabled,omitempty"`
	Params          *json.RawMessage `json:"params,omitempty"`
	CooldownSec     *int             `json:"cooldownSec,omitempty"`
	RequiresAck     *bool            `json:"requiresAck,omitempty"`
	DefaultSeverity *string          `json:"defaultSeverity,omitempty"`
	DisplayName     *string          `json:"displayName,omitempty"`
	// GroupIDFilter: JSON null clears the filter (rule fires fleet-wide);
	// non-null binds the rule to that DeviceGroup. Sentinel-pointer
	// pattern: nil = not present in payload, pointer-to-empty = explicit
	// clear.
	GroupIDFilter *string `json:"groupIdFilter,omitempty"`
}

// UpdateRule handles PUT /api/v1/admin/alerts/rules/:id.
func (h *AdminHandlers) UpdateRule(c echo.Context) error {
	id := c.Param("id")
	ctx := c.Request().Context()

	existing, err := h.Store.GetRule(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return echoErr(c, http.StatusNotFound, "rule not found")
		}
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}

	dec := json.NewDecoder(c.Request().Body)
	dec.DisallowUnknownFields()
	var body updateRuleBody
	if err := dec.Decode(&body); err != nil {
		return echoErr(c, http.StatusBadRequest, "invalid JSON: "+err.Error())
	}

	// Validate params against existing paramsSchema if provided.
	if body.Params != nil {
		if err := validateParamsAgainstSchema(*body.Params, existing.ParamsSchema); err != nil {
			return echoErr(c, http.StatusBadRequest, "params validation: "+err.Error())
		}
		var parsed map[string]any
		if err := json.Unmarshal(*body.Params, &parsed); err != nil {
			return echoErr(c, http.StatusBadRequest, "params parse: "+err.Error())
		}
		existing.Params = parsed
	}
	if body.Enabled != nil {
		existing.Enabled = *body.Enabled
	}
	if body.CooldownSec != nil {
		existing.CooldownSec = *body.CooldownSec
	}
	if body.RequiresAck != nil {
		existing.RequiresAck = *body.RequiresAck
	}
	if body.DefaultSeverity != nil {
		sev, err := ParseLoose(*body.DefaultSeverity)
		if err != nil {
			return echoErr(c, http.StatusBadRequest, "invalid defaultSeverity: "+err.Error())
		}
		existing.DefaultSeverity = sev
	}
	if body.DisplayName != nil {
		existing.DisplayName = *body.DisplayName
	}
	if body.GroupIDFilter != nil {
		// Explicit empty string clears the filter (rule fires
		// fleet-wide). Non-empty binds to that DeviceGroup.
		if *body.GroupIDFilter == "" {
			existing.GroupIDFilter = nil
		} else {
			v := *body.GroupIDFilter
			existing.GroupIDFilter = &v
		}
	}

	if err := h.Store.UpdateRule(ctx, *existing); err != nil {
		if errors.Is(err, ErrNotFound) {
			return echoErr(c, http.StatusNotFound, "rule not found")
		}
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}

	// Return the updated rule.
	updated, err := h.Store.GetRule(ctx, id)
	if err != nil {
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, updated)
}

// ResetRule handles POST /api/v1/admin/alerts/rules/:id/reset. It restores the
// rule to the code-owned defaults declared in rules.BuiltinRules.
func (h *AdminHandlers) ResetRule(c echo.Context) error {
	id := c.Param("id")
	ctx := c.Request().Context()

	var def RuleDefault
	if h.Rules != nil {
		d, ok := h.Rules.Lookup(id)
		if !ok {
			return echoErr(c, http.StatusNotFound, "no built-in rule with id "+id)
		}
		def = d
	} else {
		return echoErr(c, http.StatusInternalServerError, "rule registry not configured")
	}

	existing, err := h.Store.GetRule(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return echoErr(c, http.StatusNotFound, "rule not found")
		}
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}

	var params map[string]any
	if len(def.Params) > 0 {
		if err := json.Unmarshal(def.Params, &params); err != nil {
			return echoErr(c, http.StatusInternalServerError, "unmarshal builtin params: "+err.Error())
		}
	}
	var paramsSchema map[string]any
	if len(def.ParamsSchema) > 0 {
		if err := json.Unmarshal(def.ParamsSchema, &paramsSchema); err != nil {
			return echoErr(c, http.StatusInternalServerError, "unmarshal builtin schema: "+err.Error())
		}
	}

	updated := AlertRule{
		ID:              existing.ID,
		DisplayName:     def.DisplayName,
		SourceType:      existing.SourceType,
		DefaultSeverity: def.DefaultSeverity,
		RequiresAck:     def.RequiresAck,
		Enabled:         def.Enabled,
		Params:          params,
		ParamsSchema:    paramsSchema,
		CooldownSec:     def.CooldownSec,
	}
	if err := h.Store.UpdateRule(ctx, updated); err != nil {
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}

	refreshed, err := h.Store.GetRule(ctx, id)
	if err != nil {
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, refreshed)
}

// ListChannels handles GET /api/v1/admin/alerts/channels.
func (h *AdminHandlers) ListChannels(c echo.Context) error {
	list, err := h.Store.ListChannels(c.Request().Context())
	if err != nil {
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}
	for i := range list {
		list[i].Config = maskChannelConfig(list[i].Config)
	}
	if list == nil {
		list = []Channel{}
	}
	return c.JSON(http.StatusOK, map[string]any{
		"channels": list,
	})
}

// channelBody is the JSON body accepted by Create/Update channel endpoints.
//
// Severities is decoded directly into []Severity so each element flows
// through Severity.UnmarshalJSON — a typo or stale uppercase value is
// rejected at decode time with a JSON error rather than being silently
// persisted into the free-form text[] column and breaking the
// dispatcher's case-stable matchesSeverity later.
type channelBody struct {
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	Enabled     bool           `json:"enabled"`
	Severities  []Severity     `json:"severities"`
	SourceTypes []string       `json:"sourceTypes"`
	Config      map[string]any `json:"config"`
}

// channelPatchBody is the request shape for PUT /alerts/channels/:id. All
// fields are pointers so callers can send a partial update — the list page's
// quick enable/disable toggle posts just `{enabled: false}` and expects the
// other fields to be preserved. Nil field = "keep existing value".
type channelPatchBody struct {
	Name        *string         `json:"name"`
	Type        *string         `json:"type"`
	Enabled     *bool           `json:"enabled"`
	Severities  *[]Severity     `json:"severities"`
	SourceTypes *[]string       `json:"sourceTypes"`
	Config      *map[string]any `json:"config"`
}

// CreateChannel handles POST /api/v1/admin/alerts/channels.
func (h *AdminHandlers) CreateChannel(c echo.Context) error {
	var body channelBody
	if err := json.NewDecoder(c.Request().Body).Decode(&body); err != nil {
		return echoErr(c, http.StatusBadRequest, "invalid JSON: "+err.Error())
	}
	if body.Name == "" || body.Type == "" {
		return echoErr(c, http.StatusBadRequest, "name and type required")
	}
	if body.Severities == nil {
		body.Severities = []Severity{}
	}
	if body.SourceTypes == nil {
		body.SourceTypes = []string{}
	}
	if body.Config == nil {
		body.Config = map[string]any{}
	}
	ch := Channel{
		Name:        body.Name,
		Type:        body.Type,
		Enabled:     body.Enabled,
		Severities:  body.Severities,
		SourceTypes: body.SourceTypes,
		Config:      body.Config,
	}
	ctx := c.Request().Context()
	id, err := h.Store.InsertChannel(ctx, ch)
	if err != nil {
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}
	got, err := h.Store.GetChannel(ctx, id)
	if err != nil {
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}
	got.Config = maskChannelConfig(got.Config)
	return c.JSON(http.StatusCreated, got)
}

// GetChannel handles GET /api/v1/admin/alerts/channels/:id.
func (h *AdminHandlers) GetChannel(c echo.Context) error {
	id := c.Param("id")
	got, err := h.Store.GetChannel(c.Request().Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return echoErr(c, http.StatusNotFound, "channel not found")
		}
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}
	got.Config = maskChannelConfig(got.Config)
	return c.JSON(http.StatusOK, got)
}

// UpdateChannel handles PUT /api/v1/admin/alerts/channels/:id. Accepts a
// partial patch: any field omitted from the request body is preserved from
// the existing row. Fields explicitly sent as empty strings for name/type
// are rejected — they are meaningful required fields.
func (h *AdminHandlers) UpdateChannel(c echo.Context) error {
	id := c.Param("id")
	ctx := c.Request().Context()

	existing, err := h.Store.GetChannel(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return echoErr(c, http.StatusNotFound, "channel not found")
		}
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}

	var body channelPatchBody
	if err := json.NewDecoder(c.Request().Body).Decode(&body); err != nil {
		return echoErr(c, http.StatusBadRequest, "invalid JSON: "+err.Error())
	}
	if body.Name != nil && *body.Name == "" {
		return echoErr(c, http.StatusBadRequest, "name must not be empty")
	}
	if body.Type != nil && *body.Type == "" {
		return echoErr(c, http.StatusBadRequest, "type must not be empty")
	}

	updated := *existing
	updated.ID = id
	if body.Name != nil {
		updated.Name = *body.Name
	}
	if body.Type != nil {
		updated.Type = *body.Type
	}
	if body.Enabled != nil {
		updated.Enabled = *body.Enabled
	}
	if body.Severities != nil {
		updated.Severities = *body.Severities
	}
	if body.SourceTypes != nil {
		updated.SourceTypes = *body.SourceTypes
	}
	if body.Config != nil {
		// Preserve any secrets the client round-tripped back in masked form.
		updated.Config = mergeMaskedSecrets(*body.Config, existing.Config)
	}

	if err := h.Store.UpdateChannel(ctx, updated); err != nil {
		if errors.Is(err, ErrNotFound) {
			return echoErr(c, http.StatusNotFound, "channel not found")
		}
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}

	got, err := h.Store.GetChannel(ctx, id)
	if err != nil {
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}
	got.Config = maskChannelConfig(got.Config)
	return c.JSON(http.StatusOK, got)
}

// DeleteChannel handles DELETE /api/v1/admin/alerts/channels/:id.
func (h *AdminHandlers) DeleteChannel(c echo.Context) error {
	id := c.Param("id")
	if err := h.Store.DeleteChannel(c.Request().Context(), id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return echoErr(c, http.StatusNotFound, "channel not found")
		}
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

// ChannelTest handles POST /api/v1/admin/alerts/channels/:id/test. It creates
// a synthetic alert, dispatches it via the configured sender, records the
// AlertDispatch row, and immediately resolves the alert so it does not
// linger in the admin inbox.
func (h *AdminHandlers) ChannelTest(c echo.Context) error {
	id := c.Param("id")
	ctx := c.Request().Context()

	ch, err := h.Store.GetChannel(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return echoErr(c, http.StatusNotFound, "channel not found")
		}
		return echoErr(c, http.StatusInternalServerError, err.Error())
	}

	sender, err := h.Senders.Get(ch.Type)
	if err != nil {
		return echoErr(c, http.StatusInternalServerError, "no sender for channel type "+ch.Type+": "+err.Error())
	}

	now := time.Now().UTC()
	synth := Alert{
		RuleID:      "system.channel_test",
		SourceType:  "system",
		TargetKey:   "channel:" + ch.ID,
		TargetLabel: "channel test — " + ch.Name,
		Severity:    SeverityInfo,
		State:       StateFiring,
		Message:     "Synthetic alert dispatched by channel test.",
		Details:     map[string]any{"channelId": ch.ID, "channelName": ch.Name},
		FiredAt:     now,
		LastSeenAt:  now,
	}
	alertID, err := h.Store.InsertAlert(ctx, synth)
	if err != nil {
		return echoErr(c, http.StatusInternalServerError, "insert synthetic alert: "+err.Error())
	}
	synth.ID = alertID

	statusCode, sendErr := sender.Send(ctx, *ch, synth)

	var scPtr *int
	if statusCode != 0 {
		sc := statusCode
		scPtr = &sc
	}
	success := sendErr == nil && statusCode < 400
	var errMsgPtr *string
	if sendErr != nil {
		msg := sendErr.Error()
		errMsgPtr = &msg
	}

	dispatchID, dispErr := h.Store.InsertDispatch(ctx, Dispatch{
		AlertID:     alertID,
		ChannelID:   ch.ID,
		ChannelName: ch.Name,
		Success:     success,
		StatusCode:  scPtr,
		ErrorMsg:    errMsgPtr,
		AttemptedAt: time.Now().UTC(),
	})
	if dispErr != nil {
		h.logger().Error("channel test: write dispatch", "err", dispErr, "alertId", alertID, "channelId", ch.ID)
	}

	// Always resolve the sentinel alert so it does not appear in the admin
	// inbox — it exists solely for FK integrity on the dispatch row.
	if resErr := h.Store.ResolveAlert(ctx, alertID, "system", "channel-test-auto-resolve"); resErr != nil {
		h.logger().Error("channel test: resolve synthetic", "err", resErr, "alertId", alertID)
	}

	if !success {
		errStr := ""
		if sendErr != nil {
			errStr = sendErr.Error()
		}
		return c.JSON(http.StatusInternalServerError, map[string]any{
			"success":    false,
			"error":      errStr,
			"statusCode": statusCode,
			"dispatchId": dispatchID,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"success":    true,
		"statusCode": statusCode,
		"dispatchId": dispatchID,
	})
}

// parseFlexibleTime parses an RFC3339 or RFC3339Nano time string. The latter
// covers JS Date.toISOString() output which always includes milliseconds
// (e.g. "2024-01-01T00:00:00.000Z"). Returns (zero, false) on failure.
func parseFlexibleTime(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// echoErr writes a uniform JSON error envelope.
func echoErr(c echo.Context, code int, msg string) error {
	return c.JSON(code, map[string]string{"error": msg})
}

// errEmptyBody is recognized by decoders to allow an empty request body.
var errEmptyBody = errors.New("empty body")

// validateParamsAgainstSchema compiles the rule's paramsSchema and validates
// the submitted params blob. Returns a descriptive error on failure.
func validateParamsAgainstSchema(paramsJSON json.RawMessage, schema map[string]any) error {
	if schema == nil {
		return nil
	}
	schemaBytes, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("marshal schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("schema.json", bytes.NewReader(schemaBytes)); err != nil {
		return fmt.Errorf("add schema resource: %w", err)
	}
	compiled, err := compiler.Compile("schema.json")
	if err != nil {
		return fmt.Errorf("compile schema: %w", err)
	}
	var doc any
	if err := json.Unmarshal(paramsJSON, &doc); err != nil {
		return fmt.Errorf("parse params: %w", err)
	}
	if err := compiled.Validate(doc); err != nil {
		return err
	}
	return nil
}

const maskPrefix = "xxxx-••••-"

// sensitiveKeys lists the (lowercased) top-level config keys whose values
// must be masked in responses.
var sensitiveKeys = map[string]bool{
	"bottoken":     true,
	"smtppassword": true,
	"routingkey":   true,
}

// sensitiveHeaderNeedles are case-insensitive substrings that mark a header
// name as sensitive.
var sensitiveHeaderNeedles = []string{"authorization", "token", "secret"}

// maskValue replaces s with a maskPrefix + last-4 form. Values shorter than
// four characters are masked to maskPrefix alone.
func maskValue(s string) string {
	if len(s) >= 4 {
		return maskPrefix + s[len(s)-4:]
	}
	return maskPrefix
}

// isMasked reports whether s looks like a value we already masked (so a PUT
// round-trip can restore the original).
func isMasked(s string) bool {
	return strings.HasPrefix(s, maskPrefix)
}

// headerKeyIsSensitive reports whether a header name (case-insensitive)
// contains any of the sensitive-header needles.
func headerKeyIsSensitive(k string) bool {
	lk := strings.ToLower(k)
	for _, n := range sensitiveHeaderNeedles {
		if strings.Contains(lk, n) {
			return true
		}
	}
	return false
}

// MaskChannelConfig is the exported mask helper. Tests use this to verify
// masking behaviour end-to-end without going through an HTTP handler.
func MaskChannelConfig(cfg map[string]any) map[string]any { return maskChannelConfig(cfg) }

// maskChannelConfig returns a deep copy of cfg with sensitive values replaced
// by maskPrefix + last4(original). Top-level keys bottoken/smtpPassword/
// routingKey are masked (case-insensitive match). Inside a "headers" map
// (case-insensitive key), any header whose name contains "authorization",
// "token", or "secret" (case-insensitive substring) is masked.
func maskChannelConfig(cfg map[string]any) map[string]any {
	if cfg == nil {
		return nil
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		lk := strings.ToLower(k)
		if sensitiveKeys[lk] {
			if s, ok := v.(string); ok {
				out[k] = maskValue(s)
				continue
			}
		}
		if lk == "headers" {
			if sub, ok := v.(map[string]any); ok {
				out[k] = maskHeaders(sub)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// maskHeaders returns a deep copy of a headers map with sensitive header
// values masked.
func maskHeaders(h map[string]any) map[string]any {
	out := make(map[string]any, len(h))
	for k, v := range h {
		if headerKeyIsSensitive(k) {
			if s, ok := v.(string); ok {
				out[k] = maskValue(s)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// mergeMaskedSecrets replaces any masked value in incoming with the
// corresponding value from existing. It scans exactly the same key set that
// maskChannelConfig covers, so a PUT round-trip of a GET response preserves
// the original secret.
func mergeMaskedSecrets(incoming, existing map[string]any) map[string]any {
	out := make(map[string]any, len(incoming))
	for k, v := range incoming {
		out[k] = v
	}
	for k, v := range incoming {
		lk := strings.ToLower(k)
		if sensitiveKeys[lk] {
			if s, ok := v.(string); ok && isMasked(s) {
				if orig, ok := existing[k]; ok {
					out[k] = orig
				}
			}
			continue
		}
		if lk == "headers" {
			sub, ok := v.(map[string]any)
			if !ok {
				continue
			}
			existingSub, _ := existing[k].(map[string]any)
			out[k] = mergeMaskedHeaders(sub, existingSub)
		}
	}
	return out
}

// mergeMaskedHeaders handles masked secret merging inside a headers map.
func mergeMaskedHeaders(incoming, existing map[string]any) map[string]any {
	out := make(map[string]any, len(incoming))
	for k, v := range incoming {
		out[k] = v
	}
	if existing == nil {
		return out
	}
	for k, v := range incoming {
		if !headerKeyIsSensitive(k) {
			continue
		}
		s, ok := v.(string)
		if !ok || !isMasked(s) {
			continue
		}
		if orig, ok := existing[k]; ok {
			out[k] = orig
		}
	}
	return out
}
