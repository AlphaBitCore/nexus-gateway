//go:build darwin

package gap_closure_test

// gap1_raw_socket_test.go — E74-S7 T7.2
//
// TestGap1RawSocket verifies FR-7.1: a process making a raw TCP dial to
// api.openai.com:443 (no HTTPS_PROXY, no NE awareness) is intercepted by
// the pf rdr rule and produces a traffic_event row with source='agent'.
//
// Integration test — requires live pf + daemon + DB.
// Listed in .coverage-allowlist under category E.

import (
	"os/exec"
	"testing"
	"time"
)

func TestGap1RawSocket(t *testing.T) {
	cfg := mustLoadConfig(t)
	pool := newDBPool(t, cfg.DBDSN)
	defer pool.Close()

	// 1. Confirm the gap1 fixture binary exists.
	fixtureBin := cfg.Gap1FixtureBin
	if fixtureBin == "" {
		t.Skip("NEXUS_GAP1_FIXTURE_BIN not set — run via runner.sh which compiles the fixture")
	}

	// 2. Generate a unique trace ID.
	traceID := uniqueTraceID("gap1")
	t.Logf("Gap 1 trace ID: %s", traceID)

	// 3. Snapshot Prometheus counter before the test.
	metricsT0 := prometheusSnapshot(cfg.PrometheusAddr)

	// 4. Run the raw socket client fixture.
	// The fixture embeds the trace ID as X-Nexus-Request-Id header.
	// Expected: fixture exits 0 (or with a non-fatal TLS warning); pf intercepts.
	cmd := exec.Command(fixtureBin, "--trace-id", traceID, "--host", "api.openai.com")
	out, err := cmd.CombinedOutput()
	t.Logf("gap1 fixture output:\n%s", string(out))
	if err != nil {
		// exec.ExitError is expected when the upstream returns HTTP error after MITM;
		// the test cares about the traffic_event row, not the upstream response.
		t.Logf("gap1 fixture exited with error (may be expected MITM response): %v", err)
	}

	// 5. Wait up to 15 s for a traffic_event row with our trace ID.
	row := waitForTrafficEvent(t, pool, traceID, 15*time.Second)

	// 6. Assert source='agent'.
	if row.Source != "agent" {
		t.Errorf("Gap 1: traffic_event.source=%q; want 'agent'", row.Source)
	}

	// 7. Assert endpoint_type is non-empty (normalizer detected the endpoint).
	if row.EndpointType == "" {
		t.Errorf("Gap 1: traffic_event.endpoint_type is empty; want non-empty (normalizer should detect endpoint)")
	} else {
		t.Logf("Gap 1: endpoint_type=%q", row.EndpointType)
	}

	// 8. Assert request_normalized is not NULL (normalizer produced content).
	if row.RequestNorm == nil {
		t.Errorf("Gap 1: traffic_event_normalized.request_normalized is NULL; normalizer should have produced content")
	} else {
		t.Logf("Gap 1: request_normalized present (len=%d bytes)", len(*row.RequestNorm))
	}

	// 9. Assert Prometheus counter incremented.
	metricsT1 := prometheusSnapshot(cfg.PrometheusAddr)
	assertPrometheusCounter(t, `nexus_agent_pf_flows_accepted_total{decision="inspect"}`,
		metricsT0, metricsT1, 1)

	t.Logf("Gap 1 PASS: traffic_event id=%s source=%s endpoint_type=%s",
		row.ID, row.Source, row.EndpointType)
}
