package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// ErrConfigKeyNotInDesired is returned when RePushConfigKey is asked to replay
// a key that is not present in the Thing's desired map. Callers typically map
// this to HTTP 404 so the admin UI can distinguish "bad key" from transient
// Hub errors.
var ErrConfigKeyNotInDesired = errors.New("config key not in thing desired state")

// ErrNoDeliveryPath is returned by RePushConfigKey / RePushAllKeys when neither
// WebSocket nor MQ can deliver the force-push. The caller is responsible for
// surfacing this so override-set / override-clear post-commit warnings fire
// consistently — silent nil here causes audit-committed overrides that the
// client never receives to look like success.
var ErrNoDeliveryPath = errors.New("no delivery path: thing not WS-connected and no MQ configured")

// RePushConfig re-pushes desired config to a specific Thing using per-key deltas.
// Used by the drift detection job to attempt repair.
// Sends via WebSocket if connected locally, otherwise publishes a hub signal per key.
func (m *Manager) RePushConfig(ctx context.Context, thingID, thingType string) error {
	thing, err := m.store.RegistryStore().GetThing(ctx, thingID)
	if err != nil {
		return err
	}
	return m.rePushConfigForThing(ctx, thingType, thing)
}

// rePushConfigForThing performs the per-key fan-out for a pre-fetched thing.
// Separated from RePushConfig to allow unit testing without a real database.
func (m *Manager) rePushConfigForThing(ctx context.Context, thingType string, thing *store.Thing) error {
	if len(thing.Desired) == 0 {
		m.logger.Warn("RePushConfig: no desired config to push",
			slog.String("event", "repush_noop"),
			slog.String("thing_id", thing.ID),
		)
		return nil
	}

	connected := m.ws != nil && m.ws.IsConnected(thing.ID)

	// Send one per-key delta message for each key in the desired map.
	for configKey, state := range thing.Desired {
		stateRaw, err := json.Marshal(state)
		if err != nil {
			return fmt.Errorf("marshal state for key %q: %w", configKey, err)
		}
		msg := ConfigChangedMessage{
			Type:       "config_changed",
			ConfigKey:  configKey,
			State:      stateRaw,
			DesiredVer: thing.DesiredVer,
		}
		data, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("marshal config_changed for key %q: %w", configKey, err)
		}

		if connected {
			if ok := m.ws.Send(thing.ID, data); ok {
				continue
			}
			// Send returned false (race or write error). Fall through to
			// the MQ branch so we don't drop the delivery silently — drift
			// repair must converge.
			m.logger.Warn("repush: WS Send returned false; trying MQ fallback",
				slog.String("event", "repush_key_ws_send_failed"),
				slog.String("thing_id", thing.ID),
				slog.String("config_key", configKey),
			)
		}

		// Not connected locally (or WS Send raced) — publish hub signal for
		// other Hub instances / for retry-on-reconnect.
		if m.mq != nil {
			sig := HubSignal{
				Action:    "config_changed",
				SourceHub: m.hubID,
				ThingType: thingType,
				ConfigKey: configKey,
				State:     state,
				Version:   thing.DesiredVer,
			}
			sigData, err := json.Marshal(sig)
			if err != nil {
				return fmt.Errorf("marshal hub signal for key %q: %w", configKey, err)
			}
			if err := m.mq.Publish(ctx, "nexus.hub.signal", sigData); err != nil {
				return fmt.Errorf("publish hub signal for key %q: %w", configKey, err)
			}
		}
	}

	return nil
}

// RePushFailure records one key whose force-push failed during a whole-Thing
// resync. Err is the string-valued error message so the result is JSON-friendly
// for the Hub HTTP response.
type RePushFailure struct {
	ConfigKey string `json:"configKey"`
	Err       string `json:"error"`
}

// RePushAllResult summarises a RePushAllKeys run. Pushed counts the keys
// that delivered (WS or MQ); Failed lists every key that erred. The two
// together cover every key in thing.Desired.
type RePushAllResult struct {
	Pushed int             `json:"pushed"`
	Failed []RePushFailure `json:"failed,omitempty"`
}

// RePushAllKeys re-pushes every key currently in thing.Desired with Force=true.
// Used by:
//   - admin "Force resync all" action on the node detail page;
//   - override-expiry job after clearing the last override on a Thing
//     (whole-Thing replay is overkill, but harmless).
//
// Per-key failures are accumulated into Failed rather than aborting the
// loop — admin operators expect "push as many as possible, tell me what
// didn't make it" semantics. The function returns (result, nil) even when
// every key failed; a non-nil error is reserved for whole-Thing failures
// like GetThing surfacing a missing Thing.
//
// Empty desired map returns (&RePushAllResult{}, nil) — not an error; a
// freshly-enrolled Thing with no template yet is valid.
func (m *Manager) RePushAllKeys(ctx context.Context, thingID string) (*RePushAllResult, error) {
	if thingID == "" {
		return nil, fmt.Errorf("thingID is required")
	}
	thing, err := m.store.RegistryStore().GetThing(ctx, thingID)
	if err != nil {
		return nil, err
	}
	res := &RePushAllResult{}
	for k := range thing.Desired {
		if err := m.rePushConfigKeyForThing(ctx, thing, k); err != nil {
			res.Failed = append(res.Failed, RePushFailure{ConfigKey: k, Err: err.Error()})
			continue
		}
		res.Pushed++
	}
	return res, nil
}

// RePushConfigKey re-pushes the current desired state for a single key to a
// specific Thing. Unlike UpdateConfig it does not bump the config template
// version, touch thing.Desired, insert a config_change_event row, or fan out
// to peer Things of the same type — it is a pure WebSocket replay driven by
// the admin "Re-sync this key" action on the Node Detail page.
//
// Returns ErrConfigKeyNotInDesired when the key is not present on the Thing
// so callers can return a clean 404 instead of a silent success.
func (m *Manager) RePushConfigKey(ctx context.Context, thingID, configKey string) error {
	if thingID == "" || configKey == "" {
		return fmt.Errorf("thingID and configKey are required")
	}
	thing, err := m.store.RegistryStore().GetThing(ctx, thingID)
	if err != nil {
		return err
	}
	return m.rePushConfigKeyForThing(ctx, thing, configKey)
}

// rePushConfigKeyForThing performs the single-key WS replay for a pre-fetched
// Thing. Separated from RePushConfigKey to enable unit testing without a real
// database: delivery preference matches rePushConfigForThing — local WebSocket
// first, fall back to nexus.hub.signal MQ broadcast when the Thing is
// connected to a peer Hub.
func (m *Manager) rePushConfigKeyForThing(ctx context.Context, thing *store.Thing, configKey string) error {
	state, ok := thing.Desired[configKey]
	if !ok {
		return ErrConfigKeyNotInDesired
	}

	stateRaw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal state for key %q: %w", configKey, err)
	}
	// Force=true so the Thing bypasses its version-equality short-circuit and
	// runs OnConfigChanged + emits a fresh shadow_report — without this flag
	// an admin replay at the same DesiredVer is a silent no-op on the client.
	msg := ConfigChangedMessage{
		Type:       "config_changed",
		ConfigKey:  configKey,
		State:      stateRaw,
		DesiredVer: thing.DesiredVer,
		Force:      true,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal config_changed for key %q: %w", configKey, err)
	}

	// WS first: ws.Send returns false when the connection has gone away
	// between IsConnected and Send (close-during-call race) or when the
	// underlying Write returned an error. In either case we must NOT report
	// success — we fall through to the MQ branch (or to ErrNoDeliveryPath
	// below) so the caller's audit/log surface remains accurate.
	if m.ws != nil && m.ws.IsConnected(thing.ID) {
		if ok := m.ws.Send(thing.ID, data); ok {
			m.logger.Info("resync: pushed single key via WS",
				slog.String("event", "resync_key_ws"),
				slog.String("thing_id", thing.ID),
				slog.String("config_key", configKey),
				slog.Int64("desired_ver", thing.DesiredVer),
			)
			return nil
		}
		m.logger.Warn("resync: WS Send returned false (conn raced or write failed)",
			slog.String("event", "resync_key_ws_send_failed"),
			slog.String("thing_id", thing.ID),
			slog.String("config_key", configKey),
		)
		// Fall through to MQ branch.
	}

	if m.mq != nil {
		// Carry ThingID + Force=true in the hub signal so the peer Hub
		// delivers a targeted forced replay (ws/signal.go respects both).
		sig := HubSignal{
			Action:    "config_changed",
			SourceHub: m.hubID,
			ThingType: thing.Type,
			ConfigKey: configKey,
			State:     state,
			Version:   thing.DesiredVer,
			ThingID:   thing.ID,
			Force:     true,
		}
		sigData, err := json.Marshal(sig)
		if err != nil {
			return fmt.Errorf("marshal hub signal for key %q: %w", configKey, err)
		}
		if err := m.mq.Publish(ctx, "nexus.hub.signal", sigData); err != nil {
			return fmt.Errorf("publish hub signal for key %q: %w", configKey, err)
		}
		m.logger.Info("resync: published hub signal for remote delivery",
			slog.String("event", "resync_key_signal"),
			slog.String("thing_id", thing.ID),
			slog.String("config_key", configKey),
			slog.Int64("desired_ver", thing.DesiredVer),
		)
		return nil
	}

	m.logger.Warn("resync: Thing not connected locally and no MQ configured",
		slog.String("event", "resync_key_unreachable"),
		slog.String("thing_id", thing.ID),
		slog.String("config_key", configKey),
	)
	return ErrNoDeliveryPath
}
