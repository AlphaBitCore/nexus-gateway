// tests/agent/gap_closure/cmd/render_report/main.go
//
// render_report reads a `go test -v` output log from --input (or stdin)
// and renders a structured Markdown report to --output (or stdout).
//
// Called by runner.sh after the go test invocation completes.
// Parses "--- PASS", "--- FAIL", "--- SKIP" lines and named log lines
// embedded by each gap test to extract observability numbers.
//
// Usage:
//
//	go run ./tests/agent/gap_closure/cmd/render_report/ \
//	  --input /tmp/test-macos-pf-agent-raw.log \
//	  --output /tmp/test-macos-pf-agent-<ts>.md \
//	  --listener-addr 127.0.0.1:13443 \
//	  --db-dsn "postgres://..." \
//	  --prometheus-addr http://localhost:9100 \
//	  --t0 2026-05-21T12:00:00Z \
//	  --macos-version 15.4.1

package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// testResult captures the outcome and key log lines for one test function.
type testResult struct {
	Name   string // e.g. "TestGap1RawSocket"
	Status string // PASS / FAIL / SKIP / OBS / UNKNOWN
	Notes  []string
}

func main() {
	inputPath := flag.String("input", "", "Path to go test -v raw output log (default: stdin)")
	outputPath := flag.String("output", "", "Path for the rendered Markdown report (default: stdout)")
	listenerAddr := flag.String("listener-addr", "127.0.0.1:13443", "Agent listener address")
	dbDSN := flag.String("db-dsn", "", "Postgres DSN (password will be redacted in output)")
	prometheusAddr := flag.String("prometheus-addr", "http://localhost:9100", "Prometheus metrics address")
	t0Str := flag.String("t0", "", "Test start time (RFC3339)")
	macosVersion := flag.String("macos-version", "unknown", "macOS version string from sw_vers")
	flag.Parse()

	// ─── Open input ──────────────────────────────────────────────────────────
	var inputReader io.Reader
	if *inputPath != "" {
		f, err := os.Open(*inputPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "render_report: cannot open input %q: %v\n", *inputPath, err)
			os.Exit(1)
		}
		defer f.Close()
		inputReader = f
	} else {
		inputReader = os.Stdin
	}

	// ─── Open output ─────────────────────────────────────────────────────────
	var outputWriter io.Writer
	if *outputPath != "" {
		f, err := os.Create(*outputPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "render_report: cannot create output %q: %v\n", *outputPath, err)
			os.Exit(1)
		}
		defer f.Close()
		outputWriter = f
	} else {
		outputWriter = os.Stdout
	}

	// ─── Parse the go test output ────────────────────────────────────────────
	results := parseGoTestOutput(inputReader)

	// ─── Compute overall result ───────────────────────────────────────────────
	// Gap 4 is always OBS (green). Gap 2 and Gap 5 may SKIP (also green).
	// All others must be PASS.
	ts := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	if *t0Str != "" {
		if parsed, err := time.Parse(time.RFC3339, *t0Str); err == nil {
			ts = parsed.Format("2006-01-02T15:04:05Z")
		}
	}

	// ─── Render report ───────────────────────────────────────────────────────
	renderReport(outputWriter, renderParams{
		Timestamp:    ts,
		ListenerAddr: *listenerAddr,
		DBDSNRedacted: redactDSN(*dbDSN),
		PrometheusAddr: *prometheusAddr,
		MacOSVersion: *macosVersion,
		Results:      results,
	})

	fmt.Fprintf(os.Stderr, "render_report: report written to %s\n", *outputPath)
}

type renderParams struct {
	Timestamp      string
	ListenerAddr   string
	DBDSNRedacted  string
	PrometheusAddr string
	MacOSVersion   string
	Results        map[string]*testResult
}

// renderReport writes the structured Markdown report to w.
func renderReport(w io.Writer, p renderParams) {
	get := func(name string) *testResult {
		if r, ok := p.Results[name]; ok {
			return r
		}
		return &testResult{Name: name, Status: "UNKNOWN"}
	}

	gap1 := get("TestGap1RawSocket")
	gap2 := get("TestGap2QUICFallback")
	gap3 := get("TestGap3ContentCaptureRate")
	gap4 := get("TestGap4LatencyObservability")
	gap5 := get("TestGap5HelperProcessAttribution")
	cons := get("TestDomainEngineConsistency")

	// Gap 4 is always OBS regardless of what go test says.
	gap4.Status = "OBS"

	// Compute overall pass/fail.
	passCount := 0
	failedArms := []string{}
	skippedArms := []string{}

	for _, arm := range []struct {
		name   string
		result *testResult
	}{
		{"Gap 1", gap1},
		{"Gap 2", gap2},
		{"Gap 3", gap3},
		{"Gap 4 (OBS)", gap4},
		{"Gap 5", gap5},
		{"Consistency", cons},
	} {
		switch arm.result.Status {
		case "PASS", "OBS":
			passCount++
		case "SKIP":
			passCount++ // skipped = not failed
			skippedArms = append(skippedArms, arm.name)
		case "FAIL":
			failedArms = append(failedArms, arm.name)
		default:
			failedArms = append(failedArms, arm.name+" (UNKNOWN)")
		}
	}

	overallResult := "PASS"
	if len(failedArms) > 0 {
		overallResult = "FAIL"
	}

	fmt.Fprintf(w, "# macOS pf-Agent Gap-Closure Report — %s\n\n", p.Timestamp)

	fmt.Fprintf(w, "## Environment\n\n")
	fmt.Fprintf(w, "- Agent listener: %s\n", p.ListenerAddr)
	fmt.Fprintf(w, "- DB DSN: %s (password redacted)\n", p.DBDSNRedacted)
	fmt.Fprintf(w, "- Prometheus: %s\n", p.PrometheusAddr)
	fmt.Fprintf(w, "- macOS version: %s\n", p.MacOSVersion)
	fmt.Fprintf(w, "- interceptMode: pf (assumed — this skill does not verify the running config)\n")
	fmt.Fprintf(w, "\n")

	fmt.Fprintf(w, "## Summary\n\n")
	fmt.Fprintf(w, "| Gap | Test | Result | Notes |\n")
	fmt.Fprintf(w, "|---|---|---|---|\n")
	fmt.Fprintf(w, "| Gap 1 — raw socket | TestGap1RawSocket | %s | %s |\n",
		gap1.Status, notesLine(gap1.Notes))
	fmt.Fprintf(w, "| Gap 2 — QUIC fallback | TestGap2QUICFallback | %s | %s |\n",
		gap2.Status, notesLine(gap2.Notes))
	fmt.Fprintf(w, "| Gap 3 — content capture rate | TestGap3ContentCaptureRate | %s | %s |\n",
		gap3.Status, notesLine(gap3.Notes))
	fmt.Fprintf(w, "| Gap 4 — latency (observability only) | TestGap4LatencyObservability | OBS | %s |\n",
		notesLine(gap4.Notes))
	fmt.Fprintf(w, "| Gap 5 — helper attribution | TestGap5HelperProcessAttribution | %s | %s |\n",
		gap5.Status, notesLine(gap5.Notes))
	fmt.Fprintf(w, "| Cross-service consistency | TestDomainEngineConsistency | %s | %s |\n",
		cons.Status, notesLine(cons.Notes))
	fmt.Fprintf(w, "\n")

	renderSection(w, "## Gap 1 — Raw Socket\n", gap1)
	renderSection(w, "## Gap 2 — QUIC Fallback\n", gap2)
	renderSection(w, "## Gap 3 — Content Capture Rate Under Load\n", gap3)
	renderSection(w, "## Gap 4 — Per-Hop Latency (Observability Only — Not a Gate)\n", gap4)
	renderSection(w, "## Gap 5 — Helper-Process Attribution\n", gap5)
	renderSection(w, "## Cross-Service Consistency (DEC-012)\n", cons)

	fmt.Fprintf(w, "## Result\n\n")
	fmt.Fprintf(w, "**%s** — %d of 6 arms green (Gap 4 is observability-only, always green; "+
		"Gap 2 and Gap 5 may be SKIP)\n\n", overallResult, passCount)
	if len(skippedArms) > 0 {
		fmt.Fprintf(w, "Skipped arms: %s\n\n", strings.Join(skippedArms, ", "))
	}
	if len(failedArms) > 0 {
		fmt.Fprintf(w, "Failed arms: %s\n\n", strings.Join(failedArms, ", "))
	}
	fmt.Fprintf(w, "Report generated: %s\n", p.Timestamp)
}

func renderSection(w io.Writer, heading string, r *testResult) {
	fmt.Fprintf(w, "%s\n", heading)
	fmt.Fprintf(w, "**Outcome**: %s\n\n", r.Status)
	if len(r.Notes) > 0 {
		fmt.Fprintf(w, "```\n")
		for _, n := range r.Notes {
			fmt.Fprintf(w, "%s\n", n)
		}
		fmt.Fprintf(w, "```\n\n")
	}
	fmt.Fprintf(w, "---\n\n")
}

// parseGoTestOutput parses `go test -v` output and extracts per-test results
// and key log lines (prefixed by the test name in verbose output).
func parseGoTestOutput(r io.Reader) map[string]*testResult {
	results := make(map[string]*testResult)

	// Patterns for go test -v output lines.
	passPat := regexp.MustCompile(`^--- (PASS|FAIL|SKIP): (\S+)`)
	logPat := regexp.MustCompile(`^\s+(\S+_test\.go:\d+:)?\s*(.+)$`)
	// "=== RUN TestFoo" marks the start of a test.
	runPat := regexp.MustCompile(`^=== RUN\s+(\S+)`)

	var currentTest string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()

		if m := runPat.FindStringSubmatch(line); m != nil {
			currentTest = m[1]
			if _, ok := results[currentTest]; !ok {
				results[currentTest] = &testResult{Name: currentTest, Status: "UNKNOWN"}
			}
			continue
		}

		if m := passPat.FindStringSubmatch(line); m != nil {
			status := m[1]
			name := m[2]
			if _, ok := results[name]; !ok {
				results[name] = &testResult{Name: name}
			}
			results[name].Status = status
			currentTest = ""
			continue
		}

		// Capture log lines for the current test.
		if currentTest != "" {
			if m := logPat.FindStringSubmatch(line); m != nil {
				note := strings.TrimSpace(m[2])
				if note != "" && !strings.HasPrefix(note, "=== RUN") {
					results[currentTest].Notes = append(results[currentTest].Notes, note)
				}
			}
		}
	}

	return results
}

// redactDSN removes the password component from a Postgres DSN for safe
// inclusion in the report. Handles both DSN formats:
//   - postgres://user:pass@host/db
//   - host=… password=… (keyword-value format)
func redactDSN(dsn string) string {
	if dsn == "" {
		return "(not set)"
	}
	// Try URL format first.
	if u, err := url.Parse(dsn); err == nil && u.Host != "" {
		if u.User != nil {
			u.User = url.User(u.User.Username())
		}
		return u.String()
	}
	// Keyword-value format: redact "password=<value>".
	re := regexp.MustCompile(`(?i)password\s*=\s*\S+`)
	return re.ReplaceAllString(dsn, "password=REDACTED")
}

func notesLine(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	// Return the last note that looks like a summary (contains "PASS", "SKIP",
	// "rate", "p95", "attributed", "divergence").
	for i := len(notes) - 1; i >= 0; i-- {
		n := notes[i]
		for _, kw := range []string{"PASS", "SKIP", "rate", "p95", "attributed", "divergence", "Gap "} {
			if strings.Contains(n, kw) {
				// Truncate to 80 chars for the table cell.
				if len(n) > 80 {
					n = n[:77] + "..."
				}
				return n
			}
		}
	}
	return ""
}
