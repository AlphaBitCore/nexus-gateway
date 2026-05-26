//go:build linux

package linux

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// chainName is the dedicated iptables chain the reconciler maintains.
// All redirect logic lives inside it; OUTPUT only ever holds a
// single `-j NEXUS_AGENT` jump.
const chainName = "NEXUS_AGENT"

// reconcileInterval is the heartbeat between drift checks. 5s is the
// Tailscale / Docker / Cilium default and gives a hard ceiling on the
// capture-gap a firewalld --reload can produce. Not user-tunable —
// faster wastes syscalls, slower violates AC-L1b's 5s SLA.
const reconcileInterval = 5 * time.Second

// Reconciler keeps the kernel netfilter state matching the canonical
// rule set built from the agent's config. It is self-healing: a
// goroutine wakes every [reconcileInterval], compares the live
// state to canonical, and re-installs via `iptables-restore` when
// drift is detected.
//
// Concurrency contract:
//   - Start spawns the loop goroutine; it returns after the first
//     reconcile completes synchronously so the caller can rely on
//     "Start returned -> capture is active".
//   - Stop blocks until the loop has exited AND the teardown
//     reconcile (remove chain + unhook) has finished. Safe to call
//     more than once.
//   - tick is single-flighted by mu so concurrent Start/Stop don't
//     interleave restore calls.
type Reconciler struct {
	log       *slog.Logger
	proxyPort int

	mu     sync.Mutex // serialises tick + Stop
	once   sync.Once  // gates Stop teardown
	stopCh chan struct{}
	doneCh chan struct{}

	// installed tracks whether the first successful install has
	// happened. Used so we log INFO on first install and WARN only
	// when subsequent ticks detect drift (an indicator that
	// firewalld / ufw / manual iptables interfered).
	installed bool
}

// NewReconciler builds a Reconciler bound to the given proxy port
// (typically 19080) and logger. It does not start the loop — call
// Start for that.
func NewReconciler(log *slog.Logger, proxyPort int) *Reconciler {
	return &Reconciler{
		log:       log.With("component", "iptables_reconciler"),
		proxyPort: proxyPort,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
}

// Start performs the initial install synchronously, then spawns the
// drift-detection loop. Returns the first install's error verbatim;
// the loop is NOT spawned on initial failure so the caller can fail
// the daemon's Start sequence cleanly.
//
// NOTE: If Start returns an error, the loop goroutine never runs
// and r.doneCh is never closed. Callers MUST NOT then call r.Stop()
// — it would block forever on `<-r.doneCh`. The owner (linux.go)
// is expected to drop its reference to the Reconciler on Start
// failure so its own Stop() doesn't try to tear down a never-
// started loop. The systemd unit's `ExecStopPost=` cleanup script
// covers any partial iptables state in that path.
func (r *Reconciler) Start(ctx context.Context) error {
	if err := r.tick(ctx); err != nil {
		return fmt.Errorf("initial iptables install: %w", err)
	}
	go r.loop(ctx)
	return nil
}

// Stop tears down the kernel state idempotently and waits for the
// loop goroutine to exit. Safe to call multiple times.
func (r *Reconciler) Stop() error {
	var teardownErr error
	r.once.Do(func() {
		close(r.stopCh)
		// Wait for the loop to observe stopCh and return.
		<-r.doneCh

		// Final teardown — remove the chain in both families.
		// Best-effort; we collect the first error for telemetry but
		// always attempt every step (see removeChain).
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		r.mu.Lock()
		defer r.mu.Unlock()
		if err := removeChain(ctx, familyV4, chainName); err != nil {
			teardownErr = fmt.Errorf("v4 teardown: %w", err)
			r.log.Warn("iptables v4 teardown error", "error", err)
		}
		if err := removeChain(ctx, familyV6, chainName); err != nil {
			if teardownErr == nil {
				teardownErr = fmt.Errorf("v6 teardown: %w", err)
			}
			r.log.Warn("ip6tables v6 teardown error", "error", err)
		}
		r.log.Info("iptables chain torn down")
	})
	return teardownErr
}

// loop is the background reconciler goroutine. Exits cleanly on
// stopCh close OR on ctx cancellation.
func (r *Reconciler) loop(ctx context.Context) {
	defer close(r.doneCh)
	t := time.NewTicker(reconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case <-t.C:
			if err := r.tick(ctx); err != nil {
				r.log.Error("reconcile failed", "error", err)
			}
		}
	}
}

// tick is one reconcile pass. It:
//  1. Builds the canonical rule set for v4 + v6.
//  2. Dumps the live state.
//  3. On first install OR drift, runs iptables-restore + ensureHook.
//  4. Logs INFO on first install, WARN on drift, silent otherwise.
//
// Single-flighted by r.mu so a Stop call can't interleave.
func (r *Reconciler) tick(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, family := range []iptablesFamily{familyV4, familyV6} {
		canonical := r.canonicalScript(family)
		canonicalRules := r.canonicalRules(family)

		live, err := dumpChain(ctx, family, chainName)
		if err != nil {
			return fmt.Errorf("%s dump: %w", family.String(), err)
		}

		drifted := !rulesEqual(live, canonicalRules)
		if drifted {
			if r.installed {
				r.log.Warn("chain drift detected; re-installing",
					"family", family.String(),
					"live_rules", len(live),
					"canonical_rules", len(canonicalRules))
			}
			if err := applyChain(ctx, family, canonical); err != nil {
				return fmt.Errorf("%s apply: %w", family.String(), err)
			}
		}
		// Always ensure the OUTPUT hook — idempotent, costs one
		// `-C` syscall if the rule's already there.
		if err := ensureHook(ctx, family, chainName); err != nil {
			return fmt.Errorf("%s hook: %w", family.String(), err)
		}
	}

	if !r.installed {
		r.installed = true
		r.log.Info("iptables chain installed",
			"chain", chainName,
			"proxy_port", r.proxyPort,
			"mark", AgentSOMark)
	}
	return nil
}

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
	b.WriteString(fmt.Sprintf(":%s - [0:0]\n", chainName))
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
