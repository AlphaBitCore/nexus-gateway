package proxy

import (
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

// TestAdmissionGate_AdmitsUpToCapAndSheds asserts the business contract:
// exactly max requests may be in flight; the (max+1)th is shed; a released
// slot re-admits.
func TestAdmissionGate_AdmitsUpToCapAndSheds(t *testing.T) {
	g := &admissionGate{max: 3}
	for i := range 3 {
		if !g.acquire() {
			t.Fatalf("request %d within cap must be admitted", i+1)
		}
	}
	if g.acquire() {
		t.Fatal("request beyond cap must be shed")
	}
	g.release()
	if !g.acquire() {
		t.Fatal("a released slot must re-admit")
	}
}

// TestAdmissionGate_ConcurrentNeverExceedsCap hammers the gate from many
// goroutines and asserts the observed concurrent in-flight count never
// exceeds max — the property that bounds heap growth under overload.
func TestAdmissionGate_ConcurrentNeverExceedsCap(t *testing.T) {
	const cap64, workers, iters = 8, 64, 200
	g := &admissionGate{max: cap64}
	var cur, peak, shed atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iters {
				if !g.acquire() {
					shed.Add(1)
					continue
				}
				n := cur.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				cur.Add(-1)
				g.release()
			}
		}()
	}
	wg.Wait()
	if p := peak.Load(); p > cap64 {
		t.Fatalf("in-flight peaked at %d, cap is %d — gate leaked capacity", p, cap64)
	}
	if g.inflight.Load() != 0 {
		t.Fatalf("in-flight must drain to zero, got %d", g.inflight.Load())
	}
	if shed.Load() == 0 {
		t.Log("note: no sheds observed (timing-dependent); cap property still verified")
	}
}

// TestWriteOverloaded_ResponseShape asserts the reject is a retryable,
// SDK-parseable 429: Retry-After present, OpenAI-shaped error envelope.
func TestWriteOverloaded_ResponseShape(t *testing.T) {
	rr := httptest.NewRecorder()
	writeOverloaded(rr, provcore.FormatOpenAI)
	if rr.Code != 429 {
		t.Fatalf("want 429, got %d", rr.Code)
	}
	if rr.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After header missing")
	}
	body := rr.Body.String()
	for _, want := range []string{`"rate_limit_error"`, `"gateway_overloaded"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("429 body missing %s: %s", want, body)
		}
	}
}
