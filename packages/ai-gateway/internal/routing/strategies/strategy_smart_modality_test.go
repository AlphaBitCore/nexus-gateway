package strategies

import (
	"encoding/json"
	"os"
	"testing"
)

// TestSmart_OnlyChatRowsDeclareAModalityFloor guards the assumption that lets
// modalityAutoTargets skip capability filtering.
//
// For every non-chat kind the endpoint IS the capability, so filtering by kind
// — which the pipeline already did when it built the pool — answers the whole
// question. That holds only while no non-chat model declares a required-
// modality floor of its own. An image EDITING model that requires an input
// image would be offered to a plain text-to-image request, silently, because
// nothing on that path reads the floor.
//
// Asserted against the shipped catalogue rather than a written-down list: the
// day someone adds such a row, this is what says the assumption expired.
func TestSmart_OnlyChatRowsDeclareAModalityFloor(t *testing.T) {
	const fixture = "../../../../../tools/db-migrate/seed/fixtures/Model.json"
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read %s: %v — the shipped catalogue is what this assumption rests on; a "+
			"test that cannot find it silently stops checking", fixture, err)
	}
	var rows []struct {
		Code               string   `json:"code"`
		Type               string   `json:"type"`
		RequiredModalities []string `json:"requiredModalities"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("parse %s: %v", fixture, err)
	}
	if len(rows) == 0 {
		t.Fatalf("the catalogue fixture parsed to zero rows — the shape moved and this test is " +
			"now checking nothing")
	}

	floors := 0
	for _, r := range rows {
		if len(r.RequiredModalities) == 0 {
			continue
		}
		floors++
		if r.Type != "chat" {
			t.Errorf("%s is a %s model declaring a required-modality floor %v — non-chat auto "+
				"does no capability filtering, so this model is offered to every request on "+
				"its endpoint regardless of what the request actually carries",
				r.Code, r.Type, r.RequiredModalities)
		}
	}
	if floors == 0 {
		t.Errorf("no row in the catalogue declares a required-modality floor — this test then " +
			"passes for the wrong reason, because it would pass equally against a catalogue " +
			"that had lost the field entirely")
	}
}
