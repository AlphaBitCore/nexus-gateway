//go:build linux

package linux

import (
	"fmt"
	"strings"
)

// canonicalScript builds the iptables-restore input for one family.
// Format envelope:
//
//	*nat
//	:CHAIN - [0:0]      ← create-or-flush the chain in one shot
//	-A CHAIN ...        ← rules
//	COMMIT
//
// The `:CHAIN - [0:0]` line is the standard idempotent
// create-or-flush in iptables-restore syntax: it ensures the chain
// exists AND starts empty before the `-A` lines append rules. With
// `--noflush`, no other chains are touched.
func (r *Reconciler) canonicalScript(family iptablesFamily) string {
	var b strings.Builder
	b.WriteString("*nat\n")
	fmt.Fprintf(&b, ":%s - [0:0]\n", chainName)
	for _, rule := range r.canonicalRules(family) {
		b.WriteString(rule + "\n")
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

// canonicalRules returns the deterministic `-A` rule lines for one
// family, in the exact order iptables emits them via `-S`. Used both
// for the restore script (joined into the envelope) and for drift
// comparison (compared line-by-line against dumpChain).
//
// Order MUST match iptables `-S` output for byte-exact comparison
// (iptables preserves insertion order, so order = rule order here).
func (r *Reconciler) canonicalRules(family iptablesFamily) []string {
	// Note: iptables -S normalises the mark to lowercase hex
	// (--mark 0x4e58). We emit the same form so byte-comparison
	// works without a re-canonicalisation step.
	rules := []string{
		fmt.Sprintf("-A %s -m mark --mark 0x%x -j RETURN", chainName, AgentSOMark),
	}
	if family == familyV4 {
		rules = append(rules,
			fmt.Sprintf("-A %s -d 127.0.0.0/8 -j RETURN", chainName))
	} else {
		rules = append(rules,
			fmt.Sprintf("-A %s -d ::1/128 -j RETURN", chainName))
	}
	rules = append(rules,
		fmt.Sprintf("-A %s -p tcp -j REDIRECT --to-ports %d", chainName, r.proxyPort))
	return rules
}

// rulesEqual compares two rule slices line-by-line, treating whitespace
// runs as equivalent (iptables sometimes emits double spaces between
// args on certain kernels) and case-insensitive (older kernels emit
// hex in uppercase).
func rulesEqual(live, canonical []string) bool {
	if len(live) != len(canonical) {
		return false
	}
	for i := range live {
		if !ruleLineEqual(live[i], canonical[i]) {
			return false
		}
	}
	return true
}

// ruleLineEqual normalises two `-A …` lines for comparison. Splits on
// runs of whitespace, lowercases, rejoins — robust to the small
// formatting variations across iptables versions.
func ruleLineEqual(a, b string) bool {
	return canonRule(a) == canonRule(b)
}

func canonRule(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}
