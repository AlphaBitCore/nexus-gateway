package metrics

import (
	"testing"
	"time"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/prometheus/client_golang/prometheus"
)

// newTestRecorder builds a Recorder against a fresh Prometheus registry so
// each test gets its own isolated registration set. The opsmetrics registry
// rejects no duplicate names (the cache keys by dotted name), but the
// underlying Prometheus registerer would.
func newTestRecorder(_ string) *Recorder {
	return NewRecorder(opsmetrics.NewRegistry(prometheus.NewRegistry()))
}

// TestAlertingSnapshot verifies that RecordRequest increments the
// snapshot-and-reset counters correctly and that a second call returns zero.
func TestAlertingSnapshot_CountsAndResets(t *testing.T) {
	r := newTestRecorder("a")

	// Record 5 requests: 3 success, 1 client error, 1 server error.
	r.RecordRequest("provider", "model", "/v1/chat", 200, time.Second, Usage{})
	r.RecordRequest("provider", "model", "/v1/chat", 201, time.Second, Usage{})
	r.RecordRequest("provider", "model", "/v1/chat", 200, time.Second, Usage{})
	r.RecordRequest("provider", "model", "/v1/chat", 404, time.Second, Usage{})
	r.RecordRequest("provider", "model", "/v1/chat", 503, time.Second, Usage{})

	total, fiveXX := r.AlertingSnapshot()
	if total != 5 {
		t.Errorf("total: want 5, got %d", total)
	}
	if fiveXX != 1 {
		t.Errorf("fiveXX: want 1, got %d", fiveXX)
	}

	// Second call must return zeros — snapshot resets counters.
	total2, fiveXX2 := r.AlertingSnapshot()
	if total2 != 0 {
		t.Errorf("second total: want 0, got %d", total2)
	}
	if fiveXX2 != 0 {
		t.Errorf("second fiveXX: want 0, got %d", fiveXX2)
	}
}

func TestAlertingSnapshot_MultipleServerErrors(t *testing.T) {
	r := newTestRecorder("b")

	for i := range 10 {
		status := 200
		if i%2 == 0 {
			status = 500
		}
		r.RecordRequest("p", "m", "/v1/chat", status, time.Millisecond, Usage{})
	}

	total, fiveXX := r.AlertingSnapshot()
	if total != 10 {
		t.Errorf("total: want 10, got %d", total)
	}
	// Statuses 500,200,500,200,500,200,500,200,500,200 → 5 server errors.
	if fiveXX != 5 {
		t.Errorf("fiveXX: want 5, got %d", fiveXX)
	}
}

func TestAlertingSnapshot_NoRequests(t *testing.T) {
	r := newTestRecorder("c")

	total, fiveXX := r.AlertingSnapshot()
	if total != 0 || fiveXX != 0 {
		t.Errorf("want (0,0), got (%d,%d)", total, fiveXX)
	}
}

func TestAlertingSnapshot_OnlyFourXXNotCounted(t *testing.T) {
	r := newTestRecorder("d")

	r.RecordRequest("p", "m", "/v1/chat", 400, time.Millisecond, Usage{})
	r.RecordRequest("p", "m", "/v1/chat", 401, time.Millisecond, Usage{})
	r.RecordRequest("p", "m", "/v1/chat", 429, time.Millisecond, Usage{})

	total, fiveXX := r.AlertingSnapshot()
	if total != 3 {
		t.Errorf("total: want 3, got %d", total)
	}
	if fiveXX != 0 {
		t.Errorf("fiveXX: want 0 for 4xx-only traffic, got %d", fiveXX)
	}
}
