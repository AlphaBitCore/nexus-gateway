package killswitch

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/interception"
)

// killSwitchHistoryCapacity caps the in-memory toggle history. Older entries
// fall off the back as new ones are appended. Not persisted across restart —
// this is an operational signal, not a legal record.
const killSwitchHistoryCapacity = 100

// KillSwitchHistoryEntry records a single kill switch toggle or force-close.
type KillSwitchHistoryEntry struct {
	At               time.Time `json:"at"`
	Engaged          bool      `json:"engaged"`
	ChangedBy        string    `json:"changedBy"`
	ForceClose       bool      `json:"forceClose"`
	ForceClosedCount int       `json:"forceClosedCount"`
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
	mu           sync.Mutex
	engaged      atomic.Bool // true = kill switch engaged (passthrough mode); false = bump active (default)
	lastChanged  time.Time
	changedBy    string
	logger       *slog.Logger
	forceCloseFn func() int // callback to force-close bumped connections; returns count
	history      []KillSwitchHistoryEntry
}

// KillSwitchState represents the current state of the kill switch.
type KillSwitchState struct {
	Engaged     bool      `json:"engaged"`
	LastChanged time.Time `json:"lastChanged"`
	ChangedBy   string    `json:"changedBy"`
}

// NewKillSwitch creates a kill switch that is NOT engaged by default (bump active).
func NewKillSwitch(logger *slog.Logger) *KillSwitch {
	ks := &KillSwitch{
		logger:  logger,
		history: make([]KillSwitchHistoryEntry, 0, killSwitchHistoryCapacity),
	}
	ks.engaged.Store(false)
	return ks
}

// recordHistoryLocked appends an entry to the bounded history. Caller must
// hold k.mu.
func (k *KillSwitch) recordHistoryLocked(entry KillSwitchHistoryEntry) {
	if len(k.history) >= killSwitchHistoryCapacity {
		// Drop the oldest entry to make room.
		copy(k.history, k.history[1:])
		k.history = k.history[:killSwitchHistoryCapacity-1]
	}
	k.history = append(k.history, entry)
}

// History returns a newest-first copy of the toggle history.
func (k *KillSwitch) History() []KillSwitchHistoryEntry {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]KillSwitchHistoryEntry, len(k.history))
	// Reverse copy so the newest entry is first.
	for i, e := range k.history {
		out[len(k.history)-1-i] = e
	}
	return out
}

// HistoryCapacity returns the maximum number of entries kept in memory.
func (k *KillSwitch) HistoryCapacity() int {
	return killSwitchHistoryCapacity
}

// SetForceCloseFunc sets the callback invoked when force-closing bumped connections.
// The function must return the number of connections that were closed.
func (k *KillSwitch) SetForceCloseFunc(fn func() int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.forceCloseFn = fn
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

	prev := k.engaged.Load()
	k.engaged.Store(engaged)
	k.lastChanged = time.Now()
	if changedBy == "" {
		changedBy = "api"
	}
	k.changedBy = changedBy

	k.logger.Info("kill switch toggled", "from", prev, "to", engaged, "changedBy", changedBy)

	k.recordHistoryLocked(KillSwitchHistoryEntry{
		At:        k.lastChanged,
		Engaged:   engaged,
		ChangedBy: k.changedBy,
	})

	state := KillSwitchState{
		Engaged:     engaged,
		LastChanged: k.lastChanged,
		ChangedBy:   k.changedBy,
	}
	k.mu.Unlock()

	return state
}

// ApplyBreakGlass applies a break-glass desired state from a PUT
// /runtime/config/killswitch request. It short-circuits when the incoming
// engaged flag matches the current state (so a redundant break-glass is a
// no-op on the event log — the caller decides whether to log it). Returns
// nil on success; callers treat the error path as "apply failed" and skip
// the event log + version bump.
func (k *KillSwitch) ApplyBreakGlass(ks interception.Killswitch) error {
	k.Toggle(ks.Engaged, "break-glass")
	return nil
}

// ForceClose disengages the kill switch AND force-closes all bumped
// connections. Returns the new state and the number of force-closed
// connections. changedBy identifies who performed the action; falls back
// to "api" if empty.
func (k *KillSwitch) ForceClose(changedBy string) (KillSwitchState, int) {
	k.mu.Lock()

	k.engaged.Store(false)
	k.lastChanged = time.Now()
	if changedBy == "" {
		changedBy = "api"
	}
	k.changedBy = changedBy

	closed := 0
	if k.forceCloseFn != nil {
		closed = k.forceCloseFn()
	}

	k.logger.Warn("kill switch force-closed", "connectionsForced", closed)

	k.recordHistoryLocked(KillSwitchHistoryEntry{
		At:               k.lastChanged,
		Engaged:          false,
		ChangedBy:        k.changedBy,
		ForceClose:       true,
		ForceClosedCount: closed,
	})

	state := KillSwitchState{
		Engaged:     false,
		LastChanged: k.lastChanged,
		ChangedBy:   k.changedBy,
	}
	k.mu.Unlock()

	return state, closed
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
