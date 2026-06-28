package ws

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// SubscribeHubSignals subscribes to nexus.hub.signal and broadcasts
// config_changed messages to local connections. Skips signals from this Hub.
//
// A nexus.hub.signal frame can force fleet-wide config (incl. the
// kill-switch) onto every connected node, so it MUST be authenticated as
// Hub-originated. `secret` is the Hub-to-Hub HMAC key; when non-empty, frames
// without a valid signature (a data-plane producer or on-path actor that can
// merely reach NATS) are dropped. The wire shape is a signed envelope around
// manager.HubSignal — decoded via manager.VerifyAndDecodeHubSignal so producer
// and consumer can never drift.
func SubscribeHubSignals(ctx context.Context, consumer mq.Consumer, pool *Pool, hubID string, secret []byte, logger *slog.Logger) {
	logger = logger.With("component", "hub_signal_subscriber")

	err := consumer.Subscribe(ctx, "nexus.hub.signal", func(_ context.Context, msg *mq.Message) error {
		sig, ok := manager.VerifyAndDecodeHubSignal(msg.Data, secret)
		if !ok {
			// Forged / unsigned / malformed — never broadcast it to nodes.
			logger.Warn("dropped unauthenticated or invalid hub signal")
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
