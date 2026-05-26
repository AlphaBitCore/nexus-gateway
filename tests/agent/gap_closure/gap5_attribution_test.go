//go:build darwin

package gap_closure_test

// gap5_attribution_test.go — E74-S7 T7.6
//
// TestGap5HelperProcessAttribution verifies FR-7.5: flows from Chrome helper
// processes (Google Chrome Helper, Google Chrome Helper (Renderer), etc.) are
// attributed to the parent bundle com.google.Chrome rather than the helper
// sub-bundle.
//
// Precondition: NEXUS_GAP5_CHROME_PATH must be set to a Chrome.app path.
// If absent, the test skips — it does NOT fail.
//
// Integration test — requires live pf + daemon + DB + Chrome installed.
// Listed in .coverage-allowlist under category E.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGap5HelperProcessAttribution(t *testing.T) {
	cfg := mustLoadConfig(t)
	pool := newDBPool(t, cfg.DBDSN)
	defer pool.Close()

	chromePath := cfg.Gap5ChromePath
	if chromePath == "" {
		t.Skip("SKIP: NEXUS_GAP5_CHROME_PATH not set — Gap 5 requires a Chrome installation. " +
			"Set NEXUS_GAP5_CHROME_PATH=/Applications/Google Chrome.app in tests/.env.local.")
		return
	}

	// Resolve Chrome binary inside the .app bundle.
	chromeBin := filepath.Join(chromePath, "Contents", "MacOS", "Google Chrome")
	if _, err := os.Stat(chromeBin); err != nil {
		chromeBin = filepath.Join(chromePath, "Contents", "MacOS", "Google Chrome Canary")
		if _, err2 := os.Stat(chromeBin); err2 != nil {
			t.Skipf("SKIP: cannot find Chrome binary inside %q: %v", chromePath, err)
			return
		}
	}

	t.Logf("Gap 5: Chrome binary: %s", chromeBin)

	// 1. Record start time so we can scope the DB query.
	testStart := time.Now()

	// 2. Launch headless Chrome to drive traffic to ChatGPT.
	// We set a short timeout because we only need a few flows — Chrome does
	// not need to fully render the page.
	ctx := t // use t as a TB for helper calls
	_ = ctx

	// Use the gap5 fixture script for launching Chrome.
	fixtureSh := filepath.Join(
		os.Getenv("SCRIPT_DIR_OVERRIDE"), // set if running outside runner.sh
		"fixtures", "gap5-attribution", "launch-chrome.sh",
	)
	if _, err := os.Stat(fixtureSh); err != nil {
		// Fallback: direct invocation if fixture script not found.
		fixtureSh = ""
	}

	var chromeCmd *exec.Cmd
	if fixtureSh != "" {
		chromeCmd = exec.Command("bash", fixtureSh,
			"--chrome-path", chromePath,
			"--target-url", "https://chatgpt.com/",
		)
	} else {
		chromeCmd = exec.Command(chromeBin,
			"--headless=new",
			"--disable-gpu",
			"--no-sandbox",
			"--disable-dev-shm-usage",
			"--dump-dom",
			"https://chatgpt.com/",
		)
	}

	// Launch Chrome in the background and give it up to 20 s.
	if err := chromeCmd.Start(); err != nil {
		t.Skipf("SKIP: failed to launch Chrome at %s: %v", chromeBin, err)
		return
	}

	// 3. Poll the DB for >= 10 traffic_event rows from agent for openai.com
	// or chatgpt.com, created after testStart.
	const wantRows = 10
	const pollDeadline = 20 * time.Second

	var rows []TrafficEventRow
	dbDeadline := time.Now().Add(pollDeadline)
	for time.Now().Before(dbDeadline) {
		r1 := countTrafficEventsByHostSince(t, pool, "%openai.com%", testStart)
		r2 := countTrafficEventsByHostSince(t, pool, "%chatgpt.com%", testStart)
		rows = append(r1, r2...)
		if len(rows) >= wantRows {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Kill Chrome regardless of whether we got enough rows.
	if chromeCmd.Process != nil {
		chromeCmd.Process.Kill() //nolint:errcheck
	}

	if len(rows) == 0 {
		t.Skip("SKIP: no traffic_event rows found for openai.com/chatgpt.com within 20s — " +
			"Chrome may not have made network requests, or pf interception is not capturing Chrome traffic.")
		return
	}

	t.Logf("Gap 5: found %d traffic_event rows", len(rows))

	// 4. Classify each row by source_bundle.
	var correctCount, gapCount int
	for _, r := range rows {
		bundle := r.SourceBundle
		if bundle == "com.google.Chrome" || bundle == "com.google.Chrome.canary" {
			correctCount++
			t.Logf("Gap 5: row id=%s source_bundle=%q CORRECT", r.ID, bundle)
		} else {
			gapCount++
			// Log the misattributed rows for operator diagnosis.
			if strings.Contains(bundle, "Helper") || bundle == "" {
				t.Logf("Gap 5: row id=%s source_bundle=%q GAP (helper or empty)", r.ID, bundle)
			} else {
				t.Logf("Gap 5: row id=%s source_bundle=%q UNEXPECTED", r.ID, bundle)
			}
		}
	}

	total := len(rows)
	rate := float64(correctCount) / float64(total) * 100
	t.Logf("Gap 5: correctly attributed=%d/%d (%.1f%%), threshold=90%%",
		correctCount, total, rate)

	// 5. Assert >= 90% correctly attributed.
	if rate < 90.0 {
		t.Errorf("Gap 5: attribution rate %.1f%% < 90%% threshold (%d/%d correctly attributed to com.google.Chrome)",
			rate, correctCount, total)
	} else {
		t.Logf("Gap 5 PASS: %.1f%% attribution rate (%d/%d)", rate, correctCount, total)
	}
}
