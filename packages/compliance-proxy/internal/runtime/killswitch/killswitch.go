package killswitch

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/interception"
)

// publishActiveGauge mirrors the engaged state onto the killswitch.active
// Prometheus gauge so Hub dashboards reflect reality (the gauge was
// registered but never Set, so it shipped a permanent 0 — a safety control that
// reported "off" while engaged). Called from every state-transition chokepoint
// (toggleLocked is the single chokepoint, covering Toggle and
// ApplyBreakGlass). Nil-safe: in unit tests metrics.Register is never called.
func publishActiveGauge(engaged bool) {
	if metrics.KillSwitchActive == nil {
		return
	}
	if engaged {
		metrics.KillSwitchActive.With().Set(1)
	} else {
		metrics.KillSwitchActive.With().Set(0)
	}
}

// KillSwitch manages the kill switch engaged/disengaged state.
// All operations are mutex-protected and safe for concurrent use.
// IsEngaged uses atomic.Bool for lock-free reads on the hot path.
//
// State is driven entirely by the Hub shadow (`killswitch` config_key):
//   - Normal path: Hub desired → OnConfigChanged → ApplyBreakGlass or Toggle
//   - Break-glass path: PUT /runtime/config/killswitch → ApplyBreakGlass
//
// There is no cross-instance publisher and no local persistence — the shadow
// is the source of truth across restarts.
type KillSwitch struct {
	mu          sync.Mutex
	engaged     atomic.Bool // true = kill switch engaged (passthrough mode); false = bump active (default)
	lastChanged time.Time
	changedBy   string
	logger      *slog.Logger
}

// KillSwitchState represents the current state of the kill switch.
type KillSwitchState struct {
	Engaged     bool      `json:"engaged"`
	LastChanged time.Time `json:"lastChanged"`
	ChangedBy   string    `json:"changedBy"`
}

// NewKillSwitch creates a kill switch that is NOT engaged by default (bump active).
func NewKillSwitch(logger *slog.Logger) *KillSwitch {
	ks := &KillSwitch{logger: logger}
	ks.engaged.Store(false)
	return ks
}

// State returns the current kill switch state.
func (k *KillSwitch) State() KillSwitchState {
	k.mu.Lock()
	defer k.mu.Unlock()
	return KillSwitchState{
		Engaged:     k.engaged.Load(),
		LastChanged: k.lastChanged,
		ChangedBy:   k.changedBy,
	}
}

// Toggle sets the engaged state and returns the new state.
// changedBy identifies who performed the toggle (e.g. user email); falls back
// to "api" if empty. Used by shadow apply (changedBy="hub-shadow") and by
// break-glass (changedBy="break-glass:<token-id>").
func (k *KillSwitch) Toggle(engaged bool, changedBy string) KillSwitchState {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.toggleLocked(engaged, changedBy)
}

// toggleLocked performs the state store + history append + log line for a
// toggle. Caller must hold k.mu. Split out from Toggle so ApplyBreakGlass can
// fold its redundant-state check and the toggle into a single critical section.
func (k *KillSwitch) toggleLocked(engaged bool, changedBy string) KillSwitchState {
	prev := k.engaged.Load()
	k.engaged.Store(engaged)
	k.lastChanged = time.Now()
	if changedBy == "" {
		changedBy = "api"
	}
	k.changedBy = changedBy

	k.logger.Info("kill switch toggled", "from", prev, "to", engaged, "changedBy", changedBy)

	publishActiveGauge(engaged)

	return KillSwitchState{
		Engaged:     engaged,
		LastChanged: k.lastChanged,
		ChangedBy:   k.changedBy,
	}
}

// ApplyBreakGlass applies a break-glass desired state from a PUT
// /runtime/config/killswitch request. It short-circuits when the incoming
// engaged flag matches the current state, so a redundant break-glass leaves
// lastChanged/changedBy where they are and emits no toggle log line. The
// durable break-glass event log owned by the PUT handler still records every
// request independently, so the re-affirmation is not lost — only the
// per-instance state stamp is spared the churn. Returns nil on success;
// callers treat the error path as "apply failed" and skip the event log +
// version bump.
func (k *KillSwitch) ApplyBreakGlass(ks interception.Killswitch) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.engaged.Load() == ks.Engaged {
		return nil
	}
	k.toggleLocked(ks.Engaged, "break-glass")
	return nil
}

// Snapshot returns the kill switch state in the shared configtypes shape
// used by the /runtime/config read surface. The Killswitch payload only
// carries the engaged flag — the audit fields (lastChanged, changedBy)
// live on the local KillSwitchState shape consumed internally.
func (k *KillSwitch) Snapshot() interception.Killswitch {
	return interception.Killswitch{Engaged: k.engaged.Load()}
}

// IsEngaged returns whether the kill switch is currently engaged
// (passthrough mode). When true, TLS bump is disabled and all connections
// are passed through. Uses atomic.Bool for lock-free reads on the hot path
// (called per-request).
func (k *KillSwitch) IsEngaged() bool {
	return k.engaged.Load()
}
