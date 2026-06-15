package store

import (
	"context"
	"fmt"
	"time"
)

// DriftedThing represents a Thing with version mismatch or drift status.
type DriftedThing struct {
	ID          string     `json:"id"`
	Type        string     `json:"type"`
	Status      string     `json:"status"`
	DesiredVer  int64      `json:"desiredVer"`
	ReportedVer int64      `json:"reportedVer"`
	LastSeenAt  *time.Time `json:"lastSeenAt"`
	// OutOfSyncKeys is the sorted list of config keys whose desired value
	// differs from their reported value. Computed inline by a JSONB key
	// diff in ListDriftedThings; always a non-nil slice (empty when all
	// present keys match, which can happen when drift was triggered by a
	// version bump without content change).
	OutOfSyncKeys []string `json:"outOfSyncKeys"`
}

// FindDriftedThings returns online/degraded Things with version mismatch (for drift job).
func (s *Store) FindDriftedThings(ctx context.Context) ([]DriftedThing, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, type, status, desired_ver, reported_ver, last_seen_at
		FROM thing
		WHERE status IN ('online', 'drift')
		  AND desired_ver != reported_ver
		ORDER BY last_seen_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("find drifted: %w", err)
	}
	defer rows.Close()

	var result []DriftedThing
	for rows.Next() {
		var dt DriftedThing
		if err := rows.Scan(&dt.ID, &dt.Type, &dt.Status, &dt.DesiredVer, &dt.ReportedVer, &dt.LastSeenAt); err != nil {
			return nil, fmt.Errorf("scan drifted: %w", err)
		}
		result = append(result, dt)
	}
	return result, nil
}

// ListDriftedThings returns Things with status='drift' or version mismatch (for API).
// Each row includes OutOfSyncKeys — the sorted set of config keys whose desired
// value is DISTINCT from their reported value — computed inline via a JSONB key
// diff subquery. The array is never NULL on the wire (it may be empty when a
// version bump was the only source of drift, or when desired/reported are both
// empty).
func (s *Store) ListDriftedThings(ctx context.Context) ([]DriftedThing, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, type, status, desired_ver, reported_ver, last_seen_at,
		  COALESCE(
		    ARRAY(
		      SELECT key
		      FROM jsonb_object_keys(COALESCE(desired, '{}'::jsonb)) AS k(key)
		      WHERE COALESCE(desired, '{}'::jsonb)->key IS DISTINCT FROM COALESCE(reported, '{}'::jsonb)->key
		      ORDER BY key
		    ),
		    ARRAY[]::text[]
		  ) AS out_of_sync_keys
		FROM thing
		WHERE status = 'drift'
		   OR (status = 'online' AND desired_ver != reported_ver)
		ORDER BY last_seen_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list drifted: %w", err)
	}
	defer rows.Close()

	var result []DriftedThing
	for rows.Next() {
		var dt DriftedThing
		var keys []string
		if err := rows.Scan(&dt.ID, &dt.Type, &dt.Status, &dt.DesiredVer, &dt.ReportedVer, &dt.LastSeenAt, &keys); err != nil {
			return nil, fmt.Errorf("scan drifted: %w", err)
		}
		if keys == nil {
			keys = []string{}
		}
		dt.OutOfSyncKeys = keys
		result = append(result, dt)
	}
	return result, nil
}

// ContentCheckCandidate identifies an online Thing whose desired and reported
// versions are EQUAL — the population the drift job's content-reconcile pass
// must per-key content-diff. Version equality means the version-only
// FindDriftedThings query skips these Things entirely, so a manual DB edit or a
// dropped/partial apply that still re-stamped the version is invisible there.
type ContentCheckCandidate struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// FindEqualVersionOnlineThings returns online Things whose desired_ver equals
// reported_ver — the candidate set for the content-reconcile pass.
//
// Scope is deliberately bounded: status='online' only (a Thing that is offline
// or already flagged 'drift' is either unreachable or already surfaced to ops,
// so re-diffing it wastes a heavy GetThing join per row) and version-equal only
// (version-unequal Things are already handled by FindDriftedThings, so including
// them here would double-repair). This query is cheap — it returns id/type only;
// the expense is the per-candidate GetShadowComparison the caller runs, which is
// why the content pass is throttled to a slower cadence than the version pass.
func (s *Store) FindEqualVersionOnlineThings(ctx context.Context) ([]ContentCheckCandidate, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, type
		FROM thing
		WHERE status = 'online'
		  AND desired_ver = reported_ver
		ORDER BY last_seen_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("find equal-version online things: %w", err)
	}
	defer rows.Close()

	var result []ContentCheckCandidate
	for rows.Next() {
		var c ContentCheckCandidate
		if err := rows.Scan(&c.ID, &c.Type); err != nil {
			return nil, fmt.Errorf("scan content candidate: %w", err)
		}
		result = append(result, c)
	}
	return result, nil
}
