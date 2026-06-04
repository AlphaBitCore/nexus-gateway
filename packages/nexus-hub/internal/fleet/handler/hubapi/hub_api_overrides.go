package hubapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	configtypes "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
)

// thingOverrideResponse is the wire shape returned by the
// override-related Hub HTTP endpoints. It mirrors the OpenAPI
// `ThingOverride` schema (camelCase) verbatim. The store-level types
// (store.ThingConfigOverride / ThingConfigOverrideWithStale) carry the
// same fields with default Go-struct field names that would not match
// the OpenAPI contract, so we project here.
type thingOverrideResponse struct {
	ConfigKey          string          `json:"configKey"`
	State              json.RawMessage `json:"state"`
	TemplateVerAtSet   int64           `json:"templateVerAtSet"`
	CurrentTemplateVer int64           `json:"currentTemplateVer"`
	Stale              bool            `json:"stale"`
	SetBy              string          `json:"setBy"`
	SetAt              time.Time       `json:"setAt"`
	Reason             *string         `json:"reason,omitempty"`
	ExpiresAt          *time.Time      `json:"expiresAt,omitempty"`
	EmergencyOverride  bool            `json:"emergencyOverride"`
}

// globalOverrideRowResponse extends thingOverrideResponse with the
// joined Thing identity columns, matching the OpenAPI
// `GlobalOverrideRow` schema. JSON field names follow the admin
// terminology boundary (CLAUDE.md): admin-facing surfaces use "node",
// not the internal "thing".
type globalOverrideRowResponse struct {
	thingOverrideResponse
	ThingID   string `json:"nodeId"`
	ThingName string `json:"nodeName"`
	ThingType string `json:"nodeType"`
}

// projectOverride converts the store row into the OpenAPI shape. For
// SetThingOverride the store may return a row through Manager that does
// not include current template version (no JOIN ran in the persistence
// path); the caller passes the template snapshot version that was
// captured at write time + sets stale=false explicitly.
func projectOverride(r store.ThingConfigOverride, currentTemplateVer int64, stale bool, logger *slog.Logger) thingOverrideResponse {
	stateBytes := r.State.Bytes()
	if len(stateBytes) == 0 {
		// Defensive: an empty state cannot occur in practice — store-level
		// UpsertOverride rejects empty bytes and OverrideState's constructor
		// rejects empty / non-object input. If we're here it means an
		// upstream bug just bypassed both gates; log loudly with a
		// well-known event so dashboards alert, then fall back to "{}" so
		// the response still satisfies the OpenAPI "state is an object"
		// schema and the admin UI doesn't crash.
		if logger != nil {
			logger.Warn("project override: state unexpectedly empty (upstream bug — store and OverrideState both reject empty)",
				slog.String("event", "override_state_unexpected_empty"),
				slog.String("thing_id", r.ThingID),
				slog.String("config_key", r.ConfigKey),
			)
		}
		stateBytes = []byte("{}")
	}
	return thingOverrideResponse{
		ConfigKey:          r.ConfigKey,
		State:              stateBytes,
		TemplateVerAtSet:   r.TemplateVerAtSet,
		CurrentTemplateVer: currentTemplateVer,
		Stale:              stale,
		SetBy:              r.SetBy,
		SetAt:              r.SetAt,
		Reason:             r.Reason,
		ExpiresAt:          r.ExpiresAt,
		EmergencyOverride:  r.EmergencyOverride,
	}
}

func projectOverrideWithStale(r store.ThingConfigOverrideWithStale, logger *slog.Logger) thingOverrideResponse {
	out := projectOverride(r.ThingConfigOverride, r.CurrentTemplateVer, r.Stale, logger)
	return out
}

// setOverrideBody mirrors the OpenAPI SetOverrideBody. State is kept as
// raw JSON so the manager can pass it straight through to the store
// without an extra encode hop. Reason / ExpiresAt are pointers so the
// HTTP layer can distinguish "field absent" from "field set to zero".
type setOverrideBody struct {
	State     json.RawMessage `json:"state"`
	Reason    *string         `json:"reason,omitempty"`
	ExpiresAt *time.Time      `json:"expiresAt,omitempty"`
}

// ListThingOverrides handles GET /api/hub/things/:id/overrides.
//
// Returns every active override on the given Thing, with current
// template version + stale flag JOINed in so the caller does not need a
// second round-trip. Empty list (no overrides) is a 200 with
// `{"overrides":[]}` — caller distinguishes "no overrides" from "thing
// not found" via the explicit GetThing call below.
func (h *HubAPI) ListThingOverrides(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return badRequest(c, "thing id is required")
	}

	if _, err := h.Mgr.Store().RegistryStore().GetThing(c.Request().Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return notFound(c, "thing not found")
		}
		return internalError(c, "list overrides failed")
	}

	rows, err := h.Mgr.Store().OverrideStore().ListOverridesByThing(c.Request().Context(), id)
	if err != nil {
		return internalError(c, "list overrides failed")
	}

	out := make([]thingOverrideResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, projectOverrideWithStale(r, h.logger()))
	}

	return c.JSON(http.StatusOK, map[string]any{"overrides": out})
}

// SetThingOverride handles PUT /api/hub/things/:id/overrides/:configKey.
//
// Hub is the last line of defense for validation: the CP admin handler
// already checked the same rules but a non-CP caller (CLI script, test
// harness) must not be able to bypass them. We re-run blacklist /
// JSON-object / reason-length / TTL-window checks here.
//
// On success the underlying Manager.SetOverride writes the override
// row, recomputes thing.desired, bumps desired_ver, writes the audit
// row, and force-pushes the affected key — all in a single
// transaction. The handler simply projects the persisted row to the
// OpenAPI shape.
func (h *HubAPI) SetThingOverride(c echo.Context) error {
	id := c.Param("id")
	configKey := c.Param("configKey")
	if id == "" || configKey == "" {
		return badRequest(c, "thing id and configKey are required")
	}

	var body setOverrideBody
	if err := c.Bind(&body); err != nil {
		return badRequest(c, "invalid request body")
	}

	if err := validateOverrideBody(configKey, body); err != nil {
		return badRequest(c, err.Error())
	}

	actor := c.Request().Header.Get("X-Nexus-Actor-Id")
	if actor == "" {
		// Body-side actor injection is not part of the contract for
		// this route — Hub treats X-Nexus-Actor-Id as ambient audit
		// metadata. Empty actor falls through to the manager which
		// will write an empty actor_id; CP enforces non-empty in
		// practice via session.
		actor = c.Request().Header.Get("X-Nexus-Actor-Name")
	}

	req := manager.SetOverrideRequest{
		ThingID:   id,
		ConfigKey: configKey,
		State:     body.State,
		SetBy:     actor,
		Reason:    body.Reason,
		ExpiresAt: body.ExpiresAt,
	}

	persisted, err := h.Mgr.SetOverride(c.Request().Context(), req)
	if err != nil {
		switch {
		case errors.Is(err, manager.ErrTemplateMissing):
			return badRequest(c, "no template exists for this key")
		case errors.Is(err, manager.ErrKeyNotOverridable):
			return badRequest(c, "config key not overridable")
		case errors.Is(err, store.ErrNotFound):
			return notFound(c, "thing not found")
		default:
			return internalError(c, "set override failed")
		}
	}

	// The persisted record reports `template_ver_at_set` but not the
	// `current` template version because the snapshot was taken at the
	// moment of write — they are equal by construction immediately
	// after the upsert, so stale=false here.
	resp := projectOverride(*persisted, persisted.TemplateVerAtSet, false, h.logger())
	return c.JSON(http.StatusOK, resp)
}

// ClearThingOverride handles DELETE /api/hub/things/:id/overrides/:configKey.
//
// Returns 404 (not idempotent 200) when no override exists for the
// pair, matching the OpenAPI contract. Manager writes the audit row
// and force-pushes the reverted key; on success this handler returns
// the canonical {"ok":true} envelope.
func (h *HubAPI) ClearThingOverride(c echo.Context) error {
	id := c.Param("id")
	configKey := c.Param("configKey")
	if id == "" || configKey == "" {
		return badRequest(c, "thing id and configKey are required")
	}

	actor := c.Request().Header.Get("X-Nexus-Actor-Id")
	if actor == "" {
		actor = c.Request().Header.Get("X-Nexus-Actor-Name")
	}

	if err := h.Mgr.ClearOverride(c.Request().Context(), id, configKey, actor); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return notFound(c, "no active override for this key")
		}
		return internalError(c, "clear override failed")
	}

	return c.JSON(http.StatusOK, map[string]any{"ok": true})
}

// ListGlobalOverrides handles GET /api/hub/things/overrides.
//
// Drives the admin "Global override registry" page. Filters are
// optional and any subset; pagination clamps to [1, 500] / [0, +inf].
// The summary block reflects the same WHERE clause as the rows so a
// filtered view shows summary numbers for that filter.
func (h *HubAPI) ListGlobalOverrides(c echo.Context) error {
	filter := store.ListOverridesFilter{
		Limit:  parseIntDefault(c.QueryParam("limit"), 50),
		Offset: parseIntDefault(c.QueryParam("offset"), 0),
	}
	if v := c.QueryParam("type"); v != "" {
		filter.ThingType = &v
	}
	if v := c.QueryParam("actor"); v != "" {
		filter.Actor = &v
	}
	if v := c.QueryParam("hasTtl"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			// Surface the parse failure rather than silently dropping the
			// filter — admins typing "yes" / "1" / "nope" must get a clear
			// signal that the filter wasn't applied, not a "no overrides
			// match" empty list.
			return badRequest(c, "invalid hasTtl: must be true or false")
		}
		filter.HasTTL = &b
	}
	if v := c.QueryParam("stale"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return badRequest(c, "invalid stale: must be true or false")
		}
		filter.Stale = &b
	}

	rows, total, summary, err := h.Mgr.Store().OverrideStore().ListAllOverrides(c.Request().Context(), filter)
	if err != nil {
		return internalError(c, "list global overrides failed")
	}

	out := make([]globalOverrideRowResponse, 0, len(rows))
	for _, r := range rows {
		out = append(out, globalOverrideRowResponse{
			thingOverrideResponse: projectOverrideWithStale(r.ThingConfigOverrideWithStale, h.logger()),
			ThingID:               r.ThingID,
			ThingName:             r.ThingName,
			ThingType:             r.ThingType,
		})
	}

	return c.JSON(http.StatusOK, map[string]any{
		"overrides": out,
		"total":     total,
		"summary": map[string]any{
			"totalNodes":        summary.TotalNodes,
			"totalOverrides":    summary.TotalOverrides,
			"staleCount":        summary.StaleCount,
			"expiringSoonCount": summary.ExpiringSoonCount,
		},
	})
}

// validateOverrideBody enforces the OpenAPI-level constraints that any
// non-CP caller must also obey. CP runs the same checks before
// forwarding so well-formed admin traffic never reaches this branch.
//
// Order matches CP's validateAdminOverrideBody: blacklist first
// (cheapest gate, hits the largest legit-but-rejected class), then
// the body-shape checks. Keeping the two sides aligned makes the
// "well-formed admin traffic never reaches this branch" invariant
// trivially observable — drift in either order would otherwise let a
// blacklisted-key + malformed-body request return different 400
// messages from CP vs. Hub depending on which check fired first.
func validateOverrideBody(configKey string, body setOverrideBody) error {
	// configtypes.IsOverridable is the canonical blacklist gate; the
	// manager re-checks on write but rejecting here gives a clean 400
	// before we even open a tx.
	if !configtypes.IsOverridable(configKey) {
		return errors.New("config key not overridable")
	}
	if len(body.State) == 0 {
		return errors.New("state is required")
	}
	if !isJSONObject(body.State) {
		return errors.New("state must be a JSON object")
	}
	if body.Reason != nil && len(*body.Reason) > 500 {
		return errors.New("reason exceeds 500 chars")
	}
	if body.ExpiresAt != nil {
		delta := time.Until(*body.ExpiresAt)
		if delta < 5*time.Minute || delta > 30*24*time.Hour {
			return errors.New("expiresAt out of range [5m, 30d]")
		}
	}
	return nil
}

// isJSONObject reports whether raw decodes to a JSON object at the top
// level (not array, scalar, or null). The cheaper alternative — peeking
// at the first non-whitespace byte for `{` — would accept malformed
// JSON like `{ "foo": ] }`; we want only well-formed objects.
func isJSONObject(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal(raw, &obj) == nil
}
