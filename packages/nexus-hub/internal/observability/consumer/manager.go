package consumer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// Startable is implemented by all consumer types (TrafficEventWriter,
// AdminAuditWriter, SIEMForwarder). Start blocks until ctx is cancelled.
type Startable interface {
	Start(ctx context.Context) error
}

// NamedConsumer pairs a Startable with a human-readable name for logs/health.
type NamedConsumer struct {
	Name     string
	Consumer Startable
}

// Manager orchestrates the lifecycle of all Hub consumers. It starts them
// concurrently, tracks health via opsmetrics gauges, and stops them when
// the parent context is cancelled.
type Manager struct {
	consumers []NamedConsumer
	logger    *slog.Logger

	healthGauge *opsmetrics.Gauge

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
	errors map[string]error
}

// NewManager creates a consumer manager. The opsmetrics registry powers
// both the /metrics scrape surface and the per-tick metrics_sample push.
// Pass nil only in test harnesses that do not exercise the metrics path.
func NewManager(consumers []NamedConsumer, logger *slog.Logger, reg *opsmetrics.Registry) *Manager {
	m := &Manager{
		consumers: consumers,
		logger:    logger.With("component", "consumer-manager"),
		errors:    make(map[string]error, len(consumers)),
	}
	if reg != nil {
		m.healthGauge = reg.NewGauge("consumer.healthy", []string{"consumer"})
	}
	return m
}

// Start launches all registered consumers in goroutines and blocks until
// ctx is cancelled or Stop is called. Safe to call only once.
func (m *Manager) Start(ctx context.Context) {
	ctx, m.cancel = context.WithCancel(ctx)

	for _, nc := range m.consumers {
		m.wg.Add(1)
		if m.healthGauge != nil {
			m.healthGauge.With(nc.Name).Set(1)
		}

		go func() {
			defer m.wg.Done()
			defer func() {
				if m.healthGauge != nil {
					m.healthGauge.With(nc.Name).Set(0)
				}
			}()

			m.logger.Info("starting consumer", "name", nc.Name)
			if err := nc.Consumer.Start(ctx); err != nil && ctx.Err() == nil {
				m.logger.Error("consumer exited with error",
					"name", nc.Name, "error", err)
				m.mu.Lock()
				m.errors[nc.Name] = err
				m.mu.Unlock()
			}
			m.logger.Info("consumer stopped", "name", nc.Name)
		}()
	}
}

// Stop cancels all consumers and waits for them to finish.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
}

// Healthy returns true if all consumers are running without errors.
func (m *Manager) Healthy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.errors) == 0
}

// Errors returns a map of consumer name -> error for consumers that exited
// with an error.
func (m *Manager) Errors() map[string]error {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]error, len(m.errors))
	for k, v := range m.errors {
		result[k] = v
	}
	return result
}

// HealthCheck returns nil if all consumers are healthy, or an aggregate
// error listing the unhealthy ones. Suitable for readiness probes.
func (m *Manager) HealthCheck() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.errors) == 0 {
		return nil
	}
	msg := "unhealthy consumers:"
	for name, err := range m.errors {
		msg += fmt.Sprintf(" %s(%v)", name, err)
	}
	return errors.New(msg)
}
