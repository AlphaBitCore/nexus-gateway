package assistant

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

// TestRegisterRoutesMountsSplit verifies the P2b command/data-stream split endpoints
// are mounted: POST .../sessions/:id/chat (start), GET .../sessions/:id/stream
// (observe), POST .../sessions/:id/interrupt (stop).
func TestRegisterRoutesMountsSplit(t *testing.T) {
	e := echo.New()
	New(Config{}).RegisterAssistantRoutes(e.Group("/api/admin"))
	want := map[string]string{
		http.MethodPost: "/assistant/sessions/:id/chat",
		http.MethodGet:  "/assistant/sessions/:id/stream",
	}
	seen := map[string]bool{}
	interrupt := false
	for _, r := range e.Routes() {
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/assistant/sessions/:id/interrupt") {
			interrupt = true
		}
		if p, ok := want[r.Method]; ok && strings.HasSuffix(r.Path, p) {
			seen[r.Method] = true
		}
	}
	if !seen[http.MethodPost] || !seen[http.MethodGet] || !interrupt {
		t.Fatalf("split routes not all mounted: chat=%v stream=%v interrupt=%v", seen[http.MethodPost], seen[http.MethodGet], interrupt)
	}
}

// TestChatStreamToolRoundStreamsActivity drives a two-round turn: round 1 emits a
// reasoning delta + a tool call (observe_health), round 2 answers. It asserts the
// SSE surfaces tool_start/tool_end and the final text — covering the reasoning /
// tool callbacks in the handler.
func TestChatStreamToolRoundStreamsActivity(t *testing.T) {
	var round int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/chat/completions") {
			w.Header().Set("Content-Type", "text/event-stream")
			if atomic.AddInt32(&round, 1) == 1 {
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"checking health\"}}]}\n\n")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"observe_health\",\"arguments\":\"{}\"}}]}}]}\n\n")
				fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{}}]}\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
				return
			}
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"All healthy.\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{}")
	}))
	defer mock.Close()

	h := New(Config{AIGatewayURL: mock.URL, CPBaseURL: mock.URL, SystemVK: "nvk_test", Model: "m"})
	_, out := driveTurn(t, h, "user-1", `{"message":"health?","model":"gpt-x"}`)
	if !strings.Contains(out, "event: tool_start") || !strings.Contains(out, "observe_health") {
		t.Fatalf("expected a tool_start event for observe_health, got:\n%s", out)
	}
	if !strings.Contains(out, "event: tool_end") {
		t.Fatalf("expected a tool_end event, got:\n%s", out)
	}
	if !strings.Contains(out, "All healthy.") {
		t.Fatalf("expected the final answer after the tool round, got:\n%s", out)
	}
}

// TestChatStream_TurnDeadline is the P2b wall-clock backstop: when the upstream
// inference hangs, the turn ctx deadline fires and the stream surfaces a
// `turn_deadline` error rather than wedging forever. StepCap bounds tool ROUNDS;
// this bounds total wall-clock.
func TestChatStream_TurnDeadline(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/chat/completions") {
			// Hang until the client cancels (turn deadline) or a short fallback, so
			// the turn ctx deadline is what ends the call — without blocking the
			// httptest server's Close on a stuck connection.
			select {
			case <-r.Context().Done():
			case <-time.After(500 * time.Millisecond):
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{}")
	}))
	defer mock.Close()

	h := New(Config{AIGatewayURL: mock.URL, CPBaseURL: mock.URL, SystemVK: "nvk_test", Model: "m"})
	h.turnDeadline = 100 * time.Millisecond
	_, out := driveTurn(t, h, "user-1", `{"message":"hi"}`)
	if !strings.Contains(out, "turn_deadline") {
		t.Fatalf("expected a turn_deadline error on a hung upstream, got:\n%s", out)
	}
}

// TestChatStream_OrdinaryFailureIsTurnFailed pins the negative branch: a non-
// deadline turn failure (here an upstream 500) surfaces `turn_failed`, NOT
// `turn_deadline` — the ctx.Err() check must distinguish a real deadline from any
// other error.
func TestChatStream_OrdinaryFailureIsTurnFailed(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/chat/completions") {
			http.Error(w, "upstream boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{}")
	}))
	defer mock.Close()

	h := New(Config{AIGatewayURL: mock.URL, CPBaseURL: mock.URL, SystemVK: "nvk_test", Model: "m"})
	_, out := driveTurn(t, h, "user-1", `{"message":"hi"}`)
	if !strings.Contains(out, "turn_failed") {
		t.Fatalf("an ordinary upstream failure must be turn_failed, got:\n%s", out)
	}
	if strings.Contains(out, "turn_deadline") {
		t.Fatalf("a non-deadline failure must NOT be turn_deadline:\n%s", out)
	}
}
