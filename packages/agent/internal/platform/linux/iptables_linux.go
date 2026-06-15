//go:build linux

package linux

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// iptablesFamily identifies which kernel netfilter family a set of
// rules targets. The wrapper uses `iptables` / `ip6tables` accordingly
// — both binaries are present on every distro that ships netfilter,
// including modern nft-only systems where they're the iptables-nft
// compat shim (Ubuntu 22+, Debian 11+, Fedora 35+, RHEL 9+).
type iptablesFamily int

const (
	familyV4 iptablesFamily = iota
	familyV6
)

// String returns the binary name for the family, e.g. "iptables" or
// "ip6tables".
func (f iptablesFamily) String() string {
	if f == familyV6 {
		return "ip6tables"
	}
	return "iptables"
}

// restoreBin returns the matching -restore tool name (e.g.
// "iptables-restore" / "ip6tables-restore"). Both are part of the
// `iptables` package on every distro that ships iptables; we assume
// availability and surface a clear error at install time if missing.
func (f iptablesFamily) restoreBin() string {
	if f == familyV6 {
		return "ip6tables-restore"
	}
	return "iptables-restore"
}

// applyChain feeds an iptables-restore script to the kernel in a
// single atomic transaction. `--noflush` means only chains explicitly
// listed in the script are touched — unrelated chains (firewalld's,
// the user's, docker's) are preserved.
//
// The script must include the `*nat\n…\nCOMMIT\n` envelope.
func applyChain(ctx context.Context, family iptablesFamily, restoreScript string) error {
	cmd := exec.CommandContext(ctx, family.restoreBin(), "--noflush")
	cmd.Stdin = strings.NewReader(restoreScript)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w (stderr=%q)",
			family.restoreBin(), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// chainAbsentErr reports whether an `iptables -t nat -S <chain>` stderr
// indicates the chain simply does not exist yet, as opposed to a real
// failure. The wording differs across netfilter backends:
//
//   - legacy iptables: "No chain/target/match by that name".
//   - iptables-nft:    "chain `X' in table `nat' is incompatible, use 'nft'
//     tool." — emitted when probing a NON-EXISTENT chain in a `nat` table
//     that ALSO holds nft-native rules (libvirt, docker, kube-proxy — i.e.
//     most real servers). The chain is genuinely absent; iptables-nft just
//     can't enumerate a missing chain cleanly once the table carries nft
//     constructs it can't translate.
//
// Both mean "absent → install it". Treating the nft variant as a hard error
// (the original behaviour) disabled interception on every such host even
// though `iptables-restore` installs and manages the chain there without
// issue — once the chain exists, `-S <chain>` reads it back fine.
func chainAbsentErr(stderr string) bool {
	if strings.Contains(stderr, "No chain") {
		return true
	}
	return strings.Contains(stderr, "incompatible") && strings.Contains(stderr, "nft")
}

// dumpChain returns the current rule lines of the named chain in the
// `nat` table for the given family, normalised by stripping the
// `iptables -t nat` prefix every line carries. Empty result + nil
// error means the chain does not exist.
//
// Output shape (one rule per line, e.g.):
//
//	-N NEXUS_AGENT
//	-A NEXUS_AGENT -m mark --mark 0x4e58 -j RETURN
//	-A NEXUS_AGENT -d 127.0.0.0/8 -j RETURN
//	-A NEXUS_AGENT -p tcp -j REDIRECT --to-ports 19080
//
// We strip the leading `-N` line (chain creation) and any trailing
// whitespace, leaving just the `-A` rules for drift comparison.
func dumpChain(ctx context.Context, family iptablesFamily, chain string) ([]string, error) {
	cmd := exec.CommandContext(ctx, family.String(), "-t", "nat", "-S", chain)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// A non-zero exit here usually just means the chain does not exist
		// yet (first install) — treat that as an empty result so the
		// reconciler proceeds to install it. The "chain absent" wording
		// differs by backend; see chainAbsentErr.
		if chainAbsentErr(stderr.String()) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s -t nat -S %s: %w (stderr=%q)",
			family.String(), chain, err, strings.TrimSpace(stderr.String()))
	}

	var rules []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-N ") {
			continue
		}
		rules = append(rules, line)
	}
	return rules, nil
}

// ensureHook idempotently appends `-A OUTPUT -j <chain>` to the nat
// table's OUTPUT chain so traffic flows through our redirect chain.
// `iptables -C` returns 0 if the rule exists, 1 if it doesn't —
// we use that to skip the append on subsequent invocations.
func ensureHook(ctx context.Context, family iptablesFamily, chain string) error {
	check := exec.CommandContext(ctx,
		family.String(), "-t", "nat", "-C", "OUTPUT", "-j", chain)
	check.Stderr = &bytes.Buffer{} // swallow "rule does not exist" noise
	if err := check.Run(); err == nil {
		// Rule already present.
		return nil
	}
	add := exec.CommandContext(ctx,
		family.String(), "-t", "nat", "-A", "OUTPUT", "-j", chain)
	var stderr bytes.Buffer
	add.Stderr = &stderr
	if err := add.Run(); err != nil {
		return fmt.Errorf("%s -t nat -A OUTPUT -j %s: %w (stderr=%q)",
			family.String(), chain, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// removeChain best-effort tears down the named chain + its OUTPUT
// hook + its rules. Each step uses `|| true` semantics — failures
// are logged but don't propagate, so the caller can run this on
// shutdown without worrying about already-removed-by-someone-else
// races. Returns the FIRST error encountered for telemetry, but
// always attempts every step.
func removeChain(ctx context.Context, family iptablesFamily, chain string) error {
	var firstErr error
	step := func(args ...string) {
		cmd := exec.CommandContext(ctx, family.String(), args...)
		cmd.Stderr = &bytes.Buffer{}
		if err := cmd.Run(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s %s: %w",
				family.String(), strings.Join(args, " "), err)
		}
	}
	// 1. Unhook from OUTPUT (idempotent — fails harmlessly if absent).
	step("-t", "nat", "-D", "OUTPUT", "-j", chain)
	// 2. Flush all rules in the chain.
	step("-t", "nat", "-F", chain)
	// 3. Delete the chain itself.
	step("-t", "nat", "-X", chain)
	return firstErr
}
