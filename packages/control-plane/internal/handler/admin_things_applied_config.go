package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/thingstats/thingstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// appliedConfigStore is the narrow read seam the /things/:id/applied-config
// handler uses. *store.DB satisfies it in production; tests swap in an
// in-memory stub so the merge logic (desired + reported + history) can be
// exercised without standing up Postgres.
type appliedConfigStore interface {
	GetThing(ctx context.Context, id string) (*thingstore.ThingRegistry, error)
	ListTemplatesByType(ctx context.Context, thingType string) ([]thingstore.ThingConfigTemplate, error)
	GetLatestConfigChangeEvent(ctx context.Context, thingType, configKey string) (*thingstore.ConfigChangeEvent, error)
}

// appliedConfigStoreFromHandler returns the test override when supplied,
// otherwise falls back to the concrete *store.DB. Mirrors the sibling
// compliance handlers' narrow-interface pattern.
func (h *AdminHandler) appliedConfigStoreFromHandler() appliedConfigStore {
	if h.AppliedConfigStore != nil {
		return h.AppliedConfigStore
	}
	return h.DB
}

// appliedConfigOverrideMeta is the projected per-key override row consumed by
// the applied-config handler. Field shape matches the OpenAPI ThingOverride
// schema verbatim (camelCase via JSON tags) so it embeds straight into the
// response without an extra rename pass. Returned by appliedConfigOverrideFetcher.
type appliedConfigOverrideMeta struct {
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

// appliedConfigOverrideFetcher loads the active override list for a Thing.
// Production wiring calls Hub via HTTP (the Hub already JOINs
// thing_config_template so currentTemplateVer + stale are pre-computed);
// tests inject a stub returning canned rows. Returning an empty slice
// is the documented "no overrides" sentinel and must not be conflated
// with the error path.
type appliedConfigOverrideFetcher interface {
	FetchOverridesForThing(ctx context.Context, thingID string) ([]appliedConfigOverrideMeta, error)
}

// appliedConfigOverrideFetcherFromHandler returns the test override when
// supplied, otherwise falls back to a Hub-HTTP-backed fetcher. When Hub is
// not configured (h.Hub == nil || BaseURL() == "") this returns nil; the
// handler then degrades gracefully — entries still carry templateState +
// templateVer, but no override metadata.
func (h *AdminHandler) appliedConfigOverrideFetcherFromHandler() appliedConfigOverrideFetcher {
	if h.AppliedConfigOverrideFetcher != nil {
		return h.AppliedConfigOverrideFetcher
	}
	if h.Hub == nil || h.Hub.BaseURL() == "" {
		return nil
	}
	return &hubAppliedConfigOverrideFetcher{hub: h.Hub, client: handlerHubProxyClient(h.HubProxyClient)}
}

// hubAppliedConfigOverrideFetcher is the production implementation of
// appliedConfigOverrideFetcher. It calls GET /api/hub/things/:id/overrides
// and projects the response into appliedConfigOverrideMeta. The Hub
// response shape is the OpenAPI ListNodeOverridesResponse —
// {"overrides":[ThingOverride...]} — which already carries
// currentTemplateVer + stale via Hub's JOIN, so this layer is a thin
// projection.
type hubAppliedConfigOverrideFetcher struct {
	hub    HubNotifier
	client *http.Client
}

// FetchOverridesForThing implements appliedConfigOverrideFetcher.
func (f *hubAppliedConfigOverrideFetcher) FetchOverridesForThing(ctx context.Context, thingID string) ([]appliedConfigOverrideMeta, error) {
	if f.hub == nil || f.hub.BaseURL() == "" {
		return nil, nil
	}
	hubURL := f.hub.BaseURL() + "/api/hub/things/" + url.PathEscape(thingID) + "/overrides"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hubURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build hub overrides request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.hub.Token())

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hub unreachable: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode == http.StatusNotFound {
		// Thing not found at Hub — propagate as an empty list, not an
		// error. The applied-config handler already validated the
		// Thing exists in CP-local DB; if Hub disagrees it likely
		// means an enrollment race we should surface as "no overrides".
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("hub returned %d", resp.StatusCode)
	}

	var body struct {
		Overrides []appliedConfigOverrideMeta `json:"overrides"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode hub response: %w", err)
	}
	return body.Overrides, nil
}

// appliedConfigLastChange is the audit stanza embedded in each configs[key]
// entry. It's a projection of store.ConfigChangeEvent down to the fields the
// Admin UI renders on the Node detail → Applied Config tab.
type appliedConfigLastChange struct {
	Timestamp         time.Time `json:"timestamp"`
	Actor             string    `json:"actor"`
	Action            string    `json:"action"`
	EmergencyOverride bool      `json:"emergencyOverride"`
}

// appliedConfigEntry is one config_key's merged view. AppliedConfig is a
// json.RawMessage (not interface{}) so the existing raw bytes can pass
// through without a round-trip through map[string]any, which re-orders keys
// and would break the byte-equal inSync check.
//
// TargetVersion and AppliedVersion duplicate the Thing row's global shadow
// counters (thing.desired_ver / thing.reported_ver) on every key. They use
// the product-facing names per the IoT terminology boundary in
// docs/developers/architecture/cross-cutting/foundation/thing-model.md §10.
//
// TemplateState + TemplateVer expose the read-only template default for the
// Configuration tab's editor drawer left pane. Override is populated when an
// active per-Node override exists for this configKey.
type appliedConfigEntry struct {
	TargetConfig   json.RawMessage            `json:"targetConfig"`
	TargetVersion  int64                      `json:"targetVersion"`
	AppliedConfig  json.RawMessage            `json:"appliedConfig"`
	AppliedVersion int64                      `json:"appliedVersion"`
	TemplateState  json.RawMessage            `json:"templateState"`
	TemplateVer    int64                      `json:"templateVer"`
	InSync         bool                       `json:"inSync"`
	LastChange     *appliedConfigLastChange   `json:"lastChange,omitempty"`
	Override       *appliedConfigOverrideMeta `json:"override,omitempty"`
}

func isJSONNullish(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// RegisterAdminNodesAppliedConfigRoutes wires the Admin UI Applied Config
// tab read endpoint. Requires admin:ReadSettings (shared with other Admin UI
// read endpoints; see NexusViewer / NexusSuperAdmin policies).
func (h *AdminHandler) RegisterAdminNodesAppliedConfigRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	g.GET("/nodes/:id/applied-config", h.GetNodeAppliedConfig, iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
}

// GetNodeAppliedConfig returns a merged view of target / applied config / last
// change for every config_key owned by the target Node's type. The target
// configuration and its version come from the Node's shadow, so the version
// reflects what the Node is actually targeted at; template state is only a
// fallback for keys not present in the target yet.
//
// Each entry also carries the read-only templateState / templateVer (used by
// the editor drawer's left pane) and, when an active override exists for this
// Node+configKey, the full override object — so the Configuration tab can render
// the 4-column merged view from a single round-trip.
//
// Responses pass stored JSON bytes through unmodified, so the inSync byte-compare
// stays meaningful for all shadow keys.
func (h *AdminHandler) GetNodeAppliedConfig(c echo.Context) error {
	id := c.Param("id")
	if id == "" {
		return c.JSON(http.StatusBadRequest, errJSON("id is required", "validation_error", "VALIDATION_ERROR"))
	}

	ctx := c.Request().Context()
	st := h.appliedConfigStoreFromHandler()

	thing, err := st.GetThing(ctx, id)
	if err != nil {
		h.Logger.Error("applied-config get node", "nodeId", id, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to read node", "server_error", "INTERNAL_ERROR"))
	}
	if thing == nil {
		return c.JSON(http.StatusNotFound, errJSON("Node not found", "not_found", "NOT_FOUND"))
	}

	templates, err := st.ListTemplatesByType(ctx, thing.Type)
	if err != nil {
		h.Logger.Error("applied-config list templates", "nodeType", thing.Type, "error", err)
		return c.JSON(http.StatusInternalServerError, errJSON("Failed to read config templates", "server_error", "INTERNAL_ERROR"))
	}

	// Override lookup is best-effort: a Hub blip should not 500 the whole
	// Configuration tab. On failure we log + proceed; entries still carry
	// templateState + templateVer, just no override metadata. The new
	// UI renders this as "no override" — the same as a healthy fleet
	// without overrides set.
	overridesByKey := map[string]appliedConfigOverrideMeta{}
	if fetcher := h.appliedConfigOverrideFetcherFromHandler(); fetcher != nil {
		overrides, oerr := fetcher.FetchOverridesForThing(ctx, id)
		if oerr != nil {
			h.Logger.Warn("applied-config fetch overrides", "nodeId", id, "error", oerr)
		} else {
			for _, ov := range overrides {
				overridesByKey[ov.ConfigKey] = ov
			}
		}
	}

	appliedByKey := map[string]json.RawMessage{}
	if len(thing.Reported) > 0 && !bytes.Equal(bytes.TrimSpace(thing.Reported), []byte("null")) {
		if err := json.Unmarshal(thing.Reported, &appliedByKey); err != nil {
			h.Logger.Error("applied-config decode applied", "nodeId", id, "error", err)
			return c.JSON(http.StatusInternalServerError, errJSON("Failed to decode applied config", "server_error", "INTERNAL_ERROR"))
		}
	}

	targetByKey := map[string]json.RawMessage{}
	if len(thing.Desired) > 0 && !bytes.Equal(bytes.TrimSpace(thing.Desired), []byte("null")) {
		if err := json.Unmarshal(thing.Desired, &targetByKey); err != nil {
			h.Logger.Error("applied-config decode target", "nodeId", id, "error", err)
			return c.JSON(http.StatusInternalServerError, errJSON("Failed to decode target config", "server_error", "INTERNAL_ERROR"))
		}
	}

	configs := make(map[string]appliedConfigEntry, len(templates)+len(overridesByKey))
	for _, tpl := range templates {
		targetBytes := tpl.State
		if targetRaw, ok := targetByKey[tpl.ConfigKey]; ok {
			targetBytes = targetRaw
		}
		appliedRaw, hasApplied := appliedByKey[tpl.ConfigKey]
		var appliedBytes json.RawMessage
		if hasApplied {
			appliedBytes = appliedRaw
		}

		entry := appliedConfigEntry{
			TargetConfig:   targetBytes,
			TargetVersion:  thing.DesiredVer,
			AppliedConfig:  appliedBytes,
			AppliedVersion: thing.ReportedVer,
			TemplateState:  tpl.State,
			TemplateVer:    tpl.Version,
			InSync:         (hasApplied && bytes.Equal(targetBytes, appliedBytes)) || (!hasApplied && isJSONNullish(targetBytes)),
		}

		if ov, ok := overridesByKey[tpl.ConfigKey]; ok {
			ovCopy := ov
			entry.Override = &ovCopy
		}

		ev, err := st.GetLatestConfigChangeEvent(ctx, thing.Type, tpl.ConfigKey)
		if err != nil {
			// History is best-effort: log and fall through so the UI still
			// renders the target/applied pair even when the audit table is
			// misbehaving for a single key.
			h.Logger.Error("applied-config get latest event",
				"nodeType", thing.Type, "configKey", tpl.ConfigKey, "error", err)
		} else if ev != nil {
			entry.LastChange = &appliedConfigLastChange{
				Timestamp:         ev.Timestamp,
				Actor:             ev.ActorName,
				Action:            ev.Action,
				EmergencyOverride: ev.EmergencyOverride,
			}
		}

		configs[tpl.ConfigKey] = entry
	}

	// Surface "orphan" overrides — keys that have an active override but no
	// matching template (template was deleted, but the override row remains
	// in thing_config_override). Without this loop the entry vanishes from
	// the applied-config response and the admin has no way to see / clear
	// the orphan from the Configuration tab. We synthesise an entry with
	// templateState=null and templateVer=0 so the UI can render "no template"
	// and offer a Clear action; the override.state IS the target state for
	// these keys (the recompute path's overrides ⊕ templates merge keeps
	// override-only keys visible in thing.desired too).
	for key, ov := range overridesByKey {
		if _, already := configs[key]; already {
			continue
		}
		targetRaw, hasTarget := targetByKey[key]
		targetBytes := targetRaw
		if !hasTarget {
			// Override exists but recompute hasn't yet placed the value into
			// thing.desired (race window between the override write and the
			// shadow refresh); fall back to the override state so the UI
			// shows what the operator actually set.
			targetBytes = ov.State
		}
		appliedRaw, hasApplied := appliedByKey[key]
		var appliedBytes json.RawMessage
		if hasApplied {
			appliedBytes = appliedRaw
		}
		ovCopy := ov
		configs[key] = appliedConfigEntry{
			TargetConfig:   targetBytes,
			TargetVersion:  thing.DesiredVer,
			AppliedConfig:  appliedBytes,
			AppliedVersion: thing.ReportedVer,
			TemplateState:  json.RawMessage("null"),
			TemplateVer:    0,
			InSync:         (hasApplied && bytes.Equal(targetBytes, appliedBytes)) || (!hasApplied && isJSONNullish(targetBytes)),
			Override:       &ovCopy,
			// LastChange intentionally nil — config_change_event tracks the
			// per-(type, key) chain which is empty for orphans.
		}
	}

	return c.JSON(http.StatusOK, map[string]any{
		"nodeId":         thing.ID,
		"nodeType":       thing.Type,
		"targetVersion":  thing.DesiredVer,
		"appliedVersion": thing.ReportedVer,
		"configs":        configs,
	})
}

// compile-time assertion: *store.DB must satisfy appliedConfigStore so the
// handler's production wiring stays honest.
var _ appliedConfigStore = (*store.DB)(nil)
