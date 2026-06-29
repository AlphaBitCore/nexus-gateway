package handler

import (
	"github.com/goccy/go-json"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/identity"
)

type fakeExemptionStore struct{ lastEntries []identity.ActiveExemption }

func (f *fakeExemptionStore) Rebuild(e []identity.ActiveExemption) { f.lastEntries = e }

func TestApplyActiveExemptions_RebuildsStore(t *testing.T) {
	store := &fakeExemptionStore{}
	state, _ := json.Marshal(identity.ActiveExemptions{
		Entries: []identity.ActiveExemption{
			{ID: "e1", SourceIP: "10.0.0.1", TargetHost: "x"},
		},
	})
	if err := ApplyActiveExemptions(store, state); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(store.lastEntries) != 1 || store.lastEntries[0].ID != "e1" {
		t.Fatalf("not rebuilt: %+v", store.lastEntries)
	}
}
