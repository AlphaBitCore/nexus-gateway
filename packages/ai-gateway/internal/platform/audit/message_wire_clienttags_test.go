package audit

import (
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

// TestBuildDetailsClientTagsPresent pins that the caller's tags reach the
// details object under the key analytics queries will address.
func TestBuildDetailsClientTagsPresent(t *testing.T) {
	dw := buildDetails(&Record{
		RequestID:  "req-1",
		ClientTags: map[string]string{"tenant_id": "42", "billing_check": "CHECKED"},
	})

	raw, err := json.Marshal(dw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		ClientTags map[string]string `json:"clientTags"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ClientTags["tenant_id"] != "42" {
		t.Errorf("tenant_id = %q, want %q", got.ClientTags["tenant_id"], "42")
	}
	if got.ClientTags["billing_check"] != "CHECKED" {
		t.Errorf("billing_check = %q, want %q", got.ClientTags["billing_check"], "CHECKED")
	}
}

// TestBuildDetailsClientTagsOmittedWhenEmpty is the compatibility guard: a
// record without tags must serialize exactly as it did before this field
// existed, so no existing traffic row's details JSON changes shape.
func TestBuildDetailsClientTagsOmittedWhenEmpty(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags map[string]string
	}{
		{"nil map", nil},
		{"empty map", map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(buildDetails(&Record{RequestID: "req-1", ClientTags: tc.tags}))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(raw), "clientTags") {
				t.Errorf("details = %s, want no clientTags key", raw)
			}
		})
	}
}
