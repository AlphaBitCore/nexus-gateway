package assistant

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	cpmetrics "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/metrics"
	metricsreg "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// stubHook lets the redactor's fail-open branches be exercised deterministically.
type stubHook struct {
	res *hookcore.HookResult
	err error
}

func (s stubHook) Execute(context.Context, *hookcore.HookInput) (*hookcore.HookResult, error) {
	return s.res, s.err
}
func (s stubHook) SupportsEndpoint(hookcore.EndpointType) bool { return true }
func (s stubHook) SupportsModality(hookcore.Modality) bool     { return true }

// TestNewPIIRedactor guards the static canonical pattern set: it must compile.
// Handler.New silently passes through if this ever fails, so this is the only
// thing standing between a typo in piiPatternDefinitions and a silent governance
// gap.
func TestNewPIIRedactor(t *testing.T) {
	if _, err := newPIIRedactor(); err != nil {
		t.Fatalf("the canonical PII patterns must construct a detector: %v", err)
	}
}

// TestRedactToolOutput_ScrubsBodyPII is the AC-2 assertion: a traffic body
// carrying PII (email/SSN) must arrive redacted in the tool result before it
// enters the prompt, and the pii_to_prompt_total counter must move.
func TestRedactToolOutput_ScrubsBodyPII(t *testing.T) {
	reg := prometheus.NewRegistry()
	cpmetrics.Register(metricsreg.NewRegistry(reg))

	r, err := newPIIRedactor()
	if err != nil {
		t.Fatal(err)
	}

	// A realistic observe_traffic_event payload: metadata + a raw body with PII.
	in := `{"id":"evt-1","model":"gpt-4o","requestBody":"email alice@example.com ssn 123-45-6789"}`
	out := r.RedactToolOutput("observe_traffic_event", in)

	if strings.Contains(out, "alice@example.com") {
		t.Errorf("email reached the prompt unredacted: %s", out)
	}
	if strings.Contains(out, "123-45-6789") {
		t.Errorf("SSN reached the prompt unredacted: %s", out)
	}
	// The default replacement template is [REDACTED_<RULE_ID>] with the rule id
	// upper-cased.
	if !strings.Contains(out, "[REDACTED_EMAIL]") || !strings.Contains(out, "[REDACTED_SSN]") {
		t.Errorf("expected the canonical redaction markers, got: %s", out)
	}
	// Non-PII metadata is preserved — the assistant must still see the model/id.
	if !strings.Contains(out, `"model":"gpt-4o"`) || !strings.Contains(out, `"id":"evt-1"`) {
		t.Errorf("non-PII metadata was lost: %s", out)
	}
	if v := counterVal(t, reg, "nexus_assistant_pii_to_prompt_total", nil); v != 1 {
		t.Errorf("pii_to_prompt_total=%v want 1", v)
	}
}

// TestRedactToolOutput_ScopedToBodyTools proves an aggregate/analysis tool's
// output is passed through verbatim — its numeric metadata must NOT be mangled by
// the phone pattern (epoch timestamps, large counts), and there is no PII there.
func TestRedactToolOutput_ScopedToBodyTools(t *testing.T) {
	reg := prometheus.NewRegistry()
	cpmetrics.Register(metricsreg.NewRegistry(reg))

	r, err := newPIIRedactor()
	if err != nil {
		t.Fatal(err)
	}

	// A 10-digit epoch-like number that the phone pattern WOULD match — but
	// observe_health is not a body-bearing tool, so the output is untouched.
	in := `{"totals":{"requests":1717329600,"tokens":9876543}}`
	out := r.RedactToolOutput("observe_health", in)
	if out != in {
		t.Errorf("aggregate-tool output must pass through unchanged; got %s", out)
	}
	if v := counterVal(t, reg, "nexus_assistant_pii_to_prompt_total", nil); v != 0 {
		t.Errorf("no redaction should be counted for a non-body tool, got %v", v)
	}
}

// TestRedactToolOutput_PassthroughWhenClean confirms a body-bearing tool whose
// output has no PII is returned byte-identically (and not counted).
func TestRedactToolOutput_PassthroughWhenClean(t *testing.T) {
	reg := prometheus.NewRegistry()
	cpmetrics.Register(metricsreg.NewRegistry(reg))

	r, err := newPIIRedactor()
	if err != nil {
		t.Fatal(err)
	}
	in := `{"id":"evt-2","requestBody":"what is the weather today"}`
	if out := r.RedactToolOutput("observe_traffic_event", in); out != in {
		t.Errorf("clean body output must pass through unchanged; got %s", out)
	}
	if v := counterVal(t, reg, "nexus_assistant_pii_to_prompt_total", nil); v != 0 {
		t.Errorf("a clean result must not increment pii_to_prompt_total, got %v", v)
	}
}

// TestRedactToolOutput_FailOpen locks the fail-open contract: a detector error or
// a non-redacting decision returns the input UNCHANGED (availability over a hard
// fail) and never counts. Fail-open is acceptable here because Execute is a pure
// in-memory regex pass with no I/O — a runtime error is near-unreachable — and
// blanking every tool result on a hiccup would break the assistant.
func TestRedactToolOutput_FailOpen(t *testing.T) {
	const in = `{"requestBody":"email alice@example.com"}`
	cases := []struct {
		name string
		hook hookcore.Hook
	}{
		{"execute error", stubHook{err: context.DeadlineExceeded}},
		{"approve decision (no match)", stubHook{res: &hookcore.HookResult{Decision: hookcore.Approve}}},
		{"modify but empty content", stubHook{res: &hookcore.HookResult{Decision: hookcore.Modify}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			cpmetrics.Register(metricsreg.NewRegistry(reg))
			r := &piiRedactor{hook: tc.hook}
			if out := r.RedactToolOutput("observe_traffic_event", in); out != in {
				t.Errorf("fail-open must return input unchanged; got %s", out)
			}
			if v := counterVal(t, reg, "nexus_assistant_pii_to_prompt_total", nil); v != 0 {
				t.Errorf("fail-open must not count a redaction, got %v", v)
			}
		})
	}
}
