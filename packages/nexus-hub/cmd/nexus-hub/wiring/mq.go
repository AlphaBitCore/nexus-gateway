package wiring

import (
	"context"
	"log/slog"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// MQResult holds the producer and consumer created by InitMQ.
type MQResult struct {
	Producer mq.Producer
	Consumer mq.Consumer
}

// InitMQ initialises the MQ producer and consumer (when cfg.MQ.Driver is
// set), ensures JetStream streams for the "nats" driver, and returns the
// result. When no driver is configured, the returned MQResult has nil
// Producer and Consumer. Callers must Close both on shutdown.
func InitMQ(ctx context.Context, cfg *config.HubConfig, logger *slog.Logger) (MQResult, error) {
	if cfg.MQ.Driver == "" {
		return MQResult{}, nil
	}

	mqCfg := mq.Config{
		Driver:    cfg.MQ.Driver,
		Namespace: "nexus_hub",
		NATS:      mq.NATSConfig{URL: cfg.MQ.NATS.URL},
	}

	producer, err := mq.NewProducer(mqCfg, logger)
	if err != nil {
		return MQResult{}, err
	}

	consumer, err := mq.NewConsumer(mqCfg, logger)
	if err != nil {
		_ = producer.Close() //nolint:errcheck
		return MQResult{}, err
	}
	logger.Info("MQ connected", "driver", cfg.MQ.Driver)

	if cfg.MQ.Driver == "nats" {
		if err := mq.Setup(ctx, cfg.MQ.NATS.URL); err != nil {
			_ = consumer.Close() //nolint:errcheck
			_ = producer.Close() //nolint:errcheck
			return MQResult{}, err
		}
		logger.Info("JetStream streams ensured")
	}

	return MQResult{Producer: producer, Consumer: consumer}, nil
}

// CloseMQAndRedis closes the MQ producer, consumer, and Redis client if non-nil.
// Intended for use as a deferred call in main.
func CloseMQAndRedis(mqRes MQResult, redisClient redis.UniversalClient) {
	if mqRes.Consumer != nil {
		_ = mqRes.Consumer.Close() //nolint:errcheck
	}
	if mqRes.Producer != nil {
		_ = mqRes.Producer.Close() //nolint:errcheck
	}
	if redisClient != nil {
		_ = redisClient.Close() //nolint:errcheck
	}
}
