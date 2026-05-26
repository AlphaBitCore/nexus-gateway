package policy

import (
	"sort"
	"testing"
)

func TestIsOverridable(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"credentials", false},
		{"virtual_keys", false},
		{"routing_rules", true},
		{"hooks", true},
		{"killswitch", true}, // explicitly NOT in blacklist
		{"observability", true},
		{"", true}, // empty is technically overridable; CP handler rejects empty separately
	}
	for _, c := range cases {
		if got := IsOverridable(c.key); got != c.want {
			t.Errorf("IsOverridable(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}

func TestIsBlacklisted(t *testing.T) {
	// IsBlacklisted is the positive inverse of IsOverridable; both must
	// stay in lock-step so a test catches one drifting away from the other.
	cases := []struct {
		key  string
		want bool
	}{
		{"credentials", true},
		{"virtual_keys", true},
		{"routing_rules", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsBlacklisted(c.key); got != c.want {
			t.Errorf("IsBlacklisted(%q) = %v, want %v", c.key, got, c.want)
		}
		if IsBlacklisted(c.key) == IsOverridable(c.key) {
			t.Errorf("IsBlacklisted(%q) and IsOverridable(%q) must be inverses", c.key, c.key)
		}
	}
}

func TestBlacklistedKeys_StableContents(t *testing.T) {
	// The blacklist must contain exactly these two keys; any other key
	// is treated as overridable. Adding/removing entries here is a
	// deliberate policy change that requires spec sign-off.
	got := BlacklistedKeys()
	sort.Strings(got)
	want := []string{"credentials", "virtual_keys"}
	if len(got) != len(want) {
		t.Fatalf("unexpected blacklist size: got %d, want %d (%v)", len(got), len(want), got)
	}
	for i, k := range want {
		if got[i] != k {
			t.Errorf("BlacklistedKeys()[%d] = %q, want %q", i, got[i], k)
		}
	}
}

func TestBlacklistedKeys_ReturnsCopy(t *testing.T) {
	// Mutating the returned slice must NOT alter the underlying policy —
	// that's the whole point of unexporting nonOverridableConfigKeys.
	keys := BlacklistedKeys()
	if len(keys) == 0 {
		t.Fatal("expected non-empty blacklist")
	}
	keys[0] = "MUTATED"
	if !IsBlacklisted("credentials") || !IsBlacklisted("virtual_keys") {
		t.Errorf("BlacklistedKeys() exposed underlying map: post-mutation Is checks failed")
	}
}
