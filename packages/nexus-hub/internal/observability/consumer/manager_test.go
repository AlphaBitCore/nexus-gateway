package consumer

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

func newTestRegistry() *opsmetrics.Registry {
	return opsmetrics.NewRegistry(prometheus.NewRegistry())
}

type fakeConsumer struct {
	startErr error
	started  chan struct{}
}

func (f *fakeConsumer) Start(ctx context.Context) error {
	if f.started != nil {
		close(f.started)
	}
	if f.startErr != nil {
		return f.startErr
	}
	<-ctx.Done()
	return nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestManager_StartAndStop(t *testing.T) {
	c1 := &fakeConsumer{started: make(chan struct{})}
	c2 := &fakeConsumer{started: make(chan struct{})}

	mgr := NewManager(
		[]NamedConsumer{
			{Name: "test-1", Consumer: c1},
			{Name: "test-2", Consumer: c2},
		},
		testLogger(),
		newTestRegistry(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)

	// Wait for both consumers to start
	select {
	case <-c1.started:
	case <-time.After(2 * time.Second):
		t.Fatal("consumer 1 did not start in time")
	}
	select {
	case <-c2.started:
	case <-time.After(2 * time.Second):
		t.Fatal("consumer 2 did not start in time")
	}

	if !mgr.Healthy() {
		t.Error("manager should be healthy while running")
	}

	cancel()
	mgr.Stop()
}

func TestManager_ErrorTracking(t *testing.T) {
	errBoom := errors.New("boom")
	c1 := &fakeConsumer{startErr: errBoom, started: make(chan struct{})}

	mgr := NewManager(
		[]NamedConsumer{
			{Name: "failing", Consumer: c1},
		},
		testLogger(),
		newTestRegistry(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)

	// Give goroutine time to fail
	time.Sleep(100 * time.Millisecond)

	if mgr.Healthy() {
		t.Error("manager should not be healthy after consumer error")
	}

	errs := mgr.Errors()
	if errs["failing"] == nil {
		t.Error("expected error for 'failing' consumer")
	}

	if err := mgr.HealthCheck(); err == nil {
		t.Error("HealthCheck should return error")
	}

	cancel()
	mgr.Stop()
}

func TestManager_HealthyWhenEmpty(t *testing.T) {
	mgr := NewManager(nil, testLogger(), newTestRegistry())
	if !mgr.Healthy() {
		t.Error("empty manager should be healthy")
	}
	if err := mgr.HealthCheck(); err != nil {
		t.Errorf("HealthCheck error: %v", err)
	}
}
