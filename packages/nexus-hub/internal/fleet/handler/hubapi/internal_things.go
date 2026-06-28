package hubapi

import (
	"encoding/base64"
	"errors"
	"github.com/goccy/go-json"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/thingtype"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// InternalThingsAPI implements /api/internal/things/* endpoints.
type InternalThingsAPI struct {
	Mgr        *manager.Manager
	MQProducer mq.Producer
	// CatB aggregates authoritative Cat B payloads from CP-owned
	// business tables. Nil is supported: SingleConfigPull falls
	// through to the legacy thing_config_template.state path, so the
	// absence of a registry is byte-identical to pre-P0-C behaviour.
	CatB *store.CatBRegistry
}

// requireThingMatch enforces cross-Thing object-level authorization on the
// internal Things API. DeviceOrServiceAuth attaches the authenticated
// *store.Thing to the context for device-token callers (agents) and nothing for
// service-token callers (CP / Hub-internal). When a device token is present, the
// thing id the request operates on — taken from the body or query, NOT from the
// authenticated identity — MUST equal the caller's own id; otherwise an enrolled
// agent could act on another Thing's row (read its config, overwrite its shadow,
// deregister it) by substituting that id. The WebSocket path already binds the
// authenticated identity this way; these HTTP fallbacks must match it. Service-
// token callers (thing == nil) are trusted and bypass the check.
//
// Reports whether the request is BLOCKED: it writes the 403 response and
// returns true when a device-token caller's id does not match, in which case the
// handler must stop (`return nil`). Returns false when authorized (matching id,
// or a service-token caller). Callers that also need the resolved Thing (e.g.
// its Name) read ThingFromContext directly after this returns false.
func requireThingMatch(c echo.Context, operatedID string) bool {
	if thing := ThingFromContext(c); thing != nil && thing.ID != operatedID {
		_ = forbidden(c, "thingId does not match authenticated device")
		return true
	}
	return false
}

// requireMutationAuthority gates a device-MUTATION handler on the internal Things
// API (register / heartbeat / shadow / break-glass / deregister / exemption).
// It composes two object-level checks:
//
//  1. Device-token callers (an enrolled agent) — bound to their OWN id, exactly
//     as requireThingMatch: the operated id MUST equal the authenticated Thing's
//     id, else 403.
//  2. Service-token callers (thing == nil; CP / ai-gateway / compliance-proxy via
//     the thingclient HTTP fallback, or Hub-internal) — must NOT impersonate an
//     AGENT. A flat INTERNAL_SERVICE_TOKEN is shared fleet-wide, so without this
//     an attacker holding any one service's token could forge an arbitrary
//     agent's shadow, flip its kill-switch via break-glass, or deregister it.
//     An honest backend service only ever self-operates on its own
//     service-type Thing (the thingclient always sends its own ThingID), so the
//     request is allowed ONLY when the operated Thing is a backend-service type
//     (thingtype.IsBackendService) — an agent or unknown type is refused.
//
// operatedTypeHint short-circuits the type lookup when the caller already knows
// the type without a DB read (the register body carries `type`); pass "" to look
// it up by operatedID. For a service-token caller whose target id cannot be
// resolved, the request fails closed (403).
//
// Residual: a service token can still
// act as a DIFFERENT backend-service Thing, because the flat token does not
// identify which edge is calling. Containing that requires per-edge credentials.
//
// Reports whether the request is BLOCKED (writes the 403 and returns true).
func (h *InternalThingsAPI) requireMutationAuthority(c echo.Context, operatedID, operatedTypeHint string) bool {
	if thing := ThingFromContext(c); thing != nil {
		// Device-token caller: bind to its own id.
		if thing.ID != operatedID {
			_ = forbidden(c, "thingId does not match authenticated device")
			return true
		}
		return false
	}

	// Service-token caller: must not act as a non-service (agent) Thing.
	typ := operatedTypeHint
	if typ == "" {
		thing, err := h.Mgr.Store().RegistryStore().GetThing(c.Request().Context(), operatedID)
		if err != nil {
			// Unknown / unreadable target — a service token cannot prove it is
			// operating on a service Thing, so refuse (fail closed).
			_ = forbidden(c, "service token may not operate on an unknown thing")
			return true
		}
		typ = thing.Type
	}
	if !thingtype.IsBackendService(typ) {
		_ = forbidden(c, "service token may not act as an agent thing")
		return true
	}
	return false
}

// GetAttestationPubKey handles GET /api/internal/things/:id/attestation-pubkey.
//
// Compliance-Proxy calls this endpoint to populate the
// AttestationKeyCache loader at verify time. Returns 200 with
// `{"publicKey": "<base64>"}` when the agent enrolled with attestation;
// 404 when no key is on record (older agent build, or Hub's Ed25519
// signing failed during enrollment while the mTLS signing succeeded).
// 404 is the explicit "not enrolled with attestation yet" signal — CP
// MUST translate it to ErrUnknownAgent + cache the absence for the
// negative-TTL window so a fleet-wide rollout doesn't hammer Hub for
// every cold lookup.
//
// Auth: gated by DeviceOrServiceAuth at routes.go — service-token
// callers (CP) are the production consumer; device-token callers
// (agents) may use it for self-introspection without harm.
func (h *InternalThingsAPI) GetAttestationPubKey(c echo.Context) error {
	thingID := strings.TrimSpace(c.Param("id"))
	if thingID == "" {
		return badRequest(c, "thing id is required")
	}
	if requireThingMatch(c, thingID) {
		return nil
	}
	pub, certExpiresAt, err := h.Mgr.Store().RegistryStore().GetAttestationPubKeyWithExpiry(c.Request().Context(), thingID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return c.JSON(http.StatusNotFound, map[string]any{
				"error": "no attestation key on record for agent",
				"code":  "ATTESTATION_NOT_ENROLLED",
			})
		}
		return handleErr(c, err)
	}
	resp := map[string]any{
		"agentId":   thingID,
		"publicKey": base64.StdEncoding.EncodeToString(pub),
	}
	// Surface the cert NotAfter so CP can reject an expired key.
	// Omitted only for a legacy stamp that never recorded it (CP then treats the
	// key as non-expiring, fail-open).
	if !certExpiresAt.IsZero() {
		resp["certExpiresAt"] = certExpiresAt.UTC().Format(time.RFC3339)
	}
	return c.JSON(http.StatusOK, resp)
}

// Register handles POST /api/internal/things/register.
func (h *InternalThingsAPI) Register(c echo.Context) error {
	var req manager.RegisterRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.ID == "" || req.Type == "" {
		return badRequest(c, "id and type are required")
	}
	// The register body carries the type, so no lookup is needed. A service token
	// registering an agent-type Thing is an impersonation attempt (agents
	// self-register with their device token) and is refused.
	if h.requireMutationAuthority(c, req.ID, req.Type) {
		return nil
	}

	resp, err := h.Mgr.RegisterThing(c.Request().Context(), req)
	if err != nil {
		return internalError(c, "registration failed")
	}
	return c.JSON(http.StatusOK, map[string]any{
		"ack":        true,
		"desired":    resp.Desired,
		"desiredVer": resp.DesiredVer,
	})
}

// Heartbeat handles POST /api/internal/things/heartbeat.
func (h *InternalThingsAPI) Heartbeat(c echo.Context) error {
	var req manager.HeartbeatRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.ID == "" || req.Status == "" {
		return badRequest(c, "id and status are required")
	}
	if h.requireMutationAuthority(c, req.ID, "") {
		return nil
	}
	// Ambient-audit metadata: take the egress IP from Echo's RealIP
	// resolution. The agent doesn't know its own NAT public IP, so the
	// Hub stamps it on arrival for the DeviceAssignment.ip_address
	// refresh inside HandleHeartbeat.
	req.IPAddress = c.RealIP()

	resp, err := h.Mgr.HandleHeartbeat(c.Request().Context(), req)
	if err != nil {
		return handleErr(c, err)
	}
	return c.JSON(http.StatusOK, resp)
}

// ShadowReport handles POST /api/internal/things/shadow.
func (h *InternalThingsAPI) ShadowReport(c echo.Context) error {
	var req manager.ShadowReportRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.ID == "" {
		return badRequest(c, "id is required")
	}
	if req.Reported == nil {
		return badRequest(c, "reported is required (use {} for empty)")
	}
	if req.ReportedVer < 0 {
		return badRequest(c, "reportedVer must be non-negative")
	}
	// Break-glass has its own dedicated route (POST /shadow/break-glass); the
	// normal shadow path carries no reason. Rejecting any reason here closes
	// the hand-crafted "POST /shadow with reason=break_glass" bypass that
	// otherwise reaches the over-privileged reconciliation path.
	if req.Reason != "" {
		return badRequest(c, "shadow report must not carry a reason; use /shadow/break-glass for break-glass")
	}
	// Object-level authority, immediately before the mutation: a device caller is
	// bound to its own id; a service token may not overwrite an AGENT's shadow.
	// Placed after the cheap shape checks so a malformed body is
	// rejected without a Thing-type lookup.
	if h.requireMutationAuthority(c, req.ID, "") {
		return nil
	}

	if err := h.Mgr.HandleShadowReport(c.Request().Context(), req); err != nil {
		return handleErr(c, err)
	}
	return c.JSON(http.StatusOK, map[string]bool{"ack": true})
}

// BreakGlassReport handles POST /api/internal/things/shadow/break-glass — the
// HTTP fallback for the thingclient break-glass wire (the WebSocket path is the
// shadow_report_break_glass frame). The route IS the break-glass signal: the
// handler stamps Reason="break_glass" itself rather than trusting a body field,
// then enforces the server-side allowlist + schema gate so a
// disallowed key or malformed state returns 400 instead of a swallowed 200.
func (h *InternalThingsAPI) BreakGlassReport(c echo.Context) error {
	var req manager.ShadowReportRequest
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.ID == "" {
		return badRequest(c, "id is required")
	}
	if req.Reported == nil {
		return badRequest(c, "reported is required (use {} for empty)")
	}
	if req.ReportedVer < 0 {
		return badRequest(c, "reportedVer must be non-negative")
	}
	if req.ActorTokenID == "" {
		return badRequest(c, "break-glass report requires actorTokenId")
	}
	// The route is the break-glass signal — stamp the reconciliation sentinel
	// regardless of any body-supplied reason.
	req.Reason = "break_glass"
	// Server-side authority gate: reject a disallowed key / malformed
	// state with 400 before dispatch (the WS path enforces the same gate inside
	// handleBreakGlassReport, where there is no response channel).
	if err := manager.ValidateBreakGlassReport(req); err != nil {
		return badRequest(c, err.Error())
	}
	// Object-level authority, immediately before the mutation: a device caller is
	// bound to its own id; a service token may not flip an AGENT's kill-switch via
	// break-glass. After the shape + allowlist/schema checks so a malformed
	// report is rejected without a Thing-type lookup.
	if h.requireMutationAuthority(c, req.ID, "") {
		return nil
	}

	if err := h.Mgr.HandleShadowReport(c.Request().Context(), req); err != nil {
		return handleErr(c, err)
	}
	return c.JSON(http.StatusOK, map[string]bool{"ack": true})
}

// BulkConfigPull handles GET /api/internal/things/config.
//
// Query: type (required) — thing type; id (optional) — thing id. When id is set,
// the response uses that Thing row's desired JSON merged over templates and
// sets desiredVer to thing.desired_ver (monotonic shadow revision). Per-key
// "version" in configs is then the same global shadow version so HTTP-fallback
// thingclient can compare against reported_ver. When id is omitted, desiredVer
// is max(thing_config_template.version) across keys (legacy; not comparable to
// a Thing's reported_ver).
func (h *InternalThingsAPI) BulkConfigPull(c echo.Context) error {
	thingType := c.QueryParam("type")
	if thingType == "" {
		return badRequest(c, "type query parameter is required")
	}

	thingID := strings.TrimSpace(c.QueryParam("id"))
	// A device-token caller may pull a specific Thing's desired config only for
	// its own id; service-token callers (thing == nil) are trusted. No-op for the
	// legacy type-only pull (id empty), which returns type-level templates.
	if thingID != "" {
		if requireThingMatch(c, thingID) {
			return nil
		}
	}

	templates, err := h.Mgr.Store().ConfigStore().GetConfigTemplates(c.Request().Context(), thingType)
	if err != nil {
		return handleErr(c, err)
	}

	if thingID != "" {
		thing, err := h.Mgr.Store().RegistryStore().GetThing(c.Request().Context(), thingID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return badRequest(c, "unknown thing id")
			}
			return handleErr(c, err)
		}
		if thing.Type != thingType {
			return badRequest(c, "thing type does not match type query parameter")
		}
		configs := make(map[string]any, len(templates))
		for _, t := range templates {
			state := t.State
			if thing.Desired != nil {
				if v, ok := thing.Desired[t.ConfigKey]; ok && v != nil {
					state = v
				}
			}
			configs[t.ConfigKey] = map[string]any{
				"state":   state,
				"version": thing.DesiredVer,
			}
		}
		return c.JSON(http.StatusOK, map[string]any{
			"configs":    configs,
			"desiredVer": thing.DesiredVer,
		})
	}

	configs := make(map[string]any, len(templates))
	var maxVer int64
	for _, t := range templates {
		configs[t.ConfigKey] = map[string]any{
			"state":   t.State,
			"version": t.Version,
		}
		if t.Version > maxVer {
			maxVer = t.Version
		}
	}

	return c.JSON(http.StatusOK, map[string]any{
		"configs":    configs,
		"desiredVer": maxVer,
	})
}

// SingleConfigPull handles GET /api/internal/things/config/:key.
//
// Dispatch:
//   - (thingType, key) has a registered Cat B loader -> invoke it and
//     return the aggregated state. A loader error surfaces as 500
//     (we deliberately do NOT fall back to thing_config_template.state
//     on loader errors, because a silent fallback would blast an empty
//     payload at the Thing on a transient DB blip).
//   - No loader registered (or no registry) -> read
//     thing_config_template.state exactly as before. This is the
//     fallback for Cat A inline keys and for Cat B keys on Hub
//     instances that haven't wired the registry.
func (h *InternalThingsAPI) SingleConfigPull(c echo.Context) error {
	key := c.Param("key")
	thingType := c.QueryParam("type")
	if thingType == "" {
		return badRequest(c, "type query parameter is required")
	}

	if loader, ok := h.CatB.Lookup(thingType, key); ok {
		ctx := c.Request().Context()
		thingID := ""
		if t := ThingFromContext(c); t != nil {
			thingID = t.ID
		}
		state, version, err := loader.Load(ctx, thingID)
		if err != nil {
			return internalError(c, "catb load failed")
		}
		return c.JSON(http.StatusOK, map[string]any{
			"configKey": key,
			"state":     state,
			"version":   version,
			"source":    "loader",
		})
	}

	tpl, err := h.Mgr.Store().ConfigStore().GetConfigTemplate(c.Request().Context(), thingType, key)
	if err != nil {
		return handleErr(c, err)
	}

	return c.JSON(http.StatusOK, map[string]any{
		"configKey": tpl.ConfigKey,
		"state":     tpl.State,
		"version":   tpl.Version,
		"source":    "template",
	})
}

// AuditUpload handles POST /api/internal/things/audit (HTTP fallback for the
// thingclient WebSocket upload path; canonical agent emit goes through
// /agent-audit). Stamps each forwarded event with the caller's resolved
// thingId / thingName from DeviceOrServiceAuth so traffic_event.thing_id and
// thing_name reflect the mTLS-bound identity (the body's thingId is
// cross-checked, never trusted blindly).
func (h *InternalThingsAPI) AuditUpload(c echo.Context) error {
	var req struct {
		ThingID string           `json:"thingId"`
		Events  []map[string]any `json:"events"`
	}
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.ThingID == "" || len(req.Events) == 0 {
		return badRequest(c, "thingId and events are required")
	}

	// Object-level authority: identical gate to the other
	// six device-mutation handlers (register / heartbeat / shadow / break-glass /
	// deregister / exemption), so stamping audit rows for a thing_id is held to
	// the same bar as mutating that thing.
	//   - Device-token callers (the canonical agent path) are bound to their OWN
	//     id: the body's thingId must match the authenticated Thing, otherwise the
	//     agent is lying about its identity and is refused.
	//   - Service-token callers (Hub-internal) hold a fleet-shared
	//     INTERNAL_SERVICE_TOKEN; without this gate any one service's token could
	//     forge traffic_event audit rows attributed to an arbitrary AGENT node id.
	//     They may therefore only self-report for a backend-service Thing; an agent
	//     or unknown id is refused (type looked up by id — the audit body carries
	//     no type hint).
	// For an authorized device caller thing.Name is then taken Hub-side (below) —
	// no per-batch DB lookup, and no chance for the agent to forge thing_name.
	if h.requireMutationAuthority(c, req.ThingID, "") {
		return nil
	}
	var thingName string
	if thing := ThingFromContext(c); thing != nil {
		thingName = thing.Name
	}

	if h.MQProducer == nil {
		return serviceUnavailable(c, "event queue temporarily unavailable, retry later")
	}

	ctx := c.Request().Context()
	ids := make([]string, 0, len(req.Events))
	for _, evt := range req.Events {
		evt["thingId"] = req.ThingID
		if thingName != "" {
			evt["thingName"] = thingName
		}
		// Stamp source unconditionally — the agent's Event struct has no
		// Source field (its identity is implicit in the upload topic and
		// the AuditUpload entry point), but downstream `traffic_event.source`
		// has a CHECK constraint that rejects empty strings. Without this
		// stamp, every agent-uploaded event fails Hub's batch insert with
		// `chk_traffic_event_source` (SQLSTATE 23514) and stalls the
		// audit pipeline for every flow that originated on an agent.
		// Service-token callers (the rare hub-internal path) get the same
		// stamp; if a Hub-internal source ever needs a different label,
		// the caller should pre-set evt["source"] before invoking this
		// endpoint.
		if _, present := evt["source"]; !present {
			evt["source"] = "agent"
		}
		// Strip empty-string values for fields the traffic_event table
		// constrains via CHECK ANY(allowlist). The agent's audit.Event
		// marshals these as JSON "" when no extraction happened (e.g.
		// passthrough flows with no LLM body to parse), and Hub's
		// consumer/message.go declares them as *string so an absent JSON
		// key produces NULL → CHECK allows NULL. A present "" key,
		// however, produces a pointer-to-empty-string → SQL writes empty
		// string → CHECK rejects. Companion to the source-stamp above:
		// same class of bug, same failure mode (chk_traffic_event_*
		// SQLSTATE 23514 stalls the agent audit pipeline).
		for _, k := range []string{"usageExtractionStatus"} {
			if v, ok := evt[k].(string); ok && v == "" {
				delete(evt, k)
			}
		}
		data, err := json.Marshal(evt)
		if err != nil {
			continue
		}
		if err := h.MQProducer.Enqueue(ctx, "nexus.event.agent", data); err != nil {
			return serviceUnavailable(c, "event queue temporarily unavailable, retry later")
		}
		if id, ok := evt["id"].(string); ok {
			ids = append(ids, id)
		}
	}

	return c.JSON(http.StatusOK, map[string]any{
		"ack":      true,
		"accepted": len(ids),
		"eventIds": ids,
	})
}

// Deregister handles POST /api/internal/things/deregister.
func (h *InternalThingsAPI) Deregister(c echo.Context) error {
	var req struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.ID == "" {
		return badRequest(c, "id is required")
	}
	if h.requireMutationAuthority(c, req.ID, "") {
		return nil
	}

	if err := h.Mgr.Deregister(c.Request().Context(), req.ID); err != nil {
		return handleErr(c, err)
	}
	return c.JSON(http.StatusOK, map[string]bool{"ack": true})
}

// ExemptionUpload handles POST /api/internal/things/exemption.
// The agent uploads a compliance-exempt host (e.g. TLS-bump auto-exemption)
// and the Hub enqueues it on nexus.event.exemption for downstream processing.
// Exemptions ride a dedicated topic because their payload schema does not
// match TrafficEventMessage (the consumer for nexus.event.agent).
func (h *InternalThingsAPI) ExemptionUpload(c echo.Context) error {
	var req struct {
		ThingID   string    `json:"thingId"`
		Host      string    `json:"host"`
		Reason    string    `json:"reason"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.ThingID == "" {
		return badRequest(c, "thingId is required")
	}
	if req.Host == "" {
		return badRequest(c, "host is required")
	}
	if req.ExpiresAt.IsZero() {
		return badRequest(c, "expiresAt is required")
	}

	// Exemption upload is an agent-only operation (only agents emit
	// TLS-bump auto-exemptions). A service-token caller therefore always targets
	// an agent Thing and is refused; the agent itself uploads with its device
	// token and is bound to its own id.
	if h.requireMutationAuthority(c, req.ThingID, "") {
		return nil
	}

	if h.MQProducer == nil {
		return serviceUnavailable(c, "event queue temporarily unavailable, retry later")
	}

	payload := map[string]any{
		"kind":      "exemption",
		"thingId":   req.ThingID,
		"host":      req.Host,
		"reason":    req.Reason,
		"expiresAt": req.ExpiresAt.Format(time.RFC3339),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return internalError(c, "encode exemption event")
	}
	if err := h.MQProducer.Enqueue(c.Request().Context(), "nexus.event.exemption", data); err != nil {
		return serviceUnavailable(c, "event queue temporarily unavailable, retry later")
	}

	return c.JSON(http.StatusOK, map[string]bool{"ack": true})
}

// updateTargetConfigKey is the thing_config_template.config_key that holds
// the pinned agent update target (version + download URL + signature).
// Operators upsert this per-agent-type via the Control Plane admin API.
const updateTargetConfigKey = "agentUpdateTarget"

// agentUpdateTarget mirrors the JSON state stored at thing_config_template
// where type='agent' and config_key='agentUpdateTarget'.
type agentUpdateTarget struct {
	Version      string `json:"version"`
	DownloadURL  string `json:"downloadUrl"`
	Signature    string `json:"signature"`
	SHA256       string `json:"sha256"`
	ReleaseNotes string `json:"releaseNotes"`
	ForceUpdate  bool   `json:"forceUpdate"`
}

// UpdateCheck handles GET /api/internal/things/update-check.
// The agent passes its current version and OS; the Hub returns the pinned
// update target (if configured) when the version differs.
func (h *InternalThingsAPI) UpdateCheck(c echo.Context) error {
	currentVersion := c.QueryParam("currentVersion")
	if currentVersion == "" {
		return badRequest(c, "currentVersion query parameter is required")
	}

	tpl, err := h.Mgr.Store().ConfigStore().GetConfigTemplate(c.Request().Context(), "agent", updateTargetConfigKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return c.JSON(http.StatusOK, map[string]any{"available": false})
		}
		return internalError(c, "read update target")
	}

	stateBytes, err := json.Marshal(tpl.State)
	if err != nil {
		return internalError(c, "encode update target")
	}

	var target agentUpdateTarget
	if err := json.Unmarshal(stateBytes, &target); err != nil {
		return internalError(c, "decode update target")
	}

	if target.Version == "" || target.Version == currentVersion {
		return c.JSON(http.StatusOK, map[string]any{"available": false})
	}

	return c.JSON(http.StatusOK, map[string]any{
		"available":    true,
		"version":      target.Version,
		"downloadUrl":  target.DownloadURL,
		"signature":    target.Signature,
		"sha256":       target.SHA256,
		"releaseNotes": target.ReleaseNotes,
		"forceUpdate":  target.ForceUpdate,
	})
}
