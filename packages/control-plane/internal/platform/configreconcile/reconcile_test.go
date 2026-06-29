package configreconcile

import (
	"context"
	"errors"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestJSONEqual_ByteIdentical(t *testing.T) {
	if !jsonEqual([]byte(`{"a":1}`), []byte(`{"a":1}`)) {
		t.Errorf("identical bytes must compare equal")
	}
}

func TestJSONEqual_KeyOrderInvariant(t *testing.T) {
	a := json.RawMessage(`{"a":1,"b":2}`)
	b := json.RawMessage(`{"b":2,"a":1}`)
	if !jsonEqual(a, b) {
		t.Errorf("key order should not cause drift false-positive")
	}
}

func TestJSONEqual_WhitespaceInvariant(t *testing.T) {
	a := json.RawMessage(`{"a":1}`)
	b := json.RawMessage(`{"a": 1}`)
	if !jsonEqual(a, b) {
		t.Errorf("whitespace should not cause drift false-positive")
	}
}

func TestJSONEqual_DifferentValuesDrift(t *testing.T) {
	if jsonEqual([]byte(`{"a":1}`), []byte(`{"a":2}`)) {
		t.Errorf("different values must compare unequal")
	}
}

func TestJSONEqual_MissingKeyDrift(t *testing.T) {
	if jsonEqual([]byte(`{"a":1,"b":2}`), []byte(`{"a":1}`)) {
		t.Errorf("missing key must compare unequal")
	}
}

func TestJSONEqual_MalformedIsUnequal(t *testing.T) {
	// Malformed JSON on either side must NOT pass as equal — drift
	// detection's job is to flag, not silently mask.
	if jsonEqual([]byte(`{`), []byte(`{}`)) {
		t.Errorf("malformed a should be unequal")
	}
	if jsonEqual([]byte(`{}`), []byte(`{`)) {
		t.Errorf("malformed b should be unequal")
	}
}

func TestJSONEqual_ArraysOrderSensitive(t *testing.T) {
	// JSON arrays ARE order-sensitive — drift detection must respect that.
	if jsonEqual([]byte(`[1,2]`), []byte(`[2,1]`)) {
		t.Errorf("array order matters in JSON; should be unequal")
	}
}

type fakeQuerier struct {
	mu    sync.Mutex
	rows  []ThingDesiredRow
	err   error
	calls []string // record (thingType,configKey)
}

func (f *fakeQuerier) QueryThingDesired(_ context.Context, thingType, configKey string) ([]ThingDesiredRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, thingType+"/"+configKey)
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

type fakeHub struct {
	mu       sync.Mutex
	calls    []hub.ConfigChangeRequest
	failWith error
}

func (f *fakeHub) NotifyConfigChange(_ context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.failWith != nil {
		return nil, f.failWith
	}
	return &hub.ConfigChangeResponse{}, nil
}

func newWatch(loader func(context.Context) (json.RawMessage, error)) Watch {
	return Watch{
		ConfigKey:    "cache",
		ThingType:    "ai-gateway",
		SourceLoader: loader,
	}
}

func TestReconciler_NoDrift_NoHubCall(t *testing.T) {
	q := &fakeQuerier{rows: []ThingDesiredRow{
		{ThingID: "t1", ThingType: "ai-gateway", DesiredJSON: json.RawMessage(`{"a":1}`)},
	}}
	h := &fakeHub{}
	r := New(q, h, testLogger(), time.Hour, []Watch{
		newWatch(func(context.Context) (json.RawMessage, error) {
			return json.RawMessage(`{"a":1}`), nil
		}),
	}, nil)
	r.tick(context.Background())

	if len(h.calls) != 0 {
		t.Errorf("no drift but Hub was called: %+v", h.calls)
	}
}

func TestReconciler_DriftFiresHubNotify(t *testing.T) {
	q := &fakeQuerier{rows: []ThingDesiredRow{
		{ThingID: "t1", ThingType: "ai-gateway", DesiredJSON: json.RawMessage(`{"a":1}`)},
	}}
	h := &fakeHub{}
	r := New(q, h, testLogger(), time.Hour, []Watch{
		newWatch(func(context.Context) (json.RawMessage, error) {
			return json.RawMessage(`{"a":2}`), nil
		}),
	}, nil)
	r.tick(context.Background())

	if len(h.calls) != 1 {
		t.Fatalf("expected 1 hub call, got %d: %+v", len(h.calls), h.calls)
	}
	got := h.calls[0]
	if got.ConfigKey != "cache" {
		t.Errorf("ConfigKey: %q", got.ConfigKey)
	}
	if got.ThingType != "ai-gateway" {
		t.Errorf("ThingType: %q", got.ThingType)
	}
	if got.ActorID != "configreconcile" || got.ActorName != "configreconcile" {
		t.Errorf("actor: %q/%q", got.ActorID, got.ActorName)
	}
}

func TestReconciler_MultipleThingsAllNotified(t *testing.T) {
	q := &fakeQuerier{rows: []ThingDesiredRow{
		{ThingID: "t1", ThingType: "ai-gateway", DesiredJSON: json.RawMessage(`{"a":1}`)},
		{ThingID: "t2", ThingType: "ai-gateway", DesiredJSON: json.RawMessage(`{"a":1}`)},
		{ThingID: "t3", ThingType: "ai-gateway", DesiredJSON: json.RawMessage(`{"a":1}`)},
	}}
	h := &fakeHub{}
	r := New(q, h, testLogger(), time.Hour, []Watch{
		newWatch(func(context.Context) (json.RawMessage, error) {
			return json.RawMessage(`{"a":99}`), nil
		}),
	}, nil)
	r.tick(context.Background())

	if len(h.calls) != 3 {
		t.Errorf("expected 3 hub calls (one per drifting thing), got %d", len(h.calls))
	}
}

func TestReconciler_SourceLoaderErrSkipsWatch(t *testing.T) {
	q := &fakeQuerier{rows: []ThingDesiredRow{{ThingID: "t1", DesiredJSON: json.RawMessage(`{}`)}}}
	h := &fakeHub{}
	r := New(q, h, testLogger(), time.Hour, []Watch{
		newWatch(func(context.Context) (json.RawMessage, error) {
			return nil, errors.New("oops")
		}),
	}, nil)
	r.tick(context.Background())

	if len(h.calls) != 0 {
		t.Errorf("loader error should not trigger hub notify: %+v", h.calls)
	}
	// Querier must not be called when source load fails.
	if len(q.calls) != 0 {
		t.Errorf("querier should not be called when loader failed: %+v", q.calls)
	}
}

func TestReconciler_QuerierErrLogsAndSkipsHub(t *testing.T) {
	q := &fakeQuerier{err: errors.New("db down")}
	h := &fakeHub{}
	r := New(q, h, testLogger(), time.Hour, []Watch{
		newWatch(func(context.Context) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		}),
	}, nil)
	r.tick(context.Background())

	if len(h.calls) != 0 {
		t.Errorf("DB error must not cascade into hub call: %+v", h.calls)
	}
}

func TestReconciler_HubFailureDoesNotCrash(t *testing.T) {
	q := &fakeQuerier{rows: []ThingDesiredRow{
		{ThingID: "t1", ThingType: "ai-gateway", DesiredJSON: json.RawMessage(`{"a":1}`)},
	}}
	h := &fakeHub{failWith: errors.New("hub unreachable")}
	r := New(q, h, testLogger(), time.Hour, []Watch{
		newWatch(func(context.Context) (json.RawMessage, error) {
			return json.RawMessage(`{"a":2}`), nil
		}),
	}, nil)
	// Must not panic — just log.
	r.tick(context.Background())
	if len(h.calls) != 1 {
		t.Errorf("hub should still be called once: %d", len(h.calls))
	}
}

func TestReconciler_NilHub_DriftLogsButDoesNotNotify(t *testing.T) {
	q := &fakeQuerier{rows: []ThingDesiredRow{
		{ThingID: "t1", ThingType: "ai-gateway", DesiredJSON: json.RawMessage(`{"a":1}`)},
	}}
	r := New(q, nil, testLogger(), time.Hour, []Watch{
		newWatch(func(context.Context) (json.RawMessage, error) {
			return json.RawMessage(`{"a":2}`), nil
		}),
	}, nil)
	// Must not panic.
	r.tick(context.Background())
}

func TestReconciler_Run_ImmediateTickAndStopsOnCancel(t *testing.T) {
	// Reconciler.Run should call tick() once immediately, then on every
	// ticker.C until ctx cancels.
	var ticks atomic.Int32
	q := &fakeQuerier{}
	r := New(q, nil, testLogger(), 50*time.Millisecond, []Watch{
		{
			ConfigKey: "k", ThingType: "t",
			SourceLoader: func(context.Context) (json.RawMessage, error) {
				ticks.Add(1)
				return json.RawMessage(`{}`), nil
			},
		},
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	// Wait long enough for at least 2 ticks (immediate + one ticker).
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if ticks.Load() < 2 {
		t.Errorf("expected >= 2 ticks, got %d", ticks.Load())
	}
}

func TestReconciler_PeriodFloor(t *testing.T) {
	// Period <= 0 should default to 60s — verify the constructor
	// substitutes rather than silently treating 0 as "tick instantly forever".
	r := New(&fakeQuerier{}, &fakeHub{}, testLogger(), 0, nil, nil)
	if r.Period != 60*time.Second {
		t.Errorf("zero period: got %v, want 60s default", r.Period)
	}
	r2 := New(&fakeQuerier{}, &fakeHub{}, testLogger(), -1*time.Second, nil, nil)
	if r2.Period != 60*time.Second {
		t.Errorf("negative period: got %v", r2.Period)
	}
}

func TestReconciler_NilLoggerDefaults(t *testing.T) {
	// nil logger must be substituted with slog.Default() so the loop never
	// panics on first log call. Verify by passing nil and exercising a path
	// that logs (DB error → Warn).
	q := &fakeQuerier{err: errors.New("db down")}
	r := New(q, &fakeHub{}, nil, time.Hour, []Watch{
		newWatch(func(context.Context) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		}),
	}, nil)
	if r.Logger == nil {
		t.Fatal("nil logger should be replaced by slog.Default()")
	}
	// Must not panic when the substituted logger is actually used.
	r.tick(context.Background())
}

func TestReconciler_PrometheusRegistryRegistersAndIncrementsOnDrift(t *testing.T) {
	// When a non-nil registry is supplied, the New constructor MUST register
	// cp_config_drift_total, and the drift path MUST Inc() the counter with
	// the (config_key, thing_type, thing_id) labels — that label tuple is
	// the operator's only signal for which Thing diverged.
	reg := prometheus.NewRegistry()
	q := &fakeQuerier{rows: []ThingDesiredRow{
		{ThingID: "t1", ThingType: "ai-gateway", DesiredJSON: json.RawMessage(`{"a":1}`)},
	}}
	h := &fakeHub{}
	r := New(q, h, testLogger(), time.Hour, []Watch{
		newWatch(func(context.Context) (json.RawMessage, error) {
			return json.RawMessage(`{"a":2}`), nil
		}),
	}, reg)
	if r.driftCounter == nil {
		t.Fatal("driftCounter must be registered when reg != nil")
	}

	r.tick(context.Background())

	got := testutil.ToFloat64(r.driftCounter.WithLabelValues("cache", "ai-gateway", "t1"))
	if got != 1 {
		t.Errorf("cp_config_drift_total{config_key=cache,thing_type=ai-gateway,thing_id=t1}: got %v, want 1", got)
	}
	// Non-drifting label tuple must remain at zero.
	other := testutil.ToFloat64(r.driftCounter.WithLabelValues("cache", "ai-gateway", "tX"))
	if other != 0 {
		t.Errorf("untouched label tuple: got %v, want 0", other)
	}
	// Counter must be discoverable via the registry's gatherer (proves it
	// was registered against the supplied Registerer, not a private one).
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("registry Gather: %v", err)
	}
	var found bool
	for _, mf := range mfs {
		if mf.GetName() == "cp_config_drift_total" {
			found = true
			break
		}
	}
	if !found {
		t.Error("cp_config_drift_total not registered against supplied registry")
	}
}

func TestReconciler_NonJSONSourceFallsBackToRawBytes(t *testing.T) {
	// If a SourceLoader returns bytes that are not valid JSON (e.g. a raw
	// binary blob loaded from a legacy column), drift detection still runs
	// — jsonEqual returns unequal — and the re-emit path must NOT panic
	// when json.Unmarshal fails; it falls back to passing the raw bytes
	// through as State. Verify the Hub is still called and the State field
	// carries those raw bytes.
	rawSource := json.RawMessage(`not valid json at all`)
	q := &fakeQuerier{rows: []ThingDesiredRow{
		{ThingID: "t1", ThingType: "ai-gateway", DesiredJSON: json.RawMessage(`{"a":1}`)},
	}}
	h := &fakeHub{}
	r := New(q, h, testLogger(), time.Hour, []Watch{
		newWatch(func(context.Context) (json.RawMessage, error) {
			return rawSource, nil
		}),
	}, nil)

	r.tick(context.Background())

	if len(h.calls) != 1 {
		t.Fatalf("expected 1 hub call on drift, got %d", len(h.calls))
	}
	// State must equal the raw bytes since Unmarshal failed; the fallback
	// hands the original slice to NotifyConfigChange so Hub can still
	// receive *something* rather than the loop dying on a panic.
	gotState, ok := h.calls[0].State.(json.RawMessage)
	if !ok {
		t.Fatalf("State should be json.RawMessage fallback, got %T (%v)", h.calls[0].State, h.calls[0].State)
	}
	if string(gotState) != string(rawSource) {
		t.Errorf("State bytes: got %q, want %q", string(gotState), string(rawSource))
	}
}

// --- Category-B pending reconcile arm ---

type fakePending struct {
	calls  int32
	healed int
	err    error
}

func (f *fakePending) ReconcilePending(_ context.Context) (int, error) {
	atomic.AddInt32(&f.calls, 1)
	return f.healed, f.err
}

// A tick drives the Pending arm exactly once per tick alongside the content
// Watches.
func TestTick_DrivesPendingArm(t *testing.T) {
	r := New(&fakeQuerier{}, &fakeHub{}, testLogger(), time.Hour, nil, nil)
	p := &fakePending{healed: 2}
	r.Pending = p
	r.tick(context.Background())
	if got := atomic.LoadInt32(&p.calls); got != 1 {
		t.Fatalf("Pending arm called %d times, want 1", got)
	}
}

// A failing Pending arm is logged but never aborts the tick.
func TestTick_PendingErrorIsNonFatal(t *testing.T) {
	r := New(&fakeQuerier{}, &fakeHub{}, testLogger(), time.Hour, nil, nil)
	p := &fakePending{err: errors.New("ledger down")}
	r.Pending = p
	r.tick(context.Background()) // must not panic
	if got := atomic.LoadInt32(&p.calls); got != 1 {
		t.Fatalf("Pending arm called %d times, want 1", got)
	}
}

// A nil Pending arm (no durable backstop wired) is a silent no-op.
func TestReconcilePending_NilArmNoOp(t *testing.T) {
	r := New(&fakeQuerier{}, &fakeHub{}, testLogger(), time.Hour, nil, nil)
	r.reconcilePending(context.Background()) // must not panic
}
