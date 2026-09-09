package classify

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
)

// This decision tree exists TWICE — here and in
// packages/agent/ui/frontend/src/lib/classify.ts — and it decides the same row
// in both places: this copy chooses what is uploaded to Hub, that copy chooses
// what badge the agent UI paints. They had drifted in two ways, and the drift
// was the defect:
//
//   - The untracked fallback was FIRST here, so a flow carrying a deny but no
//     stamped DomainRuleID classified "untracked" — which is not uploaded at
//     the shipped default level, so the denial never reached the console. The
//     TypeScript side worked around exactly that with a fall-through and
//     painted the same row "blocked".
//   - BUMP_FAILED_MINT_FALLBACK_RELAY was handled there and not here.
//
// The table below is the shared contract, named case-for-case with
// packages/agent/ui/frontend/tests/lib/classify.test.ts so the two can be
// diffed by eye. A case added on
// one side and not the other should look obviously missing.
func TestClassify_SharedDecisionTable(t *testing.T) {
	cases := []struct {
		name string
		ev   event.Event
		want Classification
	}{
		{
			name: "nothing matched and nothing happened is untracked",
			ev:   event.Event{},
			want: ClassUntracked,
		},
		{
			// The case the defect dropped. A denial is evidence that policy
			// ACTED; whether a rule id got stamped is a detail of how the
			// verdict was reached, not a reason to discard it.
			name: "deny with no rule id is blocked, not untracked",
			ev:   event.Event{Action: "deny"},
			want: ClassBlocked,
		},
		{
			name: "hook rejection with no rule id is blocked",
			ev:   event.Event{HookDecision: "REJECT_HARD"},
			want: ClassBlocked,
		},
		{
			name: "hook block_soft with no rule id is blocked",
			ev:   event.Event{HookDecision: "BLOCK_SOFT"},
			want: ClassBlocked,
		},
		{
			name: "hook approve with no rule id is processed",
			ev:   event.Event{HookDecision: "APPROVE"},
			want: ClassProcessed,
		},
		{
			name: "matched rule, hook approved is processed",
			ev:   event.Event{DomainRuleID: "r1", HookDecision: "APPROVE"},
			want: ClassProcessed,
		},
		{
			name: "matched rule, hook rejected is blocked",
			ev:   event.Event{DomainRuleID: "r1", HookDecision: "reject_hard"},
			want: ClassBlocked,
		},
		{
			name: "matched rule, bump failed",
			ev:   event.Event{DomainRuleID: "r1", BumpStatus: "BUMP_FAILED"},
			want: ClassBumpFailed,
		},
		{
			name: "matched rule, bump failed passthrough",
			ev:   event.Event{DomainRuleID: "r1", BumpStatus: "BUMP_FAILED_PASSTHROUGH"},
			want: ClassBumpFailed,
		},
		{
			// Was handled only on the TypeScript side.
			name: "matched rule, bump failed mint fallback relay",
			ev:   event.Event{DomainRuleID: "r1", BumpStatus: "BUMP_FAILED_MINT_FALLBACK_RELAY"},
			want: ClassBumpFailed,
		},
		{
			name: "matched rule, transport error is a bump failure",
			ev:   event.Event{DomainRuleID: "r1", ErrorCode: "TLS_HANDSHAKE"},
			want: ClassBumpFailed,
		},
		{
			// Gating the bump-failure branch on DomainRuleID is what keeps a
			// transport error on a host nobody asked to inspect out of the
			// upload: we never wanted to bump it, so it did not fail to bump.
			name: "transport error with no rule id is untracked, not a bump failure",
			ev:   event.Event{ErrorCode: "TLS_HANDSHAKE"},
			want: ClassUntracked,
		},
		{
			name: "matched rule, no verdict is inspect",
			ev:   event.Event{DomainRuleID: "r1"},
			want: ClassInspect,
		},
		{
			// Legacy row from an older daemon: the verb carries the match,
			// no rule id was recorded. The TypeScript side has always
			// honoured this; matched() is where both sides now say so.
			name: "legacy action=inspect with no rule id is inspect, not untracked",
			ev:   event.Event{Action: "inspect"},
			want: ClassInspect,
		},
		{
			name: "legacy action=inspect with a bump failure is a bump failure",
			ev:   event.Event{Action: "inspect", BumpStatus: "BUMP_FAILED"},
			want: ClassBumpFailed,
		},
		{
			// Bump failure still beats hook outcome: a flow that never bumped
			// cannot have run hooks, whatever HookDecision was stamped.
			name: "bump failure outranks a stamped hook decision",
			ev:   event.Event{DomainRuleID: "r1", BumpStatus: "BUMP_FAILED", HookDecision: "APPROVE"},
			want: ClassBumpFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.ev); got != tc.want {
				t.Errorf("Classify(%+v) = %q, want %q", tc.ev, got, tc.want)
			}
		})
	}
}

// The consequence arm. Classification only matters because ShouldUpload reads
// it, and at the SHIPPED DEFAULT level "processed" the difference between
// untracked and blocked is the difference between a denial an operator can see
// and one they cannot.
func TestClassify_DenialReachesTheConsoleAtTheDefaultLevel(t *testing.T) {
	const shippedDefault = "processed"

	for _, ev := range []event.Event{
		{Action: "deny"},                     // no rule id
		{HookDecision: "REJECT_HARD"},        // no rule id
		{DomainRuleID: "r1", Action: "deny"}, /* with rule id */
	} {
		class := Classify(ev)
		if class != ClassBlocked {
			t.Errorf("Classify(%+v) = %q, want blocked", ev, class)
			continue
		}
		if !ShouldUpload(class, shippedDefault) {
			t.Errorf("a denial classified %q is not uploaded at the shipped default level %q — it never reaches the console",
				class, shippedDefault)
		}
	}

	// The sibling: a genuinely untracked flow is still withheld at that level,
	// which is the whole point of having the level.
	if ShouldUpload(Classify(event.Event{}), shippedDefault) {
		t.Error("an untracked flow is being uploaded at the default level — the level no longer filters anything")
	}
}
