package exemption

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/identity"
)

// B13 requires existing locks to be exercised under concurrency. The store's existing
// concurrency test covers Add + IsExempt + List, but not Rebuild — which is the writer a
// Hub config push actually uses, and therefore the writer whose interleaving matters:
// sync.RWMutex blocks new readers once a writer is queued, so Rebuild is what makes a
// reader's lock acquisition wait.
//
// This also guards the invariant a caller would crash on. IsExempt returns
// (bool, *Exemption); a (true, nil) pair would nil-deref at the call site, and a
// (false, non-nil) pair would mean the store reported a match it did not honour.

func TestStore_IsExemptUnderRebuild(t *testing.T) {
	s := NewStore(slog.New(slog.NewTextHandler(nopWriter{}, nil)))

	granting := []identity.ActiveExemption{{
		ID:         "ex-1",
		SourceIP:   "10.0.0.7",
		TargetHost: "api.openai.com",
		ExpiresAt:  time.Now().Add(time.Hour).Format(time.RFC3339),
		Reason:     "concurrency-test",
	}}

	const (
		readers = 8
		writers = 2
	)

	// The counters are what make this test able to fail for the right reason. A
	// fixed-iteration version can complete every Rebuild before the readers are
	// scheduled, so it would pass having observed exactly one snapshot — proving
	// nothing about the swap while looking like it had. Instead both sides run until
	// a reader has observed BOTH the granting and the empty snapshot, and the test
	// fails if that never happens (D8': confirm the arm actually reached the state
	// under test, not merely the lines under test).
	var sawExempt, sawEmpty atomic.Int64

	var wg sync.WaitGroup
	wg.Add(readers + writers)

	stop := make(chan struct{})

	for range readers {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ok, e := s.IsExempt("10.0.0.7", "api.openai.com")
				// The well-formedness invariant, in both directions.
				if ok && e == nil {
					t.Error("IsExempt returned (true, nil): a caller dereferencing the exemption " +
						"would crash. The bool and the pointer must agree.")
					return
				}
				if !ok && e != nil {
					t.Errorf("IsExempt returned (false, %v): the store reported an exemption it "+
						"did not honour", e.ID)
					return
				}
				if ok {
					// A reader that sees the granting snapshot must see the WHOLE of it.
					// Copy-on-write shares *Exemption pointers, so a writer that ever
					// mutated one in place would surface here as a matched entry whose
					// fields belong to a different generation.
					if e.SourceIP != "10.0.0.7" || e.TargetHost != "api.openai.com" {
						t.Errorf("IsExempt matched an entry with fields from another snapshot: "+
							"sourceIP=%q targetHost=%q", e.SourceIP, e.TargetHost)
						return
					}
					sawExempt.Add(1)
				} else {
					sawEmpty.Add(1)
				}
			}
		}()
	}

	for range writers {
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				// Alternate between granting and empty, which is what a config push that
				// revokes the last exemption looks like.
				if i%2 == 0 {
					s.Rebuild(granting)
				} else {
					s.Rebuild(nil)
				}
			}
		}()
	}

	// Wait for PROOF of interleaving rather than for a duration.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if sawExempt.Load() > 0 && sawEmpty.Load() > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	gotExempt, gotEmpty := sawExempt.Load(), sawEmpty.Load()
	close(stop)

	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()
	// A lock-ordering bug shows up as never finishing at all, so the deadline here is
	// generous: it is a deadlock detector, not a latency assertion.
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("readers and writers did not finish: IsExempt and Rebuild deadlocked")
	}

	if gotExempt == 0 || gotEmpty == 0 {
		t.Fatalf("readers observed exempt=%d empty=%d in 10s: a read was never interleaved with "+
			"a swap, so this test proves nothing about the concurrent rebuild it exists to guard",
			gotExempt, gotEmpty)
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
