package hubapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/agentca"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// InternalThingsAPI implements /api/internal/things/* endpoints.
type InternalThingsAPI struct {
	Mgr        *manager.Manager
	MQProducer mq.Producer
	CA         *agentca.CA
	// CatB aggregates authoritative Cat B payloads from CP-owned
	// business tables. Nil is supported: SingleConfigPull falls
	// through to the legacy thing_config_template.state path, so the
	// absence of a registry is byte-identical to pre-P0-C behaviour.
	CatB *store.CatBRegistry
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
	pub, err := h.Mgr.Store().RegistryStore().GetAttestationPubKey(c.Request().Context(), thingID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return c.JSON(http.StatusNotFound, map[string]any{
				"error": "no attestation key on record for agent",
				"code":  "ATTESTATION_NOT_ENROLLED",
			})
		}
		return handleErr(c, err)
	}
	return c.JSON(http.StatusOK, map[string]any{
		"agentId":   thingID,
		"publicKey": base64.StdEncoding.EncodeToString(pub),
	})
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
	if req.Reason != "" && req.Reason != "break_glass" {
		return badRequest(c, "invalid reason; must be empty or 'break_glass'")
	}
	if req.Reason == "break_glass" && req.ActorTokenID == "" {
		return badRequest(c, "break_glass report requires actorTokenId")
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

	templates, err := h.Mgr.Store().ConfigStore().GetConfigTemplates(c.Request().Context(), thingType)
	if err != nil {
		return handleErr(c, err)
	}

	thingID := strings.TrimSpace(c.QueryParam("id"))
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

	// DeviceOrServiceAuth attaches a *store.Thing for device-token callers
	// (the canonical agent path). Service-token callers (Hub-internal) have
	// no Thing attached and are trusted to self-report thingId in the body.
	//
	// For device-token callers we use the resolved Thing as the source of
	// truth: the body's thingId must match (otherwise the agent is lying
	// about its identity) and thing.Name is taken Hub-side — no DB lookup
	// per batch, and no chance for the agent to forge thing_name.
	thing := ThingFromContext(c)
	var thingName string
	if thing != nil {
		if thing.ID != req.ThingID {
			return forbidden(c, "thingId does not match authenticated device")
		}
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

	// When the caller authenticated with a device token, their Thing is
	// attached to the context. Refuse requests where the body thingId does
	// not match the authenticated device. Service-token callers have no
	// Thing attached (ThingFromContext returns nil) and are trusted.
	if thing := ThingFromContext(c); thing != nil && thing.ID != req.ThingID {
		return forbidden(c, "thingId does not match authenticated device")
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

// RenewCert handles POST /api/internal/things/renew-cert.
// The agent sends a fresh CSR signed with a newly generated keypair; the
// Hub's agent CA signs it and returns the new certificate + CA chain.
func (h *InternalThingsAPI) RenewCert(c echo.Context) error {
	var req struct {
		ThingID string `json:"thingId"`
		CsrPEM  string `json:"csr"`
	}
	if err := c.Bind(&req); err != nil {
		return badRequest(c, "invalid request body")
	}
	if req.ThingID == "" {
		return badRequest(c, "thingId is required")
	}
	if req.CsrPEM == "" {
		return badRequest(c, "csr is required")
	}

	// Device-token callers may only renew their own cert. Service-token
	// callers (ThingFromContext == nil) bypass this check.
	if thing := ThingFromContext(c); thing != nil && thing.ID != req.ThingID {
		return forbidden(c, "thingId does not match authenticated device")
	}

	if h.CA == nil {
		return serviceUnavailable(c, "agent CA not configured, retry later")
	}

	// Cert CN keeps the `device-` prefix — it's a PKI subject identifier
	// snapshot, not a wire field; renaming it would change the issued cert
	// CN namespace and invalidate the existing CA-signed identities.
	result, err := h.CA.SignCSR(req.CsrPEM, fmt.Sprintf("device-%s", req.ThingID))
	if err != nil {
		return badRequest(c, fmt.Sprintf("CSR signing failed: %v", err))
	}

	return c.JSON(http.StatusOK, map[string]any{
		"certificate": result.CertPEM,
		"gatewayCA":   result.CaCertPEM,
		"expiresAt":   result.ExpiresAt.Format(time.RFC3339),
		"serial":      result.Serial,
	})
}
