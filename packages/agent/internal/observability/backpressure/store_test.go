package backpressure

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNilStoreSafe(t *testing.T) {
	var s *Store
	if s.IsThrottled() {
		t.Error("nil store should not be throttled")
	}
	s.Update(99999) // must not panic
	if s.HighWatermark() != 0 || s.LowWatermark() != 0 {
		t.Error("nil store accessors should return 0")
	}
	s.Poll(context.Background(), nil) // must not panic
}

func TestDefaults(t *testing.T) {
	s := NewStore(Config{})
	if s.HighWatermark() != 500 {
		t.Errorf("default HighWatermark: want 500, got %d", s.HighWatermark())
	}
	if s.LowWatermark() != 200 {
		t.Errorf("default LowWatermark: want 200, got %d", s.LowWatermark())
	}
}

func TestRejectInvertedThresholds(t *testing.T) {
	// LowWatermark >= HighWatermark would flap on every Update;
	// constructor must fall back to defaults.
	s := NewStore(Config{HighWatermark: 100, LowWatermark: 200})
	if s.HighWatermark() != 500 || s.LowWatermark() != 200 {
		t.Errorf("inverted thresholds should fall back to defaults; got high=%d low=%d", s.HighWatermark(), s.LowWatermark())
	}
}

func TestEnterAtHigh_ExitAtLow(t *testing.T) {
	s := NewStore(Config{HighWatermark: 500, LowWatermark: 200})

	// Below high — stay off
	s.Update(499)
	if s.IsThrottled() {
		t.Error("499 < 500 must not throttle")
	}

	// Cross high — throttle
	s.Update(500)
	if !s.IsThrottled() {
		t.Error("500 >= 500 must throttle")
	}

	// Way above high — still throttled
	s.Update(10000)
	if !s.IsThrottled() {
		t.Error("still throttled at 10000")
	}

	// Drop into hysteresis gap (200 < x < 500) — must STAY throttled
	s.Update(300)
	if !s.IsThrottled() {
		t.Error("hysteresis: 300 in (200,500) must remain throttled")
	}
	s.Update(201)
	if !s.IsThrottled() {
		t.Error("hysteresis: 201 > 200 must remain throttled")
	}

	// Cross low — exit
	s.Update(200)
	if s.IsThrottled() {
		t.Error("200 <= 200 must exit throttle")
	}

	// Way below — still off
	s.Update(0)
	if s.IsThrottled() {
		t.Error("0 must not throttle")
	}

	// Climb back into hysteresis gap from below — must STAY off
	s.Update(499)
	if s.IsThrottled() {
		t.Error("hysteresis: 499 < 500 (from below) must remain off")
	}
}

func TestNoFlapOnSingleUpdate(t *testing.T) {
	// One Update at a value inside the hysteresis gap should not
	// change state regardless of starting state — proving Update is
	// edge-triggered, not level-triggered.
	s := NewStore(Config{HighWatermark: 500, LowWatermark: 200})
	for range 10 {
		s.Update(350)
	}
	if s.IsThrottled() {
		t.Error("repeated Updates inside hysteresis gap from cold start must not throttle")
	}
}

func TestPoll_PrimesAndTicks(t *testing.T) {
	var depth atomic.Int64
	depth.Store(600) // start above HighWatermark

	s := NewStore(Config{HighWatermark: 500, LowWatermark: 200, PollInterval: 30 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		s.Poll(ctx, func() int { return int(depth.Load()) })
		close(done)
	}()

	// Prime call should fire immediately + flip throttle on. Give it
	// a tick or two of headroom to avoid scheduler flakiness.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if s.IsThrottled() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !s.IsThrottled() {
		t.Fatal("Poll did not prime throttle within 500 ms")
	}

	// Drop depth and wait for the next tick to clear the flag.
	depth.Store(0)
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !s.IsThrottled() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s.IsThrottled() {
		t.Fatal("Poll did not clear throttle within 500 ms after depth dropped")
	}

	cancel()
	<-done
}

func TestPoll_HonorsContextCancel(t *testing.T) {
	s := NewStore(Config{PollInterval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Poll(ctx, func() int { return 0 })
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Poll did not exit after ctx cancel")
	}
}

func TestConcurrentReadWriteRaceFree(t *testing.T) {
	s := NewStore(Config{HighWatermark: 100, LowWatermark: 50})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	// 100 readers
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = s.IsThrottled()
				}
			}
		}()
	}
	// 4 writers cycling depth from 0 to 200 to 0 ...
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 1000 {
				s.Update(i % 200)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}
