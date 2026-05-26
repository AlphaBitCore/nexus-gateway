package policy

import "testing"

func TestValidModelRiskTiers(t *testing.T) {
	tiers := ValidModelRiskTiers()
	if len(tiers) != 4 {
		t.Errorf("expected 4 tiers, got %d", len(tiers))
	}
	seen := map[ModelRiskTier]bool{}
	for _, tier := range tiers {
		if seen[tier] {
			t.Errorf("duplicate tier: %s", tier)
		}
		seen[tier] = true
	}
}

func TestValidModelApprovalStatuses(t *testing.T) {
	statuses := ValidModelApprovalStatuses()
	if len(statuses) != 4 {
		t.Errorf("expected 4 statuses, got %d", len(statuses))
	}
	seen := map[ModelApprovalStatus]bool{}
	for _, s := range statuses {
		if seen[s] {
			t.Errorf("duplicate status: %s", s)
		}
		seen[s] = true
	}
}
