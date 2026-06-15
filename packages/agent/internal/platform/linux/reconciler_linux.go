//go:build linux

package linux

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
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

// maxReconcileFailuresBeforeDegraded is how many consecutive failed
// reconcile ticks the agent tolerates before InterceptionHealth reports
// the capture layer as degraded. A single failure is usually transient
// xtables-lock contention (we don't pass `-w`); two back-to-back
// failures (~10s) means the agent genuinely cannot maintain the
// redirect chain — lost CAP_NET_ADMIN, a broken iptables binary, or an
// nft ruleset the compat shim can't write — and the host is silently
// capturing nothing. Drift caused by firewalld/ufw flushes is NOT a
// failure: the very next tick re-applies the chain successfully and
// resets the counter, so routine reloads never surface as degraded.
const maxReconcileFailuresBeforeDegraded = 2

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
	// firewalld / ufw / manual iptables interfered). Atomic so
	// Health() can read it without contending on the tick mutex
	// (a tick can hold mu for the duration of several iptables execs).
	installed atomic.Bool

	// Health snapshot, all updated at the end of every tick via
	// recordHealth and read lock-free by Health(). consecutiveFailures
	// counts back-to-back failed reconciles (reset to 0 on success);
	// lastReconcileNS is the unix-nanos timestamp of the most recent
	// tick; lastErr holds the most recent tick error string ("" on
	// success).
	consecutiveFailures atomic.Int32
	lastReconcileNS     atomic.Int64
	lastErr             atomic.Pointer[string]
}

// ReconcilerHealth is a point-in-time snapshot of the reconciler's
// ability to keep the NEXUS_AGENT redirect chain installed. Consumed by
// the Linux platform's InterceptionHealth() reporter.
type ReconcilerHealth struct {
	// Installed is true once the first reconcile has successfully
	// installed the chain. Never flips back to false — a later failure
	// is surfaced via ConsecutiveFailures, not by un-installing.
	Installed bool
	// ConsecutiveFailures is the number of back-to-back failed reconcile
	// ticks; 0 means the last tick succeeded.
	ConsecutiveFailures int
	// LastReconcileAt is when the most recent tick ran. Zero before the
	// first tick.
	LastReconcileAt time.Time
	// LastError is the most recent tick's error text, "" on success.
	LastError string
}

// Health returns a lock-free snapshot of the reconciler's chain-upkeep
// state. Safe to call concurrently with the reconcile loop.
func (r *Reconciler) Health() ReconcilerHealth {
	h := ReconcilerHealth{
		Installed:           r.installed.Load(),
		ConsecutiveFailures: int(r.consecutiveFailures.Load()),
	}
	if ns := r.lastReconcileNS.Load(); ns > 0 {
		h.LastReconcileAt = time.Unix(0, ns)
	}
	if p := r.lastErr.Load(); p != nil {
		h.LastError = *p
	}
	return h
}

// Healthy reports whether the redirect chain is installed and not
// persistently failing to reconcile. degradedReason returns the
// actionable explanation when not healthy ("" when healthy).
func (r *Reconciler) degradedReason() string {
	h := r.Health()
	if !h.Installed {
		return "iptables redirect chain not installed"
	}
	if h.ConsecutiveFailures >= maxReconcileFailuresBeforeDegraded {
		return fmt.Sprintf("iptables redirect chain repair failing (%d consecutive errors: %s)",
			h.ConsecutiveFailures, h.LastError)
	}
	return ""
}

// recordHealth folds a single tick's outcome into the health snapshot.
// On success it resets the failure counter and marks installed; on
// failure it increments the counter and stores the error text. Called
// at the tail of every tick.
func (r *Reconciler) recordHealth(now time.Time, err error) {
	r.lastReconcileNS.Store(now.UnixNano())
	if err != nil {
		r.consecutiveFailures.Add(1)
		msg := err.Error()
		r.lastErr.Store(&msg)
		return
	}
	r.consecutiveFailures.Store(0)
	empty := ""
	r.lastErr.Store(&empty)
	r.installed.Store(true)
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

// tick is one reconcile pass. It runs reconcileOnce and folds the
// outcome into the health snapshot (recordHealth) so InterceptionHealth
// can surface a persistently-failing reconcile as degraded. The error
// is returned verbatim for the caller's logging / Start failure path.
//
// Single-flighted by r.mu so a Stop call can't interleave.
func (r *Reconciler) tick(ctx context.Context) error {
	err := r.reconcileOnce(ctx)
	r.recordHealth(time.Now(), err)
	return err
}

// reconcileOnce performs one reconcile pass. It:
//  1. Builds the canonical rule set for v4 + v6.
//  2. Dumps the live state.
//  3. On first install OR drift, runs iptables-restore + ensureHook.
//  4. Logs INFO on first install, WARN on drift, silent otherwise.
func (r *Reconciler) reconcileOnce(ctx context.Context) error {
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
			if r.installed.Load() {
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

	if !r.installed.Load() {
		r.installed.Store(true)
		r.log.Info("iptables chain installed",
			"chain", chainName,
			"proxy_port", r.proxyPort,
			"mark", AgentSOMark)
	}
	return nil
}
