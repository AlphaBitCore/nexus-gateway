package mq

import (
	"fmt"
	"log/slog"
)

// ProducerFactory creates a Producer for a specific driver.
type ProducerFactory func(cfg Config, logger *slog.Logger) (Producer, error)

// ConsumerFactory creates a Consumer for a specific driver.
type ConsumerFactory func(cfg Config, logger *slog.Logger) (Consumer, error)

var (
	producerFactories = map[string]ProducerFactory{}
	consumerFactories = map[string]ConsumerFactory{}
)

// RegisterDriver registers producer and consumer factories for a named driver.
// The NATS driver is registered automatically via the init() in register.go;
// no separate blank import is required:
//
//	import _ "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
func RegisterDriver(driver string, pf ProducerFactory, cf ConsumerFactory) {
	producerFactories[driver] = pf
	consumerFactories[driver] = cf
}

// NewProducer creates a Producer for the driver named in cfg.Driver.
// The NATS driver is registered automatically; import this package to activate it.
func NewProducer(cfg Config, logger *slog.Logger) (Producer, error) {
	f, ok := producerFactories[cfg.Driver]
	if !ok {
		return nil, fmt.Errorf("mq: unknown or unregistered driver %q (blank-import the driver sub-package)", cfg.Driver)
	}
	return f(cfg, logger)
}

// NewConsumer creates a Consumer for the driver named in cfg.Driver.
// The NATS driver is registered automatically; import this package to activate it.
func NewConsumer(cfg Config, logger *slog.Logger) (Consumer, error) {
	f, ok := consumerFactories[cfg.Driver]
	if !ok {
		return nil, fmt.Errorf("mq: unknown or unregistered driver %q (blank-import the driver sub-package)", cfg.Driver)
	}
	return f(cfg, logger)
}
