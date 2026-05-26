// Package telemetry provides a hot-swappable OpenTelemetry TracerProvider.
//
// SwappableTracerProvider wraps an OTEL TracerProvider with atomic hot-swap
// capability: atomic.Pointer for lock-free reads on every span creation,
// sync.Mutex only for Reconfigure (rare, serialized). Old providers are
// shut down in a background goroutine with a 5-second timeout.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
	"go.opentelemetry.io/otel/trace/noop"
)

// Config holds the configuration for the tracing provider.
type Config struct {
	Enabled      bool
	Endpoint     string // OTLP HTTP endpoint
	ServiceName  string
	SamplingRate float64 // 0.0–1.0
}

// providerState holds a TracerProvider and whether it is a real (SDK) provider
// that needs to be shut down, as opposed to a no-op provider.
type providerState struct {
	provider trace.TracerProvider
	// sdkProvider is non-nil only when the provider is an SDK TracerProvider
	// that requires Shutdown.
	sdkProvider *sdktrace.TracerProvider
}

// SwappableTracerProvider implements trace.TracerProvider and supports
// atomic hot-swap of the underlying provider via Reconfigure.
type SwappableTracerProvider struct {
	embedded.TracerProvider // satisfies the embedded interface constraint
	current                 atomic.Pointer[providerState]
	mu                      sync.Mutex // serializes Reconfigure calls
	logger                  *slog.Logger
}

// Tracer delegates to the current underlying TracerProvider.
// This is the hot path — uses atomic load, no locks.
func (s *SwappableTracerProvider) Tracer(name string, options ...trace.TracerOption) trace.Tracer {
	st := s.current.Load()
	return st.provider.Tracer(name, options...)
}

// compile-time interface check
var _ trace.TracerProvider = (*SwappableTracerProvider)(nil)

// Init creates a SwappableTracerProvider and registers it as the global
// OTEL TracerProvider via otel.SetTracerProvider.
func Init(ctx context.Context, cfg Config, logger *slog.Logger) (*SwappableTracerProvider, error) {
	st, err := newProvider(ctx, cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("telemetry init: %w", err)
	}

	s := &SwappableTracerProvider{logger: logger}
	s.current.Store(st)
	otel.SetTracerProvider(s)

	logger.Info("telemetry provider initialized",
		"enabled", cfg.Enabled,
		"endpoint", cfg.Endpoint,
		"service", cfg.ServiceName,
		"samplingRate", cfg.SamplingRate,
	)
	return s, nil
}

// Reconfigure creates a new underlying provider from cfg, atomically swaps
// it in, and shuts down the old provider in a background goroutine with a
// 5-second timeout.
func (s *SwappableTracerProvider) Reconfigure(cfg Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, err := newProvider(context.Background(), cfg, s.logger)
	if err != nil {
		return fmt.Errorf("telemetry reconfigure: %w", err)
	}

	old := s.current.Swap(st)
	s.shutdownOldAsync(old)

	s.logger.Info("telemetry provider reconfigured",
		"enabled", cfg.Enabled,
		"endpoint", cfg.Endpoint,
		"service", cfg.ServiceName,
		"samplingRate", cfg.SamplingRate,
	)
	return nil
}

// Shutdown performs a clean shutdown of the current provider. Should be
// called on process exit.
func (s *SwappableTracerProvider) Shutdown(ctx context.Context) error {
	st := s.current.Load()
	if st.sdkProvider != nil {
		return st.sdkProvider.Shutdown(ctx)
	}
	return nil
}

// shutdownOldAsync shuts down the old provider in a goroutine with a
// 5-second timeout.
func (s *SwappableTracerProvider) shutdownOldAsync(old *providerState) {
	if old == nil || old.sdkProvider == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := old.sdkProvider.Shutdown(ctx); err != nil {
			s.logger.Warn("failed to shutdown old tracer provider", "error", err)
		}
	}()
}

// newProvider builds a providerState from the given config. When tracing is
// disabled or no endpoint is configured, it returns a no-op provider.
func newProvider(ctx context.Context, cfg Config, logger *slog.Logger) (*providerState, error) {
	if !cfg.Enabled || cfg.Endpoint == "" {
		logger.Debug("telemetry disabled or no endpoint, using no-op provider",
			"enabled", cfg.Enabled, "endpoint", cfg.Endpoint)
		return &providerState{provider: noop.NewTracerProvider()}, nil
	}

	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpoint(cfg.Endpoint),
		otlptracehttp.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(cfg.ServiceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}

	sampler := sdktrace.ParentBased(
		sdktrace.TraceIDRatioBased(cfg.SamplingRate),
	)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	return &providerState{
		provider:    tp,
		sdkProvider: tp,
	}, nil
}
