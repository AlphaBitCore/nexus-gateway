package assistant

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mockUpstream returns a server that answers the AI Gateway chat SSE with a
// simple no-tool reply, and any admin path (the situation snapshot) with empty
// JSON the agent tolerates.
func mockUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/chat/completions") {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"All healthy.\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{}")
	}))
}

// TestChatStreamEndToEnd proves the P2b split path: POST starts the turn (202), the
// GET stream surfaces its inference as SSE text + a done event.
func TestChatStreamEndToEnd(t *testing.T) {
	mock := mockUpstream(t)
	defer mock.Close()

	h := New(Config{AIGatewayURL: mock.URL, CPBaseURL: mock.URL, SystemVK: "nvk_test", Model: "m"})
	code, out := driveTurn(t, h, "user-1", `{"message":"is the gateway healthy?"}`)
	if code != http.StatusAccepted {
		t.Fatalf("StartChat code = %d, want 202; body:\n%s", code, out)
	}
	if !strings.Contains(out, "event: text") || !strings.Contains(out, "All healthy.") {
		t.Fatalf("expected streamed assistant text, got:\n%s", out)
	}
	if !strings.Contains(out, "event: done") {
		t.Fatalf("expected a done event, got:\n%s", out)
	}
}

// countingUpstream is mockUpstream plus an atomic counter of admin-path (non-
// inference) requests — i.e. the situation snapshot's reads — so the NFR-11/AC-5
// per-turn call ceiling can be asserted end-to-end against the REAL situation.
func countingUpstream(t *testing.T, adminHits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/chat/completions") {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		atomic.AddInt32(adminHits, 1) // an admin-path (situation) read
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{}")
	}))
}

// TestChatStream_SituationCallCeiling is the AC-5 end-to-end assertion (NFR-11): the
// FIRST turn for a caller makes at most the documented ceiling of admin reads (the
// situation's ~8), and a SECOND turn within the TTL makes ZERO — proving the cache is
// actually wired through BuildWebAgent + the real runtime.Situation, not just the
// wrapper in isolation. A no-tool model reply means the only admin reads in a turn are
// the situation's.
func TestChatStream_SituationCallCeiling(t *testing.T) {
	var adminHits int32
	mock := countingUpstream(t, &adminHits)
	defer mock.Close()

	h := New(Config{AIGatewayURL: mock.URL, CPBaseURL: mock.URL, SystemVK: "nvk_test", Model: "m"})

	// Same caller across both turns (situation cache is keyed by userId); a fresh
	// session id per turn so the serialization guard never trips.
	turn := func() {
		if code, body := driveTurn(t, h, "user-1", `{"message":"hi"}`); code != http.StatusAccepted {
			t.Fatalf("StartChat code = %d; body:\n%s", code, body)
		}
	}

	turn()
	first := atomic.LoadInt32(&adminHits)
	if first < 1 || first > 8 {
		t.Fatalf("turn 1 admin reads = %d, want within the documented ceiling [1,8] (the situation snapshot)", first)
	}

	atomic.StoreInt32(&adminHits, 0)
	turn() // same caller, within TTL → snapshot served from cache
	if second := atomic.LoadInt32(&adminHits); second != 0 {
		t.Fatalf("turn 2 admin reads = %d, want 0 (situation must be served from the NFR-11 cache)", second)
	}
}

func TestChatStreamRejectsEmptyMessage(t *testing.T) {
	h := New(Config{SystemVK: "nvk_test"})
	code, _ := driveTurn(t, h, "user-1", `{"message":""}`)
	if code != http.StatusBadRequest {
		t.Fatalf("empty message must be 400, got %d", code)
	}
}

func TestChatStreamRequiresSystemVK(t *testing.T) {
	h := New(Config{}) // no system VK configured
	code, _ := driveTurn(t, h, "user-1", `{"message":"hi"}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("missing system VK must be 503, got %d", code)
	}
}

// TestBearerTokenSourceForwardsVerbatim asserts the identity-passthrough contract:
// the caller's Authorization is forwarded unchanged (never a service account), and
// an absent bearer is a hard error (no silent un-authed self-call).
func TestBearerTokenSourceForwardsVerbatim(t *testing.T) {
	hdr, val, err := newBearerTokenSource("Bearer abc.def").Credential(context.TODO())
	if err != nil || hdr != "Authorization" || val != "Bearer abc.def" {
		t.Fatalf("must forward the caller bearer verbatim, got (%q,%q,%v)", hdr, val, err)
	}
	if _, _, err := newBearerTokenSource("").Credential(context.TODO()); err == nil {
		t.Fatal("an absent caller bearer must be an error, not a silent un-authed call")
	}
}

// TestNew_TurnDeadlineOverride: a configured TurnDeadline replaces the default
// wall-clock backstop; zero keeps the default.
func TestNew_TurnDeadlineOverride(t *testing.T) {
	if h := New(Config{TurnDeadline: 7 * time.Second}); h.turnDeadline != 7*time.Second {
		t.Fatalf("override not applied: %v", h.turnDeadline)
	}
	if h := New(Config{}); h.turnDeadline != turnDeadline {
		t.Fatalf("zero config must keep the default: %v", h.turnDeadline)
	}
}
