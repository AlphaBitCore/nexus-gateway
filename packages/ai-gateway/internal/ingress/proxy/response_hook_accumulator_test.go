package proxy

import (
	"testing"

	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// TestResponseHookAccumulator_FoldsRepeatedScans verifies that scanning the same
// hook across multiple checkpoints folds to ONE record with summed latency and the
// latest decision — the core guard against N× inflation of the response aggregates.
func TestResponseHookAccumulator_FoldsRepeatedScans(t *testing.T) {
	var acc responseHookAccumulator
	acc.add([]hookcore.HookResult{{Order: 0, HookID: "h1", HookName: "quality", Decision: hookcore.Approve, LatencyMs: 0, LatencyUs: 120}})
	acc.add([]hookcore.HookResult{{Order: 0, HookID: "h1", HookName: "quality", Decision: hookcore.Approve, LatencyMs: 0, LatencyUs: 130}})
	acc.add([]hookcore.HookResult{{Order: 0, HookID: "h1", HookName: "quality", Decision: hookcore.RejectHard, Reason: "blocked", LatencyMs: 1, LatencyUs: 1400}})

	got := acc.finalize()
	if len(got) != 1 {
		t.Fatalf("expected 1 folded row, got %d", len(got))
	}
	if got[0].LatencyUs != 120+130+1400 {
		t.Fatalf("LatencyUs not summed: got %d want %d", got[0].LatencyUs, 120+130+1400)
	}
	if got[0].LatencyMs != 1 {
		t.Fatalf("LatencyMs not summed: got %d want 1", got[0].LatencyMs)
	}
	if got[0].Decision != hookcore.RejectHard || got[0].Reason != "blocked" {
		t.Fatalf("latest decision/reason not kept: %+v", got[0])
	}
}

// TestResponseHookAccumulator_KeyIgnoresOrder proves the key uses stable identity,
// not the volatile per-scan Order — so a hook whose pipeline-rebuild index shifts
// still folds into one row (Order is carried as the latest output value).
func TestResponseHookAccumulator_KeyIgnoresOrder(t *testing.T) {
	var acc responseHookAccumulator
	acc.add([]hookcore.HookResult{{Order: 0, HookID: "h1", LatencyUs: 100}})
	acc.add([]hookcore.HookResult{{Order: 2, HookID: "h1", LatencyUs: 50}})

	got := acc.finalize()
	if len(got) != 1 {
		t.Fatalf("order drift split the hook into %d rows", len(got))
	}
	if got[0].LatencyUs != 150 {
		t.Fatalf("LatencyUs=%d want 150", got[0].LatencyUs)
	}
	if got[0].Order != 2 {
		t.Fatalf("Order=%d want 2 (latest scan wins)", got[0].Order)
	}
}

// TestResponseHookAccumulator_DistinctHooksKeepOrder verifies distinct hooks stay
// separate and emit in first-seen order, summed across scans.
func TestResponseHookAccumulator_DistinctHooksKeepOrder(t *testing.T) {
	var acc responseHookAccumulator
	acc.add([]hookcore.HookResult{{HookID: "a", LatencyUs: 10}, {HookID: "b", LatencyUs: 20}})
	acc.add([]hookcore.HookResult{{HookID: "a", LatencyUs: 5}, {HookID: "b", LatencyUs: 7}})

	got := acc.finalize()
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(got))
	}
	if got[0].HookID != "a" || got[1].HookID != "b" {
		t.Fatalf("first-seen order not preserved: %+v", got)
	}
	if got[0].LatencyUs != 15 || got[1].LatencyUs != 27 {
		t.Fatalf("sums wrong: %+v", got)
	}
}

// TestResponseHookAccumulator_ImplementationIDDisambiguates ensures two bindings of
// the same ImplementationID with distinct HookIDs do not collapse together.
func TestResponseHookAccumulator_ImplementationIDDisambiguates(t *testing.T) {
	var acc responseHookAccumulator
	acc.add([]hookcore.HookResult{
		{HookID: "x", ImplementationID: "pii", LatencyUs: 1},
		{HookID: "y", ImplementationID: "pii", LatencyUs: 2},
	})
	if got := acc.finalize(); len(got) != 2 {
		t.Fatalf("distinct hookIds sharing implId collapsed: %d rows", len(got))
	}
}

// TestResponseHookAccumulator_EmptyAndSixtyThreeFold reproduces the observed
// "RESPONSE PIPELINE (63)" duplication: 63 identical checkpoint scans must fold to
// ONE appended row with summed microsecond latency — not 63 rows.
func TestResponseHookAccumulator_EmptyAndSixtyThreeFold(t *testing.T) {
	var acc responseHookAccumulator
	if acc.finalize() != nil {
		t.Fatal("empty finalize should be nil")
	}
	for range 63 {
		acc.add([]hookcore.HookResult{{HookID: "h1", HookName: "response-quality-signals", Decision: hookcore.Approve, LatencyUs: 100}})
	}
	rows := appendHookTrace(nil, "response", acc.finalize())
	if len(rows) != 1 {
		t.Fatalf("63 scans must fold to 1 row, got %d (N× inflation)", len(rows))
	}
	if rows[0].LatencyUs != 6300 {
		t.Fatalf("summed LatencyUs=%d want 6300", rows[0].LatencyUs)
	}
	if rows[0].Stage != "response" || rows[0].HookID != "h1" {
		t.Fatalf("unexpected row: %+v", rows[0])
	}
}
