package tlsbump

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestAttestationReplayCache_FirstSeenInsertsAndReturnsFalse(t *testing.T) {
	c := NewAttestationReplayCache()
	if c.Seen(1716100000, "ab12cd34ef56789012345678901234ab") {
		t.Error("first seen should return false")
	}
	if c.Len() != 1 {
		t.Errorf("Len = %d; want 1", c.Len())
	}
}

func TestAttestationReplayCache_SecondSeenReturnsTrue(t *testing.T) {
	c := NewAttestationReplayCache()
	const nonce = "ab12cd34ef56789012345678901234ab"
	_ = c.Seen(1716100000, nonce)
	if !c.Seen(1716100000, nonce) {
		t.Error("second seen should return true (replay)")
	}
}

func TestAttestationReplayCache_DifferentNonceSameTSNotReplay(t *testing.T) {
	c := NewAttestationReplayCache()
	_ = c.Seen(1716100000, "aa11bb22cc33dd44ee55ff6677889900")
	if c.Seen(1716100000, "aa11bb22cc33dd44ee55ff6677889901") {
		t.Error("different nonce at same ts must not be flagged as replay")
	}
}

func TestAttestationReplayCache_SameNonceDifferentTSNotReplay(t *testing.T) {
	c := NewAttestationReplayCache()
	const nonce = "aa11bb22cc33dd44ee55ff6677889900"
	_ = c.Seen(1716100000, nonce)
	if c.Seen(1716100001, nonce) {
		t.Error("different ts with same nonce must not be flagged as replay")
	}
}

func TestAttestationReplayCache_TTLExpiry(t *testing.T) {
	c := NewAttestationReplayCacheWith(100*time.Millisecond, 1024)
	clock := time.Unix(0, 0)
	c.now = func() time.Time { return clock }

	_ = c.Seen(1, "00000000000000000000000000000000")
	clock = clock.Add(200 * time.Millisecond)
	if c.Seen(1, "00000000000000000000000000000000") {
		t.Error("after TTL the same tuple must NOT be replay (entry expired)")
	}
}

func TestAttestationReplayCache_CapEvictsOldest(t *testing.T) {
	c := NewAttestationReplayCacheWith(time.Hour, 2)
	clock := time.Unix(0, 0)
	c.now = func() time.Time { return clock }

	// Insert 3 entries; cap=2 → oldest must be evicted.
	for i := range int64(3) {
		clock = clock.Add(time.Second)
		_ = c.Seen(i, "11111111111111111111111111111111")
	}
	if c.Len() != 2 {
		t.Errorf("Len = %d; want 2 (cap)", c.Len())
	}
}

func TestAttestationReplayCache_CapZeroClampsToOne(t *testing.T) {
	c := NewAttestationReplayCacheWith(time.Minute, 0)
	if c.cap != 1 {
		t.Errorf("cap = %d; want 1 after clamp", c.cap)
	}
}

func TestAttestationReplayCache_ConcurrentSeen(t *testing.T) {
	// Race detector + many goroutines hitting the same tuple. Exactly
	// ONE goroutine should record "first" (Seen returns false); the
	// rest must observe the replay (true).
	c := NewAttestationReplayCache()
	const nonce = "deadbeef00000000000000000000beef"
	const ts = int64(1716100100)

	var firsts, replays int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seen := c.Seen(ts, nonce)
			mu.Lock()
			defer mu.Unlock()
			if seen {
				replays++
			} else {
				firsts++
			}
		}()
	}
	wg.Wait()
	if firsts != 1 {
		t.Errorf("concurrent first-seen count = %d; want 1", firsts)
	}
	if replays != 63 {
		t.Errorf("concurrent replay count = %d; want 63", replays)
	}
}

func TestAttestationReplayCache_EvictExpiredBeforeOldest(t *testing.T) {
	// Cap=3: insert 3 entries with TTL=50ms; advance past TTL; insert
	// a 4th. The sweep should drop all 3 expired entries via the
	// first-pass cleanup, NOT the oldest-eviction fallback.
	c := NewAttestationReplayCacheWith(50*time.Millisecond, 3)
	clock := time.Unix(0, 0)
	c.now = func() time.Time { return clock }

	for i := range int64(3) {
		_ = c.Seen(i, "aa")
	}
	clock = clock.Add(100 * time.Millisecond)
	_ = c.Seen(99, "bb"+strconv.Itoa(0))
	if c.Len() != 1 {
		t.Errorf("Len after sweep = %d; want 1 (only the fresh insert)", c.Len())
	}
}

func TestFormatTSNonce_IsDeterministic(t *testing.T) {
	a := formatTSNonce(1716100000, "abcd")
	b := formatTSNonce(1716100000, "abcd")
	if a != b {
		t.Errorf("formatTSNonce not deterministic: %q vs %q", a, b)
	}
	// Different inputs must differ.
	if formatTSNonce(1716100000, "abcd") == formatTSNonce(1716100001, "abcd") {
		t.Error("ts difference must change key")
	}
	if formatTSNonce(1716100000, "abcd") == formatTSNonce(1716100000, "abce") {
		t.Error("nonce difference must change key")
	}
}
