//go:build darwin

package gap_closure_test

// gap_cross_service_consistency_test.go — E74-S7 T7.7
//
// TestDomainEngineConsistency verifies DEC-012: the same interception_domain
// row produces identical inspect|passthrough|deny decisions when evaluated
// by the agent's pf listener path and by the Compliance Proxy.
//
// Agent-side decisions are derived by querying the interception_domain table
// and applying the same host-matching logic as domain.Engine.MatchHost (a
// read-only DB query — no import of the shared package needed here since
// the logic is simple enough to re-implement for test purposes and we want
// this harness to be standalone).
//
// CP-side decisions are observed by issuing a TCP CONNECT through the CP
// proxy address and mapping the response to a decision.
//
// Integration test — requires live pf daemon + Compliance Proxy + DB.
// Listed in .coverage-allowlist under category E.

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// domainDecision is the three-valued result of host-level domain evaluation.
type domainDecision string

const (
	decisionInspect     domainDecision = "inspect"
	decisionPassthrough domainDecision = "passthrough"
	decisionDeny        domainDecision = "deny"
	decisionUnknown     domainDecision = "unknown"
)

func TestDomainEngineConsistency(t *testing.T) {
	cfg := mustLoadConfig(t)
	pool := newDBPool(t, cfg.DBDSN)
	defer pool.Close()

	domains := cfg.ConsistencyDomains
	t.Logf("Consistency: testing %d domains: %v", len(domains), domains)

	// Check CP reachability — skip the arm (not fail) if CP is down.
	if !isTCPReachable(cfg.CPProxyAddr, 3*time.Second) {
		t.Skipf("SKIP: Compliance Proxy not reachable at %s — start the CP dev stack first. "+
			"Cross-service consistency arm requires CP running on localhost:3128.", cfg.CPProxyAddr)
		return
	}

	type result struct {
		Domain        string
		AgentDecision domainDecision
		CPDecision    domainDecision
		Match         bool
	}

	var results []result
	divergences := 0

	for _, domain := range domains {
		// 1. Agent-side: query interception_domain table and derive decision.
		agentDecision := agentSideDecision(t, pool, domain)

		// 2. CP-side: issue a TCP CONNECT and map the response.
		cpDecision := cpSideDecision(t, cfg.CPProxyAddr, domain)

		match := agentDecision == cpDecision
		results = append(results, result{
			Domain:        domain,
			AgentDecision: agentDecision,
			CPDecision:    cpDecision,
			Match:         match,
		})

		if !match {
			divergences++
			t.Logf("DIVERGENCE: domain=%q agent=%s cp=%s", domain, agentDecision, cpDecision)
		} else {
			t.Logf("MATCH: domain=%q decision=%s", domain, agentDecision)
		}
	}

	// 3. Report per-domain results.
	t.Log("Cross-service consistency results:")
	t.Log("  Domain | Agent | CP | Match")
	t.Log("  -------|-------|----|---------")
	for _, r := range results {
		t.Logf("  %-40s | %-12s | %-12s | %v",
			r.Domain, r.AgentDecision, r.CPDecision, r.Match)
	}

	// 4. Assert zero divergences.
	if divergences > 0 {
		t.Errorf("Cross-service consistency: %d divergence(s) detected — "+
			"domain.Engine decisions differ between agent and CP path. "+
			"See DEC-012 for the invariant this test protects.", divergences)
	} else {
		t.Logf("Consistency PASS: 0 divergences across %d domains", len(domains))
	}
}

// agentSideDecision derives the host-level decision for domain by querying
// the interception_domain table, replicating the host-match logic of
// domain.Engine.MatchHost. Returns decisionInspect if a matching enabled
// row exists, decisionPassthrough otherwise.
//
// Note (CODE-DEC-001): the listener only does host-level decisions (inspect
// vs passthrough). DENY is a path-level decision inside tlsbump — it cannot
// be observed at TCP accept time. This function reflects that contract.
func agentSideDecision(t testing.TB, pool *pgxpool.Pool, domain string) domainDecision {
	t.Helper()

	// Query the interception_domain table for a matching, enabled row.
	// Match types: EXACT, SUFFIX, REGEX — same logic as domain.Engine.
	const query = `
		SELECT id
		FROM interception_domain
		WHERE enabled = true
		  AND (
		    (host_match_type = 'EXACT'  AND host_pattern = $1)
		 OR (host_match_type = 'SUFFIX' AND $1 LIKE '%' || host_pattern)
		 OR (host_match_type = 'REGEX'  AND $1 ~ host_pattern)
		  )
		LIMIT 1
	`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	row := pool.QueryRow(ctx, query, domain)
	var id string
	if err := row.Scan(&id); err != nil {
		// No matching row — passthrough (default).
		return decisionPassthrough
	}
	return decisionInspect
}

// cpSideDecision issues a TCP CONNECT through the CP proxy and maps the
// HTTP response code to a domain decision.
//
// Mapping (per CODE-DEC-001 — CP does host-level decision at CONNECT time):
//   200 Connection Established → inspect (CP will MITM)
//   407 Proxy Auth Required    → inspect (CP wants auth — treated as inspect)
//   403 Forbidden / 4xx        → deny
//   connection refused / reset → passthrough (CP passed it through or is down)
//   timeout                    → unknown (CP did not respond)
func cpSideDecision(t testing.TB, cpAddr, domain string) domainDecision {
	t.Helper()

	addr := cpAddr
	if !strings.Contains(addr, ":") {
		addr = addr + ":3128"
	}

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Logf("cpSideDecision: cannot connect to CP at %s: %v", addr, err)
		return decisionUnknown
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck

	// Send a CONNECT request. We deliberately send an empty body after CONNECT
	// so the proxy's decision (accept / reject / reset) can be observed.
	connectReq := fmt.Sprintf("CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\nProxy-Connection: close\r\n\r\n",
		domain, domain)
	if _, err := fmt.Fprint(conn, connectReq); err != nil {
		t.Logf("cpSideDecision: write CONNECT failed: %v", err)
		return decisionUnknown
	}

	// Read the response status line.
	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		// Connection reset or closed immediately — CP may have passed it through
		// (passthrough = don't intercept = don't CONNECT-proxy).
		return decisionPassthrough
	}

	statusLine = strings.TrimSpace(statusLine)
	t.Logf("cpSideDecision: domain=%s CONNECT response: %q", domain, statusLine)

	// Parse HTTP status code.
	parts := strings.Fields(statusLine)
	if len(parts) < 2 {
		return decisionUnknown
	}
	switch {
	case strings.HasPrefix(parts[1], "200"):
		return decisionInspect // CP accepted the CONNECT — it will MITM this host.
	case strings.HasPrefix(parts[1], "407"):
		return decisionInspect // CP wants auth — it is intercepting this host.
	case strings.HasPrefix(parts[1], "4"), strings.HasPrefix(parts[1], "5"):
		return decisionDeny // CP rejected the CONNECT.
	default:
		return decisionUnknown
	}
}

// isTCPReachable returns true if a TCP connection to addr succeeds within timeout.
func isTCPReachable(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
