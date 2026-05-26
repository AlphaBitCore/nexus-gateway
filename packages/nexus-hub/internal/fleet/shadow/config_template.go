package shadow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConfigTemplate is a row from thing_config_template.
type ConfigTemplate struct {
	Type      string    `json:"type"`
	ConfigKey string    `json:"configKey"`
	State     any       `json:"state"`
	Version   int64     `json:"version"`
	UpdatedAt time.Time `json:"updatedAt"`
	UpdatedBy string    `json:"updatedBy"`
}

// GetConfigTemplates returns all templates for a given Thing type.
func (s *Store) GetConfigTemplates(ctx context.Context, thingType string) ([]ConfigTemplate, error) {
	rows, err := s.db.Query(ctx, `
		SELECT type, config_key, state, version, updated_at, COALESCE(updated_by, '')
		FROM thing_config_template
		WHERE type = $1
		ORDER BY config_key
	`, thingType)
	if err != nil {
		return nil, fmt.Errorf("get config templates: %w", err)
	}
	defer rows.Close()

	var templates []ConfigTemplate
	for rows.Next() {
		var t ConfigTemplate
		var stateRaw []byte
		if err := rows.Scan(&t.Type, &t.ConfigKey, &stateRaw, &t.Version, &t.UpdatedAt, &t.UpdatedBy); err != nil {
			return nil, fmt.Errorf("scan template: %w", err)
		}
		if err := decodeJSONB(stateRaw, &t.State, "state"); err != nil {
			return nil, err
		}
		templates = append(templates, t)
	}
	return templates, nil
}

// GetConfigTemplate returns a single template by type and key.
func (s *Store) GetConfigTemplate(ctx context.Context, thingType, configKey string) (*ConfigTemplate, error) {
	var t ConfigTemplate
	var stateRaw []byte
	err := s.db.QueryRow(ctx, `
		SELECT type, config_key, state, version, updated_at, COALESCE(updated_by, '')
		FROM thing_config_template
		WHERE type = $1 AND config_key = $2
	`, thingType, configKey).Scan(&t.Type, &t.ConfigKey, &stateRaw, &t.Version, &t.UpdatedAt, &t.UpdatedBy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get config template: %w", err)
	}
	if err := decodeJSONB(stateRaw, &t.State, "state"); err != nil {
		return nil, err
	}
	return &t, nil
}

// ConfigTemplateCatalogEntry groups the config keys that exist for a single
// Thing type in thing_config_template. Used by the admin Config Sync filters
// to populate the Type / Config Key selects from real data instead of a
// hardcoded allow-list.
type ConfigTemplateCatalogEntry struct {
	ThingType  string   `json:"thingType"`
	ConfigKeys []string `json:"configKeys"`
}

// ListConfigTemplateCatalog returns every (type, config_key) pair present in
// thing_config_template grouped by type, sorted deterministically. The
// result drives the admin Config Sync history filters, so empty or in-flight
// templates would otherwise leak "bump-mode"-style stale options into the UI.
func (s *Store) ListConfigTemplateCatalog(ctx context.Context) ([]ConfigTemplateCatalogEntry, error) {
	rows, err := s.db.Query(ctx, `
		SELECT type, config_key
		FROM thing_config_template
		ORDER BY type, config_key
	`)
	if err != nil {
		return nil, fmt.Errorf("list config template catalog: %w", err)
	}
	defer rows.Close()

	byType := make(map[string][]string)
	var order []string
	for rows.Next() {
		var t, key string
		if err := rows.Scan(&t, &key); err != nil {
			return nil, fmt.Errorf("scan catalog row: %w", err)
		}
		if _, ok := byType[t]; !ok {
			order = append(order, t)
		}
		byType[t] = append(byType[t], key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog rows: %w", err)
	}

	out := make([]ConfigTemplateCatalogEntry, 0, len(order))
	for _, t := range order {
		out = append(out, ConfigTemplateCatalogEntry{ThingType: t, ConfigKeys: byType[t]})
	}
	return out, nil
}

// UpsertConfigTemplate creates or updates a config template and increments version.
// Returns the new version. Used inside a transaction.
func (s *Store) UpsertConfigTemplate(ctx context.Context, tx pgx.Tx, thingType, configKey string, state any, actorID string) (int64, error) {
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return 0, fmt.Errorf("marshal state: %w", err)
	}

	var newVer int64
	err = tx.QueryRow(ctx, `
		INSERT INTO thing_config_template (type, config_key, state, version, updated_at, updated_by)
		VALUES ($1, $2, $3, 1, NOW(), $4)
		ON CONFLICT (type, config_key) DO UPDATE SET
			state      = EXCLUDED.state,
			version    = thing_config_template.version + 1,
			updated_at = NOW(),
			updated_by = EXCLUDED.updated_by
		RETURNING version
	`, thingType, configKey, stateJSON, actorID).Scan(&newVer)
	if err != nil {
		return 0, fmt.Errorf("upsert template: %w", err)
	}
	return newVer, nil
}

// UpsertConfigTemplateAt is UpsertConfigTemplate with an explicit version
// supplied by the caller. The UPDATE branch only commits when the incoming
// version strictly exceeds the stored version; a stale incoming version is a
// silent no-op (ErrNotFound is returned from QueryRow when no row is RETURNING).
// Used exclusively by break-glass reconciliation: the data plane has already
// bumped its own key version locally and the Hub must adopt that exact value
// rather than the usual monotonic +1. Must be called inside a transaction.
func (s *Store) UpsertConfigTemplateAt(ctx context.Context, tx pgx.Tx, thingType, configKey string, state any, version int64, actorID string) (int64, error) {
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return 0, fmt.Errorf("marshal state: %w", err)
	}

	var newVer int64
	err = tx.QueryRow(ctx, `
		INSERT INTO thing_config_template (type, config_key, state, version, updated_at, updated_by)
		VALUES ($1, $2, $3, $4, NOW(), $5)
		ON CONFLICT (type, config_key) DO UPDATE SET
			state      = EXCLUDED.state,
			version    = EXCLUDED.version,
			updated_at = NOW(),
			updated_by = EXCLUDED.updated_by
		WHERE EXCLUDED.version > thing_config_template.version
		RETURNING version
	`, thingType, configKey, stateJSON, version, actorID).Scan(&newVer)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("upsert template at %d: %w", version, err)
	}
	return newVer, nil
}
