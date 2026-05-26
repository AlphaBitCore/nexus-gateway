package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// RegisterRequest is the input for Thing registration.
type RegisterRequest struct {
	ID            string         `json:"id"`
	Type          string         `json:"type"`
	Name          string         `json:"name,omitempty"`
	Version       string         `json:"version"`
	Address       string         `json:"address"`
	MetricsURL    string         `json:"metricsUrl,omitempty"`
	ManagementURL string         `json:"managementUrl,omitempty"`
	Role          string         `json:"role,omitempty"`
	RuntimeAPIURL string         `json:"runtimeApiUrl,omitempty"`
	Metadata      map[string]any `json:"metadata"`
	// PhysicalID is the stable natural key — hardware fingerprint for
	// agents, yaml-configured id (or hostname+type+port fallback) for
	// services. Persisted into thing.physical_id; subject to the
	// partial UNIQUE(type='agent', physical_id) DB constraint.
	PhysicalID string `json:"physicalId,omitempty"`
}

// RegisterResponse is returned after successful registration.
type RegisterResponse struct {
	Desired    map[string]any `json:"desired"`
	DesiredVer int64          `json:"desiredVer"`
}

// RegisterThing registers a Thing and returns its desired config.
// On reconnect it performs a narrow session touch (preserving auth_type,
// conn_protocol, enrolled_by, desired, and shadow state). Only the first
// connection triggers a full enrollment upsert.
func (m *Manager) RegisterThing(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	m.logger.Info("registering thing", "thing_id", req.ID, "thing_type", req.Type)

	templates, err := m.store.ConfigStore().GetConfigTemplates(ctx, req.Type)
	if err != nil {
		return nil, fmt.Errorf("load templates for %s: %w", req.Type, err)
	}

	desired := make(map[string]any, len(templates))
	var maxVer int64
	for _, t := range templates {
		desired[t.ConfigKey] = t.State
		if t.Version > maxVer {
			maxVer = t.Version
		}
	}

	// Prefer a session touch; fall back to first-time enrollment if the row
	// does not yet exist. Only the enrollment path writes auth_type and
	// conn_protocol, preventing reconnects from resetting mTLS-enrolled agents
	// back to the "bearer" default (bug C2).
	touchErr := m.store.AuthStore().TouchThingSession(ctx, store.TouchSessionParams{
		ID:         req.ID,
		Name:       req.Name,
		Version:    req.Version,
		Address:    req.Address,
		PhysicalID: req.PhysicalID,
	})
	switch {
	case touchErr == nil:
		// Already enrolled — session refreshed, desired state unchanged.
	case errors.Is(touchErr, store.ErrNotFound):
		// Default Name to ID when the client didn't send one, so the row
		// never lands with an empty admin-display name. Services started
		// before the `Config.ThingName` plumbing existed will use the ID
		// fallback; new services can override via cfg.ThingName.
		name := req.Name
		if name == "" {
			name = req.ID
		}
		if err := m.store.RegistryStore().UpsertThingEnrollmentWithDesiredVer(ctx, store.UpsertThingParams{
			ID:         req.ID,
			Type:       req.Type,
			Name:       name,
			Version:    req.Version,
			Address:    req.Address,
			Status:     "online",
			Metadata:   req.Metadata,
			Desired:    desired,
			PhysicalID: req.PhysicalID,
		}, maxVer); err != nil {
			return nil, fmt.Errorf("enroll thing: %w", err)
		}
	default:
		return nil, fmt.Errorf("touch thing: %w", touchErr)
	}

	// Persist the service-type extension row (thing_service.metrics_url +
	// role) so the runtime introspection bridge (e31-s7) and the CP UI
	// Nodes detail page can resolve where to reach the Thing's
	// /debug/runtime endpoint. Idempotent UPSERT — safe whether this
	// register call hit the touch path or the first-time enrollment
	// path. Skipped for agent-type Things (agents don't expose a
	// metrics URL — they sit behind NAT).
	if req.Type != "agent" {
		if err := m.store.RegistryStore().UpsertThingService(ctx, req.ID, req.MetricsURL, req.ManagementURL, req.Role); err != nil {
			m.logger.Warn("upsert thing_service after register failed",
				"thing_id", req.ID, "error", err)
		}
	}

	// Cache desired in Redis
	m.cacheDesired(ctx, req.Type, desired)

	thing, err := m.store.RegistryStore().GetThing(ctx, req.ID)
	if err != nil {
		return nil, fmt.Errorf("get thing after register: %w", err)
	}
	if thing.Desired != nil {
		desired = thing.Desired
	}

	// The thingclient decodes register/connected desired payload into
	// map[string]ConfigState ({state, version}). Keep this shape aligned with
	// config pull and WS delta payloads so initial desiredCache is valid.
	desiredStates := make(map[string]any, len(desired))
	for key, state := range desired {
		desiredStates[key] = map[string]any{
			"state":   state,
			"version": thing.DesiredVer,
		}
	}
	return &RegisterResponse{
		Desired:    desiredStates,
		DesiredVer: thing.DesiredVer,
	}, nil
}

// Deregister marks a Thing as offline.
func (m *Manager) Deregister(ctx context.Context, id string) error {
	return m.store.RegistryStore().MarkOffline(ctx, id)
}

func (m *Manager) cacheDesired(ctx context.Context, thingType string, desired map[string]any) {
	if m.redis == nil {
		return
	}
	for key, val := range desired {
		data, err := json.Marshal(val)
		if err != nil {
			continue
		}
		rkey := fmt.Sprintf("nexus:desired:%s:%s", thingType, key)
		m.redis.Set(ctx, rkey, data, 1*time.Hour)
	}
}
