package manager

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// ErrConfigKeyNotInDesired is returned when RePushConfigKey is asked to replay
// a key that is not present in the Thing's desired map. Callers typically map
// this to HTTP 404 so the admin UI can distinguish "bad key" from transient
// Hub errors.
var ErrConfigKeyNotInDesired = errors.New("config key not in thing desired state")

// ErrNoDeliveryPath is returned by RePushConfigKey when neither WebSocket nor
// MQ can deliver the force-push. The caller is responsible for
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
			sigData, err := SignHubSignal(sig, m.signalSecret)
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

// RePushAllResult summarises a ForceResyncAll run. Pushed counts the keys that
// will reach the Thing (delivered now via WS/MQ, or guaranteed via the
// heartbeat pull after the desired_ver bump); Failed lists every key that erred
// for a non-delivery reason. The two together cover every key in thing.Desired.
type RePushAllResult struct {
	Pushed int             `json:"pushed"`
	Failed []RePushFailure `json:"failed,omitempty"`
}

// RePushConfigKey re-pushes the current desired state for a single key to a
// specific Thing without bumping desired_ver. It is the post-commit delivery
// primitive for the override / break-glass paths, which have ALREADY bumped
// desired_ver inside their own transaction — so a successful WS push is an
// immediacy optimization and ErrNoDeliveryPath there just means "the heartbeat
// pull (driven by the already-bumped version) will deliver it." It is NOT used
// for the admin force-resync actions, which must bump the version themselves
// (ForceResyncKey / ForceResyncAll) so an in-sync HTTP-fallback Thing is not a
// silent no-op.
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

// bumpDesiredVerForResync re-stamps the Thing's desired_ver, rewriting the
// current desired map unchanged, so an admin force-resync converges on Things
// reachable only via the HTTP heartbeat-pull path — not just WS-connected ones.
//
// Why the bump is required: rePushConfigKeyForThing delivers a Force=true
// config_changed over local WS, or best-effort over the nexus.hub.signal MQ
// broadcast. An HTTP-fallback Thing is on no Hub's WS pool, so the MQ broadcast
// is silently dropped (ws/signal.go pool.Send → not-found), and because a pure
// replay does NOT bump desired_ver the heartbeat version-compare
// (desired_ver != reported_ver) never fires a pull either — the resync is a
// silent no-op while the admin sees success. Bumping desired_ver makes the
// resync behave like every other reliable config delivery: the heartbeat pull
// (and any reconnect snapshot) carries it. The override / break-glass paths do
// NOT use this — they already bump desired_ver inside their own tx.
//
// Takes AcquireConfigVersionLock first so the per-Thing bump serializes
// against the type-fanout and admin-override version allocations. Mutates
// thing.DesiredVer in place so the subsequent immediate WS push carries the new
// version. Empty desired is a no-op (nothing to deliver).
func (m *Manager) bumpDesiredVerForResync(ctx context.Context, thing *store.Thing) error {
	if len(thing.Desired) == 0 {
		return nil
	}
	pool := m.txPool()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("resync begin tx %s: %w", thing.ID, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if err := m.store.RegistryStore().AcquireConfigVersionLock(ctx, tx, thing.Type); err != nil {
		return fmt.Errorf("resync acquire config version lock %s: %w", thing.Type, err)
	}
	newVer, err := m.store.RegistryStore().WriteDesiredAndBumpVer(ctx, tx, thing.ID, thing.Desired)
	if err != nil {
		return fmt.Errorf("resync bump desired ver %s: %w", thing.ID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("resync commit %s: %w", thing.ID, err)
	}
	thing.DesiredVer = newVer
	return nil
}

// ForceResyncKey is the admin "Re-sync this key" action. Unlike the internal
// RePushConfigKey (used post-commit by the override / break-glass paths, which
// have already bumped desired_ver) it FIRST bumps desired_ver so the resync
// reaches HTTP-fallback Things via the heartbeat pull, then does an immediate
// best-effort WS/MQ push at the new version. ErrNoDeliveryPath from the push is
// non-fatal: the version bump above guarantees delivery on the next heartbeat
// even when no live WS receiver exists, so reporting it as a failure would lie
// in the opposite direction. Returns ErrConfigKeyNotInDesired (→ 404)
// when the key is absent, store.ErrNotFound when the Thing is missing.
func (m *Manager) ForceResyncKey(ctx context.Context, thingID, configKey string) error {
	if thingID == "" || configKey == "" {
		return fmt.Errorf("thingID and configKey are required")
	}
	thing, err := m.store.RegistryStore().GetThing(ctx, thingID)
	if err != nil {
		return err
	}
	if _, ok := thing.Desired[configKey]; !ok {
		return ErrConfigKeyNotInDesired
	}
	if err := m.bumpDesiredVerForResync(ctx, thing); err != nil {
		return err
	}
	if err := m.rePushConfigKeyForThing(ctx, thing, configKey); err != nil && !errors.Is(err, ErrNoDeliveryPath) {
		return err
	}
	return nil
}

// ForceResyncAll is the admin "Force resync all" action. It bumps desired_ver
// once (so every key converges via the heartbeat pull on HTTP-fallback Things)
// then pushes each key immediately, best-effort, over WS/MQ at the new
// version. A per-key ErrNoDeliveryPath is counted as Pushed because the version
// bump already guarantees heartbeat delivery; any other per-key error is
// recorded in Failed without aborting the loop. Empty desired returns
// (&RePushAllResult{}, nil).
func (m *Manager) ForceResyncAll(ctx context.Context, thingID string) (*RePushAllResult, error) {
	if thingID == "" {
		return nil, fmt.Errorf("thingID is required")
	}
	thing, err := m.store.RegistryStore().GetThing(ctx, thingID)
	if err != nil {
		return nil, err
	}
	if err := m.bumpDesiredVerForResync(ctx, thing); err != nil {
		return nil, err
	}
	res := &RePushAllResult{}
	for k := range thing.Desired {
		if err := m.rePushConfigKeyForThing(ctx, thing, k); err != nil && !errors.Is(err, ErrNoDeliveryPath) {
			res.Failed = append(res.Failed, RePushFailure{ConfigKey: k, Err: err.Error()})
			continue
		}
		res.Pushed++
	}
	return res, nil
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
		// Hub signals must be HMAC-signed: peer Hubs verify via
		// VerifyAndDecodeHubSignal and DROP unsigned frames, so a plain
		// json.Marshal here would make the targeted resync silently
		// undeliverable — and would let any actor with bare NATS access
		// forge a forced replay.
		sigData, err := SignHubSignal(sig, m.signalSecret)
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
