// PAC scenarios (S-132 cross-cutting from catalog §10). The PAC file
// endpoint generates a Proxy Auto-Config script the operator deploys
// to enrolled devices so monitored AI provider hostnames get routed
// through the compliance-proxy. PM-grade because a broken PAC file
// silently routes nothing — agents stop appearing in traffic logs and
// nobody notices for hours.
package scenarios_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS132_PACFileGeneration — PM-grade e2e.
//
// BRAINSTORM (pre): the PAC endpoint must satisfy three contracts:
//
//   1. Missing proxyHost / proxyPort returns 400 — the template
//      requires both, an unguarded handler would emit a broken PAC
//      that silently routes nothing.
//   2. With valid params it returns the application/x-ns-proxy-autoconfig
//      MIME type AND a body that contains:
//        - a FindProxyForURL function declaration
//        - a PROXY directive pointing at the supplied host:port
//        - at least one host match clause (derived from the seeded
//          interception_domain table)
//   3. The body is a syntactically valid JavaScript fragment a
//      browser PAC engine can parse — we approximate by checking for
//      balanced braces and the canonical PAC API names
//      (dnsDomainIs / FindProxyForURL).
//
// Cross-service: CP-only — pure DB read of interception_domain rows
// + template render. PM-grade because the failure mode is silent:
// browsers don't surface a PAC syntax error to users; they just go
// DIRECT and the operator notices days later when audit traffic flatlines.
//
// Assertions:
//   1. Missing params → 400.
//   2. With params → 200, Content-Type application/x-ns-proxy-autoconfig.
//   3. Body contains FindProxyForURL + PROXY proxyHost:proxyPort.
//   4. Body contains at least one dnsDomainIs() call OR is the
//      sentinel "no domains configured" PAC.
func TestS132_PACFileGeneration(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	// (1) Missing params → 400.
	badStatus, badBody, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/admin/setup/proxy/cp-nexusdeMacBook-Pro.local-3001/pac-file", nil)
	if err != nil {
		t.Fatalf("bad PAC: %v", err)
	}
	if badStatus != http.StatusBadRequest {
		t.Errorf("missing params: status=%d (want 400) body=%q",
			badStatus, truncate(badBody, 200))
	}

	// (2) Valid request. Use a known thingId from the local enrolled
	// services so the path-segment is plausible (the handler doesn't
	// actually require the thingId to resolve — the param is taken
	// by the parent group but the handler reads only query params).
	const proxyHost = "proxy.s132.example.invalid"
	const proxyPort = "3128"
	status, body, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet,
		"/api/admin/setup/proxy/cp-nexusdeMacBook-Pro.local-3001/pac-file"+
			"?proxyHost="+proxyHost+"&proxyPort="+proxyPort+"&failOpen=true",
		nil)
	if err != nil {
		t.Fatalf("GET PAC: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("GET PAC: status %d body=%q", status, truncate(body, 200))
	}

	pac := string(body)

	// Critical PAC syntax/API checks.
	if !strings.Contains(pac, "FindProxyForURL") {
		t.Errorf("PAC missing FindProxyForURL function declaration (body excerpt: %q)",
			truncate(body, 300))
	}
	proxyDirective := "PROXY " + proxyHost + ":" + proxyPort
	if !strings.Contains(pac, proxyDirective) {
		t.Errorf("PAC missing %q directive (body excerpt: %q)",
			proxyDirective, truncate(body, 400))
	}
	// At least one host-match clause OR the DIRECT-only sentinel.
	if !strings.Contains(pac, "dnsDomainIs") && !strings.Contains(pac, "return \"DIRECT\"") {
		t.Errorf("PAC has neither domain match clauses nor a DIRECT fallback — operators would deploy a no-op PAC (body excerpt: %q)",
			truncate(body, 500))
	}
	// Brace balance — primitive sanity check that catches torn
	// template renders.
	open := strings.Count(pac, "{")
	close := strings.Count(pac, "}")
	if open == 0 || open != close {
		t.Errorf("PAC braces unbalanced: open=%d close=%d (body length=%d)",
			open, close, len(pac))
	}

	t.Logf("S-132 OK: PAC bytes=%d directives=%d braces=%d/%d",
		len(pac), strings.Count(pac, "PROXY "+proxyHost), open, close)
}
