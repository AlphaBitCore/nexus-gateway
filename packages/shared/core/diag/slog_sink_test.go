package diag

import (
	"context"
	"log/slog"
	"testing"
	"time"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// mockThingClient is a test double for the ThingClientPusher interface.
type mockThingClient struct {
	push func(ctx context.Context, evt opsmetrics.DiagEvent) error
}

func (m *mockThingClient) PushDiagEvent(ctx context.Context, evt opsmetrics.DiagEvent) error {
	return m.push(ctx, evt)
}

// mockLocalBuffer is a test double for the LocalBufferInserter interface.
type mockLocalBuffer struct {
	inserts []opsmetrics.DiagEvent
}

func (m *mockLocalBuffer) Insert(evt opsmetrics.DiagEvent) error {
	m.inserts = append(m.inserts, evt)
	return nil
}

func TestSlogSinkEmitsErrorAsDiagEvent(t *testing.T) {
	captured := make(chan opsmetrics.DiagEvent, 4)
	tc := &mockThingClient{push: func(_ context.Context, e opsmetrics.DiagEvent) error {
		captured <- e
		return nil
	}}

	sink := NewSlogSink(SlogSinkConfig{
		ThingClient: tc,
		ThingID:     "thing-1",
		Source:      "test",
	})
	logger := slog.New(sink)
	logger.Error("dial fail", "upstream", "api.openai.com:443")

	select {
	case evt := <-captured:
		if evt.Level != opsmetrics.LevelError {
			t.Errorf("level = %q, want %q", evt.Level, opsmetrics.LevelError)
		}
		if evt.Message != "dial fail" {
			t.Errorf("message = %q, want %q", evt.Message, "dial fail")
		}
		if evt.Source != "test" {
			t.Errorf("source = %q, want %q", evt.Source, "test")
		}
		if evt.ThingID != "thing-1" {
			t.Errorf("thingID = %q, want %q", evt.ThingID, "thing-1")
		}
		if evt.EventType != opsmetrics.EventTypeError {
			t.Errorf("eventType = %q, want %q", evt.EventType, opsmetrics.EventTypeError)
		}
		if evt.MessageHash == "" {
			t.Errorf("messageHash empty; want md5(level|source|message)")
		}
		if evt.RepeatCount != 1 {
			t.Errorf("repeatCount = %d, want 1", evt.RepeatCount)
		}
		if evt.Attrs["upstream"] != "api.openai.com:443" {
			t.Errorf("attrs not captured: %v", evt.Attrs)
		}
	case <-time.After(time.Second):
		t.Fatalf("no DiagEvent received within 1s")
	}
}

// TestSlogSinkExtractsTraceID asserts the auto-extract contract: a slog
// record that carries a `trace_id` string attr lands its value on
// DiagEvent.TraceID (the typed column) and is consumed from the loose
// Attrs map so the JSON payload isn't carrying the same value twice.
//
// This is the load-bearing test for the "handler stamps trace_id once at
// request entry, every downstream slog emit picks it up" contract — if
// the SlogSink ever stops lifting the key, the Hub thing_diag_event.trace_id
// column goes silently NULL for new emits.
func TestSlogSinkExtractsTraceID(t *testing.T) {
	captured := make(chan opsmetrics.DiagEvent, 4)
	tc := &mockThingClient{push: func(_ context.Context, e opsmetrics.DiagEvent) error {
		captured <- e
		return nil
	}}

	sink := NewSlogSink(SlogSinkConfig{
		ThingClient: tc,
		ThingID:     "thing-1",
		Source:      "test",
	})
	logger := slog.New(sink).With(TraceIDAttrKey, "trace-abc-123")
	logger.Error("upstream timed out", "upstream", "api.openai.com:443")

	select {
	case evt := <-captured:
		if evt.TraceID != "trace-abc-123" {
			t.Errorf("TraceID = %q, want %q", evt.TraceID, "trace-abc-123")
		}
		if _, dupe := evt.Attrs[TraceIDAttrKey]; dupe {
			t.Errorf("Attrs still carries trace_id = %v; want consumed into typed field", evt.Attrs[TraceIDAttrKey])
		}
		if evt.Attrs["upstream"] != "api.openai.com:443" {
			t.Errorf("non-trace attrs lost: %v", evt.Attrs)
		}
	case <-time.After(time.Second):
		t.Fatalf("no DiagEvent received within 1s")
	}
}

// TestSlogSinkTraceIDAbsent asserts that records emitted off any request
// scope (no trace_id attr) land with an empty TraceID — not "" stamped
// from a malformed default, not panic. The Hub writer's "" → NULL pointer
// indirection then keeps the column NULL.
func TestSlogSinkTraceIDAbsent(t *testing.T) {
	captured := make(chan opsmetrics.DiagEvent, 4)
	tc := &mockThingClient{push: func(_ context.Context, e opsmetrics.DiagEvent) error {
		captured <- e
		return nil
	}}

	sink := NewSlogSink(SlogSinkConfig{ThingClient: tc, ThingID: "thing-1", Source: "test"})
	logger := slog.New(sink)
	logger.Error("boot-time fault", "phase", "init")

	select {
	case evt := <-captured:
		if evt.TraceID != "" {
			t.Errorf("TraceID = %q, want empty when no attr supplied", evt.TraceID)
		}
	case <-time.After(time.Second):
		t.Fatalf("no DiagEvent received within 1s")
	}
}

// TestSlogSinkTraceIDNonString covers the defensive branch: a non-string
// trace_id attr (e.g. logged as int by accident) does NOT crash, does NOT
// stamp the typed field, and the malformed value survives in Attrs so the
// operator can still find the offending log line.
func TestSlogSinkTraceIDNonString(t *testing.T) {
	captured := make(chan opsmetrics.DiagEvent, 4)
	tc := &mockThingClient{push: func(_ context.Context, e opsmetrics.DiagEvent) error {
		captured <- e
		return nil
	}}

	sink := NewSlogSink(SlogSinkConfig{ThingClient: tc, ThingID: "thing-1", Source: "test"})
	logger := slog.New(sink)
	logger.Error("malformed trace attr", slog.Int(TraceIDAttrKey, 42))

	select {
	case evt := <-captured:
		if evt.TraceID != "" {
			t.Errorf("TraceID = %q, want empty for non-string attr", evt.TraceID)
		}
		if evt.Attrs[TraceIDAttrKey] != int64(42) {
			t.Errorf("malformed trace_id should survive in Attrs; got %v", evt.Attrs[TraceIDAttrKey])
		}
	case <-time.After(time.Second):
		t.Fatalf("no DiagEvent received within 1s")
	}
}

func TestSlogSinkSuppressesInfoByDefault(t *testing.T) {
	captured := make(chan opsmetrics.DiagEvent, 4)
	tc := &mockThingClient{push: func(_ context.Context, e opsmetrics.DiagEvent) error {
		captured <- e
		return nil
	}}

	sink := NewSlogSink(SlogSinkConfig{ThingClient: tc, ThingID: "thing-1", Source: "test"})
	logger := slog.New(sink)
	logger.Info("startup ok")

	select {
	case e := <-captured:
		t.Fatalf("unexpected emit at INFO level: %+v", e)
	case <-time.After(100 * time.Millisecond):
		// expected: INFO is suppressed when IncludeInfo=false (default)
	}
}

// TestSlogSinkFatalGoesToLocalBuffer verifies that FATAL records (slog
// level >= ERROR+4) are persisted via the LocalBufferInserter alongside
// the normal thingclient push, so a hard crash before the next WS flush
// still preserves the event for backfill on next start.
func TestSlogSinkFatalGoesToLocalBuffer(t *testing.T) {
	captured := make(chan opsmetrics.DiagEvent, 4)
	tc := &mockThingClient{push: func(_ context.Context, e opsmetrics.DiagEvent) error {
		captured <- e
		return nil
	}}
	buf := &mockLocalBuffer{}

	sink := NewSlogSink(SlogSinkConfig{
		ThingClient: tc,
		LocalBuffer: buf,
		ThingID:     "thing-1",
		Source:      "test",
	})
	logger := slog.New(sink)
	// FATAL-equivalent: slog.LevelError + 4. mapLevel maps this to LevelFatal.
	logger.Log(context.Background(), slog.LevelError+4, "process going down")

	select {
	case evt := <-captured:
		if evt.Level != opsmetrics.LevelFatal {
			t.Errorf("level = %q, want %q", evt.Level, opsmetrics.LevelFatal)
		}
	case <-time.After(time.Second):
		t.Fatalf("no DiagEvent received within 1s")
	}

	if len(buf.inserts) != 1 {
		t.Fatalf("local buffer inserts = %d, want 1", len(buf.inserts))
	}
	if buf.inserts[0].Level != opsmetrics.LevelFatal {
		t.Errorf("buffered level = %q, want %q", buf.inserts[0].Level, opsmetrics.LevelFatal)
	}
}

// TestSlogSinkRoutesDisconnected verifies that non-FATAL events land in the
// reconnect buffer (NOT in the ThingClient) when IsWSConnected reports false.
func TestSlogSinkRoutesDisconnected(t *testing.T) {
	captured := make(chan opsmetrics.DiagEvent, 4)
	tc := &mockThingClient{push: func(_ context.Context, e opsmetrics.DiagEvent) error {
		captured <- e
		return nil
	}}
	rb := NewReconnectBuffer(ReconnectBufferConfig{MaxLen: 4, MaxAge: time.Hour})

	sink := NewSlogSink(SlogSinkConfig{
		ThingClient:     tc,
		ReconnectBuffer: rb,
		IsWSConnected:   func() bool { return false },
		ThingID:         "thing-1",
		Source:          "test",
	})
	logger := slog.New(sink)
	logger.Error("offline boom")

	select {
	case e := <-captured:
		t.Fatalf("unexpected ThingClient push while disconnected: %+v", e)
	case <-time.After(50 * time.Millisecond):
		// expected: nothing on the wire
	}
	if got := rb.Pending(); got != 1 {
		t.Errorf("ReconnectBuffer.Pending = %d, want 1", got)
	}
	drained := rb.Drain()
	if len(drained) != 1 || drained[0].Message != "offline boom" {
		t.Errorf("Drain = %v, want [offline boom]", drained)
	}
}

// TestSlogSinkRoutesConnected verifies that events flow straight to the
// ThingClient (and NOT to the reconnect buffer) when IsWSConnected reports
// true.
func TestSlogSinkRoutesConnected(t *testing.T) {
	captured := make(chan opsmetrics.DiagEvent, 4)
	tc := &mockThingClient{push: func(_ context.Context, e opsmetrics.DiagEvent) error {
		captured <- e
		return nil
	}}
	rb := NewReconnectBuffer(ReconnectBufferConfig{MaxLen: 4, MaxAge: time.Hour})

	sink := NewSlogSink(SlogSinkConfig{
		ThingClient:     tc,
		ReconnectBuffer: rb,
		IsWSConnected:   func() bool { return true },
		ThingID:         "thing-1",
		Source:          "test",
	})
	logger := slog.New(sink)
	logger.Error("online boom")

	select {
	case e := <-captured:
		if e.Message != "online boom" {
			t.Errorf("captured message = %q, want online boom", e.Message)
		}
	case <-time.After(time.Second):
		t.Fatalf("ThingClient never received the event")
	}
	if got := rb.Pending(); got != 0 {
		t.Errorf("ReconnectBuffer.Pending = %d, want 0 (online path skips buffer)", got)
	}
}

func TestMapLevel_AllBranches(t *testing.T) {
	// Map every slog level slot the sink cares about. Drift here means
	// SIEM filtering by level would silently mis-bucket events.
	cases := []struct {
		in   slog.Level
		want string
	}{
		{slog.LevelDebug, "info"}, // default fall-through
		{slog.LevelInfo, "info"},  // default fall-through (info gets routed by IncludeInfo)
		{slog.LevelWarn, "warn"},
		{slog.LevelWarn + 1, "warn"},
		{slog.LevelError, "error"},
		{slog.LevelError + 1, "error"},
		{slog.LevelError + 4, "fatal"},
		{slog.LevelError + 8, "fatal"},
	}
	for _, c := range cases {
		got := mapLevel(c.in)
		if got != c.want {
			t.Errorf("mapLevel(%v): got %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSlogSink_WithAttrsClonesAndCarriesAttrs asserts the post-PR-G
// contract: WithAttrs with a non-empty list returns a CLONE that prepends
// the attrs onto every record at Handle time. The previous "no-op"
// behaviour silently dropped attrs added via slog.Logger.With(...), which
// is exactly what producers need to stamp trace_id once at request entry.
// An empty-attrs call still returns the same handler (cheap fast-path).
// WithGroup remains a no-op because DiagEvent.Attrs is a flat map.
func TestSlogSink_WithAttrsClonesAndCarriesAttrs(t *testing.T) {
	captured := make(chan opsmetrics.DiagEvent, 4)
	tc := &mockThingClient{push: func(_ context.Context, e opsmetrics.DiagEvent) error {
		captured <- e
		return nil
	}}

	sink := NewSlogSink(SlogSinkConfig{ThingClient: tc, ThingID: "t", Source: "s"})

	// Empty-attrs fast-path: identity.
	if w := sink.WithAttrs(nil); w != sink {
		t.Errorf("WithAttrs(nil) should return identity; got %p vs %p", w, sink)
	}
	// Non-empty: clone diverges from the root pointer.
	w := sink.WithAttrs([]slog.Attr{slog.String("trace_id", "tid-1")})
	if w == sink {
		t.Errorf("WithAttrs(non-empty) returned the same handler; want clone")
	}
	// The clone must carry the attrs onto every record. Drive a real
	// log.Error and assert the typed trace_id field is populated.
	logger := slog.New(w)
	logger.Error("boom", "k", "v")
	select {
	case evt := <-captured:
		if evt.TraceID != "tid-1" {
			t.Errorf("clone did not carry trace_id; got %q", evt.TraceID)
		}
		if evt.Attrs["k"] != "v" {
			t.Errorf("on-record attrs missing on clone: %v", evt.Attrs)
		}
	case <-time.After(time.Second):
		t.Fatalf("no DiagEvent received within 1s")
	}

	// WithGroup stays a flat-map no-op.
	if g := sink.WithGroup("grp"); g != sink {
		t.Errorf("WithGroup returned a different handler: %p vs %p", g, sink)
	}
}

// TestDedupTickEmitsSummariesAfterWindow verifies that suppressed repeats
// produce a summary event with RepeatCount > 1 once the dedup window expires.
func TestDedupTickEmitsSummariesAfterWindow(t *testing.T) {
	now := time.Now().UTC()
	clock := func() time.Time { return now }
	dedup := opsmetrics.NewDedup(clock, 100*time.Millisecond, 16)

	captured := make(chan opsmetrics.DiagEvent, 8)
	tc := &mockThingClient{push: func(_ context.Context, e opsmetrics.DiagEvent) error {
		captured <- e
		return nil
	}}
	sink := NewSlogSink(SlogSinkConfig{
		ThingClient: tc,
		Dedup:       dedup,
		ThingID:     "thing-1",
		Source:      "test",
	})
	logger := slog.New(sink)
	logger.Error("repeat boom") // first emit
	logger.Error("repeat boom") // suppressed
	logger.Error("repeat boom") // suppressed

	// Drain the first emit so the captured channel only carries summaries
	// from this point on.
	select {
	case <-captured:
	case <-time.After(time.Second):
		t.Fatal("first emit never arrived")
	}

	// Advance clock past the window; Tick should produce one summary with
	// RepeatCount=3.
	now = now.Add(time.Second)
	summaries := dedup.Tick()
	if len(summaries) != 1 {
		t.Fatalf("Tick returned %d summaries, want 1", len(summaries))
	}
	if summaries[0].RepeatCount != 3 {
		t.Errorf("RepeatCount = %d, want 3", summaries[0].RepeatCount)
	}
	if summaries[0].Message != "repeat boom" {
		t.Errorf("Message = %q, want %q", summaries[0].Message, "repeat boom")
	}

	// A second Tick (no further submits) returns zero summaries.
	if more := dedup.Tick(); len(more) != 0 {
		t.Errorf("second Tick returned %d, want 0", len(more))
	}
}

// TestSlogSinkDedupSuppression confirms the optional Dedup reference is
// honoured: a repeated message within the dedup window goes through
// once, then the second occurrence is suppressed.
func TestSlogSinkDedupSuppression(t *testing.T) {
	captured := make(chan opsmetrics.DiagEvent, 4)
	tc := &mockThingClient{push: func(_ context.Context, e opsmetrics.DiagEvent) error {
		captured <- e
		return nil
	}}
	dedup := opsmetrics.NewDedup(time.Now, time.Minute, 16)

	sink := NewSlogSink(SlogSinkConfig{
		ThingClient: tc,
		Dedup:       dedup,
		ThingID:     "thing-1",
		Source:      "test",
	})
	logger := slog.New(sink)
	logger.Error("repeated boom")
	logger.Error("repeated boom") // suppressed
	logger.Error("repeated boom") // suppressed

	got := 0
loop:
	for {
		select {
		case <-captured:
			got++
		case <-time.After(100 * time.Millisecond):
			break loop
		}
	}
	if got != 1 {
		t.Errorf("emits = %d, want 1 (dedup should suppress the second + third)", got)
	}
}

// TestNewSlogSink_OpsRegAutoConstructsDedup covers the auto-wire branch:
// when SlogSinkConfig carries an OpsReg and no explicit Dedup, NewSlogSink
// constructs a Dedup with sensible defaults and registers the
// diag.dedup_collapsed_total counter against the supplied registry.
func TestNewSlogSink_OpsRegAutoConstructsDedup(t *testing.T) {
	reg := opsmetrics.NewRegistry(nil) // nil prom is fine — counter still wires
	sink := NewSlogSink(SlogSinkConfig{
		ThingID: "thing-1",
		Source:  "test-service",
		OpsReg:  reg,
	})
	if sink.cfg.Dedup == nil {
		t.Fatal("Dedup should be auto-constructed when OpsReg is non-nil")
	}
}

// TestNewSlogSink_OpsRegRespectsExplicitDedup covers the precedence:
// a caller-supplied Dedup wins over the auto-construct path.
func TestNewSlogSink_OpsRegRespectsExplicitDedup(t *testing.T) {
	reg := opsmetrics.NewRegistry(nil)
	explicitDedup := opsmetrics.NewDedup(time.Now, 30*time.Second, 50)
	sink := NewSlogSink(SlogSinkConfig{
		ThingID: "thing-1",
		Source:  "test-service",
		Dedup:   explicitDedup,
		OpsReg:  reg,
	})
	if sink.cfg.Dedup != explicitDedup {
		t.Error("explicit Dedup should win over OpsReg auto-construct")
	}
}
