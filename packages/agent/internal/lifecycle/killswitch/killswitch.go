// Package killswitch holds the agent's fleet kill switch — the runtime
// flag operators flip to make the agent stop intercepting TLS traffic on
// every machine in the fleet at once. Mirrors compliance-proxy's
// runtimeapi.KillSwitch, trimmed to what an agent needs (no force-close
// over a tunnel registry, no break-glass HTTP API).
//
// Semantic (canonical, shared with compliance-proxy and the wire):
// engaged=true means the kill switch is engaged and the connection bridge
// must passthrough without TLS-bumping. engaged=false (the fail-safe
// default) means normal operation — bump is active. No inversion between
// the wire field, the internal store, and any caller.
package killswitch

import (
	"context"
	"github.com/goccy/go-json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/interception"
)

// Snapshot describes the kill switch state at a point in time.
// Returned by Switch.Snapshot for introspection.
type Snapshot struct {
	Engaged     bool      `json:"engaged"`
	LastChanged time.Time `json:"last_changed"`
	ChangedBy   string    `json:"changed_by"`
}

// Switch is the agent kill switch. Construct one per process via New.
type Switch struct {
	engaged atomic.Bool

	mu          sync.Mutex
	lastChanged time.Time
	changedBy   string

	logger *slog.Logger
}

// New constructs a Switch with the kill switch disengaged (bump active)
// by default — fail-safe baseline so a Switch that never receives a
// shadow update still allows normal interception.
func New(logger *slog.Logger) *Switch {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Switch{logger: logger, lastChanged: time.Now().UTC(), changedBy: "init"}
	s.engaged.Store(false)
	return s
}

// IsEngaged returns true when the kill switch is engaged (agent must
// passthrough without MITM). False means normal operation — bump is
// active.
func (s *Switch) IsEngaged() bool { return s.engaged.Load() }

// Toggle sets the engaged flag and records the actor + timestamp.
// changedBy falls back to "api" when empty.
func (s *Switch) Toggle(engaged bool, changedBy string) Snapshot {
	if changedBy == "" {
		changedBy = "api"
	}

	s.mu.Lock()
	prev := s.engaged.Load()
	s.engaged.Store(engaged)
	now := time.Now().UTC()
	s.lastChanged = now
	s.changedBy = changedBy
	s.mu.Unlock()

	if prev != engaged {
		s.logger.Info("kill switch toggled",
			"event", "killswitch_toggled",
			"from", prev, "to", engaged, "changedBy", changedBy)
	}

	return Snapshot{Engaged: engaged, LastChanged: now, ChangedBy: changedBy}
}

// SnapshotState returns a copy of the current state. Renamed from
// Snapshot to avoid collision with the Snapshot type name; the
// runtimeintrospect Source convention is to expose a Snapshot() method,
// so a separate IntrospectSnapshot wrapper around this is added in the
// e31-s12 wiring.
func (s *Switch) SnapshotState() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{
		Engaged:     s.engaged.Load(),
		LastChanged: s.lastChanged,
		ChangedBy:   s.changedBy,
	}
}

// ApplyShadowState satisfies shadow.ShadowApplier. Decodes a
// configtypes.Killswitch payload and Toggles when the value differs.
// Empty / null payload is a no-op per the ShadowApplier contract — an
// initial shadow tick before Hub aggregation must not flip the switch.
func (s *Switch) ApplyShadowState(_ context.Context, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var ks interception.Killswitch
	if err := json.Unmarshal(raw, &ks); err != nil {
		return err
	}
	if ks.Engaged != s.engaged.Load() {
		s.Toggle(ks.Engaged, "hub-shadow")
	}
	return nil
}
