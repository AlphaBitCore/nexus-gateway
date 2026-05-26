package manager

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// GetThingDetail returns a single Thing with full detail.
func (m *Manager) GetThingDetail(ctx context.Context, id string) (*store.Thing, error) {
	return m.store.RegistryStore().GetThing(ctx, id)
}

// GetThingManagementURL returns the management_url for the given thingID.
func (m *Manager) GetThingManagementURL(ctx context.Context, id string) (string, error) {
	return m.store.RegistryStore().GetThingManagementURL(ctx, id)
}

// ListThings returns a filtered, paginated list of Things.
func (m *Manager) ListThings(ctx context.Context, p store.ListThingsParams) (*store.ListThingsResult, error) {
	return m.store.RegistryStore().ListThings(ctx, p)
}

// GetDriftedThings returns all Things with drift status or version mismatch (for API).
func (m *Manager) GetDriftedThings(ctx context.Context) ([]store.DriftedThing, error) {
	return m.store.RegistryStore().ListDriftedThings(ctx)
}
