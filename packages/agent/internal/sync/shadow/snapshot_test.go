package shadow

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestParseSnapshot_EmptyMap(t *testing.T) {
	// An empty map produces an empty (but non-nil) snapshot with
	// FetchedAt stamped to "now" and zero version.
	before := time.Now()
	snap, err := ParseSnapshot(map[string]any{})
	if err != nil {
		t.Fatalf("ParseSnapshot: %v", err)
	}
	if snap == nil {
		t.Fatal("snapshot must not be nil on empty map")
	}
	if snap.Version != 0 {
		t.Errorf("Version: got %d, want 0", snap.Version)
	}
	if len(snap.HookConfigs) != 0 {
		t.Errorf("HookConfigs: got %d, want 0", len(snap.HookConfigs))
	}
	if len(snap.InterceptionDomains) != 0 {
		t.Errorf("InterceptionDomains: got %d, want 0", len(snap.InterceptionDomains))
	}
	if snap.FetchedAt.Before(before) {
		t.Errorf("FetchedAt must be stamped at parse-time; got %v vs before=%v", snap.FetchedAt, before)
	}
}

func TestParseSnapshot_AllFieldsPopulated(t *testing.T) {
	in := map[string]any{
		"configVersion": 12,
		"interceptionDomains": []any{
			map[string]any{
				"id":                "dom-1",
				"name":              "openai",
				"hostPattern":       "api.openai.com",
				"hostMatchType":     "EXACT",
				"adapterId":         "openai-compat",
				"enabled":           true,
				"priority":          50,
				"defaultPathAction": "PROCESS",
				"onAdapterError":    "FAIL_OPEN",
				"networkZone":       "PUBLIC",
				"paths": []any{
					map[string]any{
						"id":          "p-1",
						"pathPattern": []any{"/v1/chat/completions"},
						"matchType":   "PREFIX",
						"action":      "PROCESS",
						"priority":    1,
						"enabled":     true,
					},
				},
			},
		},
	}
	snap, err := ParseSnapshot(in)
	if err != nil {
		t.Fatalf("ParseSnapshot: %v", err)
	}
	if snap.Version != 12 {
		t.Errorf("Version: got %d, want 12", snap.Version)
	}
	if len(snap.InterceptionDomains) != 1 {
		t.Fatalf("InterceptionDomains: got %d, want 1", len(snap.InterceptionDomains))
	}
	d := snap.InterceptionDomains[0]
	if d.ID != "dom-1" || d.HostPattern != "api.openai.com" || d.AdapterID != "openai-compat" {
		t.Errorf("InterceptionDomain fields: %+v", d)
	}
	if len(d.Paths) != 1 || d.Paths[0].ID != "p-1" {
		t.Errorf("Paths not parsed: %+v", d.Paths)
	}
}

func TestParseSnapshot_MarshalError(t *testing.T) {
	// json.Marshal fails on a value with a NaN float — chan can't be
	// supplied via map[string]any in vanilla tests, but NaN is the
	// canonical way to provoke a marshal error from encoding/json.
	in := map[string]any{"bogus": math.NaN()}
	snap, err := ParseSnapshot(in)
	if err == nil {
		t.Fatal("ParseSnapshot must error when json.Marshal fails")
	}
	if snap != nil {
		t.Fatalf("on error, snapshot must be nil; got %+v", snap)
	}
	if !strings.Contains(err.Error(), "configsync: marshal config") {
		t.Errorf("error must be wrapped with 'configsync: marshal config'; got %v", err)
	}
}

func TestParseSnapshot_UnmarshalError(t *testing.T) {
	// A type mismatch on a known field (string in place of int for
	// configVersion) makes json.Unmarshal fail.
	in := map[string]any{"configVersion": "not-an-int"}
	snap, err := ParseSnapshot(in)
	if err == nil {
		t.Fatal("ParseSnapshot must error when json.Unmarshal fails")
	}
	if snap != nil {
		t.Fatalf("on error, snapshot must be nil; got %+v", snap)
	}
	if !strings.Contains(err.Error(), "configsync: unmarshal config") {
		t.Errorf("error must be wrapped with 'configsync: unmarshal config'; got %v", err)
	}
}
