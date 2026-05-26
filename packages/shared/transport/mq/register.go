package mq

import "log/slog"

func init() {
	RegisterDriver("nats",
		func(cfg Config, logger *slog.Logger) (Producer, error) {
			return NewNATSProducer(cfg.NATS, logger, GetOrCreateMetrics(cfg.Namespace))
		},
		func(cfg Config, logger *slog.Logger) (Consumer, error) {
			return NewNATSConsumer(cfg.NATS, logger, GetOrCreateMetrics(cfg.Namespace))
		},
	)
}
