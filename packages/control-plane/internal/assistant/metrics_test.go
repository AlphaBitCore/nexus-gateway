package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	cpmetrics "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/metrics"
	metricsreg "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// freshMetrics rebinds the package-level assistant instruments onto a brand-new
// Prometheus registry and returns it so a test can read the resulting counter
// values. cpmetrics.Register touches global vars, so these tests are not
// t.Parallel.
func freshMetrics(t *testing.T) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewRegistry()
	cpmetrics.Register(metricsreg.NewRegistry(reg))
	return reg
}

// counterVal returns the value of the named counter whose labels are a superset
// of `want`, or 0 when no matching series exists.
func counterVal(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if labelsSuperset(m, want) && m.Counter != nil {
				return m.Counter.GetValue()
			}
		}
	}
	return 0
}

func labelsSuperset(m *dto.Metric, want map[string]string) bool {
	got := make(map[string]string, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// TestMetrics_NavigateTurnIncrementsTurnToolNav drives a turn where the model
// calls the navigate canvas tool. One turn must move three instruments at once:
// the per-turn outcome (ok), the tool-invocation (navigate/ok), and the
// cross-page navigation North-Star numerator.
func TestMetrics_NavigateTurnIncrementsTurnToolNav(t *testing.T) {
	reg := freshMetrics(t)

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
	driveTurn(t, h, "user-1", `{"message":"show me cost"}`)

	if v := counterVal(t, reg, "nexus_assistant_turns_total", map[string]string{"result": "ok"}); v != 1 {
		t.Errorf("turns_total{ok}=%v want 1", v)
	}
	if v := counterVal(t, reg, "nexus_assistant_navigations_total", nil); v != 1 {
		t.Errorf("navigations_total=%v want 1", v)
	}
	if v := counterVal(t, reg, "nexus_assistant_tool_invocations_total", map[string]string{"tool": "navigate", "result": "ok"}); v != 1 {
		t.Errorf("tool_invocations_total{navigate,ok}=%v want 1", v)
	}
}

// TestMetrics_UnknownToolClampsLabel is the cardinality guard: a model that emits
// a tool name the agent does not have must NOT create a per-name time series. The
// label collapses to tool="unknown" and the result is "error" (the loop reports
// "no such tool"). Asserts both the unknown bucket exists and the raw name does not.
func TestMetrics_UnknownToolClampsLabel(t *testing.T) {
	reg := freshMetrics(t)

	var round int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/chat/completions") {
			w.Header().Set("Content-Type", "text/event-stream")
			if atomic.AddInt32(&round, 1) == 1 {
				// A tool name the web registry does not have.
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"definitely_not_a_tool\",\"arguments\":\"{}\"}}]}}]}\n\n")
				fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{}}]}\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
				return
			}
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{}")
	}))
	defer mock.Close()

	h := New(Config{AIGatewayURL: mock.URL, CPBaseURL: mock.URL, SystemVK: "nvk_test", Model: "m"})
	driveTurn(t, h, "user-1", `{"message":"call a fake tool"}`)

	if v := counterVal(t, reg, "nexus_assistant_tool_invocations_total", map[string]string{"tool": "unknown", "result": "error"}); v != 1 {
		t.Errorf("tool_invocations_total{unknown,error}=%v want 1", v)
	}
	// The raw model-emitted name must NOT have become a label value.
	if v := counterVal(t, reg, "nexus_assistant_tool_invocations_total", map[string]string{"tool": "definitely_not_a_tool"}); v != 0 {
		t.Errorf("raw unknown tool name leaked as a label: got %v want 0", v)
	}
}

// TestMetrics_UnavailableAndUnsupportedAuth pins the two pre-inference reject
// outcomes onto distinct result labels (they must be counted, not silently
// dropped — these reject outcomes are the raw-error-exposure / unavailable
// signal, distinct from a clean turn).
func TestMetrics_UnavailableAndUnsupportedAuth(t *testing.T) {
	t.Run("missing system VK → unavailable", func(t *testing.T) {
		reg := freshMetrics(t)
		h := New(Config{}) // no SystemVK
		if code, _ := driveTurn(t, h, "user-1", `{"message":"hi"}`); code != http.StatusServiceUnavailable {
			t.Fatalf("missing system VK must be 503, got %d", code)
		}
		if v := counterVal(t, reg, "nexus_assistant_turns_total", map[string]string{"result": "unavailable"}); v != 1 {
			t.Errorf("turns_total{unavailable}=%v want 1", v)
		}
	})

	t.Run("non-bearer principal → unsupported_auth", func(t *testing.T) {
		reg := freshMetrics(t)
		h := New(Config{SystemVK: "nvk_test"})
		e := echo.New()
		// No Authorization header → not a bearer principal (reaches the bearer gate).
		req := httptest.NewRequest(http.MethodPost, "/api/admin/assistant/sessions/s1/chat", strings.NewReader(`{"message":"hi"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		c.SetParamNames("id")
		c.SetParamValues("s1")
		if err := h.StartChat(c); err != nil {
			t.Fatal(err)
		}
		if v := counterVal(t, reg, "nexus_assistant_turns_total", map[string]string{"result": "unsupported_auth"}); v != 1 {
			t.Errorf("turns_total{unsupported_auth}=%v want 1", v)
		}
	})
}

// resolveConfirm drives makeConfirm in a goroutine and resolves the parked
// confirm with the given decision, returning once the ConfirmFunc has returned.
func resolveConfirm(t *testing.T, h *Handler, decision bool) {
	t.Helper()
	var mu sync.Mutex
	var callID string
	send := func(event string, payload any) {
		if event == "confirm" {
			mu.Lock()
			callID = payload.(map[string]any)["callId"].(string)
			mu.Unlock()
		}
	}
	cf := h.makeConfirm("u", "sess", send)
	done := make(chan struct{})
	go func() {
		_, _ = cf(context.Background(), fakeConfirmTool{}, json.RawMessage(`{"engage":true}`), "r")
		close(done)
	}()
	for range 200 {
		mu.Lock()
		id := callID
		mu.Unlock()
		if id != "" {
			h.confirms.decide("u:sess:"+id, decision, "")
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("confirm did not resolve")
	}
}

// TestMetrics_ConfirmDecisions pins the dangerous-write gate decision counter
// for the three observable outcomes: allow, deny, and fail-safe timeout.
func TestMetrics_ConfirmDecisions(t *testing.T) {
	t.Run("allow", func(t *testing.T) {
		reg := freshMetrics(t)
		h := New(Config{})
		resolveConfirm(t, h, true)
		if v := counterVal(t, reg, "nexus_assistant_confirms_total", map[string]string{"decision": "allow"}); v != 1 {
			t.Errorf("confirms_total{allow}=%v want 1", v)
		}
	})

	t.Run("deny", func(t *testing.T) {
		reg := freshMetrics(t)
		h := New(Config{})
		resolveConfirm(t, h, false)
		if v := counterVal(t, reg, "nexus_assistant_confirms_total", map[string]string{"decision": "deny"}); v != 1 {
			t.Errorf("confirms_total{deny}=%v want 1", v)
		}
	})

	t.Run("ctx cancel → cancelled", func(t *testing.T) {
		reg := freshMetrics(t)
		h := New(Config{})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			_, _ = h.makeConfirm("u", "sess", func(string, any) {})(
				ctx, fakeConfirmTool{}, json.RawMessage(`{}`), "r")
			close(done)
		}()
		time.Sleep(20 * time.Millisecond) // let the confirm park in its select
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("confirm did not unwind on ctx cancel")
		}
		if v := counterVal(t, reg, "nexus_assistant_confirms_total", map[string]string{"decision": "cancelled"}); v != 1 {
			t.Errorf("confirms_total{cancelled}=%v want 1", v)
		}
	})

	t.Run("timeout → fail-safe deny", func(t *testing.T) {
		reg := freshMetrics(t)
		h := New(Config{})
		h.confirmTimeout = 10 * time.Millisecond
		ok, err := h.makeConfirm("u", "sess", func(string, any) {})(
			context.Background(), fakeConfirmTool{}, json.RawMessage(`{}`), "r")
		if ok || err != nil {
			t.Fatalf("timeout must fail-safe deny: ok=%v err=%v", ok, err)
		}
		if v := counterVal(t, reg, "nexus_assistant_confirms_total", map[string]string{"decision": "timeout"}); v != 1 {
			t.Errorf("confirms_total{timeout}=%v want 1", v)
		}
	})
}
