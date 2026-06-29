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
