package ws

import (
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

func TestPool(t *testing.T) {
	pool := NewPool(opsmetrics.NewRegistry(prometheus.NewRegistry()), slog.Default())

	t.Run("SendNotConnected", func(t *testing.T) {
		ok := pool.Send("nonexistent", []byte("hello"))
		if ok {
			t.Error("Send to nonexistent should return false")
		}
	})

	t.Run("IsConnectedEmpty", func(t *testing.T) {
		if pool.IsConnected("anything") {
			t.Error("IsConnected on empty pool should return false")
		}
	})

	t.Run("BroadcastEmpty", func(t *testing.T) {
		n := pool.Broadcast("some-type", []byte("hello"))
		if n != 0 {
			t.Errorf("Broadcast on empty pool should return 0, got %d", n)
		}
	})

	t.Run("CountEmpty", func(t *testing.T) {
		if pool.Count() != 0 {
			t.Errorf("empty pool count should be 0, got %d", pool.Count())
		}
	})
}
