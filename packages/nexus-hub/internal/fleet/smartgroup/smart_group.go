package smartgroup

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/device"
)

// SmartGroupSnapshot is one row from the recompute query: a smart
// group's id + its parsed predicate. Predicate is `*device.Predicate`
// so a malformed JSONB row surfaces as a per-group error at load time
// rather than silently producing an empty membership.
type SmartGroupSnapshot struct {
	ID        string
	Predicate device.Predicate
}

// ListSmartGroups returns every DeviceGroup with a non-NULL
// `membership_query`. Decoded once per recompute tick — the Hub job
// holds onto the decoded form and iterates devices.
func (s *Store) ListSmartGroups(ctx context.Context) ([]SmartGroupSnapshot, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, membership_query FROM "DeviceGroup"
		WHERE membership_query IS NOT NULL
	`)
	if err != nil {
		return nil, fmt.Errorf("list smart groups: %w", err)
	}
	defer rows.Close()
	out := []SmartGroupSnapshot{}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("scan smart group: %w", err)
		}
		var p device.Predicate
		if err := json.Unmarshal(raw, &p); err != nil {
			// Skip malformed rows but surface the error so the
			// recompute job logs which group is broken. Other smart
			// groups continue to recompute — a single bad row
			// shouldn't take out the whole fleet.
			return out, fmt.Errorf("smart group %s: bad predicate: %w", id, err)
		}
		out = append(out, SmartGroupSnapshot{ID: id, Predicate: p})
	}
	return out, rows.Err()
}

// LoadDevicesForSmartGroupEval returns every agent Thing as a
// device.Device. Reads the columns the predicate evaluator
// understands directly from `thing` +
// joins DeviceAssignment for boundUserId / boundUserOrgPath. Used by
// the Hub recompute job.
//
// Returns devices in stable id order so the recompute writer's
// (group_id, device_id) UPSERT is deterministic across runs.
func (s *Store) LoadDevicesForSmartGroupEval(ctx context.Context) ([]struct {
	ID  string
	Dev device.Device
}, error) {
	// Also pull every IamGroup the bound user is a member of
	// (IamGroupMembership.principalId = userId). The aggregated
	// idp_group_ids array is fed into Device.IdpGroupIDs so the
	// idp_group_member predicate can check membership without an
	// additional per-device round-trip.
	rows, err := s.db.Query(ctx, `
		SELECT
			t.id,
			COALESCE(t.os, ''),
			COALESCE(t.os_version, ''),
			COALESCE(t.version, ''),
			COALESCE(t.hostname, ''),
			COALESCE(t.primary_ip, ''),
			COALESCE(t.physical_id, ''),
			COALESCE(t.status, ''),
			COALESCE(da."userId", '')        AS bound_user_id,
			COALESCE(o.path, '')             AS bound_user_org_path,
			COALESCE(EXTRACT(EPOCH FROM t.enrolled_at)::bigint, 0)    AS enrolled_at_sec,
			COALESCE(EXTRACT(EPOCH FROM t.last_seen_at)::bigint, 0)   AS last_heartbeat_sec,
			COALESCE(
				(
					SELECT array_agg(igm."groupId" ORDER BY igm."groupId")
					FROM "IamGroupMembership" igm
					WHERE igm."principalType" IN ('nexus_user', 'admin_user')
					  AND igm."principalId" = da."userId"
				),
				ARRAY[]::text[]
			) AS idp_group_ids,
			COALESCE(t.tags, ARRAY[]::text[]) AS tags
		FROM thing t
		LEFT JOIN "DeviceAssignment" da
		    ON da."deviceId" = t.id AND da."releasedAt" IS NULL
		LEFT JOIN "NexusUser" u ON u.id = da."userId"
		LEFT JOIN "Organization" o ON o.id = u."organizationId"
		WHERE t.type = 'agent'
		ORDER BY t.id
	`)
	if err != nil {
		return nil, fmt.Errorf("load devices for smart group eval: %w", err)
	}
	defer rows.Close()
	out := []struct {
		ID  string
		Dev device.Device
	}{}
	for rows.Next() {
		var id string
		var d device.Device
		if err := rows.Scan(
			&id,
			&d.OS, &d.OSVersion, &d.AgentVersion,
			&d.Hostname, &d.PrimaryIP, &d.PhysicalID, &d.Status,
			&d.BoundUserID, &d.BoundUserOrgPath,
			&d.EnrolledAtSec, &d.LastHeartbeatSec,
			&d.IdpGroupIDs,
			&d.Tags,
		); err != nil {
			return nil, fmt.Errorf("scan device for smart eval: %w", err)
		}
		// Metadata escape hatch is not loaded here — keeping the
		// hot path lean. When a customer needs metadata-based smart
		// groups, lift it from thing.metadata jsonb with a
		// dedicated CTE.
		out = append(out, struct {
			ID  string
			Dev device.Device
		}{ID: id, Dev: d})
	}
	return out, rows.Err()
}

// EvictExpiredMemberships drops every static DeviceGroupMembership
// row whose expires_at has passed. Returns the count of rows removed
// for observability. Smart-cache rows never carry expiry (they're
// recomputed fresh each tick) so this query only touches the
// static-membership table.
func (s *Store) EvictExpiredMemberships(ctx context.Context) (int, error) {
	tag, err := s.db.Exec(ctx, `
		DELETE FROM "DeviceGroupMembership"
		WHERE expires_at IS NOT NULL AND expires_at <= NOW()
	`)
	if err != nil {
		return 0, fmt.Errorf("evict expired memberships: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ReplaceSmartGroupCache atomically swaps every cached row for the
// given group with the supplied member set. Uses DELETE+INSERT inside
// a single transaction so readers (GroupsOfDevice, etc.) never see a
// partial result. The empty-deviceIDs case is supported (group's
// predicate matches nothing right now → cache is emptied).
func (s *Store) ReplaceSmartGroupCache(ctx context.Context, groupID string, deviceIDs []string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM device_group_membership_cache WHERE group_id = $1`, groupID); err != nil {
		return fmt.Errorf("clear cache for %s: %w", groupID, err)
	}
	for _, did := range deviceIDs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO device_group_membership_cache (group_id, device_id) VALUES ($1, $2)
			ON CONFLICT (group_id, device_id) DO NOTHING
		`, groupID, did); err != nil {
			return fmt.Errorf("insert cache row (%s,%s): %w", groupID, did, err)
		}
	}
	return tx.Commit(ctx)
}
