package decision

import "testing"

func TestActionFromLegacy(t *testing.T) {
	cases := []struct {
		inflight InflightAction
		storage  StorageAction
		want     Action
	}{
		{InflightBlockHard, StorageRedact, ActionBlock},
		{InflightBlockSoft, StorageRedact, ActionBlock},
		{InflightRedact, StorageKeep, ActionRedact},
		{InflightApprove, StorageKeep, ActionApprove},
		{InflightApprove, "", ActionApprove},
		{InflightApprove, StorageRedact, ActionRedact},      // pathological → safe-upgrade
		{InflightApprove, StorageDropContent, ActionRedact}, // pathological → safe-upgrade
		{"", "", ActionBlock},                               // legacy defaults
	}
	for _, c := range cases {
		if got := ActionFromLegacy(c.inflight, c.storage); got != c.want {
			t.Errorf("ActionFromLegacy(%q,%q)=%q want %q", c.inflight, c.storage, got, c.want)
		}
	}
}

func TestActionValid(t *testing.T) {
	if !ActionApprove.Valid() || !ActionRedact.Valid() || !ActionBlock.Valid() {
		t.Fatal("the three real actions must be Valid")
	}
	if Action("bogus").Valid() || Action("").Valid() {
		t.Fatal("unknown action must be invalid")
	}
}

func TestCarriesRedaction(t *testing.T) {
	cases := []struct {
		name string
		r    *CompliancePipelineResult
		want bool
	}{
		{"nil", nil, false},
		{"modify", &CompliancePipelineResult{Decision: Modify}, true},
		// The BlockSoft branch now keys on RedactionApplicable (set by mergeResults),
		// NOT raw span/content presence — so a co-firing redact masked by soft-block
		// (flag true) carries, while a soft-block whose only spans are advisory/audit-
		// only (flag false) does not, even if a ModifiedContent slice is present on the
		// struct. This documents that the merge-computed flag is authoritative.
		{"blocksoft-applicable", &CompliancePipelineResult{Decision: BlockSoft, RedactionApplicable: true}, true},
		{"blocksoft-flag-false-ignores-raw-content", &CompliancePipelineResult{Decision: BlockSoft, ModifiedContent: []ContentBlock{{}}}, false},
		{"blocksoft-without-redaction", &CompliancePipelineResult{Decision: BlockSoft}, false},
		{"approve", &CompliancePipelineResult{Decision: Approve}, false},
		{"rejecthard", &CompliancePipelineResult{Decision: RejectHard}, false},
	}
	for _, c := range cases {
		if got := c.r.CarriesRedaction(); got != c.want {
			t.Errorf("%s: CarriesRedaction()=%v want %v", c.name, got, c.want)
		}
	}
}
