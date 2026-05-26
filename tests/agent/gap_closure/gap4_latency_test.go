//go:build darwin

package gap_closure_test

// gap4_latency_test.go — E74-S7 T7.5
//
// TestGap4LatencyObservability measures the p95 user-space overhead
// added by the pf redirect + daemon listener + SNI peek.
//
// This test is OBSERVABILITY-ONLY — it ALWAYS passes regardless of the
// measured value. Never calls t.Fail() or t.Error().
//
// Integration test — requires live pf + daemon.
// Listed in .coverage-allowlist under category E.

import (
	"crypto/tls"
	"fmt"
	"net"
	"sort"
	"testing"
	"time"
)

func TestGap4LatencyObservability(t *testing.T) {
	cfg := mustLoadConfig(t)
	targetHost := cfg.Gap4TargetHost
	const sampleCount = 50

	t.Logf("Gap 4: measuring p95 latency over %d samples to %s", sampleCount, targetHost)

	// 1. Collect latency samples.
	type sample struct {
		connTimeMs float64 // time from Dial() call to TCP ESTABLISHED
		ttfbMs     float64 // time from first byte written to first byte received
	}

	var samples []sample
	addr := net.JoinHostPort(targetHost, "443")

	for i := 0; i < sampleCount; i++ {
		s := gap4MeasureOne(t, addr, targetHost)
		if s != nil {
			samples = append(samples, *s)
		}
		// Small pause between samples to avoid hammering the listener.
		time.Sleep(50 * time.Millisecond)
	}

	if len(samples) == 0 {
		t.Log("Gap 4 OBS: no samples collected — pf listener may not be running. Reporting 0ms.")
		t.Log("Gap 4 p95 overhead: 0 ms (no samples)")
		return // Always pass.
	}

	// 2. Compute overheads (ttfb - connTime) and sort for p95.
	overheads := make([]float64, 0, len(samples))
	for _, s := range samples {
		overhead := s.ttfbMs - s.connTimeMs
		if overhead < 0 {
			overhead = 0
		}
		overheads = append(overheads, overhead)
	}
	sort.Float64s(overheads)

	p95 := percentile(overheads, 95)
	p50 := percentile(overheads, 50)
	t.Logf("Gap 4: samples=%d p50=%.1fms p95=%.1fms", len(samples), p50, p95)

	// 3. Log comparison with NE baseline if provided.
	if cfg.Gap4NEBaselineMs > 0 {
		ratio := p95 / float64(cfg.Gap4NEBaselineMs) * 100
		t.Logf("Gap 4 p95 overhead: %.1f ms (NE baseline: %d ms, ratio: %.0f%%)",
			p95, cfg.Gap4NEBaselineMs, ratio)
	} else {
		t.Logf("Gap 4 p95 overhead: %.1f ms (no NE baseline provided)", p95)
	}

	// This test ALWAYS passes — it is a measurement, not a gate.
	// The operator uses the output in the report to compare with the NE baseline.
}

// gap4MeasureOne measures one round-trip to addr via the pf-intercepted path.
// Returns nil if the connection failed (e.g., host unreachable).
func gap4MeasureOne(t testing.TB, addr, host string) *struct {
	connTimeMs float64
	ttfbMs     float64
} {
	t.Helper()

	dialStart := time.Now()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	dialDone := time.Now()
	if err != nil {
		t.Logf("Gap 4: dial failed (host may be unreachable): %v", err)
		return nil
	}
	defer conn.Close()

	connTimeMs := float64(dialDone.Sub(dialStart).Nanoseconds()) / 1e6

	tlsCfg := &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: true, //nolint:gosec // test fixture — MITM cert expected
		MinVersion:         tls.VersionTLS12,
	}
	tlsConn := tls.Client(conn, tlsCfg)
	tlsConn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck

	writeStart := time.Now()
	req := fmt.Sprintf(
		"GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n",
		host,
	)
	if _, err := fmt.Fprint(tlsConn, req); err != nil {
		// Write failed — likely TLS handshake in progress. Measure to here.
		writeEnd := time.Now()
		ttfbMs := float64(writeEnd.Sub(writeStart).Nanoseconds()) / 1e6
		return &struct {
			connTimeMs float64
			ttfbMs     float64
		}{connTimeMs, ttfbMs}
	}

	// Read first byte.
	buf := make([]byte, 1)
	_, readErr := tlsConn.Read(buf)
	readDone := time.Now()
	if readErr != nil {
		// No response — but we still have a connTime measurement.
		return &struct {
			connTimeMs float64
			ttfbMs     float64
		}{connTimeMs, 0}
	}

	ttfbMs := float64(readDone.Sub(writeStart).Nanoseconds()) / 1e6
	return &struct {
		connTimeMs float64
		ttfbMs     float64
	}{connTimeMs, ttfbMs}
}

// percentile returns the value at the given percentile (0–100) in a sorted slice.
func percentile(sorted []float64, pct int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * float64(pct) / 100.0)
	return sorted[idx]
}
