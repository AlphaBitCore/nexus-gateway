package wiring

import (
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// InitMQProducer creates a NATS MQ producer when cfg.MQ.Driver is set.
// Returns nil producer (no error) when MQ is not configured.
// The caller must call producer.Close() when done.
func InitMQProducer(cfg *config.Config, logger *slog.Logger) (mq.Producer, error) {
	if cfg.MQ.Driver == "" {
		return nil, nil
	}
	mqCfg := mq.Config{
		Driver:    cfg.MQ.Driver,
		Namespace: "nexus_compliance_proxy",
		NATS:      mq.NATSConfig{URL: cfg.MQ.NATS.URL},
	}
	p, err := mq.NewProducer(mqCfg, logger)
	if err != nil {
		return nil, err
	}
	slog.Info("MQ producer initialized", "driver", cfg.MQ.Driver)
	return p, nil
}
