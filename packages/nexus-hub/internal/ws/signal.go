package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// hubSignal is the shape of messages on nexus.hub.signal.
//
// Force marks the signal as an admin-triggered re-sync replay; the subscriber
// propagates it onto the local WebSocket broadcast so the receiving Thing
// bypasses its version-equality short-circuit.
type hubSignal struct {
	Action    string         `json:"action"`
	SourceHub string         `json:"sourceHub"`
	ThingType string         `json:"thingType"`
	ConfigKey string         `json:"configKey"`
	State     any            `json:"state"`
	Version   int64          `json:"version"`
	ThingID   string         `json:"thingId,omitempty"`
	Desired   map[string]any `json:"desired,omitempty"`
	Force     bool           `json:"force,omitempty"`
}

// SubscribeHubSignals subscribes to nexus.hub.signal and broadcasts
// config_changed messages to local connections. Skips signals from this Hub.
func SubscribeHubSignals(ctx context.Context, consumer mq.Consumer, pool *Pool, hubID string, logger *slog.Logger) {
	logger = logger.With("component", "hub_signal_subscriber")

	err := consumer.Subscribe(ctx, "nexus.hub.signal", func(_ context.Context, msg *mq.Message) error {
		var sig hubSignal
		if err := json.Unmarshal(msg.Data, &sig); err != nil {
			logger.Warn("invalid hub signal", "error", err)
			return nil
		}

		if sig.SourceHub == hubID {
			return nil
		}

		switch sig.Action {
		case "config_changed":
			broadcast := ConfigChangedMessage{
				Type:       "config_changed",
				ConfigKey:  sig.ConfigKey,
				State:      sig.State,
				DesiredVer: sig.Version,
				Desired:    sig.Desired,
				Force:      sig.Force,
			}
			data, err := json.Marshal(broadcast)
			if err != nil {
				logger.Error("marshal hub signal broadcast", "error", err)
				return nil //nolint:nilerr // signal already consumed; surface only via log
			}

			if sig.ThingID != "" {
				pool.Send(sig.ThingID, data)
			} else if sig.ThingType != "" {
				pool.Broadcast(sig.ThingType, data)
			}

		default:
			logger.Debug("unknown hub signal action", "action", sig.Action)
		}

		return nil
	})
	if err != nil {
		// During graceful shutdown the parent ctx is cancelled and the MQ
		// driver returns context.Canceled — that is the normal stop path,
		// not an error worth paging on. Anything else (NATS connection
		// drop mid-run, malformed subject, etc.) stays at error level so
		// it still shows up in diag-event ERROR groups.
		if errors.Is(err, context.Canceled) {
			logger.Info("hub signal subscription ended (context canceled)")
			return
		}
		logger.Error("hub signal subscription failed", "error", err)
	}
}
