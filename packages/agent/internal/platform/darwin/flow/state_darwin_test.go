//go:build darwin

package flow

import (
	"sync"
	"testing"
)

// TestState_DstHost_EmptyBeforeSet confirms the zero-value State reports
// an empty host (no nil-pointer deref through the atomic).
func TestState_DstHost_EmptyBeforeSet(t *testing.T) {
	var s State
	if got := s.DstHost(); got != "" {
		t.Fatalf("zero-value DstHost() = %q, want empty", got)
	}
}

// TestState_DstHost_SetThenGet confirms a set value round-trips, including
// an overwrite (the flow_update_host SNI rewrite).
func TestState_DstHost_SetThenGet(t *testing.T) {
	var s State
	s.SetDstHost("1.2.3.4")
	if got := s.DstHost(); got != "1.2.3.4" {
		t.Fatalf("DstHost() = %q, want 1.2.3.4", got)
	}
	s.SetDstHost("api.openai.com")
	if got := s.DstHost(); got != "api.openai.com" {
		t.Fatalf("DstHost() after rewrite = %q, want api.openai.com", got)
	}
}

// TestState_DstHost_ConcurrentSetGet exercises the atomic across many
// goroutines — the property the cross-goroutine (reader vs bridge) access
// depends on. Under -race this must be clean and every read must observe
// one of the written values (never a torn/empty result once set).
func TestState_DstHost_ConcurrentSetGet(t *testing.T) {
	var s State
	s.SetDstHost("seed")
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 1000 {
				s.SetDstHost("h")
			}
		}()
		go func() {
			defer wg.Done()
			for range 1000 {
				if s.DstHost() == "" {
					t.Error("DstHost() returned empty after seed")
					return
				}
			}
		}()
	}
	wg.Wait()
}
