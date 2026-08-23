package proxy

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// bufLogger returns a logger writing into a buffer the test can read back.
func bufLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})), &buf
}

func traceOf(primary, recovery []string) *routingAuditTrace {
	t := &routingAuditTrace{}
	for _, id := range primary {
		t.Targets = append(t.Targets, routingAuditTarget{ModelID: id})
	}
	for _, id := range recovery {
		t.RecoveryTargets = append(t.RecoveryTargets, routingAuditTarget{ModelID: id})
	}
	return t
}

// The invariant fires on the shape all three defects had: the model that served
// the request is not among the ones the router recorded.
func TestDispatchInvariant_WarnsWhenTheServedModelWasNotACandidate(t *testing.T) {
	log, buf := bufLogger()
	checkRoutedTargetWasACandidate(&audit.Record{
		RequestID:     "req-1",
		RoutedModelID: "moonshot-v1-128k",
		RoutingTrace:  traceOf([]string{"gpt-audio-mini"}, []string{"gpt-4o"}),
	}, log)

	out := buf.String()
	if !strings.Contains(out, "routing invariant") {
		t.Fatalf("no warning for a served model outside its own candidate list:\n%s", out)
	}
	// The log line has to carry enough to act on. A warning that says an
	// invariant broke without naming the model or the set it broke against
	// sends whoever reads it back to the database to find out what happened,
	// which is the manual work this exists to remove.
	for _, want := range []string{"req-1", "moonshot-v1-128k", "gpt-audio-mini", "gpt-4o"} {
		if !strings.Contains(out, want) {
			t.Errorf("the warning does not carry %q:\n%s", want, out)
		}
	}
}

// A recovery target that serves is legitimate — that is what recovery is for —
// and must not warn. This is the false-positive that would make the invariant
// noise, because failover onto a recovery target is a NORMAL outcome and would
// otherwise fire on every one of them.
func TestDispatchInvariant_SilentWhenARecoveryTargetServed(t *testing.T) {
	log, buf := bufLogger()
	checkRoutedTargetWasACandidate(&audit.Record{
		RequestID:     "req-2",
		RoutedModelID: "gpt-4o",
		RoutingTrace:  traceOf([]string{"gpt-audio-mini"}, []string{"gpt-4o"}),
	}, log)
	if buf.Len() != 0 {
		t.Errorf("a recovery target serving is normal failover and must not warn:\n%s", buf.String())
	}
}

// Three silences, each a case where warning would report the absence of a
// candidate list rather than a departure from one.
func TestDispatchInvariant_SilentWithoutSomethingToCompare(t *testing.T) {
	for name, rec := range map[string]*audit.Record{
		"no routing trace at all": {RoutedModelID: "m"},
		"nothing was dispatched":  {RoutingTrace: traceOf([]string{"a"}, nil)},
		"an empty candidate set":  {RoutedModelID: "m", RoutingTrace: traceOf(nil, nil)},
		"a trace of another type": {RoutedModelID: "m", RoutingTrace: struct{ X int }{1}},
	} {
		t.Run(name, func(t *testing.T) {
			log, buf := bufLogger()
			checkRoutedTargetWasACandidate(rec, log)
			if buf.Len() != 0 {
				t.Errorf("warned with nothing to compare against:\n%s", buf.String())
			}
		})
	}
}

// THE WIRING, asserted through a real request rather than by calling the
// checker directly.
//
// Every assertion above constructs the record itself, so all of them pass
// against a checker that finalize never calls — the same blind spot that let a
// content policy ship unwired. This drives ServeProxy end to end with an
// executor that attributes the response to a model the router never put
// forward, which is exactly #138's shape, and reads the log the handler was
// given.
func TestDispatchInvariant_IsWiredIntoFinalize(t *testing.T) {
	log, buf := bufLogger()
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`),
		// The router will hand the handler gpt-4o (stubRouterCacheTest's
		// default target); the executor reports a model that was never in the
		// plan.
		Target:   routingcore.RoutingTarget{ProviderID: "p-x", ProviderName: "x", ModelID: "never-routed", AdapterType: "openai"},
		Attempts: []executor.Attempt{{StatusCode: http.StatusOK, Dispatched: true, Target: routingcore.RoutingTarget{ProviderID: "p-x", ProviderName: "x", ModelID: "never-routed", AdapterType: "openai"}}},
	}}
	deps := makeFakeDeps(t, fexec, &fakeBridge{})
	deps.Logger = log

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vk")
	h(httptest.NewRecorder(), req)

	if !strings.Contains(buf.String(), "routing invariant") {
		t.Fatalf("a request served by a model the router never selected produced no warning.\n"+
			"  the checker's own tests pass whether or not finalize calls it; this is the "+
			"one that can tell.\n  log was:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "never-routed") {
		t.Errorf("the warning fired but does not name the model that served: %s", buf.String())
	}
}

// The OTHER enqueue site. There are two, and the test above covers only the one
// the ServeProxy chain uses (stage_accounting). Handler.finalize is the tail
// shared by seven handlers — guardrail, stt, realtime, and four video routes —
// and deleting the call from it left every test in this package green.
//
// Driven through finalize itself rather than through one of those seven: it is
// the single point they all pass, so it IS the call site, not a helper standing
// in for one.
func TestDispatchInvariant_IsWiredIntoTheOtherEnqueueSite(t *testing.T) {
	log, buf := bufLogger()
	deps := makeFakeDeps(t, &fakeExecutor{}, &fakeBridge{})
	deps.Logger = log
	h := NewHandler(deps)

	h.finalize(&audit.Record{
		RequestID:     "req-finalize",
		RoutedModelID: "never-routed",
		RoutingTrace:  traceOf([]string{"gpt-4o"}, nil),
	}, time.Now())

	if !strings.Contains(buf.String(), "routing invariant") {
		t.Fatalf("Handler.finalize enqueued a record whose served model was never a "+
			"candidate, without a warning.\n"+
			"  seven handlers reach the audit pipeline through this function and none "+
			"of them is covered by the ServeProxy test.\n  log was:\n%s", buf.String())
	}
}

// The complement of the wiring test, on the same path: an ordinary request
// served by the model the router picked must produce NOTHING. Without this, a
// checker that warns unconditionally passes the test above and floods
// production with a warning on every single request.
func TestDispatchInvariant_OrdinaryRequestIsSilent(t *testing.T) {
	log, buf := bufLogger()
	fexec := &fakeExecutor{Result: &executor.ExecutionResult{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`),
		Target:     routingcore.RoutingTarget{ProviderID: "p-openai", ProviderName: "openai", ModelID: "gpt-4o", AdapterType: "openai"},
		Attempts: []executor.Attempt{{StatusCode: http.StatusOK, Dispatched: true,
			Target: routingcore.RoutingTarget{ProviderID: "p-openai", ProviderName: "openai", ModelID: "gpt-4o", AdapterType: "openai"}}},
	}}
	deps := makeFakeDeps(t, fexec, &fakeBridge{})
	deps.Logger = log

	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIChat,
		BodyFormat: provcore.FormatOpenAI,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vk")
	h(httptest.NewRecorder(), req)

	if strings.Contains(buf.String(), "routing invariant") {
		t.Errorf("an ordinary request tripped the invariant — it would fire on all "+
			"production traffic:\n%s", buf.String())
	}
}
