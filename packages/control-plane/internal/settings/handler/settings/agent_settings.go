package settings

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// GetAgentSettings returns the fleet-wide agent runtime defaults pushed to
// every agent via the agent_settings shadow config key.
//
// Currently surfaces:
//   - quitAllowed: whether the user may quit the agent process (gates the
//     Restart Agent / Quit menu items in the Swift menu bar).
//   - shutdownWarning: optional per-locale warning text shown when the user
//     attempts to quit (when quitAllowed is true).
//
// Fields logLevel, heartbeatIntervalSec, auditDrainIntervalSec,
// configSyncIntervalSec, and auditBatchSize are stripped on PUT — the
// agent runtime ignores them. PUT strips rather than 400s so existing
// admin UIs don't regress; the stripped blob never reaches the wire.
func (h *Handler) GetAgentSettings(c echo.Context) error {
	ctx := c.Request().Context()

	// All fields are surfaced even when absent from the stored
	// blob — zero-value Go defaults map cleanly to the JSON nulls /
	// zeros that the UI's form bindings expect. Setters below treat
	// "field omitted from PATCH body" as "keep existing value" so a
	// partial UI form save doesn't zero out the rest.
	settings := map[string]any{}
	raw, err := h.meta.GetSystemMetadata(ctx, "agent.settings")
	if err != nil {
		h.logger.Error("load agent settings", "error", err)
		return internalServerError(c, "Internal server error")
	}
	if raw != nil {
		_ = json.Unmarshal(raw, &settings)
	}

	resp := map[string]any{
		// Quit policy
		"quitAllowed":            mapBool(settings, "quitAllowed", false),
		"shutdownWarningEnabled": mapBool(settings, "shutdownWarningEnabled", false),
		// Updater
		"autoUpdateEnabled": mapBool(settings, "autoUpdateEnabled", false),
		"autoUpdateChannel": mapString(settings, "autoUpdateChannel"),
		// trafficUploadLevel — closed enum {all,processed,blocked} that
		// gates which agent flows reach Hub. Empty in the response means
		// the admin never set it; agent falls back to "processed" (the
		// recommended default, applied in config.applyDefaults).
		"trafficUploadLevel": mapString(settings, "trafficUploadLevel"),
		// themeId — theme pack ID the agent Dashboard should render with.
		// Open enum (any non-empty string admins type); the Dashboard
		// resolves it against /themes/*.json at load and falls back to
		// the bundled `default` theme on miss. Empty = "let each agent
		// use its local pick", which is the natural starting state.
		"themeId": mapString(settings, "themeId"),
		// forceQUICFallbackBundles — bundle-ID allowlist for the macOS
		// NE proxy's "kill UDP to force HTTP/2 fallback" behaviour. Only
		// browsers + Electron AI desktop apps belong here. System
		// processes (mdnsresponder, dhcp, ntp) MUST NOT be added —
		// closing their UDP takes the host network down (fail-open safety).
		// Empty/absent = no UDP gets killed (safe-default).
		"forceQUICFallbackBundles": mapStringSlice(settings, "forceQUICFallbackBundles"),
		// attestationEnabled — fleet-wide opt-in for agent attestation.
		// When true, agents sign every outbound CONNECT with their Ed25519
		// attestation key; when CP verifies the signature it transparently
		// tunnels the request (skipping its own MITM + hook pipeline) and
		// records attestation_verified=true in traffic_event. Default false:
		// attestation is a perf optimization, not a security gate — flipping
		// it false anywhere in the chain (here, per-agent, or CP-side flag)
		// keeps every request flowing through full MITM as before.
		"attestationEnabled": mapBool(settings, "attestationEnabled", false),
	}
	if sw, ok := settings["shutdownWarning"].(map[string]any); ok {
		shutdownWarning := make(map[string]string, len(sw))
		for k, v := range sw {
			if s, ok := v.(string); ok {
				shutdownWarning[k] = s
			}
		}
		resp["shutdownWarning"] = shutdownWarning
	}
	return c.JSON(http.StatusOK, resp)
}

// mapBool / mapString return the typed value at key from the
// JSON-decoded settings map, falling back to the supplied default
// when the key is missing OR the type differs. Centralises the noisy
// type-switch logic that the GetAgentSettings response builder used
// to repeat per-field.
func mapBool(m map[string]any, key string, fallback bool) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return fallback
}

func mapString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// mapStringSlice extracts a JSON []string from the settings map.
// Returns empty (not nil) so JSON serialisation produces `[]` instead
// of `null`, which the UI's chip-list editor can iterate without a
// nil-check. Silently drops any non-string elements rather than
// surfacing an error — admin-controlled config should never crash
// the GET response just because a stray non-string slipped in.
func mapStringSlice(m map[string]any, key string) []string {
	raw, ok := m[key].([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// UpdateAgentSettings updates the fleet-wide agent runtime defaults.
// Accepts quitAllowed + shutdownWarning (multi-locale map). Fields omitted
// from the request body are left unchanged. The next configreconcile tick
// (~60 s) propagates the merged blob to every online agent thing.
func (h *Handler) UpdateAgentSettings(c echo.Context) error {
	var body struct {
		QuitAllowed            *bool             `json:"quitAllowed"`
		ShutdownWarning        map[string]string `json:"shutdownWarning"`
		ShutdownWarningEnabled *bool             `json:"shutdownWarningEnabled"`
		AutoUpdateEnabled      *bool             `json:"autoUpdateEnabled"`
		AutoUpdateChannel      *string           `json:"autoUpdateChannel"`
		TrafficUploadLevel     *string           `json:"trafficUploadLevel"`
		ThemeID                *string           `json:"themeId"`
		// ForceQUICFallbackBundles is a *pointer-to-slice* so we can
		// distinguish "field absent" (don't touch existing value) from
		// "field present, empty" (admin explicitly cleared the list).
		// Both states matter — clearing must propagate to NE so admin
		// can disable QUIC blocking entirely without a code change.
		ForceQUICFallbackBundles *[]string `json:"forceQUICFallbackBundles"`
		AttestationEnabled       *bool     `json:"attestationEnabled"`
	}
	if err := c.Bind(&body); err != nil {
		return c.JSON(http.StatusBadRequest, errJSON("Invalid request body", "validation_error", ""))
	}

	ctx := c.Request().Context()

	// Load existing settings.
	current := map[string]any{}
	raw, err := h.meta.GetSystemMetadata(ctx, "agent.settings")
	if err == nil && raw != nil {
		var stored map[string]any
		if json.Unmarshal(raw, &stored) == nil {
			for k, v := range stored {
				current[k] = v
			}
		}
	}

	// Strip dead fields from any legacy persisted blob. The agent runtime
	// ignores these; carrying them through PUT would silently re-introduce
	// them into newly-saved state and Hub-push payloads.
	for _, dead := range []string{
		"logLevel",
		"heartbeatIntervalSec",
		"auditDrainIntervalSec",
		"configSyncIntervalSec",
		"auditBatchSize",
	} {
		delete(current, dead)
	}

	// Apply updates.
	before := map[string]any{}
	for k, v := range current {
		before[k] = v
	}
	if body.QuitAllowed != nil {
		current["quitAllowed"] = *body.QuitAllowed
	}
	if body.ShutdownWarning != nil {
		current["shutdownWarning"] = body.ShutdownWarning
	}
	if body.ShutdownWarningEnabled != nil {
		current["shutdownWarningEnabled"] = *body.ShutdownWarningEnabled
	}
	if body.AutoUpdateEnabled != nil {
		current["autoUpdateEnabled"] = *body.AutoUpdateEnabled
	}
	if body.AutoUpdateChannel != nil {
		// Pin to known channel names so a typo doesn't silently
		// route updates to a non-existent track.
		switch *body.AutoUpdateChannel {
		case "stable", "beta", "":
			current["autoUpdateChannel"] = *body.AutoUpdateChannel
		default:
			return c.JSON(http.StatusBadRequest, errJSON("autoUpdateChannel must be stable or beta", "validation_error", ""))
		}
	}
	if body.TrafficUploadLevel != nil {
		// Closed enum — empty means "use agent default" (which is
		// "processed"). Anything else gets rejected so an admin typo
		// doesn't silently propagate through Hub configreconcile and
		// then quietly fall back inside the agent. Better to surface
		// the bad value at write time than debug an agent dropping
		// rows weeks later.
		switch *body.TrafficUploadLevel {
		case "", "all", "processed", "blocked":
			current["trafficUploadLevel"] = *body.TrafficUploadLevel
		default:
			return c.JSON(http.StatusBadRequest, errJSON(
				"trafficUploadLevel must be all|processed|blocked (or empty for default)",
				"validation_error", ""))
		}
	}
	if body.ThemeID != nil {
		// Open enum — themes ship as JSON files under each app's
		// public/themes/, and we don't want a handler restart whenever
		// a new theme pack lands. Validate only as "non-pathological
		// short printable ASCII" so a typo here can't be a SQL/HTML
		// vector via the audit log; the agent Dashboard handles
		// unknown IDs by falling back to the bundled `default` theme,
		// so a typo at write-time just means "no rebrand applied".
		t := strings.TrimSpace(*body.ThemeID)
		if len(t) > 64 {
			return c.JSON(http.StatusBadRequest, errJSON(
				"themeId must be ≤ 64 characters", "validation_error", ""))
		}
		for _, r := range t {
			if r < 0x20 || r > 0x7e {
				return c.JSON(http.StatusBadRequest, errJSON(
					"themeId must be printable ASCII", "validation_error", ""))
			}
		}
		current["themeId"] = t
	}
	if body.ForceQUICFallbackBundles != nil {
		// Sanitize: drop empties + dedupe + cap at 64 entries. Each
		// entry must look like a bundle ID (printable ASCII without
		// whitespace, max 200 chars). Hard reject if any entry fails
		// validation rather than silently drop — surfacing the bad
		// value lets the admin see what they typed wrong instead of
		// debugging "I added it and it disappeared". The 64-cap and
		// 200-char-cap defend against an over-stuffed admin payload
		// blowing up the agent_settings JSON blob (Hub serves this
		// inline; size matters).
		seen := make(map[string]struct{})
		clean := make([]string, 0, len(*body.ForceQUICFallbackBundles))
		for _, b := range *body.ForceQUICFallbackBundles {
			b = strings.TrimSpace(b)
			if b == "" {
				continue
			}
			if len(b) > 200 {
				return c.JSON(http.StatusBadRequest, errJSON(
					"forceQUICFallbackBundles entry exceeds 200 chars: "+b[:32]+"…",
					"validation_error", ""))
			}
			for _, r := range b {
				if r < 0x20 || r > 0x7E || r == ' ' {
					return c.JSON(http.StatusBadRequest, errJSON(
						"forceQUICFallbackBundles entries must be printable ASCII without whitespace",
						"validation_error", ""))
				}
			}
			if _, dup := seen[b]; dup {
				continue
			}
			seen[b] = struct{}{}
			clean = append(clean, b)
		}
		if len(clean) > 64 {
			return c.JSON(http.StatusBadRequest, errJSON(
				"forceQUICFallbackBundles supports at most 64 entries",
				"validation_error", ""))
		}
		current["forceQUICFallbackBundles"] = clean
	}
	if body.AttestationEnabled != nil {
		current["attestationEnabled"] = *body.AttestationEnabled
	}

	aa := middleware.AdminAuthFromContext(c)
	updatedBy := ""
	if aa != nil {
		updatedBy = aa.KeyID
	}
	if err := h.meta.SetSystemMetadata(ctx, "agent.settings", current, updatedBy); err != nil {
		h.logger.Error("save agent settings", "error", err)
		return internalServerError(c, "Failed to save settings")
	}

	// Increment config version so agents pick up the change.
	h.incrementConfigVersion(ctx)

	ae := audit.EntryFor(c, iam.ResourceSettings, iam.VerbUpdate)
	ae.BeforeState = before
	ae.AfterState = current
	h.audit.LogObserved(ctx, ae)

	return c.JSON(http.StatusOK, current)
}
