// Package configreconcile runs a periodic drift watchdog comparing the
// Control Plane's source-of-truth config tables against the corresponding
// `thing.desired.<key>` JSON on each online thing. On drift, it logs a
// structured warning, increments cp_config_drift_total, and re-emits
// Hub.NotifyConfigChange once per cycle to heal.
//
// This goroutine is the out-of-band repair path for already-divergent state:
// system_metadata and thing.desired can drift when a Hub.NotifyConfigChange
// call fails silently (e.g. fire-and-forget discard, or a Hub restart that
// lost the connection mid-broadcast). The handler-level fix returns 502 on Hub
// failure for in-flight saves, but cannot heal divergence that pre-dates the
// current process boot.
//
// Two complementary arms (the actual watch set is assembled by the caller in
// cmd/control-plane/wiring/reconcile.go — keep this list in step with it):
//
// 1. Content-diff Watches (Category A — full state lives in thing.desired):
//
//   - cache                                  (ai-gateway): 3-tier prompt cache blob.
//
//   - semantic_cache.config                  (ai-gateway): L2 embedding config row.
//
//   - response_cache.extract_config          (ai-gateway): L1 extract cache toggle/TTL.
//
//   - response_cache.time_sensitive_patterns (ai-gateway): freshness rule list.
//
//   - agent_settings                         (agent): defaults set by admin in the UI.
//
//   - killswitch          (compliance-proxy + agent): emergency disable flag.
//
//   - gateway_passthrough (ai-gateway): emergency passthrough toggle.
//     Each loads CP source-of-truth, diffs it against thing.desired.<key>, and
//     re-pushes the full state on mismatch. Every key pushed via hub.PushTypeA
//     has a matching watch here, so the unified propagation-failure response
//     (hub.PropagationErrorJSON) can truthfully promise auto-recovery.
//
//     2. The Pending arm (Category B — bare version bump, no state in thing.desired,
//     so content-diff cannot apply): credentials / virtual_keys / routing_rules /
//     quota_* / providers / models. A durable ledger records the intended vs
//     acknowledged version per key; ReconcilePending re-pushes any key whose last
//     push CP never confirmed.
//
// The package is intentionally small and free of side-channel concerns: it owns
// no DB connections, no Hub clients — both arms are injected so unit tests can
// drive them.
package configreconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
)

// Watch describes one config key the reconcile loop monitors.
type Watch struct {
	// ConfigKey is the Hub shadow key (e.g. "cache").
	ConfigKey string
	// ThingType filters which `thing` rows are inspected. Set to the type
	// that owns this config (e.g. "ai-gateway"). Empty matches all types.
	ThingType string
	// SourceLoader returns the canonical source-of-truth bytes for this
	// key, marshaled as it would be sent over Hub.NotifyConfigChange.State.
	// Errors are non-fatal: the watch is skipped for this tick.
	SourceLoader func(ctx context.Context) (json.RawMessage, error)
}

// Reconciler runs the periodic drift sweep.
type Reconciler struct {
	DB      Querier
	Hub     HubNotifier
	Logger  *slog.Logger
	Period  time.Duration
	Watches []Watch

	// Pending is the Category-B version-reconcile arm. The content-diff
	// Watches above cannot heal Category-B keys (credentials / virtual_keys /
	// routing_rules / quota_* / providers / models): those push a bare version
	// bump and carry no state into thing.desired, so there is nothing to diff.
	// Pending instead re-pushes any key whose last push CP never confirmed.
	// Nil when no durable backstop is wired (dev/no-DB). Driven once per tick.
	Pending PendingReconciler

	driftCounter *prometheus.CounterVec
}

// PendingReconciler re-drives Category-B config pushes whose acknowledgement CP
// never received. *hub.Client satisfies it via ReconcilePending.
type PendingReconciler interface {
	// ReconcilePending re-pushes unconfirmed keys and returns the count healed.
	ReconcilePending(ctx context.Context) (int, error)
}

// Querier is the narrow DB interface the reconcile job needs.
// store.DB satisfies it via its existing Pool method.
type Querier interface {
	QueryThingDesired(ctx context.Context, thingType, configKey string) ([]ThingDesiredRow, error)
}

// ThingDesiredRow is one (thing_id, config_key) snapshot.
type ThingDesiredRow struct {
	ThingID     string
	ThingType   string
	DesiredJSON json.RawMessage // the value at thing.desired -> configKey
}

// HubNotifier is the narrow contract the reconcile loop needs from hub.
// hub.Client satisfies this via its existing NotifyConfigChange method.
type HubNotifier interface {
	NotifyConfigChange(ctx context.Context, req hub.ConfigChangeRequest) (*hub.ConfigChangeResponse, error)
}

// New constructs a Reconciler. metrics may be nil for tests; in that case
// the package-level cp_config_drift_total registration is skipped.
func New(db Querier, hub HubNotifier, logger *slog.Logger, period time.Duration, watches []Watch, reg prometheus.Registerer) *Reconciler {
	if logger == nil {
		logger = slog.Default()
	}
	if period <= 0 {
		period = 60 * time.Second
	}
	var driftCounter *prometheus.CounterVec
	if reg != nil {
		driftCounter = promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "cp_config_drift_total",
			Help: "Number of times a CP source-of-truth diverged from a thing.desired entry, labelled by config_key + thing_type + thing_id.",
		}, []string{"config_key", "thing_type", "thing_id"})
	}
	return &Reconciler{
		DB:           db,
		Hub:          hub,
		Logger:       logger,
		Period:       period,
		Watches:      watches,
		driftCounter: driftCounter,
	}
}

// Run blocks until ctx is canceled, ticking every Period to sweep all
// Watches. Failures inside one tick are logged but never abort the loop.
func (r *Reconciler) Run(ctx context.Context) {
	r.Logger.Info("config reconcile loop starting",
		"period", r.Period,
		"watches", len(r.Watches),
	)
	ticker := time.NewTicker(r.Period)
	defer ticker.Stop()

	// Run one immediate tick on startup so a freshly-deployed CP catches
	// any pre-existing drift before the first interval elapses.
	r.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			r.Logger.Info("config reconcile loop stopping")
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

func (r *Reconciler) tick(ctx context.Context) {
	for _, w := range r.Watches {
		r.checkWatch(ctx, w)
	}
	r.reconcilePending(ctx)
}

// reconcilePending drives the Category-B version-reconcile arm once. Failures
// are logged but never abort the tick (the content Watches must still run).
func (r *Reconciler) reconcilePending(ctx context.Context) {
	if r.Pending == nil {
		return
	}
	healed, err := r.Pending.ReconcilePending(ctx)
	if err != nil {
		r.Logger.Warn("config reconcile: pending Category-B reconcile failed", "error", err)
		return
	}
	if healed > 0 {
		r.Logger.Info("config reconcile: re-pushed unconfirmed Category-B keys", "healed", healed)
	}
}

func (r *Reconciler) checkWatch(ctx context.Context, w Watch) {
	source, err := w.SourceLoader(ctx)
	if err != nil {
		r.Logger.Warn("config reconcile: source load failed",
			"config_key", w.ConfigKey,
			"thing_type", w.ThingType,
			"error", err,
		)
		return
	}
	rows, err := r.DB.QueryThingDesired(ctx, w.ThingType, w.ConfigKey)
	if err != nil {
		r.Logger.Warn("config reconcile: thing.desired query failed",
			"config_key", w.ConfigKey,
			"thing_type", w.ThingType,
			"error", err,
		)
		return
	}

	for _, row := range rows {
		if !jsonEqual(source, row.DesiredJSON) {
			r.Logger.Warn("config reconcile: drift detected",
				"config_key", w.ConfigKey,
				"thing_id", row.ThingID,
				"thing_type", row.ThingType,
				"source_bytes", len(source),
				"desired_bytes", len(row.DesiredJSON),
			)
			if r.driftCounter != nil {
				r.driftCounter.WithLabelValues(w.ConfigKey, row.ThingType, row.ThingID).Inc()
			}
			// Heal: re-emit Hub.NotifyConfigChange with the source-of-truth payload.
			// Best effort — if Hub is also down, the next tick retries.
			if r.Hub != nil {
				var state any
				if err := json.Unmarshal(source, &state); err != nil {
					state = source // fallback: pass as raw bytes
				}
				if _, err := r.Hub.NotifyConfigChange(ctx, hub.ConfigChangeRequest{
					ThingType: w.ThingType,
					ConfigKey: w.ConfigKey,
					State:     state,
					ActorID:   "configreconcile",
					ActorName: "configreconcile",
					// No SourceIP: this is the background drift healer, not an
					// HTTP request — there is no human source address to stamp.
					// Left empty explicitly so the absence is intentional, not
					// an omission.
					SourceIP: "",
				}); err != nil {
					r.Logger.Warn("config reconcile: re-emit failed",
						"config_key", w.ConfigKey,
						"thing_id", row.ThingID,
						"error", err,
					)
				}
			}
		}
	}
}

// jsonEqual returns true if the two byte slices encode the same JSON value
// modulo whitespace and key order. Implementation is canonical: marshal
// after Unmarshal-into-any to normalize.
func jsonEqual(a, b json.RawMessage) bool {
	if bytes.Equal(a, b) {
		return true
	}
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	ab, err := json.Marshal(av)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(bv)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}
