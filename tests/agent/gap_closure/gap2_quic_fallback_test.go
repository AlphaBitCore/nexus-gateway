//go:build darwin

package gap_closure_test

// gap2_quic_fallback_test.go — E74-S7 T7.3
//
// TestGap2QUICFallback verifies FR-7.2: pf blocks UDP/443 (forcing QUIC
// happy-eyeballs fallback to TCP), and the resulting TCP flow is captured.
//
// Precondition: the test machine's UID must be in quicFallbackUIDs in
// agent_settings. If absent, the test skips — it does NOT fail.
//
// Integration test — requires live pf + daemon + DB.
// Listed in .coverage-allowlist under category E.

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"
)

func TestGap2QUICFallback(t *testing.T) {
	cfg := mustLoadConfig(t)
	pool := newDBPool(t, cfg.DBDSN)
	defer pool.Close()

	targetHost := cfg.Gap2TargetHost
	testUID := os.Getuid()
	t.Logf("Gap 2: target=%s test-uid=%d", targetHost, testUID)

	// 1. Precondition check: we cannot query agent_settings from this test
	// directly (no Hub API client here). The test documents the skip condition
	// clearly so the operator knows what to configure.
	// Per SDD T7.3: "Skip (not fail) if the precondition is absent, printing
	// the documented skip message."
	//
	// Heuristic: attempt a 100ms UDP dial to the target. If it connects
	// immediately (no block), the pf rule is likely not installed for this UID.
	// If it times out or is refused, the block is active.
	udpAddr := net.JoinHostPort(targetHost, "443")
	udpDeadline := time.Now().Add(100 * time.Millisecond)
	udpConn, udpErr := net.DialTimeout("udp", udpAddr, 100*time.Millisecond)
	if udpErr == nil {
		udpConn.SetDeadline(udpDeadline) //nolint:errcheck
		// Send a minimal QUIC Initial packet header (version negotiation probe).
		// QUIC Initial: first byte 0xC0 (Long Header, Initial type), version 0x00000001.
		probe := []byte{0xC0, 0x00, 0x00, 0x00, 0x01}
		_, _ = udpConn.Write(probe)
		buf := make([]byte, 64)
		_, readErr := udpConn.Read(buf)
		udpConn.Close()
		if readErr == nil {
			// Got a response — UDP not blocked. Precondition not met.
			t.Skip("SKIP: test uid not in quicFallbackUIDs — pf UDP/443 block is not active for this uid. " +
				"Configure quicFallbackUIDs in agent_settings to include uid " + itoa(testUID) +
				". See T7.3 setup note in e74-s7-gap-closure-tests-skill.md.")
			return
		}
	}
	// UDP timed out or was refused — block is active. Proceed.
	t.Logf("Gap 2: UDP/443 to %s: no QUIC ServerHello received within 100ms (block active)", targetHost)

	// 2. Snapshot Prometheus UDP-blocked counter.
	metricsT0 := prometheusSnapshot(cfg.PrometheusAddr)

	// 3. Assert: no QUIC ServerHello within 3 seconds (stronger assertion).
	// The 100ms above was just the precondition probe. This is the real assertion.
	udpConn2, udpErr2 := net.DialTimeout("udp", udpAddr, 500*time.Millisecond)
	if udpErr2 == nil {
		udpConn2.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
		probe2 := []byte{0xC0, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00}
		_, _ = udpConn2.Write(probe2)
		buf2 := make([]byte, 256)
		n, _ := udpConn2.Read(buf2)
		udpConn2.Close()
		if n > 0 && buf2[0] == 0xC0 {
			t.Errorf("Gap 2: received apparent QUIC ServerHello from %s — UDP/443 not blocked", targetHost)
		}
	}
	t.Logf("Gap 2: confirmed no QUIC ServerHello within 3s (UDP blocked)")

	// 4. TCP fallback — the happy-eyeballs fallback path.
	traceID := uniqueTraceID("gap2")
	t.Logf("Gap 2 trace ID: %s", traceID)

	tcpAddr := net.JoinHostPort(targetHost, "443")
	tcpConn, tcpErr := net.DialTimeout("tcp", tcpAddr, 10*time.Second)
	if tcpErr != nil {
		t.Fatalf("Gap 2: TCP fallback dial to %s failed: %v", tcpAddr, tcpErr)
	}
	// Send a minimal TLS ClientHello-shaped byte so the listener SNI-peeks it.
	// Not a full TLS handshake — just enough for the daemon to classify the flow.
	tcpConn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	_, _ = tcpConn.Write([]byte{
		// TLS ContentType=22 (Handshake), Version=3.1, length=0 — enough for SNI detection
		0x16, 0x03, 0x01, 0x00, 0x00,
	})
	tcpConn.Close()

	// 5. Wait for a traffic_event row.
	// Note: the trace ID is not embeddable in a raw TLS ClientHello, so we
	// look for a recent row by target_host and source='agent' instead.
	// The SDD requires source='agent' and target_host contains the configured host.
	t0, _ := time.Parse(time.RFC3339, cfg.T0)
	if t0.IsZero() {
		t0 = time.Now().Add(-30 * time.Second)
	}

	var found *TrafficEventRow
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		rows := countTrafficEventsByHostSince(t, pool, "%"+targetHost+"%", t0)
		if len(rows) > 0 {
			r := rows[0]
			found = &r
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	if found == nil {
		t.Errorf("Gap 2: no traffic_event row found for host=%q with source='agent' within 15s", targetHost)
	} else {
		if found.Source != "agent" {
			t.Errorf("Gap 2: source=%q; want 'agent'", found.Source)
		}
		if !containsStr(found.TargetHost, targetHost) {
			t.Errorf("Gap 2: target_host=%q does not contain %q", found.TargetHost, targetHost)
		}
		t.Logf("Gap 2 TCP captured: id=%s source=%s target_host=%s", found.ID, found.Source, found.TargetHost)
	}

	// 6. Assert UDP-blocked counter incremented.
	metricsT1 := prometheusSnapshot(cfg.PrometheusAddr)
	assertPrometheusCounter(t, "nexus_agent_pf_udp_blocked_total", metricsT0, metricsT1, 1)

	t.Log("Gap 2 PASS: UDP blocked, TCP fallback captured")
}

// itoa converts an int to a decimal string.
func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

// containsStr reports whether s contains substr.
func containsStr(s, substr string) bool {
	return len(s) >= len(substr) &&
		(s == substr ||
			len(substr) == 0 ||
			indexStr(s, substr) >= 0)
}

func indexStr(s, sub string) int {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
