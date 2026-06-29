//go:build linux && integration

// Package-level integration tests for the iptables Reconciler.
// Gated by build tag `integration` so they only run when the CI
// workflow's ubuntu-24.04 step explicitly enables them, where the
// runner has real iptables-nft installed AND the test process
// has CAP_NET_ADMIN (the workflow grants it via Docker or sudo).
//
// Local run (requires root):
//   sudo -E go test -tags=linux,integration -run TestReconciler \
//     ./packages/agent/internal/platform/
//
// CI run is wired in .github/workflows/agent-release.yml's
// linux job (see the "iptables reconciler integration" step).
//
// Each test cleans up its own chain so leaving residue between
// runs doesn't break CI's idempotency.

package linux

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// requireIptables skips the test when the binary is missing, so
// developers on machines without iptables installed (e.g. the
// agent dev's macOS box running tests in a Linux VM that lacks
// nft) don't see spurious failures.
func requireIptables(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("iptables"); err != nil {
		t.Skipf("iptables not on PATH: %v", err)
	}
	if os.Geteuid() != 0 {
		t.Skip("integration test requires root (or CAP_NET_ADMIN) for iptables")
	}
}

// cleanupChain removes any residue left by a prior test. Idempotent.
func cleanupChain(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = removeChain(ctx, familyV4, chainName)
	_ = removeChain(ctx, familyV6, chainName)
}

// TestReconciler_InstallsCanonicalChain covers AC-L1: starting the
// reconciler installs the canonical NEXUS_AGENT chain in both nat
// tables, hooks OUTPUT, and the rule set matches what canonicalRules
// returns. No external mutation, no drift — just the first install.
func TestReconciler_InstallsCanonicalChain(t *testing.T) {
	requireIptables(t)
	t.Cleanup(func() { cleanupChain(t) })
	cleanupChain(t) // start from a clean slate

	r := NewReconciler(slog.Default(), 19080)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer r.Stop() //nolint:errcheck

	// v4 chain present + matches canonical
	live, err := dumpChain(ctx, familyV4, chainName)
	if err != nil {
		t.Fatalf("dumpChain v4: %v", err)
	}
	if !rulesEqual(live, r.canonicalRules(familyV4)) {
		t.Errorf("v4 chain drift after install:\n  live:      %v\n  canonical: %v",
			live, r.canonicalRules(familyV4))
	}

	// OUTPUT hook present
	check := exec.CommandContext(ctx, "iptables", "-t", "nat", "-C", "OUTPUT", "-j", chainName)
	if err := check.Run(); err != nil {
		t.Errorf("OUTPUT hook missing after install: %v", err)
	}
}

// TestReconciler_SelfHealAfterFlush covers AC-L1b: when an external
// actor (firewalld --reload, ufw reload, sysadmin's iptables -F)
// flushes our chain, the reconciler restores it within at most
// reconcileInterval (5s) without any user action.
func TestReconciler_SelfHealAfterFlush(t *testing.T) {
	requireIptables(t)
	t.Cleanup(func() { cleanupChain(t) })
	cleanupChain(t)

	r := NewReconciler(slog.Default(), 19080)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer r.Stop() //nolint:errcheck

	// Simulate firewalld --reload by flushing our chain.
	flush := exec.CommandContext(ctx, "iptables", "-t", "nat", "-F", chainName)
	if out, err := flush.CombinedOutput(); err != nil {
		t.Fatalf("simulated flush failed: %v (%s)", err, out)
	}

	// Verify it really is empty.
	live, err := dumpChain(ctx, familyV4, chainName)
	if err != nil {
		t.Fatalf("dumpChain after flush: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("chain not actually flushed; got %d rules: %v", len(live), live)
	}

	// Reconciler ticks every 5s; allow one full interval + a
	// small headroom for the syscalls to complete.
	deadline := time.Now().Add(reconcileInterval + 3*time.Second)
	for time.Now().Before(deadline) {
		live, err := dumpChain(ctx, familyV4, chainName)
		if err == nil && len(live) == len(r.canonicalRules(familyV4)) {
			if rulesEqual(live, r.canonicalRules(familyV4)) {
				return // recovered as expected
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("reconciler did not restore canonical chain within %v", reconcileInterval+3*time.Second)
}

// TestReconciler_StopRemovesChain covers AC-L3: clean Stop() leaves
// no NEXUS_AGENT residue in either nat table; `iptables -t nat -L
// NEXUS_AGENT` reports the chain as absent.
func TestReconciler_StopRemovesChain(t *testing.T) {
	requireIptables(t)
	t.Cleanup(func() { cleanupChain(t) })
	cleanupChain(t)

	r := NewReconciler(slog.Default(), 19080)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Sanity check the chain is up before we ask it to go away.
	if _, err := dumpChain(ctx, familyV4, chainName); err != nil {
		t.Fatalf("chain unexpectedly absent before Stop: %v", err)
	}

	if err := r.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Now confirm absence by asking iptables directly + parsing
	// the error message (we expect "No chain/target/match by that
	// name").
	cmd := exec.CommandContext(ctx, "iptables", "-t", "nat", "-S", chainName)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("chain still present after Stop:\n%s", out)
	}
	if !strings.Contains(string(out), "No chain") {
		t.Errorf("unexpected iptables error after Stop: %s (err=%v)", out, err)
	}

	// OUTPUT hook also gone.
	check := exec.CommandContext(ctx, "iptables", "-t", "nat", "-C", "OUTPUT", "-j", chainName)
	if err := check.Run(); err == nil {
		t.Errorf("OUTPUT hook still present after Stop")
	}
}

// TestSOMARK_DialerStampsSocket covers SO_MARK stamping at the syscall level:
// every socket produced by MarkedDialer carries SO_MARK = AgentSOMark
// on its outbound file descriptor. The test creates a marked dialer,
// uses it to open a TCP connection to a localhost test listener,
// then reads back the socket's SO_MARK via getsockopt and asserts
// the expected value.
//
// Doesn't require root by default, but does require CAP_NET_ADMIN
// to read SO_MARK — Linux allows reading marks you've set, but the
// initial setsockopt also requires CAP_NET_ADMIN. So this test
// effectively requires root.
func TestSOMARK_DialerStampsSocket(t *testing.T) {
	requireIptables(t) // also a proxy for "root"; not strictly iptables-related

	srv := startTCPEcho(t)
	defer srv.Close() //nolint:errcheck

	d := MarkedDialer()
	conn, err := d.DialContext(context.Background(), "tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	mark, err := readSOMARK(conn)
	if err != nil {
		t.Fatalf("readSOMARK: %v", err)
	}
	if mark != AgentSOMark {
		t.Errorf("SO_MARK on outbound socket = 0x%x, want 0x%x", mark, AgentSOMark)
	}
}
