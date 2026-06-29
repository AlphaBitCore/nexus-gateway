//go:build vectorscan

package matcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestSeedRules_NoVectorscanResidual is a hard gate: EVERY shipped seed rule
// MUST compile into the Vectorscan database, never the RE2 residual. A rule in
// the residual is scanned by the slow RE2 engine on every request — the
// hooks-ON perf regression root cause (one `[\s\S]{0,400}` rule between two
// (?m)^ anchors cost 41% of matcher CPU). These are OUR default rules; they are
// authored to be Vectorscan-native. If this fails, rewrite the offending
// pattern (drop (?m)^ multiline anchors around wide bounded repeats, shrink the
// bound, or anchor to a literal) until it compiles under Vectorscan.
func TestSeedRules_NoVectorscanResidual(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "..", "tools", "db-migrate", "seed", "fixtures", "rule.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rule.json: %v", err)
	}
	var rules []struct {
		RuleID  string `json:"ruleId"`
		Pattern string `json:"pattern"`
		Flags   string `json:"flags"`
	}
	if err := json.Unmarshal(data, &rules); err != nil {
		t.Fatalf("unmarshal rule.json: %v", err)
	}
	if len(rules) == 0 {
		t.Fatal("no seed rules loaded")
	}
	residual := 0
	for _, r := range rules {
		m, _ := CompileVectorscan([]Pattern{{ID: 0, Expr: r.Pattern, Flags: r.Flags}})
		if m.(*vectorscanMatcher).residual != nil {
			residual++
			t.Errorf("rule %q falls to RE2 residual (not Vectorscan-native): flags=%q expr=%q",
				r.RuleID, r.Flags, r.Pattern)
		}
	}
	t.Logf("checked %d seed rules: %d in RE2 residual (must be 0)", len(rules), residual)
}
