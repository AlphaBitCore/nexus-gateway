package wiring

import (
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// MQResult holds the producer and consumer created by InitMQ.
type MQResult struct {
	Producer mq.Producer
	Consumer mq.Consumer
}

// Close shuts down both the producer and consumer if they are non-nil.
func (r MQResult) Close() {
	if r.Producer != nil {
		_ = r.Producer.Close() //nolint:errcheck
	}
	if r.Consumer != nil {
		_ = r.Consumer.Close() //nolint:errcheck
	}
}

// InitMQ creates a NATS-backed producer and consumer for the control-plane
// namespace. Returns a zero MQResult (nil fields) when cfg.MQ.Driver is empty.
// Callers must call Close() on non-nil Producer/Consumer.
func InitMQ(cfg *config.Config, logger *slog.Logger) (MQResult, error) {
	if cfg.MQ.Driver == "" {
		return MQResult{}, nil
	}

	mqCfg := mq.Config{
		Driver:    cfg.MQ.Driver,
		Namespace: "nexus_control_plane",
		NATS:      mq.NATSConfig{URL: cfg.MQ.NATS.URL},
	}

	producer, err := mq.NewProducer(mqCfg, logger)
	if err != nil {
		return MQResult{}, err
	}
	logger.Info("MQ producer initialized", "driver", cfg.MQ.Driver)

	consumer, err := mq.NewConsumer(mqCfg, logger)
	if err != nil {
		_ = producer.Close()
		return MQResult{}, err
	}
	logger.Info("MQ consumer initialized", "driver", cfg.MQ.Driver)

	return MQResult{Producer: producer, Consumer: consumer}, nil
}
