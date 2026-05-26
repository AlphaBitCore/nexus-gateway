package killswitch

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

func TestNew_Defaults(t *testing.T) {
	s := New(nil)
	if s.IsEngaged() {
		t.Fatalf("default should be engaged=false (bump allowed); got true")
	}
	if got := s.SnapshotState().ChangedBy; got != "init" {
		t.Errorf("default changedBy = %q, want %q", got, "init")
	}
}

func TestToggle_RoundTrip(t *testing.T) {
	s := New(nil)
	snap := s.Toggle(true, "admin@nexus.ai")
	if !snap.Engaged {
		t.Fatalf("snap.Engaged = false, want true")
	}
	if !s.IsEngaged() {
		t.Fatalf("IsEngaged should be true after engaging the switch")
	}
	if snap.ChangedBy != "admin@nexus.ai" {
		t.Errorf("changedBy = %q, want admin@nexus.ai", snap.ChangedBy)
	}

	s.Toggle(false, "")
	if s.IsEngaged() {
		t.Fatalf("IsEngaged should be false after disengaging")
	}
	if got := s.SnapshotState().ChangedBy; got != "api" {
		t.Errorf("empty changedBy should default to %q, got %q", "api", got)
	}
}

func TestApplyShadowState_NoOpOnEmpty(t *testing.T) {
	s := New(nil)
	for _, raw := range [][]byte{nil, json.RawMessage(`null`), json.RawMessage{}} {
		if err := s.ApplyShadowState(context.Background(), raw); err != nil {
			t.Fatalf("empty/null should be no-op, got error: %v", err)
		}
	}
	if s.IsEngaged() {
		t.Errorf("empty payload should not flip the switch off-default")
	}
}

func TestApplyShadowState_DecodeAndToggle(t *testing.T) {
	s := New(nil)
	if err := s.ApplyShadowState(context.Background(), json.RawMessage(`{"engaged":true}`)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !s.IsEngaged() {
		t.Fatalf("expected switch engaged after applying engaged=true")
	}

	if err := s.ApplyShadowState(context.Background(), json.RawMessage(`{"engaged":false}`)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if s.IsEngaged() {
		t.Fatalf("expected switch disengaged after applying engaged=false")
	}
}

func TestApplyShadowState_RedundantNoLog(t *testing.T) {
	s := New(nil)
	// Default is engaged=false. Re-applying the same value must not record
	// a Toggle (verified indirectly: changedBy stays "init").
	if err := s.ApplyShadowState(context.Background(), json.RawMessage(`{"engaged":false}`)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := s.SnapshotState().ChangedBy; got != "init" {
		t.Errorf("redundant apply should not record a Toggle, changedBy = %q", got)
	}
}

func TestApplyShadowState_Malformed(t *testing.T) {
	s := New(nil)
	if err := s.ApplyShadowState(context.Background(), json.RawMessage(`not-json`)); err == nil {
		t.Errorf("expected error on malformed payload")
	}
}

func TestConcurrentToggleAndIsEngaged(t *testing.T) {
	s := New(nil)
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				s.Toggle(i%4 == 0, "race")
			} else {
				_ = s.IsEngaged()
			}
		}(i)
	}
	wg.Wait()
}
