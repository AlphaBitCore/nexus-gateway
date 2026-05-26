package conn

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

func TestManager_AcquireRelease(t *testing.T) {
	m := NewManager(10)

	connID, err := m.Acquire()
	if err != nil {
		t.Fatalf("unexpected error on Acquire: %v", err)
	}
	if connID == "" {
		t.Fatal("expected non-empty connection ID")
	}
	if m.ActiveCount() != 1 {
		t.Fatalf("expected active count 1, got %d", m.ActiveCount())
	}

	m.Release(connID)
	if m.ActiveCount() != 0 {
		t.Fatalf("expected active count 0 after release, got %d", m.ActiveCount())
	}
}

func TestManager_RejectAtCapacity(t *testing.T) {
	m := NewManager(2)

	id1, err := m.Acquire()
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	if id1 == "" {
		t.Fatal("expected non-empty connection ID for first acquire")
	}

	id2, err := m.Acquire()
	if err != nil {
		t.Fatalf("second acquire failed: %v", err)
	}
	if id2 == "" {
		t.Fatal("expected non-empty connection ID for second acquire")
	}
	if id1 == id2 {
		t.Fatal("expected unique connection IDs")
	}

	// Third acquire should fail.
	_, err = m.Acquire()
	if err == nil {
		t.Fatal("expected error on third acquire, got nil")
	}
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("expected ErrAtCapacity, got: %v", err)
	}

	// Active count should still be 2 (rollback on reject).
	if m.ActiveCount() != 2 {
		t.Fatalf("expected active count 2 after rejection, got %d", m.ActiveCount())
	}

	// After releasing one, acquire should succeed again.
	m.Release(id1)
	_, err = m.Acquire()
	if err != nil {
		t.Fatalf("acquire after release should succeed: %v", err)
	}
}

func TestManager_Concurrent(t *testing.T) {
	const workers = 100
	m := NewManager(workers)

	var wg sync.WaitGroup
	errs := make(chan error, workers)

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := m.Acquire()
			if err != nil {
				errs <- err
				return
			}
			if id == "" {
				errs <- errors.New("empty connection ID")
				return
			}
			// Simulate work.
			m.Release(id)
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("unexpected error in concurrent test: %v", err)
	}

	if m.ActiveCount() != 0 {
		t.Fatalf("expected 0 active after all releases, got %d", m.ActiveCount())
	}
}

func TestManager_ActiveCount(t *testing.T) {
	m := NewManager(5)

	if m.ActiveCount() != 0 {
		t.Fatalf("expected 0 active initially, got %d", m.ActiveCount())
	}

	ids := make([]string, 3)
	for i := range 3 {
		id, err := m.Acquire()
		if err != nil {
			t.Fatalf("acquire %d failed: %v", i, err)
		}
		ids[i] = id
	}
	if m.ActiveCount() != 3 {
		t.Fatalf("expected 3 active, got %d", m.ActiveCount())
	}

	m.Release(ids[0])
	if m.ActiveCount() != 2 {
		t.Fatalf("expected 2 active after one release, got %d", m.ActiveCount())
	}

	m.Release(ids[1])
	m.Release(ids[2])
	if m.ActiveCount() != 0 {
		t.Fatalf("expected 0 active after all releases, got %d", m.ActiveCount())
	}
}

func TestManager_AcquireWithInfo(t *testing.T) {
	m := NewManager(5)

	id, err := m.AcquireWithInfo("192.168.1.1", "api.example.com:443")
	if err != nil {
		t.Fatalf("AcquireWithInfo failed: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty connection ID")
	}

	conns := m.ActiveConnections()
	if len(conns) != 1 {
		t.Fatalf("expected 1 active connection, got %d", len(conns))
	}

	c := conns[0]
	if c.ID != id {
		t.Errorf("expected ID %q, got %q", id, c.ID)
	}
	if c.SourceIP != "192.168.1.1" {
		t.Errorf("expected sourceIP %q, got %q", "192.168.1.1", c.SourceIP)
	}
	if c.TargetHost != "api.example.com:443" {
		t.Errorf("expected targetHost %q, got %q", "api.example.com:443", c.TargetHost)
	}
	if c.ConnectedAt.IsZero() {
		t.Error("expected non-zero ConnectedAt")
	}
	if time.Since(c.ConnectedAt) > 5*time.Second {
		t.Error("ConnectedAt should be recent")
	}

	m.Release(id)

	conns = m.ActiveConnections()
	if len(conns) != 0 {
		t.Fatalf("expected 0 active connections after release, got %d", len(conns))
	}
}

// withRegisteredMetrics binds the package-level metric pointers to a fresh
// prometheus.Registerer so Acquire/Release/AcquireWithInfo hit the
// "metrics != nil" branches. Restores the prior nil pointers on cleanup so
// other tests in this package keep their existing "no metrics registered"
// invariant.
func withRegisteredMetrics(t *testing.T) {
	t.Helper()
	prev := struct {
		active *registry.Gauge
		total  *registry.Counter
	}{
		active: metrics.ConnectionsActive,
		total:  metrics.ConnectionsTotal,
	}
	metrics.Register(registry.NewRegistry(prometheus.NewRegistry()))
	t.Cleanup(func() {
		metrics.ConnectionsActive = prev.active
		metrics.ConnectionsTotal = prev.total
	})
}

func TestManager_MetricsBranchesExercised(t *testing.T) {
	// When the conn-metrics globals are registered (the prod startup path),
	// Acquire / AcquireWithInfo / Release / rejection-at-capacity all
	// branch into the Inc/Set calls. Without this test those nil-guards
	// stay uncovered because every other test runs with the unregistered
	// defaults.
	withRegisteredMetrics(t)

	m := NewManager(1)

	// Acquire success path: ConnectionsActive.Set on success.
	id, err := m.AcquireWithInfo("10.0.0.1", "host.example:443")
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}

	// Rejection-at-capacity: ConnectionsTotal.With("rejected_capacity").Inc.
	if _, err := m.Acquire(); !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("expected ErrAtCapacity, got: %v", err)
	}

	// Release: ConnectionsActive.Set after decrement.
	m.Release(id)
	if m.ActiveCount() != 0 {
		t.Fatalf("expected 0 active after release, got %d", m.ActiveCount())
	}
}

func TestManager_ActiveConnections_MultipleConns(t *testing.T) {
	m := NewManager(10)

	id1, _ := m.AcquireWithInfo("10.0.0.1", "host1.example.com:443")
	id2, _ := m.AcquireWithInfo("10.0.0.2", "host2.example.com:443")
	id3, _ := m.AcquireWithInfo("10.0.0.3", "host1.example.com:443")

	conns := m.ActiveConnections()
	if len(conns) != 3 {
		t.Fatalf("expected 3 active connections, got %d", len(conns))
	}

	// Release one and verify it disappears.
	m.Release(id2)
	conns = m.ActiveConnections()
	if len(conns) != 2 {
		t.Fatalf("expected 2 active connections after release, got %d", len(conns))
	}

	// Verify id2 is no longer present.
	for _, c := range conns {
		if c.ID == id2 {
			t.Error("released connection should not appear in ActiveConnections")
		}
	}

	m.Release(id1)
	m.Release(id3)

	if m.ActiveCount() != 0 {
		t.Fatalf("expected 0 active after all releases, got %d", m.ActiveCount())
	}
}
