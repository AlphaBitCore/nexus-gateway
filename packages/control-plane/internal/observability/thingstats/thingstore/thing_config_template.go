package thingstore

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"time"
)

// ThingConfigTemplate is the per-(type, config_key) desired state record.
// It stores the canonical desired config for a given Thing type and config key.
// New Things are initialized from these templates on enrollment.
type ThingConfigTemplate struct {
	Type      string          `json:"type"`
	ConfigKey string          `json:"configKey"`
	State     json.RawMessage `json:"state"`
	Version   int64           `json:"version"`
	UpdatedAt time.Time       `json:"updatedAt"`
	UpdatedBy *string         `json:"updatedBy,omitempty"`
}

// GetTemplate returns a single template by (type, config_key).
// Returns (nil, nil) if not found.
func (store *Store) GetTemplate(ctx context.Context, thingType, configKey string) (*ThingConfigTemplate, error) {
	query := `
		SELECT type, config_key, state, version, updated_at, updated_by
		FROM thing_config_template
		WHERE type = $1 AND config_key = $2`
	var t ThingConfigTemplate
	err := store.pool.QueryRow(ctx, query, thingType, configKey).Scan(
		&t.Type, &t.ConfigKey, &t.State, &t.Version, &t.UpdatedAt, &t.UpdatedBy,
	)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("get template: %w", err)
	}
	return &t, nil
}

// ListTemplatesByType returns all templates for a given Thing type.
func (store *Store) ListTemplatesByType(ctx context.Context, thingType string) ([]ThingConfigTemplate, error) {
	query := `
		SELECT type, config_key, state, version, updated_at, updated_by
		FROM thing_config_template
		WHERE type = $1
		ORDER BY config_key`
	rows, err := store.pool.Query(ctx, query, thingType)
	if err != nil {
		return nil, fmt.Errorf("list templates by type: %w", err)
	}
	defer rows.Close()

	var result []ThingConfigTemplate
	for rows.Next() {
		var t ThingConfigTemplate
		if err := rows.Scan(&t.Type, &t.ConfigKey, &t.State, &t.Version, &t.UpdatedAt, &t.UpdatedBy); err != nil {
			return nil, fmt.Errorf("scan template: %w", err)
		}
		result = append(result, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate templates: %w", err)
	}
	return result, nil
}
