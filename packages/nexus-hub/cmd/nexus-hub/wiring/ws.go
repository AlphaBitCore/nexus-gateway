package wiring

import (
	"context"
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/ws"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// StartWSSignalSubscriber starts the goroutine that subscribes to Hub signal
// events from MQ and broadcasts them to connected WebSocket clients. No-ops
// when mqConsumer is nil.
func StartWSSignalSubscriber(
	ctx context.Context,
	hubID string,
	mqConsumer mq.Consumer,
	wsPool *ws.Pool,
	signalSecret []byte,
	logger *slog.Logger,
) {
	if mqConsumer == nil {
		return
	}
	go ws.SubscribeHubSignals(ctx, mqConsumer, wsPool, hubID, signalSecret, logger)
}
