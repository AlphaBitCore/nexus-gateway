package audit

import (
	"testing"

	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
)

// TestToMessage_OmitsNormalizedKeepsComplianceRecord locks the Phase-1
// invariant for the compliance proxy: the wire message no longer carries a
// normalized projection or redaction spans (the control plane recomputes
// normalized at view time from the stored, already-redacted body), yet the
// compliance record itself — captured body, hook decision/reason, and the
// blocking rule — is fully preserved. This is the proof that dropping the
// normalized stamp did not weaken the audit trail.
func TestToMessage_OmitsNormalizedKeepsComplianceRecord(t *testing.T) {
	reason := "pii-detected"
	e := AuditEvent{
		ID:                  "cp-ev-1",
		Provider:            "openai",
		Model:               "gpt-4o",
		IngressFormat:       "openai",
		RequestBody:         sharedaudit.Body{InlineBytes: []byte(`{"model":"gpt-4o"}`)},
		RequestHookDecision: "reject",
		RequestHookReason:   &reason,
		RequestBlockingRule: []byte(`{"ruleId":"r-pii"}`),
	}

	msg := toMessage(e, "thing-1", "Proxy One")

	// The domain-matched adapter is carried as ingress_format so the control
	// plane's view-time recompute keys on the authoritative adapter for cp rows.
	if msg.IngressFormat != "openai" {
		t.Fatalf("ingress_format (domain-matched adapter) lost: %q", msg.IngressFormat)
	}

	// No normalized projection / redaction spans on the wire.
	if msg.RequestNormalized != nil || msg.ResponseNormalized != nil {
		t.Fatalf("compliance-proxy must not ship normalized, got req=%s resp=%s",
			msg.RequestNormalized, msg.ResponseNormalized)
	}
	if msg.RequestRedactionSpans != nil || msg.ResponseRedactionSpans != nil {
		t.Fatal("compliance-proxy must not ship redaction spans")
	}

	// The compliance record is intact.
	if msg.RequestHookDecision != "reject" {
		t.Fatalf("hook decision lost: %q", msg.RequestHookDecision)
	}
	if msg.RequestHookReason != "pii-detected" {
		t.Fatalf("hook reason lost: %q", msg.RequestHookReason)
	}
	if msg.RequestBlockingRule == nil {
		t.Fatal("blocking rule must be preserved")
	}
	if string(msg.RequestBody.InlineBytes) != `{"model":"gpt-4o"}` {
		t.Fatalf("captured body lost: %s", msg.RequestBody.InlineBytes)
	}
	if msg.ProviderName != "openai" {
		t.Fatalf("provider lost: %q", msg.ProviderName)
	}
}
