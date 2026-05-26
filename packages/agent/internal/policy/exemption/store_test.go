package exemption

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestNewStoreDefaults(t *testing.T) {
	s := NewStore(Config{Enabled: true})
	if s.cfg.FailureThreshold != 3 || s.cfg.WindowSeconds != 60 || s.cfg.ExemptionDurationSec != 86400 {
		t.Fatalf("expected defaults applied; got %+v", s.cfg)
	}
}

func TestAddAndIsExempt(t *testing.T) {
	s := NewStore(DefaultConfig())
	s.Add("api.example.com", "test", SourceAdmin, time.Hour)
	exempt, reason := s.IsExempt("api.example.com")
	if !exempt || reason != "test" {
		t.Fatalf("expected exempt=true reason=test; got exempt=%v reason=%q", exempt, reason)
	}
}

func TestRemove(t *testing.T) {
	s := NewStore(DefaultConfig())
	s.Add("foo.com", "x", SourceAdmin, time.Hour)
	s.Remove("foo.com")
	if exempt, _ := s.IsExempt("foo.com"); exempt {
		t.Fatal("expected exempt=false after remove")
	}
}

func TestWildcardMatch(t *testing.T) {
	s := NewStore(DefaultConfig())
	s.Add("*.example.com", "wildcard", SourceAdmin, time.Hour)
	if exempt, _ := s.IsExempt("api.example.com"); !exempt {
		t.Fatal("expected wildcard match for api.example.com")
	}
	if exempt, _ := s.IsExempt("deep.sub.example.com"); !exempt {
		t.Fatal("expected wildcard match for deep.sub.example.com")
	}
	if exempt, _ := s.IsExempt("example.com"); exempt {
		t.Fatal("wildcard *.example.com should not match bare example.com")
	}
}

func TestRecordFailureBelowThreshold(t *testing.T) {
	s := NewStore(Config{Enabled: true, FailureThreshold: 3, WindowSeconds: 60, ExemptionDurationSec: 3600})
	// 2 failures should NOT exempt
	for range 2 {
		s.RecordFailure("flaky.com")
	}
	if exempt, _ := s.IsExempt("flaky.com"); exempt {
		t.Fatal("expected NOT exempt below threshold")
	}
}

func TestRecordFailureExceedsThreshold(t *testing.T) {
	s := NewStore(Config{Enabled: true, FailureThreshold: 3, WindowSeconds: 60, ExemptionDurationSec: 3600})
	var lastExempted bool
	for i := range 3 {
		exempted, _ := s.RecordFailure("pinned.com")
		if i == 2 {
			lastExempted = exempted
		}
	}
	if !lastExempted {
		t.Fatal("expected auto-exempted on 3rd failure")
	}
	if exempt, _ := s.IsExempt("pinned.com"); !exempt {
		t.Fatal("expected pinned.com to be exempt")
	}
}

func TestDenylistBlocksAutoExempt(t *testing.T) {
	s := NewStore(Config{Enabled: true, FailureThreshold: 2, WindowSeconds: 60, ExemptionDurationSec: 3600})
	s.SetDenylist([]string{"bank.com"})
	for range 5 {
		s.RecordFailure("bank.com")
	}
	if exempt, _ := s.IsExempt("bank.com"); exempt {
		t.Fatal("denylisted host should NEVER be auto-exempted")
	}
}

func TestDisabledNoOp(t *testing.T) {
	s := NewStore(Config{Enabled: false, FailureThreshold: 1})
	s.RecordFailure("foo.com")
	if exempt, _ := s.IsExempt("foo.com"); exempt {
		t.Fatal("disabled store should not exempt")
	}
}

func TestCleanup(t *testing.T) {
	s := NewStore(DefaultConfig())
	s.Add("expired.com", "old", SourceAuto, -time.Hour) // already expired
	s.Add("active.com", "active", SourceAdmin, time.Hour)
	removed := s.Cleanup()
	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}
	if exempt, _ := s.IsExempt("expired.com"); exempt {
		t.Fatal("expired entry should be gone")
	}
	if exempt, _ := s.IsExempt("active.com"); !exempt {
		t.Fatal("active entry should remain")
	}
}

func TestSetAllowlistPreservesAuto(t *testing.T) {
	s := NewStore(Config{Enabled: true, FailureThreshold: 1, WindowSeconds: 60, ExemptionDurationSec: 3600})
	// Auto-exempt one host
	s.RecordFailure("auto.com")
	// Replace allowlist
	s.SetAllowlist([]string{"admin1.com", "admin2.com"})
	// Auto entry should still exist
	if exempt, _ := s.IsExempt("auto.com"); !exempt {
		t.Fatal("auto entry should be preserved after SetAllowlist")
	}
	// Admin entries present
	if exempt, _ := s.IsExempt("admin1.com"); !exempt {
		t.Fatal("expected admin1.com to be exempt")
	}
}

func TestPendingAutoExemptions(t *testing.T) {
	s := NewStore(Config{Enabled: true, FailureThreshold: 1, WindowSeconds: 60, ExemptionDurationSec: 3600})
	s.RecordFailure("a.com")
	s.Add("admin.com", "manual", SourceAdmin, time.Hour)
	pending := s.PendingAutoExemptions()
	if len(pending) != 1 || pending[0].Host != "a.com" {
		t.Fatalf("expected 1 auto entry (a.com); got %+v", pending)
	}
}

func TestApplyShadowState(t *testing.T) {
	tests := []struct {
		name             string
		raw              string
		wantAllowExempt  []string // hosts that should IsExempt => true after apply
		wantDenylistHits []string // hosts that should be denied
		wantErr          bool
		leaveAllowlistAs []string // seed allowlist before apply (to assert no-ops preserve state)
	}{
		{
			name:             "happy path — admin + denylist populated",
			raw:              `{"auto_exempt_cert_pinned":true,"admin_exemptions":["a.com","b.com"],"denylist":["bank.com"]}`,
			wantAllowExempt:  []string{"a.com", "b.com"},
			wantDenylistHits: []string{"bank.com"},
		},
		{
			name:             "empty raw is no-op — keeps yaml defaults",
			raw:              "",
			leaveAllowlistAs: []string{"yaml-default.com"},
			wantAllowExempt:  []string{"yaml-default.com"},
		},
		{
			name:             "null is no-op",
			raw:              "null",
			leaveAllowlistAs: []string{"yaml-default.com"},
			wantAllowExempt:  []string{"yaml-default.com"},
		},
		{
			name:             "empty object is no-op",
			raw:              "{}",
			leaveAllowlistAs: []string{"yaml-default.com"},
			wantAllowExempt:  []string{"yaml-default.com"},
		},
		{
			name:    "malformed json errors",
			raw:     `{"admin_exemptions":[`,
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore(DefaultConfig())
			if len(tc.leaveAllowlistAs) > 0 {
				s.SetAllowlist(tc.leaveAllowlistAs)
			}
			err := s.ApplyShadowState(context.Background(), json.RawMessage(tc.raw))
			if (err != nil) != tc.wantErr {
				t.Fatalf("ApplyShadowState err=%v wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			for _, host := range tc.wantAllowExempt {
				if exempt, _ := s.IsExempt(host); !exempt {
					t.Errorf("expected %s to be exempt", host)
				}
			}
			for _, host := range tc.wantDenylistHits {
				if exempt, _ := s.IsExempt(host); exempt {
					t.Errorf("expected %s to be denied (not exempt)", host)
				}
			}
		})
	}
}

func TestApplyShadowState_ReplacesAllowlist(t *testing.T) {
	s := NewStore(DefaultConfig())
	s.SetAllowlist([]string{"old.com"})
	raw := json.RawMessage(`{"admin_exemptions":["new.com"],"denylist":[]}`)
	if err := s.ApplyShadowState(context.Background(), raw); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if exempt, _ := s.IsExempt("old.com"); exempt {
		t.Error("old.com should be removed after shadow apply")
	}
	if exempt, _ := s.IsExempt("new.com"); !exempt {
		t.Error("new.com should be exempt after shadow apply")
	}
}

// TestSetConfig_AppliesZeroDefaults covers SetConfig — when a Hub
// shadow push lands with zero FailureThreshold / WindowSeconds /
// ExemptionDurationSec, SetConfig must apply the same defaults the
// constructor does so the runtime stays safe regardless of how the
// new config arrived.
func TestSetConfig_AppliesZeroDefaults(t *testing.T) {
	s := NewStore(Config{Enabled: true})
	s.SetConfig(Config{Enabled: true}) // all numeric fields zero
	if s.cfg.FailureThreshold != 3 {
		t.Errorf("FailureThreshold = %d, want 3 default", s.cfg.FailureThreshold)
	}
	if s.cfg.WindowSeconds != 60 {
		t.Errorf("WindowSeconds = %d, want 60 default", s.cfg.WindowSeconds)
	}
	if s.cfg.ExemptionDurationSec != 86400 {
		t.Errorf("ExemptionDurationSec = %d, want 86400 default", s.cfg.ExemptionDurationSec)
	}
}

// TestSetConfig_KeepsExplicitValues covers the non-zero branches of
// SetConfig — explicit operator values must NOT be overwritten by
// the defaults block.
func TestSetConfig_KeepsExplicitValues(t *testing.T) {
	s := NewStore(Config{Enabled: true})
	s.SetConfig(Config{
		Enabled:              true,
		FailureThreshold:     5,
		WindowSeconds:        30,
		ExemptionDurationSec: 3600,
	})
	if s.cfg.FailureThreshold != 5 || s.cfg.WindowSeconds != 30 || s.cfg.ExemptionDurationSec != 3600 {
		t.Errorf("explicit values overwritten: %+v", s.cfg)
	}
}

// TestList_ReturnsCurrentEntries covers List(). Without this the
// admin /agent-status endpoint that surfaces exemptions to operators
// has no unit-level pin against a future refactor that drops entries.
func TestList_ReturnsCurrentEntries(t *testing.T) {
	s := NewStore(DefaultConfig())
	s.Add("a.example.com", "manual", SourceAdmin, 0)
	s.Add("b.example.com", "manual", SourceAdmin, 0)

	got := s.List()
	if len(got) != 2 {
		t.Fatalf("List len: got %d, want 2", len(got))
	}
	seen := map[string]bool{}
	for _, e := range got {
		seen[e.Host] = true
	}
	if !seen["a.example.com"] || !seen["b.example.com"] {
		t.Errorf("List missing hosts: %+v", got)
	}
}

// TestRunCleanupLoop_RemovesExpiredOnTick covers the loop body —
// after one tick, an entry whose ExpiresAt has passed must be
// removed. Without this, the cleanup goroutine has no behavioral
// pin and a future refactor could quietly break expiry sweeping.
func TestRunCleanupLoop_RemovesExpiredOnTick(t *testing.T) {
	s := NewStore(DefaultConfig())
	// Add an entry that's already expired.
	s.Add("expired.example.com", "test", SourceAuto, -time.Hour)
	if got := len(s.List()); got != 1 {
		t.Fatalf("setup: want 1 entry, got %d", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.RunCleanupLoop(ctx, 10*time.Millisecond)
		close(done)
	}()

	// Allow at least one tick to fire.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(s.List()) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(s.List()); got != 0 {
		t.Errorf("expired entry should be cleaned up; got %d remaining", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("RunCleanupLoop did not exit on ctx.Done")
	}
}

// TestPendingAutoExemptions_ReturnsOnlyAutoSource covers the
// PendingAutoExemptions filter — admin-source entries must NOT
// surface to the upload pipeline (Hub already knows about them).
func TestPendingAutoExemptions_ReturnsOnlyAutoSource(t *testing.T) {
	s := NewStore(DefaultConfig())
	s.Add("admin.example.com", "admin", SourceAdmin, 0)
	s.Add("auto.example.com", "auto", SourceAuto, time.Hour)

	got := s.PendingAutoExemptions()
	if len(got) != 1 {
		t.Fatalf("PendingAutoExemptions len: got %d, want 1", len(got))
	}
	if got[0].Host != "auto.example.com" {
		t.Errorf("expected auto.example.com, got %q", got[0].Host)
	}
}
