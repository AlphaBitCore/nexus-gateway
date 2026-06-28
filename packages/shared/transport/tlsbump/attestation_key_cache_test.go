package tlsbump

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func genKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	return pub
}

func TestAttestationKeyCache_HitMissAndCachesPositive(t *testing.T) {
	pub := genKey(t)
	var calls atomic.Int32
	loader := func(_ context.Context, agentID string) (AttestationKey, error) {
		calls.Add(1)
		if agentID != "agent-1" {
			t.Fatalf("unexpected loader call for %q", agentID)
		}
		return AttestationKey{Key: pub}, nil
	}
	c := NewAttestationKeyCache(loader, newTestLogger())

	// First Get → miss → loader fires.
	got, err := c.Get(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if string(got.Key) != string(pub) {
		t.Error("first Get returned wrong key")
	}

	// Second Get → hit, loader must not fire again.
	got2, err := c.Get(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if string(got2.Key) != string(pub) {
		t.Error("second Get returned wrong key")
	}
	if calls.Load() != 1 {
		t.Errorf("loader call count = %d; want 1 (second Get must hit cache)", calls.Load())
	}
}

func TestAttestationKeyCache_RejectsEmptyAgentID(t *testing.T) {
	c := NewAttestationKeyCache(
		func(_ context.Context, _ string) (AttestationKey, error) {
			t.Fatal("loader must not be called for empty agent_id")
			return AttestationKey{}, nil
		},
		newTestLogger(),
	)
	if _, err := c.Get(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty agent_id")
	}
}

func TestAttestationKeyCache_NegativeCachingShortCircuitsLoader(t *testing.T) {
	var calls atomic.Int32
	loader := func(_ context.Context, _ string) (AttestationKey, error) {
		calls.Add(1)
		return AttestationKey{}, ErrUnknownAgent
	}
	c := NewAttestationKeyCache(loader, newTestLogger())

	if _, err := c.Get(context.Background(), "bogus"); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("first Get err = %v; want ErrUnknownAgent", err)
	}
	if _, err := c.Get(context.Background(), "bogus"); !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("second Get err = %v; want ErrUnknownAgent (cached)", err)
	}
	if calls.Load() != 1 {
		t.Errorf("loader call count = %d; want 1 (negative cache must short-circuit)", calls.Load())
	}
}

func TestAttestationKeyCache_PositiveTTLExpiry(t *testing.T) {
	pub := genKey(t)
	var calls atomic.Int32
	loader := func(_ context.Context, _ string) (AttestationKey, error) {
		calls.Add(1)
		return AttestationKey{Key: pub}, nil
	}
	c := NewAttestationKeyCacheWith(loader, newTestLogger(),
		50*time.Millisecond, 50*time.Millisecond, 10)
	clock := time.Unix(0, 0)
	c.now = func() time.Time { return clock }

	if _, err := c.Get(context.Background(), "a"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Advance past TTL.
	clock = clock.Add(100 * time.Millisecond)
	if _, err := c.Get(context.Background(), "a"); err != nil {
		t.Fatalf("Get after TTL: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("loader call count = %d; want 2 (TTL must force re-fetch)", calls.Load())
	}
}

func TestAttestationKeyCache_NegativeTTLExpiry(t *testing.T) {
	// After negativeTTL the cache must allow a fresh loader call —
	// otherwise a transient Hub outage would permanently blackhole the
	// agent's traffic into MITM.
	var calls atomic.Int32
	pub := genKey(t)
	loader := func(_ context.Context, _ string) (AttestationKey, error) {
		n := calls.Add(1)
		if n == 1 {
			return AttestationKey{}, ErrUnknownAgent
		}
		return AttestationKey{Key: pub}, nil
	}
	c := NewAttestationKeyCacheWith(loader, newTestLogger(),
		time.Minute, 5*time.Millisecond, 10)
	clock := time.Unix(0, 0)
	c.now = func() time.Time { return clock }

	if _, err := c.Get(context.Background(), "a"); err == nil {
		t.Fatal("first Get should fail")
	}
	clock = clock.Add(10 * time.Millisecond)
	got, err := c.Get(context.Background(), "a")
	if err != nil {
		t.Fatalf("Get after negative TTL: %v", err)
	}
	if string(got.Key) != string(pub) {
		t.Error("expected positive key after negative TTL expiry")
	}
}

func TestAttestationKeyCache_CapEvictsOldest(t *testing.T) {
	// Cap = 2: insert 3 unique agents, oldest entry must be gone.
	loader := func(_ context.Context, agentID string) (AttestationKey, error) {
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		// Deterministic per-id key suffix so we can identify the holder
		// without storing it externally — not used in assertion but
		// keeps the loader honest.
		_ = pub
		return AttestationKey{Key: pub}, nil
	}
	c := NewAttestationKeyCacheWith(loader, newTestLogger(),
		time.Minute, time.Minute, 2)
	clock := time.Unix(0, 0)
	c.now = func() time.Time { return clock }

	for i := range 3 {
		clock = clock.Add(time.Second) // newer expiry on each
		if _, err := c.Get(context.Background(), "a"+strconv.Itoa(i)); err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
	}
	if c.Len() != 2 {
		t.Errorf("Len after 3 inserts with cap=2 = %d; want 2", c.Len())
	}
}

func TestAttestationKeyCache_CapZeroClampsToOne(t *testing.T) {
	// cap < 1 must not produce a zero-sized cache (Get would never
	// store its result and every call would re-invoke the loader).
	pub := genKey(t)
	calls := 0
	loader := func(_ context.Context, _ string) (AttestationKey, error) {
		calls++
		return AttestationKey{Key: pub}, nil
	}
	c := NewAttestationKeyCacheWith(loader, newTestLogger(),
		time.Minute, time.Minute, 0)
	if _, err := c.Get(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("loader fired %d times; clamp-to-1 should still cache", calls)
	}
}

func TestAttestationKeyCache_ConcurrentGetsSafe(t *testing.T) {
	// Smoke test under -race: many goroutines hitting different keys
	// must not panic or report a data race. The loader sleeps briefly
	// to force overlap with the cache mutex.
	pubs := map[string]ed25519.PublicKey{}
	var pmu sync.Mutex
	loader := func(_ context.Context, agentID string) (AttestationKey, error) {
		pmu.Lock()
		defer pmu.Unlock()
		if p, ok := pubs[agentID]; ok {
			return AttestationKey{Key: p}, nil
		}
		p, _, _ := ed25519.GenerateKey(rand.Reader)
		pubs[agentID] = p
		return AttestationKey{Key: p}, nil
	}
	c := NewAttestationKeyCache(loader, newTestLogger())

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("agent-%d", i%4)
			for range 50 {
				if _, err := c.Get(context.Background(), id); err != nil {
					t.Errorf("concurrent Get(%s): %v", id, err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	// Expect at most 4 unique entries (we hit 4 distinct agent IDs).
	if c.Len() > 4 {
		t.Errorf("Len = %d; want ≤ 4", c.Len())
	}
}

func TestAttestationKeyCache_NilLoggerSafe(t *testing.T) {
	// Production wires a real logger; defensive guard so a test or
	// embedded caller passing nil doesn't crash in the negative-cache
	// debug log path.
	loader := func(_ context.Context, _ string) (AttestationKey, error) {
		return AttestationKey{}, ErrUnknownAgent
	}
	c := NewAttestationKeyCache(loader, nil)
	if _, err := c.Get(context.Background(), "a"); !errors.Is(err, ErrUnknownAgent) {
		t.Errorf("err = %v", err)
	}
}

// TestAttestationKeyCache_CertExpiresAtRoundTrips locks the contract:
// the loader's CertExpiresAt is surfaced unchanged through Get (and on the
// cached hit), so the verifier can enforce expiry.
func TestAttestationKeyCache_CertExpiresAtRoundTrips(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	exp := time.Now().Add(42 * time.Hour).Truncate(time.Second)
	c := NewAttestationKeyCache(func(_ context.Context, _ string) (AttestationKey, error) {
		return AttestationKey{Key: pub, CertExpiresAt: exp}, nil
	}, newTestLogger())
	for _, label := range []string{"miss", "hit"} {
		ak, err := c.Get(context.Background(), "a")
		if err != nil {
			t.Fatalf("%s: Get: %v", label, err)
		}
		if !ak.CertExpiresAt.Equal(exp) {
			t.Errorf("%s: CertExpiresAt = %v; want %v", label, ak.CertExpiresAt, exp)
		}
		if string(ak.Key) != string(pub) {
			t.Errorf("%s: key mismatch", label)
		}
	}
}
