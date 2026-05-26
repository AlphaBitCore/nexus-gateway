package diag

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

func mkEvt(msg string) opsmetrics.DiagEvent {
	return opsmetrics.DiagEvent{
		ThingID:     "thing-1",
		OccurredAt:  time.Unix(0, 0).UTC(),
		Level:       opsmetrics.LevelError,
		EventType:   opsmetrics.EventTypeError,
		Source:      "test",
		Message:     msg,
		MessageHash: msg,
		RepeatCount: 1,
	}
}

func TestNewReconnectBuffer_AppliesDefaults(t *testing.T) {
	// Zero MaxLen / MaxAge / nil Clock must coerce to defaults — without
	// this, a careless caller would silently get a zero-capacity buffer
	// that drops every event.
	rb := NewReconnectBuffer(ReconnectBufferConfig{})
	if rb.maxLen != 100 {
		t.Errorf("default MaxLen: got %d, want 100", rb.maxLen)
	}
	if rb.maxAge != 5*time.Minute {
		t.Errorf("default MaxAge: got %v, want 5m", rb.maxAge)
	}
	if rb.clock == nil {
		t.Error("clock must default to time.Now, not nil")
	}
}

func TestReconnectBuffer_BoundsByLength(t *testing.T) {
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	dropped := reg.NewCounter("diag.dropped_total", []string{"reason"}).With("reconnect_overflow")

	rb := NewReconnectBuffer(ReconnectBufferConfig{
		MaxLen:  2,
		MaxAge:  time.Hour,
		Dropped: dropped,
		Clock:   time.Now,
	})
	rb.Add(mkEvt("a"))
	rb.Add(mkEvt("b"))
	rb.Add(mkEvt("c")) // should evict "a", increment dropped

	got := rb.Drain()
	if len(got) != 2 {
		t.Fatalf("Drain len = %d, want 2", len(got))
	}
	if got[0].Message != "b" || got[1].Message != "c" {
		t.Errorf("messages = %q,%q; want b,c", got[0].Message, got[1].Message)
	}

	// Dropped counter should report 1.
	samples := reg.Collect()
	var dv float64
	for _, s := range samples {
		if s.Name == "diag.dropped_total" {
			dv = s.Value
		}
	}
	if dv != 1 {
		t.Errorf("dropped_total = %v, want 1", dv)
	}
}

func TestReconnectBuffer_BoundsByAge(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }

	rb := NewReconnectBuffer(ReconnectBufferConfig{
		MaxLen: 100,
		MaxAge: 50 * time.Millisecond,
		Clock:  clock,
	})
	rb.Add(mkEvt("old"))

	// Advance the simulated clock past the max age window.
	now = now.Add(time.Second)
	rb.Add(mkEvt("new"))

	got := rb.Drain()
	if len(got) != 1 || got[0].Message != "new" {
		t.Fatalf("Drain = %+v; want only [new]", messagesOf(got))
	}
}

func TestReconnectBuffer_DrainClearsAndReturns(t *testing.T) {
	rb := NewReconnectBuffer(ReconnectBufferConfig{MaxLen: 4, MaxAge: time.Hour})
	rb.Add(mkEvt("x"))
	rb.Add(mkEvt("y"))

	first := rb.Drain()
	if len(first) != 2 {
		t.Fatalf("first drain len = %d, want 2", len(first))
	}

	if got := rb.Pending(); got != 0 {
		t.Errorf("Pending after drain = %d, want 0", got)
	}

	second := rb.Drain()
	if len(second) != 0 {
		t.Errorf("second drain len = %d, want 0", len(second))
	}
}

func TestReconnectBuffer_NilSafe(t *testing.T) {
	var rb *ReconnectBuffer
	rb.Add(mkEvt("nope")) // must not panic
	if got := rb.Pending(); got != 0 {
		t.Errorf("nil Pending = %d, want 0", got)
	}
	if got := rb.Drain(); got != nil {
		t.Errorf("nil Drain = %v, want nil", got)
	}
}

func messagesOf(evts []opsmetrics.DiagEvent) []string {
	out := make([]string, len(evts))
	for i, e := range evts {
		out[i] = e.Message
	}
	return out
}
