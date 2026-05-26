package alerteval

import (
	"testing"
	"time"
)

// fakeAggregator is a no-op Aggregator just so Engine.Register can be
// driven and the per-rule runtime map gets populated.
type fakeAggregator struct{ id string }

func (f fakeAggregator) RuleID() string                    { return f.id }
func (f fakeAggregator) Sources() []EventSource            { return nil }
func (f fakeAggregator) OnEvent(_ *Runtime, _ *Event)      {}
func (f fakeAggregator) MinWarmupSec(_ map[string]any) int { return 0 }
func (f fakeAggregator) Tick(_ *Runtime, _ map[string]any, _ time.Time) []Decision {
	return nil
}

// TestNewEngine_AppliesDefaults covers NewEngine + every accessor
// in one shot. Engine constructed with zero TickSec / zero StartTime
// must apply defaults; the scheduler.Job accessors must return the
// pinned constants (a future refactor that renames EngineJobID would
// break the admin UI's job-list filtering).
func TestNewEngine_AppliesDefaults(t *testing.T) {
	e := NewEngine(Config{}, nil, nil, nil, nil, nil)
	if e == nil {
		t.Fatal("NewEngine returned nil")
	}
	if e.cfg.TickSec != 5 {
		t.Errorf("TickSec default: got %d, want 5", e.cfg.TickSec)
	}
	if e.cfg.StartTime.IsZero() {
		t.Error("StartTime should be set to now when zero")
	}

	if got := e.ID(); got != EngineJobID {
		t.Errorf("ID = %q, want %q", got, EngineJobID)
	}
	if got := e.Name(); got != engineJobName {
		t.Errorf("Name = %q, want %q", got, engineJobName)
	}
	if got := e.Description(); got != engineJobDescription {
		t.Errorf("Description = %q, want %q", got, engineJobDescription)
	}
	if got := e.Interval(); got != 5*time.Second {
		t.Errorf("Interval = %v, want 5s", got)
	}
	if !e.RunOnStart() {
		t.Error("RunOnStart should be true so cold start gate handles warmup")
	}
}

// TestNewEngine_ExplicitTickSec covers the non-zero TickSec branch.
func TestNewEngine_ExplicitTickSec(t *testing.T) {
	e := NewEngine(Config{TickSec: 10}, nil, nil, nil, nil, nil)
	if e.Interval() != 10*time.Second {
		t.Errorf("Interval after TickSec=10: got %v", e.Interval())
	}
}

// TestNewEngine_ExplicitStartTime covers the non-zero StartTime
// branch — production scheduler bootstrap pins a deterministic time
// so tests can simulate "process started 1h ago" to bypass warmup.
func TestNewEngine_ExplicitStartTime(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e := NewEngine(Config{StartTime: start}, nil, nil, nil, nil, nil)
	if !e.cfg.StartTime.Equal(start) {
		t.Errorf("StartTime: got %v, want %v", e.cfg.StartTime, start)
	}
}

// TestRegister_InstallsAggregatorAndRuntime covers Register: an
// Aggregator becomes a key in both aggregators and runtimes so the
// per-rule Runtime is always co-located. Without this, a rule push
// during Run would race against Runtime construction.
func TestRegister_InstallsAggregatorAndRuntime(t *testing.T) {
	e := NewEngine(Config{}, nil, nil, nil, nil, nil)
	agg := fakeAggregator{id: "test.rule"}
	e.Register(agg)

	if _, ok := e.aggregators["test.rule"]; !ok {
		t.Error("Register: aggregator not installed")
	}
	if _, ok := e.runtimes["test.rule"]; !ok {
		t.Error("Register: per-rule Runtime not installed")
	}
}

// TestSubjectToSource_KnownAndUnknown covers subjectToSource — known
// subjects map back to their EventSource, unknown subjects return
// the zero value + false so handleMQMessage can ignore stray
// subscriptions cleanly.
func TestSubjectToSource_KnownAndUnknown(t *testing.T) {
	for src, sub := range trafficSubjects {
		got, ok := subjectToSource(sub)
		if !ok {
			t.Errorf("subjectToSource(%q) returned ok=false", sub)
			continue
		}
		if got != src {
			t.Errorf("subjectToSource(%q) = %v, want %v", sub, got, src)
		}
	}
	if _, ok := subjectToSource("nexus.event.does-not-exist"); ok {
		t.Error("unknown subject must return ok=false")
	}
}

// TestSnapshotAggregators_CopyIsIndependent covers
// snapshotAggregators — modifying the returned slice must not
// affect Engine.aggregators (the original use case: Run iterates
// while a concurrent Register inserts).
func TestSnapshotAggregators_CopyIsIndependent(t *testing.T) {
	e := NewEngine(Config{}, nil, nil, nil, nil, nil)
	e.Register(fakeAggregator{id: "a"})
	e.Register(fakeAggregator{id: "b"})

	snap := e.snapshotAggregators()
	if len(snap) != 2 {
		t.Fatalf("snapshot len: got %d, want 2", len(snap))
	}
	// Mutating the snapshot's backing array must not poison the engine —
	// snapshotAggregators must hand back an independent slice. Capture the
	// original RuleID at the same position so we can detect element bleed.
	origID := snap[0].RuleID()
	snap[0] = fakeAggregator{id: "mutated"}
	if _, stillRegistered := e.aggregators[origID]; !stillRegistered {
		t.Errorf("snapshot element mutation removed %q from engine.aggregators", origID)
	}
	if _, leaked := e.aggregators["mutated"]; leaked {
		t.Errorf("snapshot element mutation leaked %q into engine.aggregators", "mutated")
	}
	if len(e.aggregators) != 2 {
		t.Errorf("snapshot mutation leaked into engine.aggregators: %d", len(e.aggregators))
	}
}

// TestCollectSubjectsLocked_DedupesAcrossAggregators covers the
// subject-set collection: two aggregators sharing a Source must
// produce ONE MQ subscription, not two (otherwise Hub fan-out would
// duplicate events to the same consumer group).
func TestCollectSubjectsLocked_DedupesAcrossAggregators(t *testing.T) {
	e := NewEngine(Config{}, nil, nil, nil, nil, nil)

	type multiSourceAgg struct {
		fakeAggregator
		srcs []EventSource
	}
	// Aggregator that overrides Sources() — we use the embedded
	// fakeAggregator for the rest.
	srcA := multiSourceAgg{
		fakeAggregator: fakeAggregator{id: "ax"},
		srcs:           []EventSource{SourceAITraffic},
	}
	srcB := multiSourceAgg{
		fakeAggregator: fakeAggregator{id: "bx"},
		srcs:           []EventSource{SourceAITraffic, SourceCompliance},
	}

	// We need to drive collectSubjectsLocked via real Aggregators;
	// the fakeAggregator returns nil Sources by default. Use a
	// wrapper struct that overrides via interface satisfaction.
	e.Register(sourcesAdapter{base: srcA.fakeAggregator, sources: srcA.srcs})
	e.Register(sourcesAdapter{base: srcB.fakeAggregator, sources: srcB.srcs})

	e.mu.Lock()
	subjects := e.collectSubjectsLocked()
	e.mu.Unlock()

	if _, ok := subjects[trafficSubjects[SourceAITraffic]]; !ok {
		t.Error("ai-traffic subject missing")
	}
	if _, ok := subjects[trafficSubjects[SourceCompliance]]; !ok {
		t.Error("compliance subject missing")
	}
	if len(subjects) != 2 {
		t.Errorf("expected 2 distinct subjects (deduped); got %d (%+v)", len(subjects), subjects)
	}
}

// TestDecodeEvent_TrafficVsAudit covers decodeEvent's two-format
// branch: admin-audit subjects decode as AdminAuditMessage, all
// other subjects as TrafficEventMessage. A regression that mixed
// the two would silently misroute audit events into the traffic
// pipeline.
func TestDecodeEvent_TrafficVsAudit(t *testing.T) {
	// Admin audit event.
	auditJSON := []byte(`{"event_id":"e1","ts":"2026-05-16T00:00:00Z","action":"x"}`)
	got, err := decodeEvent(SourceAdminAudit, auditJSON)
	if err != nil {
		t.Fatalf("audit decode: %v", err)
	}
	if got.Kind != EventAudit {
		t.Errorf("audit decode: Kind = %v, want EventAudit", got.Kind)
	}
	if got.Audit == nil {
		t.Error("audit decode: Audit field nil")
	}

	// Traffic event.
	trafficJSON := []byte(`{"event_id":"t1","ts":"2026-05-16T00:00:00Z","virtual_key_id":"vk-1"}`)
	got, err = decodeEvent(SourceAITraffic, trafficJSON)
	if err != nil {
		t.Fatalf("traffic decode: %v", err)
	}
	if got.Kind != EventTraffic {
		t.Errorf("traffic decode: Kind = %v, want EventTraffic", got.Kind)
	}
	if got.Traffic == nil {
		t.Error("traffic decode: Traffic field nil")
	}
}

// TestDecodeEvent_MalformedSurfacesErr covers the unmarshal error
// branches in decodeEvent — garbage in must surface as err, not a
// half-initialized Event.
func TestDecodeEvent_MalformedSurfacesErr(t *testing.T) {
	if _, err := decodeEvent(SourceAdminAudit, []byte(`not json`)); err == nil {
		t.Error("audit decode of garbage must error")
	}
	if _, err := decodeEvent(SourceAITraffic, []byte(`not json`)); err == nil {
		t.Error("traffic decode of garbage must error")
	}
}

// TestAggMatchesSource_HitAndMiss covers aggMatchesSource — the
// Engine uses this to skip routing an event to an Aggregator that
// doesn't subscribe to that EventSource.
func TestAggMatchesSource_HitAndMiss(t *testing.T) {
	agg := sourcesAdapter{
		base:    fakeAggregator{id: "x"},
		sources: []EventSource{SourceAITraffic, SourceCompliance},
	}
	if !aggMatchesSource(agg, SourceAITraffic) {
		t.Error("ai-traffic must match")
	}
	if !aggMatchesSource(agg, SourceCompliance) {
		t.Error("compliance must match")
	}
	if aggMatchesSource(agg, SourceAgent) {
		t.Error("agent must NOT match this aggregator")
	}
	// No-source aggregator never matches.
	if aggMatchesSource(fakeAggregator{id: "y"}, SourceAITraffic) {
		t.Error("aggregator with no Sources should not match anything")
	}
}

// sourcesAdapter lets the dedupe test inject a custom Sources() list
// without forcing every fakeAggregator caller to carry that field.
type sourcesAdapter struct {
	base    fakeAggregator
	sources []EventSource
}

func (s sourcesAdapter) RuleID() string                    { return s.base.RuleID() }
func (s sourcesAdapter) Sources() []EventSource            { return s.sources }
func (s sourcesAdapter) OnEvent(rt *Runtime, evt *Event)   { s.base.OnEvent(rt, evt) }
func (s sourcesAdapter) MinWarmupSec(p map[string]any) int { return s.base.MinWarmupSec(p) }
func (s sourcesAdapter) Tick(rt *Runtime, p map[string]any, now time.Time) []Decision {
	return s.base.Tick(rt, p, now)
}
