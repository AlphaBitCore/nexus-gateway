//go:build linux

package linux

import (
	"log/slog"
	"strings"
	"testing"
)

// TestCanonicalRules_v4 pins the exact rule lines the reconciler will
// install for IPv4. If iptables ever changes its `-S` normalisation
// these will need updating — caught by AC-L1b drift detection in CI
// integration tests against real iptables.
func TestCanonicalRules_v4(t *testing.T) {
	r := NewReconciler(slog.Default(), 19080)
	rules := r.canonicalRules(familyV4)

	want := []string{
		"-A NEXUS_AGENT -m mark --mark 0x4e58 -j RETURN",
		"-A NEXUS_AGENT -d 127.0.0.0/8 -j RETURN",
		"-A NEXUS_AGENT -p tcp -j REDIRECT --to-ports 19080",
	}
	if len(rules) != len(want) {
		t.Fatalf("v4 rule count: got %d, want %d", len(rules), len(want))
	}
	for i, r := range rules {
		if r != want[i] {
			t.Errorf("v4 rule[%d]:\n  got:  %q\n  want: %q", i, r, want[i])
		}
	}
}

// TestCanonicalRules_v6 verifies the v6 variant uses the loopback /128
// instead of 127/8, and otherwise matches the v4 shape.
func TestCanonicalRules_v6(t *testing.T) {
	r := NewReconciler(slog.Default(), 19080)
	rules := r.canonicalRules(familyV6)

	want := []string{
		"-A NEXUS_AGENT -m mark --mark 0x4e58 -j RETURN",
		"-A NEXUS_AGENT -d ::1/128 -j RETURN",
		"-A NEXUS_AGENT -p tcp -j REDIRECT --to-ports 19080",
	}
	for i, r := range rules {
		if r != want[i] {
			t.Errorf("v6 rule[%d]:\n  got:  %q\n  want: %q", i, r, want[i])
		}
	}
}

// TestCanonicalScript verifies the iptables-restore envelope shape:
// `*nat`, `:CHAIN - [0:0]`, the rule lines in order, `COMMIT`,
// trailing newline. iptables-restore is strict about this format
// (no leading whitespace, COMMIT must be on its own line).
func TestCanonicalScript(t *testing.T) {
	r := NewReconciler(slog.Default(), 8080)
	script := r.canonicalScript(familyV4)

	wantLines := []string{
		"*nat",
		":NEXUS_AGENT - [0:0]",
		"-A NEXUS_AGENT -m mark --mark 0x4e58 -j RETURN",
		"-A NEXUS_AGENT -d 127.0.0.0/8 -j RETURN",
		"-A NEXUS_AGENT -p tcp -j REDIRECT --to-ports 8080",
		"COMMIT",
		"", // trailing newline gives an empty element after Split
	}
	got := strings.Split(script, "\n")
	if len(got) != len(wantLines) {
		t.Fatalf("script line count: got %d, want %d. Script:\n%s",
			len(got), len(wantLines), script)
	}
	for i, line := range got {
		if line != wantLines[i] {
			t.Errorf("script line[%d]:\n  got:  %q\n  want: %q", i, line, wantLines[i])
		}
	}
}

// TestRulesEqual covers the formatting tolerance: iptables sometimes
// emits double spaces or different case in hex on certain kernels.
// rulesEqual must normalise both sides.
func TestRulesEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"identical", []string{"-A X -j Y"}, []string{"-A X -j Y"}, true},
		{"diff len", []string{"-A X"}, []string{"-A X", "-A Y"}, false},
		{"diff rule", []string{"-A X -j Y"}, []string{"-A X -j Z"}, false},
		{"case diff", []string{"-A X --mark 0x4E58 -j RETURN"}, []string{"-A X --mark 0x4e58 -j RETURN"}, true},
		{"whitespace diff", []string{"-A X  -j  Y"}, []string{"-A X -j Y"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rulesEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("rulesEqual(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestPortFromAddr covers the addr-string → port int helper used to
// configure the REDIRECT target.
func TestPortFromAddr(t *testing.T) {
	cases := []struct {
		addr    string
		want    int
		wantErr bool
	}{
		{"127.0.0.1:19080", 19080, false},
		{"0.0.0.0:8080", 8080, false},
		{":3000", 3000, false},
		{"127.0.0.1:abc", 0, true},
		{"no-colon", 0, true},
		{"127.0.0.1:0", 0, true},
		{"127.0.0.1:65536", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			got, err := portFromAddr(tc.addr)
			if tc.wantErr {
				if err == nil {
					t.Errorf("portFromAddr(%q): expected error, got port=%d", tc.addr, got)
				}
				return
			}
			if err != nil {
				t.Errorf("portFromAddr(%q): %v", tc.addr, err)
				return
			}
			if got != tc.want {
				t.Errorf("portFromAddr(%q) = %d, want %d", tc.addr, got, tc.want)
			}
		})
	}
}
