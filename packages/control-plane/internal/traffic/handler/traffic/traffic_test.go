package traffic

import "testing"

func TestParseExcludeInternalParam_DefaultTrue(t *testing.T) {
	if !parseExcludeInternalParam("") {
		t.Fatal("empty excludeInternal should default to true")
	}
}

func TestParseExcludeInternalParam_FalseLikeValuesStillExcludeInternal(t *testing.T) {
	cases := []string{"false", "FALSE", "0", "no", "off", " false "}
	for _, v := range cases {
		if !parseExcludeInternalParam(v) {
			t.Fatalf("value %q should still exclude internal rows", v)
		}
	}
}

func TestParseExcludeInternalParam_TrueLikeValuesIncludeInternal(t *testing.T) {
	cases := []string{"true", "1", "yes", "on", "TRUE"}
	for _, v := range cases {
		if parseExcludeInternalParam(v) {
			t.Fatalf("value %q should include internal rows", v)
		}
	}
}

func TestParseCacheStatusParam(t *testing.T) {
	tests := []struct {
		raw     string
		want    *string
		wantErr bool
	}{
		// Empty drops the filter.
		{"", nil, false},
		// Only the unified values are valid.
		{"HIT", strPtr("HIT"), false},
		{"MISS", strPtr("MISS"), false},
		// Older gateway-internal enum values are explicitly rejected —
		// drill-down on those lives in the audit drawer, not in the filter.
		{"HIT_LIVE", nil, true},
		{"DISABLED", nil, true},
		{"SKIP_NO_CACHE", nil, true},
		{"PASSTHROUGH_SKIP", nil, true},
		// Other invalid values also reject.
		{"SKIP_STREAM", nil, true},
		// case-sensitive — UI emits uppercase only.
		{"hit", nil, true},
		{"true", nil, true},
		{"false", nil, true},
		{"<script>", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseCacheStatusParam(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseCacheStatusParam(%q) wantErr but got nil", tc.raw)
				}
				if got != nil {
					t.Fatalf("parseCacheStatusParam(%q) wantErr but got value %q", tc.raw, *got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCacheStatusParam(%q) unexpected error: %v", tc.raw, err)
			}
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("parseCacheStatusParam(%q) = %q, want nil", tc.raw, *got)
			case tc.want != nil && got == nil:
				t.Fatalf("parseCacheStatusParam(%q) = nil, want %q", tc.raw, *tc.want)
			case tc.want != nil && got != nil && *tc.want != *got:
				t.Fatalf("parseCacheStatusParam(%q) = %q, want %q", tc.raw, *got, *tc.want)
			}
		})
	}
}
