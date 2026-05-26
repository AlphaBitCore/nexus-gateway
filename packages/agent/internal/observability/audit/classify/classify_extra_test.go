// Coverage for classify.go: full Classify decision-tree matrix +
// ShouldUpload level × classification matrix.
//
// Binding [[feedback_agent_traffic_upload_level]]: trafficUploadLevel
// enum {all,processed,blocked}; default "processed"; deny/block/error
// (i.e. Blocked + BumpFailed) always bypass the filter. These tests
// pin that contract — Blocked + BumpFailed are upload-eligible at
// every level including the strictest "blocked"-only mode.
package classify

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/observability/audit/event"
)

// Classify — every decision-tree arm

func TestClassify_DecisionTreeFullMatrix(t *testing.T) {
	cases := []struct {
		name string
		ev   event.Event
		want Classification
	}{
		{
			name: "empty DomainRuleID → Untracked (first match wins)",
			ev:   event.Event{DomainRuleID: ""},
			want: ClassUntracked,
		},
		{
			name: "DomainRuleID set + ErrorCode set → BumpFailed",
			ev:   event.Event{DomainRuleID: "d1", ErrorCode: "AGENT_MTLS_FAILED"},
			want: ClassBumpFailed,
		},
		{
			name: "DomainRuleID set + BumpStatus=BUMP_FAILED → BumpFailed",
			ev:   event.Event{DomainRuleID: "d1", BumpStatus: "BUMP_FAILED"},
			want: ClassBumpFailed,
		},
		{
			name: "DomainRuleID set + BumpStatus=BUMP_FAILED_PASSTHROUGH → BumpFailed",
			ev:   event.Event{DomainRuleID: "d1", BumpStatus: "BUMP_FAILED_PASSTHROUGH"},
			want: ClassBumpFailed,
		},
		{
			name: "DomainRuleID set + HookDecision=REJECT_HARD (uppercase) → Blocked",
			ev:   event.Event{DomainRuleID: "d1", HookDecision: "REJECT_HARD", BumpStatus: "BUMP_SUCCESS"},
			want: ClassBlocked,
		},
		{
			name: "DomainRuleID set + HookDecision=reject_hard (lowercase) → Blocked",
			ev:   event.Event{DomainRuleID: "d1", HookDecision: "reject_hard", BumpStatus: "BUMP_SUCCESS"},
			want: ClassBlocked,
		},
		{
			name: "HookDecision=BLOCK_SOFT → Blocked",
			ev:   event.Event{DomainRuleID: "d1", HookDecision: "BLOCK_SOFT"},
			want: ClassBlocked,
		},
		{
			name: "HookDecision=DENY → Blocked",
			ev:   event.Event{DomainRuleID: "d1", HookDecision: "DENY"},
			want: ClassBlocked,
		},
		{
			name: "HookDecision=APPROVE (uppercase) → Processed",
			ev:   event.Event{DomainRuleID: "d1", HookDecision: "APPROVE"},
			want: ClassProcessed,
		},
		{
			name: "HookDecision=approve (lowercase) → Processed",
			ev:   event.Event{DomainRuleID: "d1", HookDecision: "approve"},
			want: ClassProcessed,
		},
		{
			name: "no HookDecision + Action=deny → Blocked",
			ev:   event.Event{DomainRuleID: "d1", Action: "deny"},
			want: ClassBlocked,
		},
		{
			name: "PathAction=PASSTHROUGH no hook → Inspect",
			ev:   event.Event{DomainRuleID: "d1", PathAction: "PASSTHROUGH"},
			want: ClassInspect,
		},
		{
			name: "PathAction=PROCESS no hook → Inspect (fallthrough)",
			ev:   event.Event{DomainRuleID: "d1", PathAction: "PROCESS"},
			want: ClassInspect,
		},
		{
			name: "DomainRuleID set with no other signals → Inspect (fallthrough)",
			ev:   event.Event{DomainRuleID: "d1"},
			want: ClassInspect,
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

// ShouldUpload — level × classification matrix
//
// Binding rule pinned here: Blocked + BumpFailed ALWAYS upload
// (across "all", "processed", "blocked") — these are the
// compliance-mandatory signals operators rely on.

func TestShouldUpload_LevelClassificationMatrix(t *testing.T) {
	type row struct {
		level string
		c     Classification
		want  bool
	}
	cases := []row{
		// level=all → everything.
		{"all", ClassUntracked, true},
		{"all", ClassInspect, true},
		{"all", ClassProcessed, true},
		{"all", ClassBlocked, true},
		{"all", ClassBumpFailed, true},

		// level=blocked → only Blocked + BumpFailed.
		{"blocked", ClassUntracked, false},
		{"blocked", ClassInspect, false},
		{"blocked", ClassProcessed, false},
		{"blocked", ClassBlocked, true},
		{"blocked", ClassBumpFailed, true},

		// level=processed (default) → Processed + Blocked + BumpFailed.
		{"processed", ClassUntracked, false},
		{"processed", ClassInspect, false},
		{"processed", ClassProcessed, true},
		{"processed", ClassBlocked, true},
		{"processed", ClassBumpFailed, true},

		// unknown level value MUST be treated as "processed" (binding:
		// fail-safe to the default; never silently downgrade to "all").
		{"weird-typo", ClassProcessed, true},
		{"weird-typo", ClassUntracked, false},
		{"", ClassProcessed, true},
		{"", ClassUntracked, false},
	}
	for _, tc := range cases {
		got := ShouldUpload(tc.c, tc.level)
		if got != tc.want {
			t.Errorf("ShouldUpload(%q, level=%q) = %v, want %v",
				tc.c, tc.level, got, tc.want)
		}
	}
}

// TestShouldUpload_BlockedAlwaysUploadsAcrossLevels is the binding-rule
// safety net: a single compliance Blocked event must reach Hub
// regardless of how restrictive the upload level is set to.
func TestShouldUpload_BlockedAlwaysUploadsAcrossLevels(t *testing.T) {
	for _, level := range []string{"all", "processed", "blocked", "garbage", ""} {
		if !ShouldUpload(ClassBlocked, level) {
			t.Errorf("Blocked event must always upload (level=%q)", level)
		}
		if !ShouldUpload(ClassBumpFailed, level) {
			t.Errorf("BumpFailed event must always upload (level=%q)", level)
		}
	}
}
