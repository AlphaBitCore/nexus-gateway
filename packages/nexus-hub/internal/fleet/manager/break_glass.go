package manager

import (
	"context"
	"errors"
	"fmt"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// handleBreakGlassReport reconciles a shadow_report produced by a data-plane
// break-glass handler. For each key the report carries a per-key version for,
// if the reported version strictly exceeds the current template version the
// Hub adopts the emergency state into thing_config_template (locked to the
// reported version, not version+1) and inserts a config_change_event with
// emergency_override=true and actor_id="break-glass:<actorTokenID>".
//
// Called from HandleShadowReport when req.Reason == "break_glass". A stale
// per-key version (reported <= current) is silently skipped — an admin write
// has already superseded the local override.
func (m *Manager) handleBreakGlassReport(ctx context.Context, req ShadowReportRequest) error {
	if len(req.KeyVersions) == 0 {
		return fmt.Errorf("break-glass report missing keyVersions")
	}

	thing, err := m.store.RegistryStore().GetThing(ctx, req.ID)
	if err != nil {
		return fmt.Errorf("get thing %s: %w", req.ID, err)
	}

	const actorName = "break-glass"
	actorID := "break-glass:" + req.ActorTokenID
	if req.ActorTokenID == "" {
		actorID = "break-glass:unknown"
	}

	pool := m.txPool()

	for key, reportedVer := range req.KeyVersions {
		state, ok := req.Reported[key]
		if !ok {
			// The key version was declared but no reported state was attached;
			// skip rather than overwriting the template with a missing value.
			continue
		}

		cur, err := m.store.ConfigStore().GetConfigTemplate(ctx, thing.Type, key)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			m.logger.Warn("break-glass: get current template",
				"type", thing.Type, "key", key, "err", err)
			continue
		}
		if cur != nil && reportedVer <= cur.Version {
			// Admin write has already superseded this emergency override.
			continue
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("break-glass begin tx %s.%s: %w", thing.Type, key, err)
		}

		if _, err := m.store.ConfigStore().UpsertConfigTemplateAt(ctx, tx, thing.Type, key, state, reportedVer, actorID); err != nil {
			// A race with a concurrent admin UPDATE can leave the WHERE clause
			// unsatisfied (ErrNotFound from the RETURNING). Treat that as a
			// silent skip — the admin write has superseded us.
			_ = tx.Rollback(ctx)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return fmt.Errorf("break-glass upsert %s.%s: %w", thing.Type, key, err)
		}

		if err := m.store.ConfigStore().InsertConfigChangeEvent(ctx, tx, store.ConfigChangeEvent{
			ThingType:         thing.Type,
			ConfigKey:         key,
			Action:            "emergency_override",
			ActorID:           actorID,
			ActorName:         actorName,
			NewState:          state,
			NewVersion:        reportedVer,
			SourceIP:          req.SourceIP,
			EmergencyOverride: true,
		}); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("break-glass insert event %s.%s: %w", thing.Type, key, err)
		}

		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("break-glass commit %s.%s: %w", thing.Type, key, err)
		}
	}
	return nil
}
