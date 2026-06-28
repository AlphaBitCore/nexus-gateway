package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/compliance"
	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// erroringFailClosedResolver wires a resolver whose only connection-stage hook
// is fail-closed AND references a factory that errors, so that under the
// connection stage's strictFailClosed=true the BuildPipeline call returns an
// error (an unbuildable fail-closed hook). This is the precondition for the
// refusal path.
func erroringFailClosedResolver(t *testing.T) *compliance.PolicyResolver {
	t.Helper()
	reg := core.NewHookRegistry()
	reg.Register("erroring-fc", func(_ *core.HookConfig) (core.Hook, error) {
		return nil, errors.New("factory boom")
	})
	return compliance.NewPolicyResolver([]core.HookConfig{
		{
			ID:                "h-err-fc",
			ImplementationID:  "erroring-fc",
			Name:              "erroring-fail-closed-hook",
			Stage:             "connection",
			Enabled:           true,
			FailBehavior:      "fail-closed",
			ApplicableIngress: []string{"ALL"},
		},
	}, reg, discardLogger())
}

// TestServeHTTP_ConnectionStage_FailClosedUnbuildable_Refuses403: the
// compliance-proxy is a DEDICATED forward proxy, not the host outbound packet
// path, so an unbuildable FAIL-CLOSED connection-stage hook MUST refuse the
// CONNECT (403) rather than "fail open" and establish an uninspected tunnel —
// the strict build error must not be logged-and-ignored.
//
// Contrast: a fail-OPEN hook that cannot build is skipped and the CONNECT
// proceeds (covered by TestServeHTTP_ConnectionStage_*_FailOpen). The
// distinguishing signal is the status code: 403 here means refused before any
// tunnel; the recorder cannot hijack, so a *passed* gate would surface as 500.
func TestServeHTTP_ConnectionStage_FailClosedUnbuildable_Refuses403(t *testing.T) {
	p := &ProxyServer{
		logger:             discardLogger(),
		compliancePipeline: erroringFailClosedResolver(t),
	}

	// Bind the real instrument set so the refusal's metric label is
	// observable: the build-failure refusal must increment
	// rejected_build_failed (an infrastructure signal), never
	// rejected_policy (which would steer an admin to the policy rules
	// instead of the broken hook config).
	promReg := prometheus.NewRegistry()
	metrics.Register(registry.NewRegistry(promReg))

	req := newConnectRequest("example.com:443")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — an unbuildable fail-closed connection hook must REFUSE the CONNECT; any other code means the tunnel was admitted and would carry uninspected traffic (fail-open regression)", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "compliance pipeline unavailable") {
		t.Fatalf("body = %q, want the infrastructure wording %q — a policy-sounding body sends the admin to the wrong fix", body, "compliance pipeline unavailable")
	}

	mfs, err := promReg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var buildFailed, policy float64
	for _, mf := range mfs {
		if mf.GetName() != "nexus_tunnels_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "result" {
					switch lp.GetValue() {
					case "rejected_build_failed":
						buildFailed = m.GetCounter().GetValue()
					case "rejected_policy":
						policy = m.GetCounter().GetValue()
					}
				}
			}
		}
	}
	if buildFailed != 1 {
		t.Errorf("nexus_tunnels_total{result=\"rejected_build_failed\"} = %v, want 1", buildFailed)
	}
	if policy != 0 {
		t.Errorf("nexus_tunnels_total{result=\"rejected_policy\"} = %v, want 0 — a build failure must not masquerade as a policy decision", policy)
	}
}
