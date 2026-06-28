package overrides

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ThingConfigOverride is a row from thing_config_override — the per-Thing
// override layer that sits on top of thing_config_template. See spec
// the node-per-thing-override-and-force-sync design §4 for the full data model.
//
// State is the wrapped OverrideState newtype: construction via
// NewOverrideState validates the JSON-object invariant ("state MUST be a JSON
// object at top level") so the store and manager cannot receive a `null` /
// array / scalar through any constructor path. Callers obtain the
// canonical bytes via State.Bytes() — JSON round-tripping (MarshalJSON /
// UnmarshalJSON) is value-preserving for object payloads.
type ThingConfigOverride struct {
	ThingID           string
	ConfigKey         string
	State             OverrideState
	TemplateVerAtSet  int64
	SetBy             string
	SetAt             time.Time
	Reason            *string
	ExpiresAt         *time.Time
	EmergencyOverride bool
}

// ThingConfigOverrideWithStale extends ThingConfigOverride with the JOIN-time
// stale-detection fields. CurrentTemplateVer reflects the matching
// thing_config_template row's version at query time; Stale is true when
// TemplateVerAtSet < CurrentTemplateVer (template moved on after override
// was set). Computed in SQL, never persisted.
type ThingConfigOverrideWithStale struct {
	ThingConfigOverride
	CurrentTemplateVer int64
	Stale              bool
}

// ThingConfigOverrideWithStaleAndThing is the row shape returned by
// ListAllOverrides — embeds the stale-aware override plus the joined
// thing.name and thing.type for the global registry page (Hub /admin
// surface + UI).
type ThingConfigOverrideWithStaleAndThing struct {
	ThingConfigOverrideWithStale
	ThingName string
	ThingType string
}

// ListOverridesFilter scopes ListAllOverrides. All fields are optional — nil
// pointer means "do not filter on this dimension". Limit defaults to 50 and
// is clamped to [1, 500]; Offset is clamped to >= 0.
type ListOverridesFilter struct {
	ThingType *string
	Actor     *string
	HasTTL    *bool
	Stale     *bool
	Limit     int
	Offset    int
}

// normalizeOverrideListFilter clamps pagination to [1, 500] / [0, +inf]. Same
// safe-defaulting pattern as normalizeListThingsParams. The function is
// exported via the *behavior* of ListAllOverrides; tests rely on the defaults.
func normalizeOverrideListFilter(f *ListOverridesFilter) {
	if f.Limit < 1 {
		f.Limit = 50
	}
	if f.Limit > 500 {
		f.Limit = 500
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
}

// ListOverridesSummary is the top-of-page aggregate for the global registry.
//
// TotalNodes counts distinct thing_id over the *filtered* row set;
// TotalOverrides is the row count over the same filter; StaleCount is the
// number of rows where the template has moved past the snapshot;
// ExpiringSoonCount is the number of rows whose expires_at falls inside
// (NOW(), NOW()+1h]. The summary is computed against the same WHERE clause as
// the row query — filtering the page also filters the summary.
type ListOverridesSummary struct {
	TotalNodes        int64
	TotalOverrides    int64
	StaleCount        int64
	ExpiringSoonCount int64
}

// GetOverride returns a single override row by (thing_id, config_key).
// Returns ErrNotFound when the row is absent.
func (s *Store) GetOverride(ctx context.Context, thingID, configKey string) (*ThingConfigOverride, error) {
	o := &ThingConfigOverride{}
	var stateRaw []byte
	err := s.db.QueryRow(ctx, `
		SELECT thing_id, config_key, state, template_ver_at_set, set_by, set_at,
		       reason, expires_at, emergency_override
		FROM thing_config_override
		WHERE thing_id = $1 AND config_key = $2
	`, thingID, configKey).Scan(
		&o.ThingID, &o.ConfigKey, &stateRaw, &o.TemplateVerAtSet, &o.SetBy, &o.SetAt,
		&o.Reason, &o.ExpiresAt, &o.EmergencyOverride,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get override %s/%s: %w", thingID, configKey, err)
	}
	o.State = overrideStateFromDB(stateRaw)
	return o, nil
}

// UpsertOverride inserts or replaces a row in thing_config_override. Every
// non-PK column is overwritten on conflict, including set_at (refreshed to
// NOW()) — the spec models a re-set as a brand new override that just happens
// to share the same key. Must run inside a tx so the caller can co-commit
// the merge-cache write (WriteDesiredAndBumpVer) atomically.
func (s *Store) UpsertOverride(ctx context.Context, tx pgx.Tx, o ThingConfigOverride) error {
	stateBytes := o.State.Bytes()
	if len(stateBytes) == 0 {
		return fmt.Errorf("upsert override %s/%s: state is empty", o.ThingID, o.ConfigKey)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO thing_config_override (
			thing_id, config_key, state, template_ver_at_set,
			set_by, set_at, reason, expires_at, emergency_override
		) VALUES (
			$1, $2, $3::jsonb, $4,
			$5, NOW(), $6, $7, $8
		)
		ON CONFLICT (thing_id, config_key) DO UPDATE SET
			state               = EXCLUDED.state,
			template_ver_at_set = EXCLUDED.template_ver_at_set,
			set_by              = EXCLUDED.set_by,
			set_at              = NOW(),
			reason              = EXCLUDED.reason,
			expires_at          = EXCLUDED.expires_at,
			emergency_override  = EXCLUDED.emergency_override
	`,
		o.ThingID, o.ConfigKey, stateBytes, o.TemplateVerAtSet,
		o.SetBy, o.Reason, o.ExpiresAt, o.EmergencyOverride,
	)
	if err != nil {
		return fmt.Errorf("upsert override %s/%s: %w", o.ThingID, o.ConfigKey, err)
	}
	return nil
}

// DeleteOverride removes a (thing_id, config_key) row. Returns existed=true
// when a row was actually removed so the caller (admin handler) can map a
// missing row to 404 without a separate Get round-trip. err != nil only on
// real DB failure — not on missing-row.
func (s *Store) DeleteOverride(ctx context.Context, tx pgx.Tx, thingID, configKey string) (bool, error) {
	tag, err := tx.Exec(ctx, `
		DELETE FROM thing_config_override
		WHERE thing_id = $1 AND config_key = $2
	`, thingID, configKey)
	if err != nil {
		return false, fmt.Errorf("delete override %s/%s: %w", thingID, configKey, err)
	}
	return tag.RowsAffected() > 0, nil
}

// ListOverridesByThing returns every override row for a single Thing with
// stale detection JOINed in. Stale = (template.version > tco.template_ver_at_set).
// Rows are ordered by config_key ascending so the UI can render deterministically.
func (s *Store) ListOverridesByThing(ctx context.Context, thingID string) ([]ThingConfigOverrideWithStale, error) {
	rows, err := s.db.Query(ctx, `
		SELECT tco.thing_id, tco.config_key, tco.state, tco.template_ver_at_set,
		       tco.set_by, tco.set_at, tco.reason, tco.expires_at, tco.emergency_override,
		       COALESCE(tct.version, 0) AS current_template_ver,
		       COALESCE(tct.version, 0) > tco.template_ver_at_set AS stale
		FROM thing_config_override tco
		JOIN thing t ON t.id = tco.thing_id
		LEFT JOIN thing_config_template tct
		  ON tct.type = t.type AND tct.config_key = tco.config_key
		WHERE tco.thing_id = $1
		ORDER BY tco.config_key ASC
	`, thingID)
	if err != nil {
		return nil, fmt.Errorf("list overrides by thing %s: %w", thingID, err)
	}
	defer rows.Close()

	var out []ThingConfigOverrideWithStale
	for rows.Next() {
		var r ThingConfigOverrideWithStale
		var stateRaw []byte
		if err := rows.Scan(
			&r.ThingID, &r.ConfigKey, &stateRaw, &r.TemplateVerAtSet,
			&r.SetBy, &r.SetAt, &r.Reason, &r.ExpiresAt, &r.EmergencyOverride,
			&r.CurrentTemplateVer, &r.Stale,
		); err != nil {
			return nil, fmt.Errorf("scan override row: %w", err)
		}
		r.State = overrideStateFromDB(stateRaw)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate override rows: %w", err)
	}
	return out, nil
}

// ListAllOverrides drives the global override registry page. Returns the page
// of rows + total (filtered) + summary aggregates in a single round-trip from
// the caller's perspective. The summary respects the same filter as the rows,
// so a filtered page shows summary numbers for that filter.
//
// Implementation: one query for rows (with LIMIT/OFFSET), one query for total
// + summary (CTE that aggregates the same filtered set). This matches the
// pattern used by ListThings (count then list) and keeps the row query simple.
func (s *Store) ListAllOverrides(ctx context.Context, filter ListOverridesFilter) ([]ThingConfigOverrideWithStaleAndThing, int64, ListOverridesSummary, error) {
	normalizeOverrideListFilter(&filter)

	// Build the WHERE clause once and share it between the row-fetch and the
	// summary aggregation so both queries see exactly the same filtered set.
	var conds []string
	args := []any{}
	idx := 1
	if filter.ThingType != nil {
		conds = append(conds, fmt.Sprintf("t.type = $%d", idx))
		args = append(args, *filter.ThingType)
		idx++
	}
	if filter.Actor != nil {
		conds = append(conds, fmt.Sprintf("tco.set_by = $%d", idx))
		args = append(args, *filter.Actor)
		idx++
	}
	if filter.HasTTL != nil {
		if *filter.HasTTL {
			conds = append(conds, "tco.expires_at IS NOT NULL")
		} else {
			conds = append(conds, "tco.expires_at IS NULL")
		}
	}
	if filter.Stale != nil {
		if *filter.Stale {
			conds = append(conds, "COALESCE(tct.version, 0) > tco.template_ver_at_set")
		} else {
			conds = append(conds, "COALESCE(tct.version, 0) <= tco.template_ver_at_set")
		}
	}
	whereSQL := ""
	if len(conds) > 0 {
		whereSQL = "WHERE " + strings.Join(conds, " AND ")
	}

	rowQuery := fmt.Sprintf(`
		SELECT tco.thing_id, tco.config_key, tco.state, tco.template_ver_at_set,
		       tco.set_by, tco.set_at, tco.reason, tco.expires_at, tco.emergency_override,
		       COALESCE(tct.version, 0) AS current_template_ver,
		       COALESCE(tct.version, 0) > tco.template_ver_at_set AS stale,
		       COALESCE(t.name, '') AS thing_name,
		       t.type AS thing_type
		FROM thing_config_override tco
		JOIN thing t ON t.id = tco.thing_id
		LEFT JOIN thing_config_template tct
		  ON tct.type = t.type AND tct.config_key = tco.config_key
		%s
		ORDER BY tco.set_at DESC, tco.thing_id ASC, tco.config_key ASC
		LIMIT $%d OFFSET $%d
	`, whereSQL, idx, idx+1)
	rowArgs := append(append([]any{}, args...), filter.Limit, filter.Offset)

	rows, err := s.db.Query(ctx, rowQuery, rowArgs...)
	if err != nil {
		return nil, 0, ListOverridesSummary{}, fmt.Errorf("list all overrides: %w", err)
	}
	defer rows.Close()

	var out []ThingConfigOverrideWithStaleAndThing
	for rows.Next() {
		var r ThingConfigOverrideWithStaleAndThing
		var stateRaw []byte
		if err := rows.Scan(
			&r.ThingID, &r.ConfigKey, &stateRaw, &r.TemplateVerAtSet,
			&r.SetBy, &r.SetAt, &r.Reason, &r.ExpiresAt, &r.EmergencyOverride,
			&r.CurrentTemplateVer, &r.Stale,
			&r.ThingName, &r.ThingType,
		); err != nil {
			return nil, 0, ListOverridesSummary{}, fmt.Errorf("scan override list row: %w", err)
		}
		r.State = overrideStateFromDB(stateRaw)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, ListOverridesSummary{}, fmt.Errorf("iterate override list rows: %w", err)
	}

	summaryQuery := fmt.Sprintf(`
		SELECT
			COUNT(*) AS total_overrides,
			COUNT(DISTINCT tco.thing_id) AS total_nodes,
			COALESCE(SUM(CASE WHEN COALESCE(tct.version, 0) > tco.template_ver_at_set THEN 1 ELSE 0 END), 0) AS stale_count,
			COALESCE(SUM(CASE WHEN tco.expires_at IS NOT NULL
			                   AND tco.expires_at >  NOW()
			                   AND tco.expires_at <= NOW() + INTERVAL '1 hour'
			                  THEN 1 ELSE 0 END), 0) AS expiring_soon
		FROM thing_config_override tco
		JOIN thing t ON t.id = tco.thing_id
		LEFT JOIN thing_config_template tct
		  ON tct.type = t.type AND tct.config_key = tco.config_key
		%s
	`, whereSQL)

	var summary ListOverridesSummary
	if err := s.db.QueryRow(ctx, summaryQuery, args...).Scan(
		&summary.TotalOverrides, &summary.TotalNodes,
		&summary.StaleCount, &summary.ExpiringSoonCount,
	); err != nil {
		return nil, 0, ListOverridesSummary{}, fmt.Errorf("summary overrides: %w", err)
	}

	return out, summary.TotalOverrides, summary, nil
}

// ListExpiredOverrides returns overrides whose expires_at fell before the
// supplied cutoff. Used by the override-expiry job: the caller supplies
// time.Now() and clears each row in a single
// transaction. Rows with expires_at IS NULL are permanent and never returned.
// Order is ascending so the oldest expirations are processed first.
func (s *Store) ListExpiredOverrides(ctx context.Context, before time.Time) ([]ThingConfigOverride, error) {
	rows, err := s.db.Query(ctx, `
		SELECT thing_id, config_key, state, template_ver_at_set, set_by, set_at,
		       reason, expires_at, emergency_override
		FROM thing_config_override
		WHERE expires_at IS NOT NULL AND expires_at < $1
		ORDER BY expires_at ASC
	`, before)
	if err != nil {
		return nil, fmt.Errorf("list expired overrides: %w", err)
	}
	defer rows.Close()

	var out []ThingConfigOverride
	for rows.Next() {
		var o ThingConfigOverride
		var stateRaw []byte
		if err := rows.Scan(
			&o.ThingID, &o.ConfigKey, &stateRaw, &o.TemplateVerAtSet,
			&o.SetBy, &o.SetAt, &o.Reason, &o.ExpiresAt, &o.EmergencyOverride,
		); err != nil {
			return nil, fmt.Errorf("scan expired override: %w", err)
		}
		o.State = overrideStateFromDB(stateRaw)
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired overrides: %w", err)
	}
	return out, nil
}
