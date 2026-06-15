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

// TestRedactToolOutput_ScrubsMintedSecrets is the F-0290 assertion: a mint /
// rotate / regenerate response body that echoes a freshly-minted credential in
// plaintext must arrive with the secret replaced by [redacted-secret] before it
// enters the prompt — the PiiDetector does not know these formats, so this is the
// only thing standing between a minted key and the LLM.
func TestRedactToolOutput_ScrubsMintedSecrets(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		secret string
	}{
		{
			name:   "virtual key nvk_",
			secret: "nvk_0123456789abcdef0123456789abcdef",
			body:   `{"id":"vk-1","key":"nvk_0123456789abcdef0123456789abcdef","name":"prod"}`,
		},
		{
			name:   "personal/admin API key nxk_",
			secret: "nxk_deadbeefcafebabe0011223344556677",
			body:   `{"apiKey":"nxk_deadbeefcafebabe0011223344556677"}`,
		},
		{
			name:   "oauth client secret nx_cs_",
			secret: "nx_cs_AbCdEf0123456789-_GhIjKlMnOpQr",
			body:   `{"clientId":"c1","clientSecret":"nx_cs_AbCdEf0123456789-_GhIjKlMnOpQr"}`,
		},
		{
			name:   "anthropic provider key sk-ant-",
			secret: "sk-ant-api03-AbCdEf0123456789GhIjKlMn",
			body:   `{"credential":"sk-ant-api03-AbCdEf0123456789GhIjKlMn"}`,
		},
		{
			name:   "openai project key sk-proj-",
			secret: "sk-proj-AbCdEf0123456789GhIjKlMnOpQr",
			body:   `{"credential":"sk-proj-AbCdEf0123456789GhIjKlMnOpQr"}`,
		},
		{
			name:   "bare sk- provider key",
			secret: "sk-AbCdEf0123456789GhIjKlMnOpQrStUv",
			body:   `{"credential":"sk-AbCdEf0123456789GhIjKlMnOpQrStUv"}`,
		},
		{
			name:   "google AIza key",
			secret: "AIzaSyABCDEFGHIJKLMNOPQRSTUVWXYZ0123456",
			body:   `{"credential":"AIzaSyABCDEFGHIJKLMNOPQRSTUVWXYZ0123456"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			cpmetrics.Register(metricsreg.NewRegistry(reg))
			r, err := newPIIRedactor()
			if err != nil {
				t.Fatal(err)
			}
			out := r.RedactToolOutput("resource_invoke", tc.body)
			if strings.Contains(out, tc.secret) {
				t.Errorf("secret reached the prompt unredacted: %s", out)
			}
			if !strings.Contains(out, secretPlaceholder) {
				t.Errorf("expected %q marker, got: %s", secretPlaceholder, out)
			}
			// Non-secret metadata must survive so the assistant can still reason.
			if !strings.Contains(out, `"`) {
				t.Errorf("body structure was destroyed: %s", out)
			}
			if v := counterVal(t, reg, "nexus_assistant_pii_to_prompt_total", nil); v != 1 {
				t.Errorf("a scrubbed secret must increment the counter once, got %v", v)
			}
		})
	}
}

// TestRedactSecrets_PreservesPrefixFragments proves the length-floored anchors do
// not false-match a bare prefix mentioned in prose (the model talking ABOUT key
// formats) — only a real key body is scrubbed.
func TestRedactSecrets_PreservesPrefixFragments(t *testing.T) {
	in := "Virtual keys start with nvk_ and admin keys with nxk_; sk- is a provider key."
	if out := redactSecrets(in); out != in {
		t.Errorf("bare prefixes must not be redacted; got %s", out)
	}
}

// TestRedactSecrets_MultipleInOneBody confirms every secret in a body is scrubbed,
// not just the first match.
func TestRedactSecrets_MultipleInOneBody(t *testing.T) {
	in := `{"a":"nvk_0123456789abcdef0123456789abcdef","b":"nxk_deadbeefcafebabe0011223344556677"}`
	out := redactSecrets(in)
	if strings.Contains(out, "nvk_0123456789abcdef") || strings.Contains(out, "nxk_deadbeefcafebabe") {
		t.Errorf("all secrets must be scrubbed; got %s", out)
	}
	if n := strings.Count(out, secretPlaceholder); n != 2 {
		t.Errorf("want 2 placeholders, got %d: %s", n, out)
	}
}

// TestRedactToolOutput_SecretAndPII verifies a body carrying BOTH a minted secret
// and PII is fully scrubbed: the secret pass and the PII pass both fire and the
// counter increments once for the combined change.
func TestRedactToolOutput_SecretAndPII(t *testing.T) {
	reg := prometheus.NewRegistry()
	cpmetrics.Register(metricsreg.NewRegistry(reg))
	r, err := newPIIRedactor()
	if err != nil {
		t.Fatal(err)
	}
	in := `{"key":"nvk_0123456789abcdef0123456789abcdef","owner":"alice@example.com"}`
	out := r.RedactToolOutput("resource_read", in)
	if strings.Contains(out, "nvk_0123456789abcdef") {
		t.Errorf("secret survived: %s", out)
	}
	if strings.Contains(out, "alice@example.com") {
		t.Errorf("PII survived: %s", out)
	}
	if !strings.Contains(out, secretPlaceholder) || !strings.Contains(out, "[REDACTED_EMAIL]") {
		t.Errorf("expected both markers, got: %s", out)
	}
	if v := counterVal(t, reg, "nexus_assistant_pii_to_prompt_total", nil); v != 1 {
		t.Errorf("combined scrub must count once, got %v", v)
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
