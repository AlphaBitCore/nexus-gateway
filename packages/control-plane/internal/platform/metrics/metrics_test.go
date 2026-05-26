package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	metricsreg "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// newTestRegistry builds an metricsreg.Registry against a fresh Prometheus
// registry so each test has an isolated registration set (Register touches
// package-level vars, so tests in this file share global state — keeping
// each Prometheus registry fresh avoids duplicate-registration panics from
// re-running Register).
func newTestRegistry() *metricsreg.Registry {
	return metricsreg.NewRegistry(prometheus.NewRegistry())
}

// TestRegister_NilRegistryIsNoOp asserts the early-return guard: passing a
// nil registry must not panic and must leave the package-level instruments
// unchanged (whatever a prior test set them to).
func TestRegister_NilRegistryIsNoOp(t *testing.T) {
	// Capture current package-level pointers so we can confirm Register(nil)
	// did not touch them.
	before := struct {
		req, auth, iam, audit *metricsreg.Counter
		dur                   *metricsreg.Histogram
	}{
		req:   RequestsTotal,
		dur:   RequestDurationMs,
		auth:  AuthAttemptsTotal,
		iam:   IAMEvalTotal,
		audit: AdminAuditLogFailedTotal,
	}

	Register(nil) // must not panic, must not mutate state

	if RequestsTotal != before.req {
		t.Errorf("RequestsTotal mutated by Register(nil)")
	}
	if RequestDurationMs != before.dur {
		t.Errorf("RequestDurationMs mutated by Register(nil)")
	}
	if AuthAttemptsTotal != before.auth {
		t.Errorf("AuthAttemptsTotal mutated by Register(nil)")
	}
	if IAMEvalTotal != before.iam {
		t.Errorf("IAMEvalTotal mutated by Register(nil)")
	}
	if AdminAuditLogFailedTotal != before.audit {
		t.Errorf("AdminAuditLogFailedTotal mutated by Register(nil)")
	}
}

// TestRegister_BindsAllInstruments asserts every documented instrument is
// non-nil after Register and is usable end-to-end (With(...).Inc/Observe
// does not panic — which it would on label-arity mismatch).
func TestRegister_BindsAllInstruments(t *testing.T) {
	reg := newTestRegistry()
	Register(reg)

	if RequestsTotal == nil {
		t.Fatal("RequestsTotal nil after Register")
	}
	if RequestDurationMs == nil {
		t.Fatal("RequestDurationMs nil after Register")
	}
	if AuthAttemptsTotal == nil {
		t.Fatal("AuthAttemptsTotal nil after Register")
	}
	if IAMEvalTotal == nil {
		t.Fatal("IAMEvalTotal nil after Register")
	}
	if AdminAuditLogFailedTotal == nil {
		t.Fatal("AdminAuditLogFailedTotal nil after Register")
	}

	// Exercise each instrument's full label-tuple. If Register passed the
	// wrong label slice the underlying CounterVec.WithLabelValues call
	// panics on arity mismatch — so a no-panic Inc/Observe is the
	// observable proof that the catalog labels (method, route_class,
	// status_class) etc. landed correctly.
	RequestsTotal.With("GET", "/admin/users", "2xx").Inc()
	RequestDurationMs.With("/admin/users").Observe(12.5)
	AuthAttemptsTotal.With("success", "password").Inc()
	IAMEvalTotal.With("allow", "hit").Inc()
	AdminAuditLogFailedTotal.With("admin.user.create").Inc()
}

// TestRegister_Idempotent asserts the doc claim "Safe to call again with
// the same registry (registry registration is idempotent on instrument
// name)" — re-calling Register against the same registry must not panic
// (duplicate Prometheus registration would) and must keep instruments
// usable.
func TestRegister_Idempotent(t *testing.T) {
	reg := newTestRegistry()
	Register(reg)
	Register(reg) // second call: must not panic on duplicate registration

	// Still usable after the second Register.
	RequestsTotal.With("POST", "/v1/chat/completions", "5xx").Inc()
	RequestDurationMs.With("/v1/chat/completions").Observe(250.0)
	AuthAttemptsTotal.With("invalid_jwt", "jwt").Inc()
	IAMEvalTotal.With("deny", "miss").Inc()
	AdminAuditLogFailedTotal.With("admin.policy.update").Inc()
}
