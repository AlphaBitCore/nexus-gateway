package specutil

import (
	"strings"
	"testing"
)

// TestValidateEmbeddingRowCount pins the F-0220 guard: a request/response
// count mismatch is converted into a named error, while a matching count
// (or an unknown request count) passes.
func TestValidateEmbeddingRowCount(t *testing.T) {
	cases := []struct {
		name     string
		expected int
		got      int
		wantErr  bool
	}{
		{"match", 3, 3, false},
		{"single match", 1, 1, false},
		{"mismatch fewer", 3, 2, true},
		{"mismatch more", 2, 3, true},
		{"unknown request count disables guard", 0, 2, false},
		{"negative request count disables guard", -1, 5, false},
		{"both zero", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateEmbeddingRowCount(tc.expected, tc.got)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for expected=%d got=%d", tc.expected, tc.got)
				}
				// The message must name both counts so operators can triage.
				for _, frag := range []string{"embedding count mismatch", "input", "embedding"} {
					if !strings.Contains(err.Error(), frag) {
						t.Errorf("error %q missing fragment %q", err.Error(), frag)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for expected=%d got=%d: %v", tc.expected, tc.got, err)
			}
		})
	}
}
