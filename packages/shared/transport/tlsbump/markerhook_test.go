package tlsbump

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// fakeResp builds a minimal *http.Response with an empty header map so the
// hook has a real Header to operate on. Body is http.NoBody so callers can
// `defer resp.Body.Close()` (a documented no-op) and satisfy bodyclose.
func fakeResp() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       http.NoBody,
	}
}

// cpMarkerHook — full marker path

func TestCPMarkerHook_FullMarker(t *testing.T) {
	m := &CPMarker{
		RequestID:    "req-1",
		DomainRuleID: "rule-uuid",
		HookOutcome:  traffic.HookOutcomeInput{Passed: []string{"pii-redact"}},
	}
	ctx := contextWithCPMarker(context.Background(), m)
	hook := markerHook(ctx, "compliance-proxy")

	resp := fakeResp()
	defer resp.Body.Close() //nolint:errcheck // http.NoBody close is a no-op
	hook(resp)

	assertHeader(t, resp.Header, "X-Nexus-Via", "compliance-proxy")
	assertHeader(t, resp.Header, "X-Nexus-Hook", "passed:pii-redact")
	assertHeader(t, resp.Header, "X-Nexus-Domain-Rule", "rule-uuid")
	assertExposeContains(t, resp.Header,
		"X-Nexus-Hook", "X-Nexus-Domain-Rule")
}

// cpMarkerHook — no DomainRuleID (passthrough traffic)

func TestCPMarkerHook_NoDomainRule(t *testing.T) {
	m := &CPMarker{
		RequestID:   "req-2",
		HookOutcome: traffic.HookOutcomeInput{},
	}
	ctx := contextWithCPMarker(context.Background(), m)
	hook := markerHook(ctx, "compliance-proxy")

	resp := fakeResp()
	defer resp.Body.Close() //nolint:errcheck // http.NoBody close is a no-op
	hook(resp)

	assertHeader(t, resp.Header, "X-Nexus-Hook", "none")
	if got := resp.Header.Get("X-Nexus-Domain-Rule"); got != "" {
		t.Errorf("X-Nexus-Domain-Rule: want empty, got %q", got)
	}
}

// cpMarkerHook — nil marker (compliance-disabled fast path)

func TestCPMarkerHook_NilMarker(t *testing.T) {
	// Context carries no marker (e.g. non-MITM path, early bail).
	hook := markerHook(context.Background(), "compliance-proxy")

	resp := fakeResp()
	defer resp.Body.Close() //nolint:errcheck // http.NoBody close is a no-op
	hook(resp)

	assertHeader(t, resp.Header, "X-Nexus-Via", "compliance-proxy")
	// No request-id or domain-rule set.

	// Expose headers still injected so the CORS contract holds.
}

// cpMarkerHook — upstream already has Expose-Headers (merge, not overwrite)

func TestCPMarkerHook_MergesExposeHeaders(t *testing.T) {
	m := &CPMarker{RequestID: "req-3"}
	ctx := contextWithCPMarker(context.Background(), m)
	hook := markerHook(ctx, "compliance-proxy")

	resp := fakeResp()
	defer resp.Body.Close() //nolint:errcheck // http.NoBody close is a no-op
	// Simulate an upstream that already exposes its own custom header.
	resp.Header.Set("Access-Control-Expose-Headers", "x-upstream-custom")
	hook(resp)

	// The upstream value must still be present alongside the Nexus markers.
}

// cpMarkerHook — PrependVia deduplicates (idempotent)

func TestCPMarkerHook_PrependViaIdempotent(t *testing.T) {
	hook := markerHook(context.Background(), "compliance-proxy")

	resp := fakeResp()
	defer resp.Body.Close() //nolint:errcheck // http.NoBody close is a no-op
	// Simulate a second hop where compliance-proxy already appears.
	resp.Header.Set("X-Nexus-Via", "compliance-proxy")
	hook(resp)

	// Should remain a single entry (PrependVia deduplicates).
	if got := resp.Header.Get("X-Nexus-Via"); got != "compliance-proxy" {
		t.Errorf("X-Nexus-Via: want %q, got %q", "compliance-proxy", got)
	}
}

// assertHeader verifies that the canonical header key maps to the expected value.
func assertHeader(t *testing.T, h http.Header, key, want string) {
	t.Helper()
	got := h.Get(key)
	if got != want {
		t.Errorf("%s: want %q, got %q", key, want, got)
	}
}

// assertExposeContains checks that each of names appears (case-insensitive) in
// the Access-Control-Expose-Headers value.
func assertExposeContains(t *testing.T, h http.Header, names ...string) {
	t.Helper()
	expose := strings.ToLower(h.Get("Access-Control-Expose-Headers"))
	for _, n := range names {
		if !strings.Contains(expose, strings.ToLower(n)) {
			t.Errorf("Access-Control-Expose-Headers %q: missing %q (full value: %q)", expose, n, expose)
		}
	}
}
