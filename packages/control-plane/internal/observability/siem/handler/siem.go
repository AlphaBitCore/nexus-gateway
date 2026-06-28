package siem

import (
	"bytes"
	"github.com/goccy/go-json"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// RegisterSIEMRoutes registers SIEM settings routes.
func (h *Handler) RegisterSIEMRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/settings/siem", h.GetSIEMConfig, iamMW(iam.ResourceAuditLog.Action(iam.VerbRead)))
	// Reconfiguring the SIEM egress redirects the ENTIRE org's audit
	// stream to an operator-supplied URL and (pre-guard) was an SSRF primitive —
	// that is a system-integration setting, not an audit-log record operation.
	// The mutating verbs are gated on the higher-blast-radius settings:write tier
	// (the verb also used by health:reset), so a narrow audit-log:write grant can
	// no longer point the audit firehose at an attacker endpoint. Read stays on
	// audit-log:read: viewing the redacted egress config is an audit concern.
	g.PUT("/settings/siem", h.UpdateSIEMConfig, iamMW(iam.ResourceSettings.Action(iam.VerbWrite)))
	g.POST("/settings/siem/test", h.TestSIEMConfig, iamMW(iam.ResourceSettings.Action(iam.VerbWrite)))
	g.GET("/settings/siem/event-types", h.ListSIEMEventTypes, iamMW(iam.ResourceAuditLog.Action(iam.VerbRead)))
}

const siemConfigKey = "siem.config"

// redactedSecretSentinel is the placeholder GetSIEMConfig returns in place of a
// stored secret header value (Authorization / x-api-key). It is a fixed marker —
// NOT a partial reveal — so a read-back leaks nothing: the client learns only
// that a value is set, never any byte of it. On UpdateSIEMConfig, a
// header whose value still equals this sentinel means "unchanged" and the
// previously stored real value is preserved (see preserveSecretHeaders); the SIEM
// forwarder therefore keeps working across a UI GET→edit→PUT round-trip without
// the admin ever re-typing the secret.
const redactedSecretSentinel = "********"

// isSecretHeader reports whether a SIEM request header carries a credential
// whose value must never be echoed back to a client in plaintext. Matching is
// case-insensitive because callers may store "Authorization" or "authorization".
func isSecretHeader(name string) bool {
	return strings.EqualFold(name, "authorization") || strings.EqualFold(name, "x-api-key")
}

// SIEMConfig holds the SIEM integration configuration.
type SIEMConfig struct {
	Enabled    bool              `json:"enabled"`
	URL        string            `json:"url"`
	Format     string            `json:"format"` // json | cef | syslog
	Headers    map[string]string `json:"headers"`
	EventTypes []string          `json:"eventTypes"`
}

// redactSecretHeaders returns a copy of headers with every secret header value
// replaced by the fixed redactedSecretSentinel (non-secret headers pass through
// unchanged). An empty stored value is returned as empty (not the sentinel) so
// the UI can distinguish "no value set" from "value set but hidden". Used by
// GetSIEMConfig so a read-back never carries a real token byte.
func redactSecretHeaders(headers map[string]string) map[string]string {
	if headers == nil {
		return nil
	}
	out := make(map[string]string, len(headers))
	for k, v := range headers {
		if isSecretHeader(k) && v != "" {
			out[k] = redactedSecretSentinel
		} else {
			out[k] = v
		}
	}
	return out
}

// preserveSecretHeaders reconciles an inbound (client) header map against the
// stored map: any secret header whose inbound value is the redaction sentinel
// (the client is replaying what GetSIEMConfig returned, not setting a new
// secret) is replaced with the previously stored real value. This stops a UI
// round-trip from overwriting a live token with the placeholder while still
// letting an admin set a genuinely new secret (any other value is taken as-is)
// or clear it (empty string is taken as-is). Mutates and returns inbound.
func preserveSecretHeaders(inbound, stored map[string]string) map[string]string {
	if inbound == nil {
		return inbound
	}
	for k, v := range inbound {
		if isSecretHeader(k) && v == redactedSecretSentinel {
			if prev, ok := stored[k]; ok {
				inbound[k] = prev
			} else {
				// No prior value to preserve — drop the placeholder rather than
				// persist the literal sentinel as if it were a real credential.
				delete(inbound, k)
			}
		}
	}
	return inbound
}

func (h *Handler) GetSIEMConfig(c echo.Context) error {
	raw, err := h.db.GetSystemMetadata(c.Request().Context(), siemConfigKey)
	if err != nil {
		h.logger.Error("get siem config", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	if raw == nil {
		return c.JSON(http.StatusOK, &SIEMConfig{Format: "json"})
	}

	var cfg SIEMConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return c.JSON(http.StatusOK, &SIEMConfig{Format: "json"})
	}

	// Redact secret header values to a fixed sentinel so the read-back leaks
	// no byte of the stored token. The previous behaviour revealed the
	// first and last 4 characters, which narrows a brute-force / shoulder-surf
	// and is itself a partial disclosure of a credential.
	cfg.Headers = redactSecretHeaders(cfg.Headers)

	return c.JSON(http.StatusOK, cfg)
}

func (h *Handler) UpdateSIEMConfig(c echo.Context) error {
	var cfg SIEMConfig
	if err := c.Bind(&cfg); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	validFormats := map[string]bool{"json": true, "cef": true, "syslog": true}
	if cfg.Format != "" && !validFormats[cfg.Format] {
		return c.JSON(http.StatusBadRequest, errJSON("format must be json, cef, or syslog", "validation_error", ""))
	}
	if cfg.URL != "" && !strings.HasPrefix(cfg.URL, "https://") && !strings.HasPrefix(cfg.URL, "http://") {
		return c.JSON(http.StatusBadRequest, errJSON("url must be a valid HTTP(S) URL", "validation_error", ""))
	}

	// Preserve any secret header the client replayed as the redaction sentinel:
	// GetSIEMConfig returns secret values masked, so a UI that GETs,
	// edits a non-secret field, and PUTs back would otherwise overwrite the live
	// token with the placeholder. Load the prior stored config and substitute the
	// real value wherever the client sent the sentinel; a genuinely new value
	// (anything else) or an empty string (clear) is taken as submitted.
	if cfg.Headers != nil {
		var prev SIEMConfig
		if priorRaw, perr := h.db.GetSystemMetadata(c.Request().Context(), siemConfigKey); perr == nil && priorRaw != nil {
			_ = json.Unmarshal(priorRaw, &prev)
		}
		cfg.Headers = preserveSecretHeaders(cfg.Headers, prev.Headers)
	}

	aa := middleware.AdminAuthFromContext(c)
	updatedBy := "unknown"
	if aa != nil {
		updatedBy = aa.KeyID
	}

	if err := h.db.SetSystemMetadata(c.Request().Context(), siemConfigKey, cfg, updatedBy); err != nil {
		h.logger.Error("update siem config", "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Internal server error", "server_error", ""))
	}

	// The Hub-side SIEM Bridge re-reads system_metadata['siem.config'] on
	// every Poll cycle, so this DB write propagates within one poll
	// interval (default 30s) without any shadow / NOTIFY plumbing.

	ae := audit.EntryFor(c, iam.ResourceAuditLog, iam.VerbWrite)
	ae.AfterState = map[string]any{"enabled": cfg.Enabled, "format": cfg.Format}
	h.audit.LogObserved(c.Request().Context(), ae)

	return c.JSON(http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) TestSIEMConfig(c echo.Context) error {
	ctx := c.Request().Context()
	raw, err := h.db.GetSystemMetadata(ctx, siemConfigKey)
	if err != nil || raw == nil {
		return c.JSON(http.StatusBadRequest, errJSON("SIEM is not configured", "validation_error", ""))
	}

	var cfg SIEMConfig
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg.URL == "" {
		return c.JSON(http.StatusBadRequest, errJSON("SIEM URL is not configured", "validation_error", ""))
	}

	testEvent := map[string]any{
		"type":      "siem_test",
		"severity":  "info",
		"timestamp": time.Now().Format(time.RFC3339),
		"message":   "SIEM integration test event from Nexus Gateway",
		"source":    "nexus-control-plane",
	}

	body, _ := json.Marshal(testEvent)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return c.JSON(http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}

	client := nexushttp.New(nexushttp.Config{
		Timeout:        10 * time.Second,
		Caller:         "cp-admin-siem",
		PropagateReqID: true,
		// The test probe dials an operator-supplied URL, so it is an SSRF
		// primitive. Scope the dial guard to THIS client only (not
		// process-wide — CP legitimately dials its own private deps); the
		// guard runs on the resolved address, defeating DNS-rebinding too. A
		// SIEM endpoint is external by nature — a private/loopback target is
		// never legitimate — so it routes through the admin-egress chokepoint
		// as ExternalOnly.
		DialControl: nexushttp.AdminEgressDialControl(nexushttp.AdminEgressExternalOnly),
	})
	resp, derr := client.Do(req)
	if resp != nil {
		defer resp.Body.Close() //nolint:errcheck
	}
	return c.JSON(http.StatusOK, siemProbeResult(resp, derr))
}

// siemProbeResult maps the outcome of the SIEM connectivity POST to the generic
// reachable/unreachable envelope returned to the admin UI.
//
// It deliberately reveals only a boolean plus a fixed message. The
// raw transport error (connection-refused vs. SSRF-guard reject vs. TLS error
// vs. timeout) and the upstream status code (401 vs 403 vs 500) are a blind-SSRF
// / internal-endpoint fingerprinting oracle, so a dial failure and an error
// status each collapse to one constant string with no statusCode field. Any two
// distinct failure causes MUST produce byte-identical bodies.
func siemProbeResult(resp *http.Response, dialErr error) map[string]any {
	if dialErr != nil {
		return map[string]any{"ok": false, "error": "Failed to reach the SIEM endpoint"}
	}
	if resp.StatusCode >= 400 {
		return map[string]any{"ok": false, "error": "The SIEM endpoint returned an error response"}
	}
	return map[string]any{"ok": true}
}

// ListSIEMEventTypes returns the full set of SIEM event types the
// Control Plane could emit, for the admin filter picker. Each row
// carries Service + Resource + Type so the UI can render a three-level
// drill-down (service → resource → event-type), matching the CatalogPicker
// hierarchy in the IAM policy editor.
//
// Admin (resource.verb) rows derive from the canonical iam.Catalog —
// the same source the IAM engine and audit pipeline use. Traffic.*
// rows are hand-listed because those names come from the SIEM bridge's
// compliance classifier (packages/nexus-hub/internal/observability/siem/classify.go),
// not from the IAM catalog; they are tagged Service "compliance" and
// grouped under a synthetic "traffic" resource so the filter UI shows
// them under Compliance > traffic.
//
// GET /api/admin/settings/siem/event-types
func (h *Handler) ListSIEMEventTypes(c echo.Context) error {
	type eventTypeInfo struct {
		Type     string `json:"type"`
		Resource string `json:"resource"`
		Service  string `json:"service"`
	}

	// Traffic event types — fixed, mirror packages/nexus-hub/internal/observability/siem/classify.go.
	// Service "compliance" because all three are compliance-classifier outputs
	// (rate limit, budget exceeded, request blocked by compliance rule).
	types := []eventTypeInfo{
		{Type: "traffic.rate_limited", Resource: "traffic", Service: string(iam.ServiceCompliance)},
		{Type: "traffic.budget_exceeded", Resource: "traffic", Service: string(iam.ServiceCompliance)},
		{Type: "traffic.request_blocked", Resource: "traffic", Service: string(iam.ServiceCompliance)},
		// Authentication event types — hand-listed (no IAM catalog resource).
		// ClassifyAdminEvent maps the dotted login actions
		// (admin.login.failed/.succeeded) to these canonical auth.* identities so
		// they survive a SIEM whitelist; without a picker entry an admin could
		// never select them and enabling any whitelist would silently stop
		// forwarding login events. Grouped under a synthetic "auth"
		// resource in the IAM service.
		{Type: "auth.login_failure", Resource: "auth", Service: string(iam.ServiceIAM)},
		{Type: "auth.login_success", Resource: "auth", Service: string(iam.ServiceIAM)},
	}

	// Admin event types — one entry per (resource, verb) pair in the
	// canonical catalog. The picker drills service → resource → verb.
	for i := range iam.Catalog {
		r := &iam.Catalog[i]
		for _, v := range r.Verbs {
			types = append(types, eventTypeInfo{
				Type:     iam.SIEMEventType(r.Name, v),
				Resource: r.Name,
				Service:  string(r.Service),
			})
		}
	}

	return c.JSON(http.StatusOK, map[string]any{"eventTypes": types})
}
