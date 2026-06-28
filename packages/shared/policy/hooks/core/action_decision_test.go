package core

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
)

func TestDecisionForAction(t *testing.T) {
	cases := []struct {
		in   decision.Action
		want Decision
	}{
		{decision.ActionApprove, Approve},
		{decision.ActionRedact, Modify},
		{decision.ActionBlock, RejectHard},
		{decision.Action("garbage"), RejectHard}, // safe default
	}
	for _, c := range cases {
		if got := DecisionForAction(c.in); got != c.want {
			t.Errorf("DecisionForAction(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

func TestStrictestAction(t *testing.T) {
	cases := []struct {
		a, b, want decision.Action
	}{
		{decision.ActionRedact, decision.ActionBlock, decision.ActionBlock},
		{decision.ActionBlock, decision.ActionRedact, decision.ActionBlock},
		{decision.ActionApprove, decision.ActionRedact, decision.ActionRedact},
		{decision.ActionRedact, decision.ActionApprove, decision.ActionRedact},
		{decision.ActionApprove, decision.ActionApprove, decision.ActionApprove},
		{"", decision.ActionApprove, decision.ActionApprove},
		{decision.ActionBlock, "", decision.ActionBlock},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := StrictestAction(c.a, c.b); got != c.want {
			t.Errorf("StrictestAction(%q,%q): got %q want %q", c.a, c.b, got, c.want)
		}
	}
}
