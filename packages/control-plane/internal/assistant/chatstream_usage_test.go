package assistant

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestChatStreamEmitsUsage proves the usage event fires when the model reports
// token usage — freezing the `usage` event in the SSE protocol contract (so the
// frontend can rely on it from day one).
func TestChatStreamEmitsUsage(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/chat/completions") {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{}")
	}))
	defer mock.Close()

	h := New(Config{AIGatewayURL: mock.URL, CPBaseURL: mock.URL, SystemVK: "nvk_test", Model: "m"})
	_, out := driveTurn(t, h, "user-1", `{"message":"hi"}`)
	if !strings.Contains(out, "event: usage") {
		t.Fatalf("expected a usage event when the model reports token usage, got:\n%s", out)
	}
}
