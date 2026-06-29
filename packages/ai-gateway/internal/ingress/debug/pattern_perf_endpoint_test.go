package debug

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

func TestVerdictFor(t *testing.T) {
	// A pattern that doesn't compile is "invalid" with a single explanatory line.
	if v, sug := verdictFor(patternPerfResult{Compiles: false}); v != "invalid" || len(sug) != 1 {
		t.Errorf("non-compiling: verdict=%q suggestions=%d, want invalid/1", v, len(sug))
	}
	// Over the adversarial-slow threshold → "slow", and a suggestion is always present.
	if v, sug := verdictFor(patternPerfResult{Compiles: true, AdversarialScanUs: adversarialSlowUs + 100}); v != "slow" || len(sug) == 0 {
		t.Errorf("slow: verdict=%q suggestions=%d, want slow/>0", v, len(sug))
	}
	// Fast but with a lint finding → "ok" and the finding's message is surfaced verbatim.
	fnd := []rulepack.LintFinding{{Severity: rulepack.LintWarn, Code: "no_literal", Message: "add a literal anchor"}}
	v, sug := verdictFor(patternPerfResult{Compiles: true, AdversarialScanUs: 30, Findings: fnd})
	if v != "ok" || len(sug) != 1 || sug[0] != "add a literal anchor" {
		t.Errorf("ok+finding: verdict=%q suggestions=%v, want ok + the finding message", v, sug)
	}
}

func TestPatternPerfHandler(t *testing.T) {
	h := PatternPerfHandler()
	call := func(body string) (*httptest.ResponseRecorder, patternPerfResult) {
		req := httptest.NewRequest(http.MethodPost, "/internal/pattern-perf-test", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h(rec, req)
		var out patternPerfResult
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec, out
	}

	// A well-anchored secret pattern compiles and gets timed on both corpora.
	rec, out := call(`{"pattern":"\\bAKIA[0-9A-Z]{16}\\b"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("good pattern: status %d", rec.Code)
	}
	if !out.Compiles {
		t.Error("a valid pattern should compile")
	}
	if out.CleanScanUs <= 0 || out.AdversarialScanUs <= 0 {
		t.Errorf("expected measured µs on both corpora, got clean=%v adversarial=%v", out.CleanScanUs, out.AdversarialScanUs)
	}
	if out.Verdict == "" {
		t.Error("expected a verdict")
	}
	// findings/suggestions must serialise as [] not null so the editor can read
	// .length / .map without a nil guard (a good pattern has neither).
	if body := rec.Body.String(); strings.Contains(body, `"findings":null`) || strings.Contains(body, `"suggestions":null`) {
		t.Errorf("findings/suggestions serialised as null: %s", body)
	}

	// A backreference cannot run on the accelerator (RE2 rejects it) → invalid.
	_, out = call(`{"pattern":"(a)\\1"}`)
	if out.Compiles {
		t.Error("a backreference must not compile")
	}
	if out.Verdict != "invalid" {
		t.Errorf("backreference verdict=%q, want invalid", out.Verdict)
	}

	// Empty pattern and malformed JSON are 400s.
	if rec, _ := call(`{"pattern":"  "}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty pattern: status %d, want 400", rec.Code)
	}
	if rec, _ := call(`{not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad json: status %d, want 400", rec.Code)
	}
}
