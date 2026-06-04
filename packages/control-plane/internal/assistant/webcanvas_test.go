package assistant

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

func TestWebCanvasEmitsDirectives(t *testing.T) {
	var got []NavigateDirective
	c := newWebCanvas(func(d NavigateDirective) { got = append(got, d) })

	if err := c.Navigate("cost", core.TrafficFilter{StatusRange: "5xx", ModelUsed: "gpt-4o"}); err != nil {
		t.Fatal(err)
	}
	if err := c.ShowEvent("evt-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Highlight("provider-x"); err == nil { // no web equivalent → reports unavailable, never fakes success
		t.Fatal("Highlight must report it is unavailable on the web, not succeed silently")
	}

	if len(got) != 2 {
		t.Fatalf("want 2 directives (navigate + show_event; highlight emits nothing), got %d", len(got))
	}
	if got[0] != (NavigateDirective{View: "cost", Status: "5xx", Model: "gpt-4o"}) {
		t.Fatalf("navigate directive wrong: %+v", got[0])
	}
	if got[1] != (NavigateDirective{View: "event", EventID: "evt-1"}) {
		t.Fatalf("show_event directive wrong: %+v", got[1])
	}
}

func TestWebCanvasNilEmitIsSafe(t *testing.T) {
	c := newWebCanvas(nil)
	if err := c.Navigate("x", core.TrafficFilter{}); err != nil {
		t.Fatal(err)
	}
	if err := c.ShowEvent("y"); err != nil {
		t.Fatal(err)
	}
}

// TestChatStreamEmitsNavigate drives a turn where the model calls the navigate
// canvas tool; the handler must surface a `navigate` SSE directive to the browser.
func TestChatStreamEmitsNavigate(t *testing.T) {
	var round int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/chat/completions") {
			w.Header().Set("Content-Type", "text/event-stream")
			if atomic.AddInt32(&round, 1) == 1 {
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"navigate\",\"arguments\":\"{\\\"view\\\":\\\"cost\\\"}\"}}]}}]}\n\n")
				fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{}}]}\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
				return
			}
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Opened the cost page.\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{}")
	}))
	defer mock.Close()

	h := New(Config{AIGatewayURL: mock.URL, CPBaseURL: mock.URL, SystemVK: "nvk_test", Model: "m"})
	_, out := driveTurn(t, h, "user-1", `{"message":"show me cost"}`)
	if !strings.Contains(out, "event: navigate") || !strings.Contains(out, "\"view\":\"cost\"") {
		t.Fatalf("expected a navigate directive for the cost view, got:\n%s", out)
	}
}
