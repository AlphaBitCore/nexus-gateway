package infra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	cfgpolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// thingOverrideGroupLookup is the narrow read seam used by the override
// + force-sync handlers to resolve the caller's IAM group memberships.
// *store.DB satisfies it in production; tests inject a stub so the
// type-scope branch can be exercised without standing up Postgres.
type thingOverrideGroupLookup interface {
	ListGroupNamesForPrincipal(ctx context.Context, principalType, principalID string) ([]string, error)
}

// RegisterAdminNodeOverridesRoutes wires the admin per-Node override CRUD
// + global registry + force-sync endpoints. The override mutation routes use
// `admin:WriteNodeOverride`; the read routes share `admin:ReadSettings`
// with the rest of the Infrastructure surface; force-sync uses
// `admin:ForceResyncNode` so it can be granted independently of override
// writes (e.g. an on-call SRE may resync without being able to mutate
// configs).
//
// The override mutation path is a thin RBAC + validation gate around Hub —
// Hub.SetOverride / Hub.ClearOverride writes the admin_audit_log row in-tx,
// so this CP handler MUST NOT call AuditWriter.Log on those routes (doing
// so would produce two audit rows per user action, breaking AC9). Force
// resync is the inverse: Hub does not audit redelivery, so CP does.
func (h *Handler) RegisterAdminNodeOverridesRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	// Static `/nodes/overrides` registered before parametric `/nodes/:id/...`
	// so Echo's first-match routing does not shadow it. Within Echo's route
	// trie the static segment wins over `:id`, but we keep the registration
	// order conservative for readability + parity with hub_api routes.go.
	g.GET("/nodes/overrides", h.ListGlobalOverrides, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
	g.GET("/nodes/:id/overrides", h.ListNodeOverrides, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
	g.PUT("/nodes/:id/overrides/:configKey", h.SetNodeOverride, iamMW(iam.ResourceNode.Action(iam.VerbWriteOverride)))
	g.DELETE("/nodes/:id/overrides/:configKey", h.ClearNodeOverride, iamMW(iam.ResourceNode.Action(iam.VerbWriteOverride)))
	g.POST("/nodes/:id/resync", h.AdminResyncNode, iamMW(iam.ResourceNode.Action(iam.VerbForceResync)))
}

// serviceThingTypes are the Thing types that provider_admin (group
// `provider-managers`) is authorised to override + force-sync. Mirrors
// spec §4.4.
var serviceThingTypes = map[string]bool{
	"ai-gateway":       true,
	"compliance-proxy": true,
	"control-plane":    true,
	"nexus-hub":        true,
}

// agentThingType is the singleton Thing type compliance_admin (group
// `compliance-team`) is authorised to override + force-sync.
const agentThingType = "agent"

// adminGroupSuperAdmins / adminGroupProviderManagers /
// adminGroupComplianceTeam name the IAM groups whose membership grants
// type-scoped access. Matches the seed in tools/db-migrate/seed/seed.ts.
const (
	adminGroupSuperAdmins       = "super-admins"
	adminGroupProviderManagers  = "provider-managers"
	adminGroupComplianceTeam    = "compliance-team"
	adminGroupComplianceTeamAlt = "compliance-officers" // future-proof alias if seed renames
)

// thingTypeRBACDecision says whether a caller's IAM group set is
// authorised to operate (override / force-sync) on a Thing of the given
// type. Returns true only when at least one membership matches the
// type-scope rule.
//
// super-admins → any type
// provider-managers → service types only
// compliance-team   → agent type only
//
// Empty group set → false (the IAM gate already rejected viewers
// upstream; defence-in-depth here keeps the policy explicit).
func thingTypeRBACDecision(groups []string, thingType string) bool {
	for _, g := range groups {
		switch g {
		case adminGroupSuperAdmins:
			return true
		case adminGroupProviderManagers:
			if serviceThingTypes[thingType] {
				return true
			}
		case adminGroupComplianceTeam, adminGroupComplianceTeamAlt:
			if thingType == agentThingType {
				return true
			}
		}
	}
	return false
}

// hubPreflightResult is the outcome of fetchThingType. Only one of
// (ThingType, Passthrough, BadGateway, NotConfigured) is meaningful per
// invocation; the caller picks the corresponding response shape.
type hubPreflightResult struct {
	// ThingType is the resolved Thing.type on a Hub 2xx with non-empty type.
	ThingType string
	// Passthrough holds Hub's verbatim 4xx body + status when the Hub
	// rejected the pre-flight read with a client error (validation, RBAC
	// at Hub layer, missing auth on the CP→Hub hop). The handler must
	// echo `Status` + `Body` to the admin so the operator sees Hub's
	// actual reason instead of a generic 502.
	Passthrough *hubPassthrough
	// BadGateway carries the slog message for the case where Hub is
	// reachable-but-broken (5xx, transport error, malformed JSON,
	// empty type). The handler maps this to errJSON 502 HUB_UNREACHABLE.
	BadGateway error
	// NotConfigured fires when the CP has no Hub binding wired (process
	// startup misconfiguration). Maps to 503 HUB_NOT_CONFIGURED.
	NotConfigured bool
	// NotFound fires on a Hub 404 specifically — the handler maps it to
	// 404 NOT_FOUND so the admin sees "node not found" rather than the
	// generic 4xx pass-through wording.
	NotFound bool
}

// hubPassthrough carries a Hub 4xx response that should be echoed
// verbatim to the admin.
type hubPassthrough struct {
	Status int
	Body   []byte
}

// fetchThingType reads the target Thing's type from Hub so the RBAC gate
// can decide before forwarding. Hub returns the canonical Thing record
// shape; we only need `.type` for this check.
//
// Error classification:
//
//   - 2xx with valid `.type` → Result.ThingType
//   - 404                   → Result.NotFound (handler returns clean
//     "node not found" to the admin)
//   - other 4xx             → Result.Passthrough (Hub-side validation /
//     RBAC failure; handler echoes Hub's status + body verbatim so the
//     operator sees the actual reason)
//   - 5xx / transport / malformed → Result.BadGateway (handler returns
//     502 HUB_UNREACHABLE — the genuinely-broken case)
//   - Hub not configured          → Result.NotConfigured (503)
func (h *Handler) fetchThingType(c echo.Context, thingID string) hubPreflightResult {
	if h.hub == nil || h.hub.BaseURL() == "" {
		return hubPreflightResult{NotConfigured: true}
	}
	req, err := http.NewRequestWithContext(c.Request().Context(),
		http.MethodGet, h.hub.BaseURL()+"/api/hub/things/"+url.PathEscape(thingID), nil)
	if err != nil {
		return hubPreflightResult{BadGateway: err}
	}
	req.Header.Set("Authorization", "Bearer "+h.hub.Token())
	resp, err := h.hubProxyClient().Do(req)
	if err != nil {
		return hubPreflightResult{BadGateway: err}
	}
	defer resp.Body.Close() //nolint:errcheck

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return hubPreflightResult{BadGateway: fmt.Errorf("read hub body: %w", readErr)}
	}

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return hubPreflightResult{NotFound: true}
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var thing struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(body, &thing); err != nil {
			return hubPreflightResult{BadGateway: fmt.Errorf("decode hub body: %w", err)}
		}
		if thing.Type == "" {
			return hubPreflightResult{BadGateway: errors.New("hub returned empty type")}
		}
		return hubPreflightResult{ThingType: thing.Type}
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// Hub validation / RBAC reject — pass through verbatim so the
		// admin sees the actual reason rather than a generic 502.
		return hubPreflightResult{Passthrough: &hubPassthrough{Status: resp.StatusCode, Body: body}}
	default:
		// 5xx / 1xx / 3xx: Hub is reachable but the response is not a
		// usable answer. Treat as bad gateway.
		return hubPreflightResult{BadGateway: fmt.Errorf("hub returned %d", resp.StatusCode)}
	}
}

// writePreflightError translates a non-success preflight outcome into
// the admin-facing HTTP response. Returns nil so handlers can `return
// h.writePreflightError(...)` directly.
//
// `op` names the calling operation for the bad-gateway warn log
// (e.g. "set override", "clear override", "resync"). Caller must have
// already checked that the result is one of the failure variants —
// passing a successful result is a programming error.
func (h *Handler) writePreflightError(c echo.Context, op, thingID string, r hubPreflightResult) error {
	switch {
	case r.NotConfigured:
		return c.JSON(http.StatusServiceUnavailable, errJSON("hub not configured", "server_error", "HUB_NOT_CONFIGURED"))
	case r.NotFound:
		return c.JSON(http.StatusNotFound, errJSON("node not found", "not_found", "NOT_FOUND"))
	case r.Passthrough != nil:
		// Echo Hub's status + body verbatim. Use JSONBlob so the body
		// is byte-for-byte preserved (assumes Hub returned application/json,
		// which is the contract for all /api/hub/* error responses).
		return c.JSONBlob(r.Passthrough.Status, r.Passthrough.Body)
	default:
		// BadGateway: log the underlying reason at warn so on-call ops can
		// trace it without exposing internals to the admin.
		h.logger.Warn(op+": fetch thing type failed",
			"thingId", thingID,
			"error", errString(r.BadGateway),
		)
		return c.JSON(http.StatusBadGateway, errJSON("hub unreachable", "server_error", "HUB_UNREACHABLE"))
	}
}

// errString returns err.Error() or "" if err is nil. Centralised here so
// the warn-log call sites stay one-liner-friendly.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// listAdminGroups returns the IAM group memberships for the current
// principal. On lookup failure returns nil (the type-scope check then
// fails closed and returns 403, which is the safe behaviour).
func (h *Handler) listAdminGroups(c echo.Context) []string {
	aa := middleware.AdminAuthFromContext(c)
	if aa == nil {
		return nil
	}
	lookup := h.thingOverrideGroupLookupFn()
	if lookup == nil {
		return nil
	}
	pt := aa.AuthPrincipalType
	if pt == "admin_user" {
		pt = "nexus_user"
	}
	groups, err := lookup.ListGroupNamesForPrincipal(c.Request().Context(), pt, aa.KeyID)
	if err != nil || groups == nil {
		return nil
	}
	return groups
}

// thingOverrideGroupLookup returns the test override when set, otherwise
// falls back to *store.DB. Mirrors appliedConfigStoreFromHandler /
// payloadCaptureMetadataStoreFromHandler.
func (h *Handler) thingOverrideGroupLookupFn() thingOverrideGroupLookup {
	if h.thingOverrideGroupLookupRef != nil {
		return h.thingOverrideGroupLookupRef
	}
	if h.db == nil {
		return nil
	}
	return h.db
}

// ListNodeOverrides handles GET /api/admin/nodes/:id/overrides.
//
// Pure proxy to Hub. RBAC is read-side admin:ReadSettings (granted by the
// IAM middleware), so no type-scope check applies — viewing the override
// list is allowed for every admin who can see Infrastructure pages.
func (h *Handler) ListNodeOverrides(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("id is required", "validation_error", "VALIDATION_ERROR"))
	}
	return h.hubForward(c, http.MethodGet,
		"/api/hub/things/"+url.PathEscape(id)+"/overrides", nil)
}

// adminSetOverrideBody is the wire shape CP accepts. State must be a JSON
// object at top level (not array / scalar / null); reason ≤ 500 chars;
// expiresAt - NOW() ∈ [5m, 30d] when set. Duplicates Hub's own validation
// so well-formed admin traffic gets a clean 400 here without the round
// trip + so the policy stays version-pinned to the OpenAPI contract.
type adminSetOverrideBody struct {
	State     json.RawMessage `json:"state"`
	Reason    *string         `json:"reason,omitempty"`
	ExpiresAt *time.Time      `json:"expiresAt,omitempty"`
}

// validateAdminOverrideBody runs the local validation gate. Returns
// (humanError, "" on success). The caller maps a non-empty error to a
// 400 response with the wire-format envelope.
func validateAdminOverrideBody(configKey string, body adminSetOverrideBody) string {
	if !cfgpolicy.IsOverridable(configKey) {
		return "config key not overridable"
	}
	if len(body.State) == 0 {
		return "state is required"
	}
	if !isJSONObjectAdmin(body.State) {
		return "state must be a JSON object"
	}
	if body.Reason != nil && len(*body.Reason) > 500 {
		return "reason exceeds 500 chars"
	}
	if body.ExpiresAt != nil {
		delta := time.Until(*body.ExpiresAt)
		if delta < 5*time.Minute || delta > 30*24*time.Hour {
			return "expiresAt out of range [5m, 30d]"
		}
	}
	return ""
}

// isJSONObjectAdmin mirrors the Hub-side helper but lives in this package
// to avoid a circular import. Returns true only when raw decodes to a
// JSON object at the top level.
func isJSONObjectAdmin(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal(raw, &obj) == nil
}

// SetNodeOverride handles PUT /api/admin/nodes/:id/overrides/:configKey.
//
// CP performs RBAC type-scope + body validation locally, then forwards
// to Hub which writes the override + audit row in-tx. Per the P-B1
// review constraint this handler MUST NOT call AuditWriter.Log — Hub
// already wrote the audit row, and a second CP write would break the
// "exactly one admin_audit_log row per mutation" invariant.
func (h *Handler) SetNodeOverride(c echo.Context) error {
	id := c.Param("id")
	configKey := c.Param("configKey")
	if id == "" || configKey == "" {
		return c.JSON(http.StatusBadRequest, errJSON("id and configKey are required", "validation_error", "VALIDATION_ERROR"))
	}

	// Buffer + parse body so we can validate before any Hub call.
	raw, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("failed to read body", "validation_error", "VALIDATION_ERROR"))
	}
	var body adminSetOverrideBody
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return c.JSON(http.StatusBadRequest, errJSON("invalid request body", "validation_error", "VALIDATION_ERROR"))
		}
	}
	if msg := validateAdminOverrideBody(configKey, body); msg != "" {
		return c.JSON(http.StatusBadRequest, errJSON(msg, "validation_error", "VALIDATION_ERROR"))
	}

	// RBAC type-scope check: requires fetching the Thing's type from Hub.
	pre := h.fetchThingType(c, id)
	if pre.ThingType == "" {
		return h.writePreflightError(c, "set override", id, pre)
	}
	if !thingTypeRBACDecision(h.listAdminGroups(c), pre.ThingType) {
		return c.JSON(http.StatusForbidden, errJSON("role cannot operate on this thing type", "forbidden", "TYPE_SCOPE_DENIED"))
	}

	// Restore the buffered body for the proxy hop.
	c.Request().Body = io.NopCloser(bytes.NewReader(raw))
	c.Request().ContentLength = int64(len(raw))

	// Forward to Hub. Audit row is written by Hub in-tx; we MUST NOT log
	// here. The proxy passes X-Nexus-Actor-Id / X-Nexus-Actor-Name through
	// hubForward so the Hub-side audit attribution carries the live admin
	// identity.
	return h.hubForward(c, http.MethodPut,
		"/api/hub/things/"+url.PathEscape(id)+"/overrides/"+url.PathEscape(configKey), nil)
}

// ClearNodeOverride handles DELETE /api/admin/nodes/:id/overrides/:configKey.
//
// Same RBAC type-scope rules as SetNodeOverride. Hub writes the audit
// row in-tx, so CP does not call AuditWriter.Log here.
func (h *Handler) ClearNodeOverride(c echo.Context) error {
	id := c.Param("id")
	configKey := c.Param("configKey")
	if id == "" || configKey == "" {
		return c.JSON(http.StatusBadRequest, errJSON("id and configKey are required", "validation_error", "VALIDATION_ERROR"))
	}

	pre := h.fetchThingType(c, id)
	if pre.ThingType == "" {
		return h.writePreflightError(c, "clear override", id, pre)
	}
	if !thingTypeRBACDecision(h.listAdminGroups(c), pre.ThingType) {
		return c.JSON(http.StatusForbidden, errJSON("role cannot operate on this thing type", "forbidden", "TYPE_SCOPE_DENIED"))
	}

	return h.hubForward(c, http.MethodDelete,
		"/api/hub/things/"+url.PathEscape(id)+"/overrides/"+url.PathEscape(configKey), nil)
}

// ListGlobalOverrides handles GET /api/admin/nodes/overrides.
//
// Pure proxy. Query params (type, actor, hasTtl, stale, limit, offset)
// pass through unchanged via hubForward's RawQuery propagation.
func (h *Handler) ListGlobalOverrides(c echo.Context) error {
	return h.hubForward(c, http.MethodGet, "/api/hub/things/overrides", nil)
}

// adminResyncBody mirrors the OpenAPI ResyncBody. ConfigKey is optional;
// when absent the request triggers a whole-Thing replay.
type adminResyncBody struct {
	ConfigKey string `json:"configKey"`
}

// AdminResyncNode handles POST /api/admin/nodes/:id/resync.
//
// Accepts an empty body for whole-Thing replay or {"configKey": "..."} for a
// single-key resync. Hub does NOT audit redelivery, so CP MUST log here.
// Audit action is `thing_force_resync` for single-key, `thing_force_resync_all`
// for whole-Thing.
func (h *Handler) AdminResyncNode(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("id is required", "validation_error", "VALIDATION_ERROR"))
	}

	raw, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("failed to read body", "validation_error", "VALIDATION_ERROR"))
	}
	var body adminResyncBody
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			return c.JSON(http.StatusBadRequest, errJSON("invalid request body", "validation_error", "VALIDATION_ERROR"))
		}
	}

	// Type-scope RBAC: same rules as override mutations. A caller who can
	// resync a service-type Thing's keys can also force them out-of-sync,
	// so the same policy applies.
	pre := h.fetchThingType(c, id)
	if pre.ThingType == "" {
		return h.writePreflightError(c, "resync", id, pre)
	}
	if !thingTypeRBACDecision(h.listAdminGroups(c), pre.ThingType) {
		return c.JSON(http.StatusForbidden, errJSON("role cannot operate on this thing type", "forbidden", "TYPE_SCOPE_DENIED"))
	}

	// Re-serialize a clean Hub-contract body — do not forward the admin
	// payload unchanged so unexpected fields are filtered out.
	hubPayload := map[string]any{}
	if body.ConfigKey != "" {
		hubPayload["configKey"] = body.ConfigKey
	}
	hubBody, err := json.Marshal(hubPayload)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, errJSON("failed to encode hub request", "server_error", ""))
	}
	c.Request().Body = io.NopCloser(bytes.NewReader(hubBody))
	c.Request().ContentLength = int64(len(hubBody))
	c.Request().Header.Set("Content-Type", "application/json")

	if err := h.hubForward(c, http.MethodPost,
		"/api/hub/things/"+url.PathEscape(id)+"/resync", nil); err != nil {
		return err
	}

	if c.Response().Status >= 200 && c.Response().Status < 300 {
		// Both single-key and all-key force-resyncs are VerbForceResync; the
		// "all keys" distinction goes in AfterState.scope so SIEM eventType
		// stays stable as node.force-resync.
		ae := audit.EntryFor(c, iam.ResourceNode, iam.VerbForceResync)
		if body.ConfigKey != "" {
			ae.AfterState = map[string]any{"configKey": body.ConfigKey, "scope": "single-key"}
		} else {
			ae.AfterState = map[string]any{"configKey": nil, "scope": "all-keys"}
		}
		ae.EntityID = id
		h.audit.LogObserved(c.Request().Context(), ae)
	}
	return nil
}
