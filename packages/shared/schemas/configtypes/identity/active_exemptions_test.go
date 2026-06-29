package identity

import (
	"github.com/goccy/go-json"
	"testing"
)

func TestActiveExemptions_JSONShape(t *testing.T) {
	in := ActiveExemptions{
		Entries: []ActiveExemption{
			{ID: "ex-1", SourceIP: "10.0.0.5", TargetHost: "api.vendor.com",
				ExpiresAt: "2026-04-21T00:00:00Z", Reason: "vendor outage", ApprovedBy: "alice"},
		},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out ActiveExemptions
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Entries) != 1 || out.Entries[0].ID != "ex-1" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}
