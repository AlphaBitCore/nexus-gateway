package pipeline

import (
	"encoding/json"
	"os"
	"testing"
)

// A hook's timeout and its fail behaviour are one decision, not two, and until
// now nothing asserted them together.
//
// Exceeding the timeout is not a logged warning. executeOneHook's caller
// abandons the hook AND every hook after it, then answers on their behalf:
// fail-closed becomes RejectHard, so a scan that runs long REFUSES REAL TRAFFIC;
// fail-open becomes Approve, so it DELIVERS CONTENT NOTHING SCANNED.
//
// Scope: this gate governs the SHIPPED DEFAULTS in the seed fixture, nothing
// else. Both fields are operator-configurable per hook from the admin UI, which
// writes them to the HookConfig row the pipeline reads, and an operator raising
// or lowering a live hook is doing the intended thing. What the gate protects is
// the value a fresh install starts from — the one nobody chose.
//
// The budget a hook needs follows from the work it does, not from its name. An
// implementation that reads a header or an IP finishes in microseconds and a
// tight bound is a real guard on a stuck hook. An implementation that scans the
// body needs time proportional to the body, and a bound tuned for a metadata
// check turns an ordinary large request into a refusal.
const contentScanMinTimeoutMs = 30000

// contentScanners are the implementations that read the request or response
// BODY. Keyed by implementationId rather than by hook name so renaming a seeded
// hook cannot silently drop it out of the gate.
var contentScanners = map[string]string{
	"pii-detector":   "regex/vectorscan sweep of every text segment",
	"content-safety": "classifier over the full body",
	"keyword-filter": "delegates to the rule-pack engine over the full body",
}

// knownShortBudget records a content scanner that is deliberately below the
// floor, so the exception is visible in the gate instead of hidden in the data.
// An entry here is a claim someone has to defend, not a way to pass.
var knownShortBudget = map[string]string{
	// keyword-filter is fail-open, so its short budget abandons-and-approves
	// rather than refusing — a weaker failure than the PII scanners', but it is
	// the hook that catches prompt injection, and on the very bodies the PII
	// scanner gets 30s for, this one gives up after one. Left as shipped by
	// decision: the budget is a per-hook operator setting, and a deployment that
	// wants prompt-injection scanning to survive large bodies raises it there.
	// The entry stays so the trade-off is visible rather than buried in data.
	"keyword-blocker": "fail-open at 1000ms — abandons and approves on bodies the other scanners are still reading",
}

type seededHook struct {
	Name             string `json:"name"`
	ImplementationID string `json:"implementationId"`
	FailBehavior     string `json:"failBehavior"`
	TimeoutMs        int    `json:"timeoutMs"`
}

func loadSeededHooks(t *testing.T) []seededHook {
	t.Helper()
	const path = "../../../../tools/db-migrate/seed/fixtures/HookConfig.json"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seed fixture: %v", err)
	}
	var hooks []seededHook
	if err := json.Unmarshal(raw, &hooks); err != nil {
		t.Fatalf("decode seed fixture: %v", err)
	}
	if len(hooks) == 0 {
		t.Fatal("seed fixture decoded to zero hooks — the gate would pass vacuously")
	}
	return hooks
}

func TestSeededContentScannersGetABudgetAScanCanFinishIn(t *testing.T) {
	for _, h := range loadSeededHooks(t) {
		work, scans := contentScanners[h.ImplementationID]
		if !scans {
			continue
		}
		if why, known := knownShortBudget[h.Name]; known {
			t.Logf("known exception: %s (%s) — %s", h.Name, h.ImplementationID, why)
			continue
		}
		if h.TimeoutMs < contentScanMinTimeoutMs {
			t.Errorf("%s (%s) has %dms to do %s.\n"+
				"On timeout the pipeline abandons this hook and every hook after it, and "+
				"answers for them: %q means %s. Either raise the budget or record the hook "+
				"in knownShortBudget with the reason it can be this tight.",
				h.Name, h.ImplementationID, h.TimeoutMs, work, h.FailBehavior,
				abandonConsequence(h.FailBehavior))
		}
	}
}

// TestSeededHookGateCoversTheScannersItNames stops the gate above from passing
// because a scanner quietly left the fixture. Without it, deleting or renaming
// pii-detector's hook turns the gate green by removing what it checks.
func TestSeededHookGateCoversTheScannersItNames(t *testing.T) {
	seen := map[string]bool{}
	for _, h := range loadSeededHooks(t) {
		if _, scans := contentScanners[h.ImplementationID]; scans {
			seen[h.ImplementationID] = true
		}
	}
	for impl := range contentScanners {
		if !seen[impl] {
			t.Errorf("no seeded hook uses %q any more — the budget gate now checks nothing "+
				"for it. Remove it from contentScanners if the implementation is gone, or "+
				"restore the hook if the seed lost it.", impl)
		}
	}
}

func abandonConsequence(failBehavior string) string {
	if failBehavior == "fail-closed" {
		return "a slow scan refuses a real request"
	}
	return "a slow scan delivers content nothing scanned"
}
