package login

import (
	"testing"
	"time"
)

func TestLimiter_AllowsUnderBudget(t *testing.T) {
	clock := time.Unix(1_000_000_000, 0)
	l := newLimiterWith(time.Minute, 3, func() time.Time { return clock })

	for i := range 3 {
		if !l.Allow("1.1.1.1", "a@x") {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
}

func TestLimiter_BlocksWhenBudgetExceeded(t *testing.T) {
	clock := time.Unix(1_000_000_000, 0)
	l := newLimiterWith(time.Minute, 3, func() time.Time { return clock })

	for i := range 3 {
		if !l.Allow("1.1.1.1", "a@x") {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
	if l.Allow("1.1.1.1", "a@x") {
		t.Fatal("4th attempt in window must be denied")
	}
}

func TestLimiter_WindowRolloff(t *testing.T) {
	clock := time.Unix(1_000_000_000, 0)
	l := newLimiterWith(time.Minute, 2, func() time.Time { return clock })

	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("first attempt")
	}
	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("second attempt")
	}
	if l.Allow("1.1.1.1", "a@x") {
		t.Fatal("third attempt must be denied inside window")
	}

	// Advance past the window — earlier attempts should age out.
	clock = clock.Add(2 * time.Minute)
	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("attempt after window rolloff should be allowed")
	}
}

func TestLimiter_KeysAreIsolated(t *testing.T) {
	clock := time.Unix(1_000_000_000, 0)
	l := newLimiterWith(time.Minute, 1, func() time.Time { return clock })

	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("a@x first attempt")
	}
	if l.Allow("1.1.1.1", "a@x") {
		t.Fatal("a@x second attempt must be denied")
	}
	// Different email from same IP: separate bucket.
	if !l.Allow("1.1.1.1", "b@x") {
		t.Fatal("b@x first attempt should be allowed")
	}
	// Different IP for same email: also separate bucket.
	if !l.Allow("2.2.2.2", "a@x") {
		t.Fatal("a@x from new IP should be allowed")
	}
}

func TestLimiter_DeniedAttemptsDoNotConsumeBudget(t *testing.T) {
	clock := time.Unix(1_000_000_000, 0)
	l := newLimiterWith(time.Minute, 2, func() time.Time { return clock })

	l.Allow("1.1.1.1", "a@x")
	l.Allow("1.1.1.1", "a@x")
	// Attacker hammers the endpoint while blocked.
	for range 20 {
		if l.Allow("1.1.1.1", "a@x") {
			t.Fatal("denied attempt leaked through")
		}
	}
	// Advance just past the original two attempts' window.
	clock = clock.Add(time.Minute + time.Second)
	// Budget must be fully restored: the blocked attempts did NOT record timestamps.
	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("expected budget restored after original window")
	}
	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("second post-rolloff attempt should also be allowed")
	}
}

func TestLimiter_EmailCaseInsensitive(t *testing.T) {
	clock := time.Unix(1_000_000_000, 0)
	l := newLimiterWith(time.Minute, 1, func() time.Time { return clock })

	if !l.Allow("1.1.1.1", "Alice@Corp.com") {
		t.Fatal("first attempt")
	}
	// Same key when normalized — must be denied.
	if l.Allow("1.1.1.1", "alice@corp.com") {
		t.Fatal("case variant must hit same bucket")
	}
}

func TestLimiter_DefaultConstructor(t *testing.T) {
	// Sanity check: NewLimiter uses the module defaults and does not panic.
	l := NewLimiter()
	if !l.Allow("1.1.1.1", "a@x") {
		t.Fatal("NewLimiter should start with budget > 0")
	}
}
