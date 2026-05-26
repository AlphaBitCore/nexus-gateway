// Package rulepack: quality_test.go — Day-1 false-positive + recall gates
// for the 5 starter packs. Each pack is evaluated against:
//
//   - testdata/benign/<short-name>.txt — corpus of benign user prompts
//     that must NOT match any rule. We allow up to maxFalsePositiveRate
//     of lines to match (Day-1 budget; tightens to spec's 2% in P-F tuning).
//
//   - testdata/attacks/<short-name>.txt — corpus of attack patterns the
//     pack is intended to catch. We require minRecall of lines to match.
//     Lines that intentionally rely on AI Guard (out of regex scope) can
//     be excluded by moving them to an adjacent "needs_ai_guard" file
//     with a comment marker; see plan Task 13 step 13.2.
//
// Both gates fail the test outright — DO NOT lower the thresholds. Tighten
// rules or relabel borderline corpus lines instead.
package rulepack_test

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
)

const (
	// maxFalsePositiveRate is the Day-1 ceiling on benign-corpus matches.
	// Spec target is 2% post-tuning (P-F); 5% is the launch budget.
	maxFalsePositiveRate = 0.05
	// minRecall is the Day-1 floor on attack-corpus matches. Spec target is
	// 90% post-tuning; 70% is the launch floor that lets us ship while AI
	// Guard fills the long tail.
	minRecall = 0.70
)

// starterPackCases enumerates the 5 packs by file basename so both tests
// share one source of truth. The "shortName" feeds testdata/{benign,attacks}.
var starterPackCases = []struct {
	yamlBasename string // under tools/db-migrate/seed/rule-packs/
	shortName    string // under packages/shared/policy/rulepack/testdata/{benign,attacks}/
	displayName  string // for subtest name
}{
	{"nexus-prompt-injection-v1.0.0.yaml", "nexus-prompt-injection", "prompt-injection"},
	{"nexus-jailbreak-v1.0.0.yaml", "nexus-jailbreak", "jailbreak"},
	{"nexus-secret-leak-v1.0.0.yaml", "nexus-secret-leak", "secret-leak"},
	{"nexus-tool-call-safety-v1.0.0.yaml", "nexus-tool-call-safety", "tool-call-safety"},
	{"nexus-content-safety-v1.0.0.yaml", "nexus-content-safety", "content-safety"},
}

// loadPackFromFile reads + parses one of the starter pack YAMLs.
func loadPackFromFile(t *testing.T, basename string) *rulepack.Pack {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "tools", "db-migrate", "seed", "rule-packs", basename)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	pack, _, err := rulepack.LoadYAML(data)
	if err != nil {
		t.Fatalf("LoadYAML %s: %v", basename, err)
	}
	return pack
}

// readCorpus reads a corpus file and returns the non-blank, non-comment
// lines verbatim. Lines starting with "#" are treated as comments and
// skipped (lets us annotate borderline cases inline).
func readCorpus(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open corpus %s: %v", path, err)
	}
	defer f.Close() //nolint:errcheck

	var lines []string
	sc := bufio.NewScanner(f)
	// Some attack lines (e.g. encoded payloads) can exceed bufio's default
	// 64 KiB buffer — bump the limit defensively.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		lines = append(lines, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan corpus %s: %v", path, err)
	}
	return lines
}

// evaluateLine runs the pack against one corpus line and returns the
// matching rule IDs (empty slice means no match — i.e. benign-as-far-as-
// this-pack-is-concerned).
func evaluateLine(pack *rulepack.Pack, line string) []string {
	blocks := []core.ContentBlock{{Type: "text", Text: line}}
	matches := rulepack.Evaluate(*pack, pack.Rules, blocks)
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, m.RuleLocalID)
	}
	return ids
}

// TestStarterPacks_FPRate_UnderThreshold guards against over-broad rules:
// no more than maxFalsePositiveRate of benign lines may match.
func TestStarterPacks_FPRate_UnderThreshold(t *testing.T) {
	for _, tc := range starterPackCases {
		t.Run(tc.displayName, func(t *testing.T) {
			pack := loadPackFromFile(t, tc.yamlBasename)
			corpus := readCorpus(t, filepath.Join("testdata", "benign", tc.shortName+".txt"))
			if len(corpus) == 0 {
				t.Fatalf("benign corpus for %s is empty — cannot evaluate FP rate", tc.shortName)
			}

			var falsePositives int
			for _, line := range corpus {
				if hits := evaluateLine(pack, line); len(hits) > 0 {
					falsePositives++
					t.Logf("FP: line=%q matched rules=%v", line, hits)
				}
			}
			fpRate := float64(falsePositives) / float64(len(corpus))
			t.Logf("%s: FP=%d/%d (%.2f%%), threshold=%.2f%%",
				tc.displayName, falsePositives, len(corpus), fpRate*100, maxFalsePositiveRate*100)
			if fpRate > maxFalsePositiveRate {
				t.Fatalf("FP rate %.2f%% exceeds Day-1 ceiling of %.2f%% (tighten rules — DO NOT lower threshold)",
					fpRate*100, maxFalsePositiveRate*100)
			}
		})
	}
}

// TestStarterPacks_Recall_AboveThreshold guards against narrow rule
// coverage: at least minRecall of attack lines must trigger at least
// one rule.
func TestStarterPacks_Recall_AboveThreshold(t *testing.T) {
	for _, tc := range starterPackCases {
		t.Run(tc.displayName, func(t *testing.T) {
			pack := loadPackFromFile(t, tc.yamlBasename)
			corpus := readCorpus(t, filepath.Join("testdata", "attacks", tc.shortName+".txt"))
			if len(corpus) == 0 {
				t.Fatalf("attack corpus for %s is empty — cannot evaluate recall", tc.shortName)
			}

			var hits int
			for _, line := range corpus {
				if matched := evaluateLine(pack, line); len(matched) > 0 {
					hits++
				} else {
					t.Logf("MISS: line=%q (no rule matched)", line)
				}
			}
			recall := float64(hits) / float64(len(corpus))
			t.Logf("%s: recall=%d/%d (%.2f%%), threshold=%.2f%%",
				tc.displayName, hits, len(corpus), recall*100, minRecall*100)
			if recall < minRecall {
				t.Fatalf("recall %.2f%% below Day-1 floor of %.2f%% (broaden rules or move line to needs_ai_guard — DO NOT lower threshold)",
					recall*100, minRecall*100)
			}
		})
	}
}
