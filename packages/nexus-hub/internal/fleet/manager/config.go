package manager

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// UpdateConfigRequest is the input for pushing a config update.
type UpdateConfigRequest struct {
	ThingType string `json:"thingType"`
	ConfigKey string `json:"configKey"`
	State     any    `json:"state"`
	Action    string `json:"action"`
	ActorID   string `json:"actorId"`
	ActorName string `json:"actorName"`
	SourceIP  string `json:"sourceIp"`
}

// UpdateConfigResponse is returned after a config update.
type UpdateConfigResponse struct {
	OK      bool  `json:"ok"`
	Version int64 `json:"version"` // thing_config_template.version for this config_key (audit / admin history)
	// ThingDesiredVer is the monotonic thing.desired_ver written to every Thing
	// row of the type after this update — the same value broadcast on config_changed.
	ThingDesiredVer int64 `json:"thingDesiredVer,omitempty"`
	ThingsNotified  int   `json:"thingsNotified"`
	ThingsOnline    int   `json:"thingsOnline"`
}

// UpdateConfig performs the 6-step config update flow:
//  1. Upsert config template (version++)
//  2. Update desired + desired_ver for all Things of the type
//  3. Cache in Redis
//  4. Insert config_change_event
//  5. Broadcast config_changed via WebSocket
//  6. Publish hub signal via MQ for peer Hubs
//
// Steps 1-4 are transactional. Steps 5-6 are best-effort.
func (m *Manager) UpdateConfig(ctx context.Context, req UpdateConfigRequest) (*UpdateConfigResponse, error) {
	m.logger.Info("config update requested",
		"event", "config_update_requested",
		"thing_type", req.ThingType,
		"config_key", req.ConfigKey,
		"action", req.Action,
		"actor_id", req.ActorID,
	)
	pool := m.txPool()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Step 1: Upsert template
	newVer, err := m.store.ConfigStore().UpsertConfigTemplate(ctx, tx, req.ThingType, req.ConfigKey, req.State, req.ActorID)
	if err != nil {
		return nil, fmt.Errorf("upsert template: %w", err)
	}

	// Step 2: Update desired for all Things of this type (monotonic thing.desired_ver)
	affected, thingShadowVer, err := m.store.RegistryStore().UpdateDesiredForType(ctx, tx, req.ThingType, req.ConfigKey, req.State, newVer)
	if err != nil {
		return nil, fmt.Errorf("update desired: %w", err)
	}

	// Step 4: Insert config change event (within same tx)
	err = m.store.ConfigStore().InsertConfigChangeEvent(ctx, tx, store.ConfigChangeEvent{
		ThingType:  req.ThingType,
		ConfigKey:  req.ConfigKey,
		Action:     req.Action,
		ActorID:    req.ActorID,
		ActorName:  req.ActorName,
		NewState:   req.State,
		NewVersion: newVer,
		SourceIP:   req.SourceIP,
	})
	if err != nil {
		return nil, fmt.Errorf("insert change event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	// Step 3: Cache in Redis (post-commit, best-effort)
	m.cacheDesiredKey(ctx, req.ThingType, req.ConfigKey, req.State)

	// Step 5: Broadcast via WebSocket (best-effort). Use thingShadowVer so all
	// Things of the type receive the same monotonic version (see store.UpdateDesiredForType).
	// Step 6: Publish MQ signal for peer Hubs (best-effort)
	notified := 0
	switch {
	case affected > 0 && thingShadowVer > 0:
		notified = m.broadcastConfigChanged(req.ThingType, req.ConfigKey, req.State, thingShadowVer)
		m.publishHubSignal(ctx, req.ThingType, req.ConfigKey, req.State, thingShadowVer)
	case affected == 0:
		m.logger.Warn("config update: no thing rows updated for type",
			"event", "config_update_no_things",
			"thing_type", req.ThingType,
			"config_key", req.ConfigKey,
			"template_version", newVer,
		)
	default:
		m.logger.Warn("config update: rows updated but thing_desired_ver is zero",
			"event", "config_update_bad_shadow_ver",
			"thing_type", req.ThingType,
			"config_key", req.ConfigKey,
			"template_version", newVer,
			"things_online", affected,
		)
	}
	m.logger.Info("config update committed and dispatched",
		"event", "config_update_dispatched",
		"thing_type", req.ThingType,
		"config_key", req.ConfigKey,
		"template_version", newVer,
		"thing_desired_ver", thingShadowVer,
		"things_online", affected,
		"things_notified", notified,
	)

	resp := &UpdateConfigResponse{
		OK:             true,
		Version:        newVer,
		ThingsNotified: notified,
		ThingsOnline:   int(affected),
	}
	if affected > 0 && thingShadowVer > 0 {
		resp.ThingDesiredVer = thingShadowVer
	}
	return resp, nil
}

// ConfigChangedMessage is the per-key delta broadcast to Things on config change.
// Things merge {ConfigKey: State} into their local desired cache, set DesiredVer
// from this message to the shared monotonic thing.desired_ver (see
// store.UpdateDesiredForType), and emit a shadow_report so reported_ver can
// catch up to that version.
//
// Force is set exclusively by admin-triggered re-sync replays (see
// Manager.RePushConfigKey): it tells the receiving Thing to run its
// OnConfigChanged callback and emit a fresh shadow_report even when
// DesiredVer does not exceed reportedVer. Normal UpdateConfig broadcasts
// never set this field.
type ConfigChangedMessage struct {
	Type       string          `json:"type"`
	ConfigKey  string          `json:"configKey"`
	State      json.RawMessage `json:"state"`
	DesiredVer int64           `json:"desiredVer"`
	Force      bool            `json:"force,omitempty"`
}

func (m *Manager) broadcastConfigChanged(thingType, configKey string, state any, ver int64) int {
	if m.ws == nil {
		m.logger.Warn("config_changed broadcast skipped: websocket server unavailable",
			"event", "config_broadcast_skipped",
			"thing_type", thingType,
			"config_key", configKey,
			"desired_ver", ver,
		)
		return 0
	}
	stateRaw, err := json.Marshal(state)
	if err != nil {
		m.logger.Error("marshal config_changed state", "error", err)
		return 0
	}
	msg := ConfigChangedMessage{
		Type:       "config_changed",
		ConfigKey:  configKey,
		State:      stateRaw,
		DesiredVer: ver,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		m.logger.Error("marshal config_changed", "error", err)
		return 0
	}
	notified := m.ws.Broadcast(thingType, data)
	m.logger.Info("config_changed broadcast sent",
		"event", "config_broadcast_sent",
		"thing_type", thingType,
		"config_key", configKey,
		"desired_ver", ver,
		"things_notified", notified,
	)
	return notified
}

// HubSignal is the MQ message for inter-Hub coordination.
//
// Force and ThingID are populated by admin re-sync replays so a peer Hub
// can deliver a forced config_changed message to exactly the Thing the
// operator clicked on. ThingID empty ⇒ broadcast to the whole ThingType
// (the default UpdateConfig semantics).
type HubSignal struct {
	Action    string `json:"action"`
	SourceHub string `json:"sourceHub"`
	ThingType string `json:"thingType"`
	ConfigKey string `json:"configKey"`
	State     any    `json:"state"`
	Version   int64  `json:"version"`
	ThingID   string `json:"thingId,omitempty"`
	Force     bool   `json:"force,omitempty"`
}

func (m *Manager) publishHubSignal(ctx context.Context, thingType, configKey string, state any, ver int64) {
	if m.mq == nil {
		return
	}
	sig := HubSignal{
		Action:    "config_changed",
		SourceHub: m.hubID,
		ThingType: thingType,
		ConfigKey: configKey,
		State:     state,
		Version:   ver,
	}
	data, err := json.Marshal(sig)
	if err != nil {
		return
	}
	if err := m.mq.Publish(ctx, "nexus.hub.signal", data); err != nil {
		m.logger.Warn("publish hub signal failed", "error", err)
	}
}

func (m *Manager) cacheDesiredKey(ctx context.Context, thingType, configKey string, state any) {
	if m.redis == nil {
		return
	}
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	rkey := fmt.Sprintf("nexus:desired:%s:%s", thingType, configKey)
	m.redis.Set(ctx, rkey, data, 0)
}
