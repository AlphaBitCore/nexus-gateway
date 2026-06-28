package debug

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

// PatternPerfHandler measures, on the REAL Vectorscan engine, how a single
// rule-pack / hook regex behaves before an author saves it — so a
// prefilter-defeating pattern is caught at authoring time instead of silently
// dominating scan cost in production. It compiles the pattern through the same
// build-tag-selected matcher the data plane uses (Vectorscan here), scans a
// fixed natural-text corpus and a fixed adversarial corpus (long alnum/base64,
// the run that explodes a wide unbounded repeat with no literal prefilter), and
// returns the per-50KB median microseconds for each alongside the static lint
// findings and a plain-language verdict + fix suggestions.
//
// It is the dynamic complement to rulepack.LintPattern (pure-Go AST analysis):
// the lint says WHY a pattern is slow, this says HOW slow on the actual engine.
//
// The control plane has no libhs, so it proxies here over the
// INTERNAL_SERVICE_TOKEN-gated /internal hop (same pattern as provider discover
// / embedding probe).
func PatternPerfHandler() http.HandlerFunc {
	cleanCorpus := buildCleanCorpus()
	adversarialCorpus := buildAdversarialCorpus()

	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Pattern string `json:"pattern"`
			Flags   string `json:"flags"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
			return
		}
		if strings.TrimSpace(req.Pattern) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "pattern is required"})
			return
		}

		lint := rulepack.LintPattern(req.Pattern, req.Flags)
		resp := patternPerfResult{
			Compiles: lint.Compiles,
			Findings: lint.Findings,
		}

		if lint.Compiles {
			m, bad := matcher.CompileDefault([]matcher.Pattern{{ID: 0, Expr: req.Pattern, Flags: req.Flags}})
			if len(bad) > 0 {
				// RE2 itself rejected it — lint already reported why; nothing to time.
				resp.Compiles = false
			} else {
				resp.CleanScanUs = medianScanUs(m, cleanCorpus)
				resp.AdversarialScanUs = medianScanUs(m, adversarialCorpus)
				if c, ok := m.(interface{ Close() error }); ok {
					_ = c.Close()
				}
			}
		}

		resp.Verdict, resp.Suggestions = verdictFor(resp)
		// Always emit arrays, never JSON null, so the editor can safely read
		// .length / .map without a nil guard.
		if resp.Findings == nil {
			resp.Findings = []rulepack.LintFinding{}
		}
		if resp.Suggestions == nil {
			resp.Suggestions = []string{}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

type patternPerfResult struct {
	Compiles          bool                   `json:"compiles"`
	Findings          []rulepack.LintFinding `json:"findings"`
	CleanScanUs       float64                `json:"cleanScanUs"`       // median µs / 50KB natural text
	AdversarialScanUs float64                `json:"adversarialScanUs"` // median µs / 50KB alnum+base64
	Verdict           string                 `json:"verdict"`           // "ok" | "slow" | "invalid"
	Suggestions       []string               `json:"suggestions"`
}

// adversarialSlowUs is the per-50KB median above which a pattern is flagged
// "slow": well-anchored rules scan a 50KB body in tens of microseconds, while a
// prefilter-defeating one runs the per-byte path into the hundreds-to-thousands.
const adversarialSlowUs = 400.0

func verdictFor(r patternPerfResult) (string, []string) {
	if !r.Compiles {
		return "invalid", []string{"The pattern does not compile on the accelerator. See the findings above (most often a backreference or lookaround, which RE2 cannot run)."}
	}
	var sug []string
	// Surface the static lint's actionable guidance verbatim — it already names
	// the missing-literal / wide-unbounded-repeat hazards in plain language.
	for _, f := range r.Findings {
		if f.Message != "" {
			sug = append(sug, f.Message)
		}
	}
	slow := r.AdversarialScanUs >= adversarialSlowUs
	if slow {
		if len(sug) == 0 {
			sug = append(sug, "On a 50KB adversarial body this pattern runs the per-byte slow path. Add a required literal substring (a fixed keyword or prefix the match must contain) and/or bound any wide character-class repeat (e.g. {4,64} instead of {4,}) so Vectorscan's literal prefilter can skip benign input.")
		}
		return "slow", sug
	}
	return "ok", sug
}

// medianScanUs scans the corpus repeatedly and returns the median wall-clock
// microseconds of one full scan, after a warmup. Median (not mean) so a GC pause
// or scheduler hiccup on one iteration does not skew the reported cost.
func medianScanUs(m matcher.Matcher, corpus string) float64 {
	seg := []string{corpus}
	for range 3 { // warmup (scratch alloc, cache)
		_ = m.Scan(seg, true)
	}
	const iters = 21
	samples := make([]float64, iters)
	for i := range samples {
		start := time.Now()
		_ = m.Scan(seg, true)
		samples[i] = float64(time.Since(start).Nanoseconds()) / 1000.0
	}
	sort.Float64s(samples)
	return samples[len(samples)/2]
}

// buildCleanCorpus returns ~50KB of natural English — the dominant compliant
// request shape, where a well-formed pattern's literal prefilter skips quickly.
func buildCleanCorpus() string {
	const sentence = "The quarterly report shows steady growth across all regions and the team is confident about the outlook for the coming fiscal period. "
	var b strings.Builder
	for b.Len() < 50000 {
		b.WriteString(sentence)
	}
	return b.String()[:50000]
}

// buildAdversarialCorpus returns ~50KB dominated by a long alnum/base64 run — the
// content that forces a wide unbounded repeat with no literal onto the per-byte
// slow path (e.g. a base64 vision-payload or a token blob in a real request).
func buildAdversarialCorpus() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var b strings.Builder
	for i := 0; b.Len() < 50000; i++ {
		b.WriteByte(alphabet[(i*31+7)%len(alphabet)])
	}
	return b.String()[:50000]
}
