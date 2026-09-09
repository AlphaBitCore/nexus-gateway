package store_test

import (
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
)

// TestNewAuthDisposition covers the resolution both auth-column arms feed, and
// the direction of the status comparison.
//
// The arm that matters most is "suspended with a NULL disabledAt": that is what
// every disable surface in the product actually writes, and a verdict that
// called it permitted is how a suspended account kept signing in.
func TestNewAuthDisposition(t *testing.T) {
	disabled := time.Unix(1_700_000_000, 0).UTC()

	cases := []struct {
		name       string
		status     string
		disabledAt *time.Time
		wantBlock  bool
		wantReason string
	}{
		{
			name:   "active with no disable timestamp is permitted",
			status: "active",
		},
		{
			name:       "suspended is refused even though disabledAt is NULL",
			status:     "suspended",
			wantBlock:  true,
			wantReason: "suspended",
		},
		{
			name:       "a disabledAt timestamp refuses an otherwise-active row",
			status:     "active",
			disabledAt: &disabled,
			wantBlock:  true,
			wantReason: "disabled",
		},
		{
			name:       "an unrecognised status fails closed",
			status:     "deactivated",
			wantBlock:  true,
			wantReason: "deactivated",
		},
		{
			name:       "an empty status fails closed rather than defaulting to permitted",
			status:     "",
			wantBlock:  true,
			wantReason: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := store.NewAuthDisposition(tc.status, tc.disabledAt)
			if got.Blocked() != tc.wantBlock {
				t.Fatalf("Blocked(): got %v, want %v (status=%q disabledAt=%v)",
					got.Blocked(), tc.wantBlock, tc.status, tc.disabledAt)
			}
			if got.Reason() != tc.wantReason {
				t.Fatalf("Reason(): got %q, want %q", got.Reason(), tc.wantReason)
			}
		})
	}
}

// TestAuthDisposition_ZeroValuePermits pins the zero value, because store
// methods return it on their error paths and a zero value that blocked would
// turn a lookup failure into a silent account lockout.
func TestAuthDisposition_ZeroValuePermits(t *testing.T) {
	var d store.AuthDisposition
	if d.Blocked() {
		t.Fatal("the zero AuthDisposition must permit; error paths return it")
	}
	if d.Reason() != "" {
		t.Fatalf("zero value carries a reason: %q", d.Reason())
	}
}
