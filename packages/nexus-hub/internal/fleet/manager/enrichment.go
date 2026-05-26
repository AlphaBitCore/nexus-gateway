package manager

import (
	"context"
	"time"
)

// MarkOffline sets a Thing's status to offline (called on WS disconnect).
func (m *Manager) MarkOffline(ctx context.Context, thingID string) {
	if err := m.store.RegistryStore().MarkOffline(ctx, thingID); err != nil {
		m.logger.Warn("mark offline failed", "thing_id", thingID, "error", err)
	}
}

// TouchLiveness refreshes last_seen_at and promotes offline→online for a
// Thing. Invoked on every successful Hub→Thing WebSocket ping so liveness
// tracks the transport rather than riding on the MQ metrics path.
func (m *Manager) TouchLiveness(ctx context.Context, thingID string) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := m.store.RegistryStore().RefreshLiveness(ctx, thingID); err != nil {
		m.logger.Debug("touch liveness failed", "thing_id", thingID, "error", err)
	}
}
