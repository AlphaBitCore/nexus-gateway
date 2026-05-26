//go:build darwin

package gap_closure_test

// gap3_load_test.go — E74-S7 T7.4
//
// TestGap3ContentCaptureRate verifies FR-7.3: under concurrent load
// (default 10 goroutines × 60 s), ≥95% of captured flows have
// request_normalized populated (fail-open content capture rate).
//
// Integration test — requires live pf + daemon + DB.
// Listed in .coverage-allowlist under category E.

import (
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGap3ContentCaptureRate(t *testing.T) {
	cfg := mustLoadConfig(t)
	pool := newDBPool(t, cfg.DBDSN)
	defer pool.Close()

	concurrency := cfg.Gap3Concurrency
	durationS := cfg.Gap3DurationS
	targetHost := cfg.Gap3TargetHost

	t.Logf("Gap 3: concurrency=%d duration=%ds target=%s", concurrency, durationS, targetHost)

	// 1. Spawn concurrency goroutines, each looping for durationS seconds.
	// Each iteration sends a minimal HTTPS GET with a unique X-Nexus-Request-Id.
	var (
		mu       sync.Mutex
		allIDs   []string
		sendErrs int
	)

	deadline := time.Now().Add(time.Duration(durationS) * time.Second)

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		workerIdx := i
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				traceID := uniqueTraceID(fmt.Sprintf("gap3w%d", workerIdx))
				if err := gap3SendRequest(targetHost, traceID); err != nil {
					mu.Lock()
					sendErrs++
					mu.Unlock()
				} else {
					mu.Lock()
					allIDs = append(allIDs, traceID)
					mu.Unlock()
				}
				// Small sleep to avoid spinning too fast on error.
				time.Sleep(50 * time.Millisecond)
			}
		}()
	}

	wg.Wait()
	t.Logf("Gap 3: sent %d requests (%d send errors)", len(allIDs), sendErrs)

	if len(allIDs) == 0 {
		t.Fatal("Gap 3: no requests were sent successfully — pf intercept may not be running")
	}

	// 2. Wait up to 30 s for all rows to land in DB.
	dbDeadline := time.Now().Add(30 * time.Second)
	var total, withContent int
	for time.Now().Before(dbDeadline) {
		total, withContent = countTrafficEventsByTraceIDs(t, pool, allIDs)
		if total >= len(allIDs)/2 {
			// Enough rows have arrived; stop waiting.
			break
		}
		time.Sleep(1 * time.Second)
	}
	// Final count after the wait.
	total, withContent = countTrafficEventsByTraceIDs(t, pool, allIDs)

	t.Logf("Gap 3: total=%d withContent=%d sentIDs=%d sendErrs=%d",
		total, withContent, len(allIDs), sendErrs)

	if total == 0 {
		t.Fatalf("Gap 3: zero traffic_event rows found for our %d trace IDs after 30s wait — "+
			"pf interception is not producing DB rows", len(allIDs))
	}

	// 3. Assert rate >= 95%.
	rate := float64(withContent) / float64(total) * 100
	t.Logf("Gap 3: content capture rate = %.1f%% (threshold: 95%%)", rate)

	if rate < 95.0 {
		// Collect misses for diagnosis BEFORE failing.
		missing := missingTraceIDs(t, pool, allIDs)
		var sb strings.Builder
		maxPrint := 20
		if len(missing) < maxPrint {
			maxPrint = len(missing)
		}
		for _, id := range missing[:maxPrint] {
			fmt.Fprintf(&sb, "  - %s\n", id)
		}
		if len(missing) > 20 {
			fmt.Fprintf(&sb, "  ... and %d more\n", len(missing)-20)
		}
		t.Logf("Gap 3: trace IDs missing from DB (first %d):\n%s", maxPrint, sb.String())
		t.Errorf("Gap 3: content capture rate %.1f%% < 95%% threshold", rate)
	}

	t.Logf("Gap 3 PASS: %.1f%% content capture rate (%d/%d)", rate, withContent, total)
}

// gap3SendRequest makes one HTTPS request with the trace ID embedded as a
// header. Intentionally avoids http.ProxyFromEnvironment so the pf path is
// the only intercept. Returns nil if the dial + write succeeded (upstream
// response code is irrelevant for this test).
func gap3SendRequest(host, traceID string) error {
	addr := net.JoinHostPort(host, "443")
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	tlsCfg := &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: true, //nolint:gosec // test fixture — MITM cert expected
		MinVersion:         tls.VersionTLS12,
	}
	tlsConn := tls.Client(conn, tlsCfg)
	tlsConn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	if err := tlsConn.Handshake(); err != nil {
		// Handshake error after MITM is expected — the daemon may not complete
		// a full TLS handshake on every path. Treat as non-fatal for this test.
		return nil //nolint:nilerr — intentional: flow was intercepted even if handshake fails
	}

	req := fmt.Sprintf(
		"GET /v1/models HTTP/1.1\r\nHost: %s\r\nX-Nexus-Request-Id: %s\r\nConnection: close\r\n\r\n",
		host, traceID,
	)
	_, writeErr := fmt.Fprint(tlsConn, req)
	return writeErr
}
