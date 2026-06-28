package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Store) UpdateLastSeen(ctx context.Context, id string) error {
	tag, err := s.db.Exec(ctx,
		`UPDATE thing SET last_seen_at = NOW(), updated_at = NOW() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("update last_seen: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RefreshLiveness refreshes last_seen_at for a Thing and promotes offline back
// to online — called from the WS ping loop once per successful Hub→Thing ping.
// It deliberately leaves enrolled, drift, and revoked statuses untouched:
// ping proves reachability, not config sync nor administrative state.
//
// On the offline→online edge it also stamps process_started_at = NOW() and
// resets reported_outcomes to {}. A Thing that went offline and came back
// almost certainly restarted (or at least re-established its WS session),
// so the previous in-memory OutcomeTracker is gone and stale ledger entries
// would mislead operators until the next successful apply rewrote them.
func (s *Store) RefreshLiveness(ctx context.Context, id string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE thing
		SET last_seen_at = NOW(),
		    updated_at   = NOW(),
		    status       = CASE WHEN status = 'offline' THEN 'online' ELSE status END,
		    process_started_at = CASE WHEN status = 'offline' THEN NOW() ELSE process_started_at END,
		    reported_outcomes  = CASE WHEN status = 'offline' THEN '{}'::jsonb ELSE reported_outcomes END
		WHERE id = $1
	`, id)
	if err != nil {
		return fmt.Errorf("refresh liveness: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// HeartbeatResult is returned by HandleHeartbeat with version info.
type HeartbeatResult struct {
	DesiredVer int64          `json:"desiredVer"`
	Desired    map[string]any `json:"desired"`
}

// Heartbeat updates last_seen_at and returns desired config if versions differ.
func (s *Store) Heartbeat(ctx context.Context, id, status string, metadata map[string]any, reportedVer int64) (*HeartbeatResult, error) {
	metaJSON, _ := json.Marshal(metadata)

	// Extract identity attributes from metadata.staticInfo so they can
	// land in first-class columns. The same values stay inside the
	// metadata jsonb blob too (caller-provided), so any legacy reader
	// continues to work; the columns just give list/detail UIs an
	// indexed, query-friendly path. Each pluck is best-effort —
	// missing keys leave the column unchanged (COALESCE).
	var hostname, primaryIP, osVal, osVersion any
	if static, ok := metadata["staticInfo"].(map[string]any); ok {
		if v, ok := static["hostname"].(string); ok && v != "" {
			hostname = v
		}
		if v, ok := static["primaryIp"].(string); ok && v != "" {
			primaryIP = v
		}
		if v, ok := static["os"].(string); ok && v != "" {
			osVal = v
		}
		if v, ok := static["osVersion"].(string); ok && v != "" {
			osVersion = v
		}
	}

	// Status gate: the HTTP heartbeat is the fallback liveness path,
	// the mirror of the WS ping loop's RefreshLiveness. Like RefreshLiveness it
	// must only promote offline→online and otherwise leave the status untouched.
	// Writing $2 ("online") unconditionally would clobber drift→online (a
	// drifted Thing prematurely clears drift, only to re-flip on the next drift
	// scan) and revoked→online (a second revocation-bypass vector). On the
	// offline→online edge it also stamps process_started_at = NOW() and resets
	// reported_outcomes to {}, exactly like RefreshLiveness: a Thing that went
	// offline and came back almost certainly restarted, so its prior in-memory
	// OutcomeTracker is gone and the stale ledger would mislead operators until
	// the next apply rewrote it. Drift clearing stays the sole job of
	// UpdateShadowReport (version-guarded).
	var desiredVer int64
	var desiredRaw []byte
	err := s.db.QueryRow(ctx, `
		UPDATE thing
		SET status      = CASE WHEN status = 'offline' THEN $2 ELSE status END,
		    process_started_at = CASE WHEN status = 'offline' THEN NOW() ELSE process_started_at END,
		    reported_outcomes  = CASE WHEN status = 'offline' THEN '{}'::jsonb ELSE reported_outcomes END,
		    metadata    = COALESCE($3::jsonb, metadata),
		    hostname    = COALESCE($4, hostname),
		    primary_ip  = COALESCE($5, primary_ip),
		    os          = COALESCE($6, os),
		    os_version  = COALESCE($7, os_version),
		    last_seen_at = NOW(),
		    updated_at  = NOW()
		WHERE id = $1
		RETURNING desired_ver, desired
	`, id, status, metaJSON, hostname, primaryIP, osVal, osVersion).Scan(&desiredVer, &desiredRaw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("heartbeat: %w", err)
	}

	result := &HeartbeatResult{DesiredVer: desiredVer}
	if desiredVer != reportedVer {
		if err := decodeJSONB(desiredRaw, &result.Desired, "desired"); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// MarkOffline sets a Thing's status to offline and updates last_seen_at.
func (s *Store) MarkOffline(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx,
		`UPDATE thing SET status = 'offline', last_seen_at = NOW(), updated_at = NOW() WHERE id = $1`, id)
	return err
}

// MarkStaleOffline sets status='offline' for Things of the given types that are
// currently online or drift and whose last_seen_at is older than threshold.
// Returns rows affected.
func (s *Store) MarkStaleOffline(ctx context.Context, types []string, threshold time.Duration) (int64, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE thing
		SET status = 'offline', updated_at = NOW()
		WHERE type = ANY($1)
		  AND status IN ('online', 'drift')
		  AND last_seen_at < NOW() - make_interval(secs => $2::double precision)
	`, types, threshold.Seconds())
	if err != nil {
		return 0, fmt.Errorf("mark stale offline: %w", err)
	}
	return tag.RowsAffected(), nil
}
