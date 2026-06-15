package drift

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// fakeStaleStore records MarkStaleOffline calls and returns scripted results.
type fakeStaleStore struct {
	calls   []staleCall
	returns []staleResult
}

type staleCall struct {
	types     []string
	threshold time.Duration
}

type staleResult struct {
	n   int64
	err error
}

func (f *fakeStaleStore) MarkStaleOffline(_ context.Context, types []string, threshold time.Duration) (int64, error) {
	// Copy the slice so later mutations to the caller's slice don't corrupt our record.
	typesCopy := append([]string(nil), types...)
	f.calls = append(f.calls, staleCall{types: typesCopy, threshold: threshold})
	if len(f.calls) > len(f.returns) {
		return 0, nil
	}
	r := f.returns[len(f.calls)-1]
	return r.n, r.err
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestStaleThingJob_DefaultThresholds(t *testing.T) {
	j := NewStaleThingJob(&fakeStaleStore{}, time.Second, testLogger(), StaleThingConfig{})
	if j.cfg.AgentThreshold != 5*time.Minute {
		t.Errorf("AgentThreshold default = %v, want 5m", j.cfg.AgentThreshold)
	}
	// F-0209: default must be >=2x the 30s ping interval to avoid false-offline
	// flap from a single jittered/missed ping.
	if j.cfg.ServiceThreshold != 90*time.Second {
		t.Errorf("ServiceThreshold default = %v, want 90s", j.cfg.ServiceThreshold)
	}
	if j.cfg.ServiceThreshold < 2*30*time.Second {
		t.Errorf("ServiceThreshold default %v < 2x pingInterval (60s) — flap risk", j.cfg.ServiceThreshold)
	}
}

func TestStaleThingJob_NegativeThresholdsUseDefaults(t *testing.T) {
	j := NewStaleThingJob(&fakeStaleStore{}, time.Second, testLogger(), StaleThingConfig{
		AgentThreshold:   -1 * time.Second,
		ServiceThreshold: 0,
	})
	if j.cfg.AgentThreshold != 5*time.Minute || j.cfg.ServiceThreshold != 90*time.Second {
		t.Errorf("defaults not applied: %+v", j.cfg)
	}
}

func TestStaleThingJob_IdentityAndInterval(t *testing.T) {
	j := NewStaleThingJob(&fakeStaleStore{}, 45*time.Second, testLogger(), StaleThingConfig{})
	if j.ID() != "stale-thing-sweep" {
		t.Errorf("ID = %q, want stale-thing-sweep", j.ID())
	}
	if j.Name() == "" {
		t.Error("Name must not be empty")
	}
	if j.Description() == "" {
		t.Error("Description must not be empty")
	}
	if j.Interval() != 45*time.Second {
		t.Errorf("Interval = %v, want 45s", j.Interval())
	}
}

func TestStaleThingJob_Run_CallsStoreTwice(t *testing.T) {
	fake := &fakeStaleStore{returns: []staleResult{{n: 2}, {n: 5}}}
	j := NewStaleThingJob(fake, time.Second, testLogger(), StaleThingConfig{
		AgentThreshold:   7 * time.Minute,
		ServiceThreshold: 45 * time.Second,
	})
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("call count = %d, want 2", len(fake.calls))
	}

	agentCall := fake.calls[0]
	if len(agentCall.types) != 1 || agentCall.types[0] != "agent" {
		t.Errorf("agent call types = %v, want [agent]", agentCall.types)
	}
	if agentCall.threshold != 7*time.Minute {
		t.Errorf("agent threshold = %v, want 7m", agentCall.threshold)
	}

	svcCall := fake.calls[1]
	wantSvcTypes := map[string]bool{"control-plane": true, "ai-gateway": true, "compliance-proxy": true}
	if len(svcCall.types) != len(wantSvcTypes) {
		t.Errorf("service call types len = %d, want %d (%v)", len(svcCall.types), len(wantSvcTypes), svcCall.types)
	}
	for _, ty := range svcCall.types {
		if !wantSvcTypes[ty] {
			t.Errorf("unexpected service type %q", ty)
		}
	}
	if svcCall.threshold != 45*time.Second {
		t.Errorf("service threshold = %v, want 45s", svcCall.threshold)
	}
}

func TestStaleThingJob_Run_AgentErrorShortCircuits(t *testing.T) {
	sentinel := errors.New("boom")
	fake := &fakeStaleStore{returns: []staleResult{{err: sentinel}}}
	j := NewStaleThingJob(fake, time.Second, testLogger(), StaleThingConfig{})
	err := j.Run(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped sentinel", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("should have stopped after first failure, got %d calls", len(fake.calls))
	}
}

func TestStaleThingJob_Run_ServiceErrorPropagates(t *testing.T) {
	sentinel := errors.New("kaboom")
	fake := &fakeStaleStore{returns: []staleResult{{n: 0}, {err: sentinel}}}
	j := NewStaleThingJob(fake, time.Second, testLogger(), StaleThingConfig{})
	err := j.Run(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped sentinel", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(fake.calls))
	}
}

func TestStaleThingJob_Run_ZeroCountsDoNotPanic(t *testing.T) {
	fake := &fakeStaleStore{returns: []staleResult{{n: 0}, {n: 0}}}
	j := NewStaleThingJob(fake, time.Second, testLogger(), StaleThingConfig{})
	if err := j.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
