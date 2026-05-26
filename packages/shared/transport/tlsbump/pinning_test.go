package tlsbump

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func defaultTestConfig() PinningConfig {
	return PinningConfig{
		Exemptions: []DomainExemption{
			{Host: "pinned.example.com", Reason: "known pinned service"},
			{Host: "*.apple.com", Reason: "Apple certificate transparency"},
		},
		AutoExempt: AutoExemptConfig{
			Enabled:           true,
			FailureThreshold:  3,
			WindowSeconds:     60,
			ExemptionDuration: 10 * time.Minute,
		},
	}
}

func TestPinningTracker_ConfiguredExemption(t *testing.T) {
	tracker := NewPinningTracker(defaultTestConfig())

	exempt, reason, status := tracker.IsExempt("pinned.example.com")
	if !exempt {
		t.Fatal("expected pinned.example.com to be exempt")
	}
	if reason != "known pinned service" {
		t.Errorf("unexpected reason: %s", reason)
	}
	if status != BumpStatusExemptConfigured {
		t.Errorf("unexpected status: %s", status)
	}

	// Case insensitivity.
	exempt, _, _ = tracker.IsExempt("Pinned.Example.Com")
	if !exempt {
		t.Fatal("expected case-insensitive match")
	}

	// Host with port should also match.
	exempt, _, _ = tracker.IsExempt("pinned.example.com:443")
	if !exempt {
		t.Fatal("expected host:port to match")
	}

	// Non-exempt host.
	exempt, _, _ = tracker.IsExempt("other.example.com")
	if exempt {
		t.Fatal("expected other.example.com to NOT be exempt")
	}
}

func TestPinningTracker_WildcardExemption(t *testing.T) {
	tracker := NewPinningTracker(defaultTestConfig())

	tests := []struct {
		host   string
		exempt bool
	}{
		{"icloud.apple.com", true},
		{"push.apple.com", true},
		{"PUSH.Apple.Com", true},
		{"apple.com", false},            // wildcard *.apple.com does not match bare domain
		{"sub.icloud.apple.com", false}, // *.apple.com matches only one level
	}

	for _, tt := range tests {
		exempt, _, _ := tracker.IsExempt(tt.host)
		if exempt != tt.exempt {
			t.Errorf("IsExempt(%q) = %v, want %v", tt.host, exempt, tt.exempt)
		}
	}
}

func TestPinningTracker_AutoExemption(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tracker := NewPinningTracker(defaultTestConfig())
	tracker.nowFunc = func() time.Time { return now }

	host := "strict-pinned.example.com"

	// First two failures: not yet auto-exempted.
	status := tracker.RecordFailure(host)
	if status != BumpStatusFailedPassthrough {
		t.Errorf("after 1 failure: got %s, want %s", status, BumpStatusFailedPassthrough)
	}
	status = tracker.RecordFailure(host)
	if status != BumpStatusFailedPassthrough {
		t.Errorf("after 2 failures: got %s, want %s", status, BumpStatusFailedPassthrough)
	}

	// Third failure triggers auto-exemption.
	status = tracker.RecordFailure(host)
	if status != BumpStatusExemptPinned {
		t.Errorf("after 3 failures: got %s, want %s", status, BumpStatusExemptPinned)
	}

	// Now IsExempt should return true.
	exempt, reason, bumpStatus := tracker.IsExempt(host)
	if !exempt {
		t.Fatal("expected auto-exemption after threshold")
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
	if bumpStatus != BumpStatusExemptPinned {
		t.Errorf("unexpected bump status: %s", bumpStatus)
	}
}

func TestPinningTracker_AutoExemptExpiry(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tracker := NewPinningTracker(defaultTestConfig())
	tracker.nowFunc = func() time.Time { return now }

	host := "expiring.example.com"

	// Trigger auto-exemption.
	tracker.RecordFailure(host)
	tracker.RecordFailure(host)
	tracker.RecordFailure(host)

	// Verify exempted.
	exempt, _, _ := tracker.IsExempt(host)
	if !exempt {
		t.Fatal("expected exempt after 3 failures")
	}

	// Advance time past the exemption duration (10 minutes).
	now = now.Add(11 * time.Minute)
	tracker.nowFunc = func() time.Time { return now }

	exempt, _, _ = tracker.IsExempt(host)
	if exempt {
		t.Fatal("expected exemption to have expired")
	}
}

func TestPinningTracker_WindowPrune(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tracker := NewPinningTracker(defaultTestConfig())
	tracker.nowFunc = func() time.Time { return now }

	host := "window-test.example.com"

	// Record 2 failures at time T.
	tracker.RecordFailure(host)
	tracker.RecordFailure(host)

	// Advance time beyond the 60-second window.
	now = now.Add(61 * time.Second)
	tracker.nowFunc = func() time.Time { return now }

	// This is only the 1st failure within the new window.
	status := tracker.RecordFailure(host)
	if status != BumpStatusFailedPassthrough {
		t.Errorf("expected old failures to be pruned, got %s", status)
	}

	// 2nd failure in new window.
	status = tracker.RecordFailure(host)
	if status != BumpStatusFailedPassthrough {
		t.Errorf("expected 2 failures in window, got %s", status)
	}

	// 3rd failure in new window triggers auto-exemption.
	status = tracker.RecordFailure(host)
	if status != BumpStatusExemptPinned {
		t.Errorf("expected auto-exemption at threshold, got %s", status)
	}
}

func TestIsPinningError(t *testing.T) {
	tests := []struct {
		err      error
		isPinned bool
	}{
		{nil, false},
		{errors.New("connection reset"), false},
		{errors.New("read timeout"), false},
		{errors.New("remote error: tls: bad certificate"), true},
		{errors.New("x509: certificate signed by unknown authority"), true},
		{errors.New("remote error: tls: certificate unknown"), true},
		{errors.New("remote error: tls: access denied"), true},
		{errors.New("tls: alert(48): unknown certificate authority"), true},
		{errors.New("certificate required"), true},
		{errors.New("TLS: ALERT received bad certificate"), true}, // case insensitivity
	}

	for _, tt := range tests {
		got := IsPinningError(tt.err)
		if got != tt.isPinned {
			errMsg := "<nil>"
			if tt.err != nil {
				errMsg = tt.err.Error()
			}
			t.Errorf("IsPinningError(%q) = %v, want %v", errMsg, got, tt.isPinned)
		}
	}
}

func TestPinningTracker_Concurrent(t *testing.T) {
	tracker := NewPinningTracker(defaultTestConfig())

	var wg sync.WaitGroup
	hosts := []string{"a.example.com", "b.example.com", "c.example.com"}

	// Run concurrent reads and writes.
	for i := range 100 {
		wg.Add(2)
		host := hosts[i%len(hosts)]

		go func(h string) {
			defer wg.Done()
			tracker.RecordFailure(h)
		}(host)

		go func(h string) {
			defer wg.Done()
			tracker.IsExempt(h)
		}(host)
	}

	wg.Wait()

	// If we get here without a race detector panic, concurrency is safe.
	// Additionally verify that at least some hosts got auto-exempted
	// (100/3 ≈ 33 failures per host, well above threshold of 3).
	for _, h := range hosts {
		exempt, _, _ := tracker.IsExempt(h)
		if !exempt {
			t.Errorf("expected %s to be auto-exempted after many failures", h)
		}
	}
}

func TestPinningTracker_AutoExemptDisabled(t *testing.T) {
	cfg := PinningConfig{
		AutoExempt: AutoExemptConfig{
			Enabled:          false,
			FailureThreshold: 3,
			WindowSeconds:    60,
		},
	}
	tracker := NewPinningTracker(cfg)

	host := "disabled.example.com"
	for i := range 10 {
		status := tracker.RecordFailure(host)
		if status != BumpStatusFailedPassthrough {
			t.Errorf("iteration %d: got %s, want %s (auto-exempt disabled)", i, status, BumpStatusFailedPassthrough)
		}
	}

	exempt, _, _ := tracker.IsExempt(host)
	if exempt {
		t.Fatal("should not be auto-exempted when auto-exempt is disabled")
	}
}
