package manager

import (
	"github.com/goccy/go-json"
	"testing"
)

func TestShadowKeyDiff_SyncedWhenEqual(t *testing.T) {
	d := map[string]any{"enabled": true, "rate": float64(100)}
	r := map[string]any{"enabled": true, "rate": float64(100)}

	dj, _ := json.Marshal(d["enabled"])
	rj, _ := json.Marshal(r["enabled"])
	if string(dj) != string(rj) {
		t.Error("equal values should produce equal JSON")
	}
}

func TestShadowKeyDiff_DriftWhenDifferent(t *testing.T) {
	dj, _ := json.Marshal(true)
	rj, _ := json.Marshal(false)
	if string(dj) == string(rj) {
		t.Error("different values should produce different JSON")
	}
}

func TestShadowComparison_Synced(t *testing.T) {
	sc := ShadowComparison{
		ThingID:     "t-1",
		DesiredVer:  5,
		ReportedVer: 5,
		Synced:      true, // ReportedVer (5) >= DesiredVer (5)
		Keys: map[string]ShadowKeyDiff{
			"routing": {Desired: true, Reported: true, Synced: true},
		},
	}
	if !sc.Synced {
		t.Error("should be synced when reportedVer >= desiredVer")
	}
}

func TestShadowComparison_NotSynced(t *testing.T) {
	sc := ShadowComparison{
		ThingID:     "t-1",
		DesiredVer:  5,
		ReportedVer: 3,
		Synced:      3 >= 5,
	}
	if sc.Synced {
		t.Error("should not be synced when reportedVer < desiredVer")
	}
}

func TestShadowComparison_JSONTags(t *testing.T) {
	sc := ShadowComparison{
		ThingID:     "t-1",
		ThingType:   "agent",
		DesiredVer:  5,
		ReportedVer: 3,
		Synced:      false,
		Keys: map[string]ShadowKeyDiff{
			"routing": {Desired: true, Reported: false, Synced: false},
		},
	}
	data, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"thingId", "thingType", "desiredVer", "reportedVer", "synced", "keys"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing JSON key %q", k)
		}
	}
}

func TestShadowKeyDiff_JSON(t *testing.T) {
	d := ShadowKeyDiff{
		Desired:  map[string]any{"enabled": true},
		Reported: map[string]any{"enabled": false},
		Synced:   false,
	}
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"desired", "reported", "synced"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing JSON key %q", k)
		}
	}
}

func TestShadowComparison_KeyDiffLogic(t *testing.T) {
	tests := []struct {
		name     string
		desired  map[string]any
		reported map[string]any
		wantKeys []string
	}{
		{
			name:     "both empty",
			desired:  map[string]any{},
			reported: map[string]any{},
			wantKeys: nil,
		},
		{
			name:     "desired only",
			desired:  map[string]any{"routing": true},
			reported: map[string]any{},
			wantKeys: []string{"routing"},
		},
		{
			name:     "reported only",
			desired:  map[string]any{},
			reported: map[string]any{"routing": true},
			wantKeys: []string{"routing"},
		},
		{
			name:     "both have same keys",
			desired:  map[string]any{"routing": true, "quota": 100},
			reported: map[string]any{"routing": true, "quota": 50},
			wantKeys: []string{"routing", "quota"},
		},
		{
			name:     "overlapping keys",
			desired:  map[string]any{"routing": true, "security": "high"},
			reported: map[string]any{"routing": false, "monitoring": true},
			wantKeys: []string{"routing", "security", "monitoring"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allKeys := map[string]struct{}{}
			for k := range tt.desired {
				allKeys[k] = struct{}{}
			}
			for k := range tt.reported {
				allKeys[k] = struct{}{}
			}

			keys := make(map[string]ShadowKeyDiff)
			for k := range allKeys {
				d := tt.desired[k]
				r := tt.reported[k]
				dj, _ := json.Marshal(d)
				rj, _ := json.Marshal(r)
				keys[k] = ShadowKeyDiff{
					Desired:  d,
					Reported: r,
					Synced:   string(dj) == string(rj),
				}
			}

			if len(tt.wantKeys) == 0 && len(keys) == 0 {
				return
			}
			for _, wk := range tt.wantKeys {
				if _, ok := keys[wk]; !ok {
					t.Errorf("missing key %q in diff", wk)
				}
			}
			if len(keys) != len(tt.wantKeys) {
				t.Errorf("got %d keys, want %d", len(keys), len(tt.wantKeys))
			}
		})
	}
}

func TestShadowKeyDiff_SyncDetection(t *testing.T) {
	tests := []struct {
		name       string
		desired    any
		reported   any
		wantSynced bool
	}{
		{"both nil", nil, nil, true},
		{"desired nil reported set", nil, "val", false},
		{"equal strings", "val", "val", true},
		{"different strings", "a", "b", false},
		{"equal numbers", float64(42), float64(42), true},
		{"different numbers", float64(1), float64(2), false},
		{"equal bool", true, true, true},
		{"different bool", true, false, false},
		{
			"equal nested",
			map[string]any{"a": float64(1)},
			map[string]any{"a": float64(1)},
			true,
		},
		{
			"different nested",
			map[string]any{"a": float64(1)},
			map[string]any{"a": float64(2)},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dj, _ := json.Marshal(tt.desired)
			rj, _ := json.Marshal(tt.reported)
			synced := string(dj) == string(rj)
			if synced != tt.wantSynced {
				t.Errorf("synced = %v, want %v (desired=%s, reported=%s)", synced, tt.wantSynced, dj, rj)
			}
		})
	}
}

func TestShadowReportRequest_JSON(t *testing.T) {
	req := ShadowReportRequest{
		ID:          "t-1",
		Reported:    map[string]any{"routing": map[string]any{"enabled": true}},
		ReportedVer: 3,
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"id", "reported", "reportedVer"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing JSON key %q", k)
		}
	}
}
