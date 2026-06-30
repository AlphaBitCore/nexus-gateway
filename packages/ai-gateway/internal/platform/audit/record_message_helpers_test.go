package audit

import "testing"

// TestSumHookLatencies_MsAndUs verifies the dual aggregation: the _ms aggregate
// keeps its shipped meaning (sum of truncated per-hook ms), while the _us aggregate
// carries precise microseconds — so a sub-millisecond hook that truncates to 0 ms
// is still visible in microseconds. The NULL-vs-0 distinction (no hook ran vs ran
// in <1ms) is preserved on both.
func TestSumHookLatencies_MsAndUs(t *testing.T) {
	rows := []HookExecRecord{
		{Stage: "request", LatencyMs: 0, LatencyUs: 120},
		{Stage: "request", LatencyMs: 2, LatencyUs: 2100},
		{Stage: "response", LatencyMs: 0, LatencyUs: 300},
	}

	if got := sumHookLatenciesUs(rows, "request"); got == nil || *got != 2220 {
		t.Fatalf("request us = %v want 2220", got)
	}
	if got := sumHookLatenciesMs(rows, "request"); got == nil || *got != 2 {
		t.Fatalf("request ms = %v want 2", got)
	}
	// The precision win: a sub-ms response hook is 0 in ms but precise in us.
	if got := sumHookLatenciesUs(rows, "response"); got == nil || *got != 300 {
		t.Fatalf("response us = %v want 300", got)
	}
	if got := sumHookLatenciesMs(rows, "response"); got == nil || *got != 0 {
		t.Fatalf("response ms = %v want 0 (present, sub-ms)", got)
	}
	// No matching stage → nil (NULL), distinguishing "no hook ran" from "ran in 0".
	if got := sumHookLatenciesUs(rows, "connection"); got != nil {
		t.Fatalf("connection us = %v want nil", got)
	}
	if got := sumHookLatenciesUs(nil, "request"); got != nil {
		t.Fatalf("empty input us = %v want nil", got)
	}
}
