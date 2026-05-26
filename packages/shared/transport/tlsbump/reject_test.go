package tlsbump

import (
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// TestStampRejectMarkers_MinimalReject verifies that a reject with only the
// hook outcome (no request-id, no domain-rule) still emits X-Nexus-Via,
// X-Nexus-Mode, X-Nexus-Hook, and Access-Control-Expose-Headers.
func TestStampRejectMarkers_MinimalReject(t *testing.T) {
	h := make(http.Header)
	outcome := traffic.HookOutcomeInput{
		Rejected:     "prompt-injection",
		RejectReason: "sql-fragment",
	}
	stampRejectMarkers(h, "compliance-proxy", "", "", outcome)

	if got := h.Get("X-Nexus-Via"); got != "compliance-proxy" {
		t.Errorf("X-Nexus-Via: want %q, got %q", "compliance-proxy", got)
	}
	wantHook := "rejected:prompt-injection:sql-fragment"
	if got := h.Get("X-Nexus-Hook"); got != wantHook {
		t.Errorf("X-Nexus-Hook: want %q, got %q", wantHook, got)
	}
	// domain-rule must be absent when empty.
	if v := h.Get("X-Nexus-Domain-Rule"); v != "" {
		t.Errorf("X-Nexus-Domain-Rule: want empty, got %q", v)
	}
	// Access-Control-Expose-Headers must be present and contain X-Nexus-Hook.
	expose := h.Get("Access-Control-Expose-Headers")
	if expose == "" {
		t.Fatal("Access-Control-Expose-Headers: want non-empty, got empty")
	}
	if !strings.Contains(strings.ToLower(expose), "x-nexus-hook") {
		t.Errorf("Access-Control-Expose-Headers missing X-Nexus-Hook: %q", expose)
	}
}

// TestStampRejectMarkers_FullSet verifies that request-id, domain-rule, via,
// mode, hook, and expose headers are all emitted when all fields are populated.
func TestStampRejectMarkers_FullSet(t *testing.T) {
	h := make(http.Header)
	outcome := traffic.HookOutcomeInput{
		Rejected:     "pii-detector",
		RejectReason: "ssn-pattern",
	}
	const (
		wantRequestID  = "req-uuid-1234"
		wantDomainRule = "domain-rule-uuid-5678"
	)
	stampRejectMarkers(h, "compliance-proxy", wantRequestID, wantDomainRule, outcome)

	if got := h.Get("X-Nexus-Via"); got != "compliance-proxy" {
		t.Errorf("X-Nexus-Via: want %q, got %q", "compliance-proxy", got)
	}
	wantHook := "rejected:pii-detector:ssn-pattern"
	if got := h.Get("X-Nexus-Hook"); got != wantHook {
		t.Errorf("X-Nexus-Hook: want %q, got %q", wantHook, got)
	}
	if got := h.Get("X-Nexus-Domain-Rule"); got != wantDomainRule {
		t.Errorf("X-Nexus-Domain-Rule: want %q, got %q", wantDomainRule, got)
	}
	// SetExposeHeaders is used (not Merge), so the full canonical list is present.
	expose := h.Get("Access-Control-Expose-Headers")
	if expose == "" {
		t.Fatal("Access-Control-Expose-Headers: want non-empty, got empty")
	}
	for _, marker := range []string{"x-nexus-hook", "x-nexus-domain-rule"} {
		if !strings.Contains(strings.ToLower(expose), marker) {
			t.Errorf("Access-Control-Expose-Headers missing %q: %q", marker, expose)
		}
	}
}

// TestStampRejectMarkers_UsesSetExposeNotMerge verifies that
// stampRejectMarkers overwrites any pre-existing Access-Control-Expose-Headers
// value rather than appending to it (synthetic response, no upstream CORS
// state to preserve).
func TestStampRejectMarkers_UsesSetExposeNotMerge(t *testing.T) {
	h := make(http.Header)
	// Simulate a pre-existing upstream CORS value that must be replaced.
	h.Set("Access-Control-Expose-Headers", "x-upstream-custom")

	outcome := traffic.HookOutcomeInput{Rejected: "keyword-filter", RejectReason: "blocked-term"}
	stampRejectMarkers(h, "compliance-proxy", "txid", "dr", outcome)

	expose := h.Get("Access-Control-Expose-Headers")
	// The upstream custom header should NOT appear — SetExposeHeaders replaces.
	if strings.Contains(strings.ToLower(expose), "x-upstream-custom") {
		t.Errorf("SetExposeHeaders should have overwritten upstream value; got %q", expose)
	}
	// The canonical Nexus list should be present.
	if !strings.Contains(strings.ToLower(expose), "x-nexus-hook") {
		t.Errorf("Access-Control-Expose-Headers missing X-Nexus-Hook after overwrite: %q", expose)
	}
}

// TestStampRejectMarkers_NoneOutcome verifies that when no hooks ran
// (zero-value HookOutcomeInput), X-Nexus-Hook is "none".
func TestStampRejectMarkers_NoneOutcome(t *testing.T) {
	h := make(http.Header)
	stampRejectMarkers(h, "compliance-proxy", "txid-abc", "", traffic.HookOutcomeInput{})

	if got := h.Get("X-Nexus-Hook"); got != "none" {
		t.Errorf("X-Nexus-Hook: want %q, got %q", "none", got)
	}
}
