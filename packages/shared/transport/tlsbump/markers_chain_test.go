package tlsbump

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// TestMarkersChain_CP_to_AIGW asserts the expected via chain and the canonical
// X-Nexus-* marker set when a response from AI Gateway flows through
// Compliance-Proxy back to a client. Per the new spec there is no per-service
// prefix anymore — both hops write into the same canonical headers and CP
// prepends into the chain via traffic.PrependChain.
//
// The test exercises only the marker plumbing (cpMarkerHook + PrependVia) using
// a synthetic *http.Response; no real network, DB, Redis, or NATS is required.
func TestMarkersChain_CP_to_AIGW(t *testing.T) {
	// Simulate the upstream response that AI Gateway produces.
	// AI Gateway initializes X-Nexus-Via and stamps its own markers before
	// forwarding; CP then prepends "compliance-proxy" and adds its own markers.
	upstream := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
	}
	upstream.Header.Set("X-Nexus-Via", "ai-gateway")
	upstream.Header.Set("X-Nexus-Cache", "MISS")
	upstream.Header.Set("X-Nexus-Hook", "none")

	// Build the per-request CP marker (would normally be set by forward_handler).
	m := &CPMarker{
		RequestID:    "cp-tx-1",
		DomainRuleID: "rule-uuid-abc",
		HookOutcome:  traffic.HookOutcomeInput{Passed: []string{"prompt-injection"}},
	}
	ctx := contextWithCPMarker(context.Background(), m)

	// Apply CP's marker hook — this mirrors what upstream.go does via responseio.Copy.
	markerHook(ctx, "compliance-proxy")(upstream)

	// --- via chain ---
	// CP prepends "compliance-proxy" to the AI Gateway's "ai-gateway" entry.
	assertHeader(t, upstream.Header, "X-Nexus-Via", "compliance-proxy, ai-gateway")

	// --- canonical chain markers (1:1 with via) ---
	// CP prepended its hook outcome onto the chain that AIGW initialised.
	for key, want := range map[string]string{
		"X-Nexus-Hook":        "passed:prompt-injection, none",
		"X-Nexus-Domain-Rule": "rule-uuid-abc",
	} {
		assertHeader(t, upstream.Header, key, want)
	}

	// --- AI Gateway-stamped headers preserved untouched ---
	// CP must not strip or overwrite the headers AIGW stamped that do not
	// have chain semantics (Cache is a single-writer field — innermost hop wins).
	assertHeader(t, upstream.Header, "X-Nexus-Cache", "MISS")

	// --- Expose-Headers covers the canonical marker set ---
	expose := strings.ToLower(upstream.Header.Get("Access-Control-Expose-Headers"))
	for _, name := range []string{
		"x-nexus-via",
		"x-nexus-hook",
	} {
		if !strings.Contains(expose, name) {
			t.Errorf("Access-Control-Expose-Headers missing %q (full value: %q)", name, expose)
		}
	}
}

// TestMarkersChain_AgentCP_AIGW asserts the full three-hop via chain when a
// response propagates from AI Gateway → Compliance-Proxy → Agent back to the
// client. Agent's actual marker injection calls traffic.PrependVia; this test
// calls it directly (the underlying primitive) rather than importing the agent
// package, avoiding a cross-package dependency.
func TestMarkersChain_AgentCP_AIGW(t *testing.T) {
	// Step 1: AI Gateway stamps the response (upstream-most service).
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
	}
	resp.Header.Set("X-Nexus-Via", "ai-gateway")
	resp.Header.Set("X-Nexus-Routed-Provider", "anthropic")

	// Step 2: CP processes the response and prepends itself to the chain.
	cpCtx := contextWithCPMarker(context.Background(), &CPMarker{
		RequestID:   "cp-1",
		HookOutcome: traffic.HookOutcomeInput{},
	})
	markerHook(cpCtx, "compliance-proxy")(resp)

	// Intermediate assertion: chain so far is "compliance-proxy, ai-gateway".
	assertHeader(t, resp.Header, "X-Nexus-Via", "compliance-proxy, ai-gateway")

	// Step 3: Agent prepends itself (mirrors MITMRelay's injectInto calling PrependVia).
	traffic.PrependVia(resp.Header, "agent")

	// Final chain must reflect request-flow order (agent → compliance-proxy → ai-gateway).
	assertHeader(t, resp.Header, "X-Nexus-Via", "agent, compliance-proxy, ai-gateway")

	// AI Gateway markers must survive all three hops untouched. The
	// aigw-provider/-model headers are gone; routed-provider stays.
	assertHeader(t, resp.Header, "X-Nexus-Routed-Provider", "anthropic")
}
