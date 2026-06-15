package agentstore

import (
	"context"
	"fmt"
)

// MembersOfGroup returns the device IDs in the given group (static
// rows in DeviceGroupMembership UNION smart-cache rows in
// device_group_membership_cache). Filters out memberships whose
// expiry window has lapsed so callers (bulk-by-group force-refresh)
// only target devices that are currently in the group.
func (store *Store) MembersOfGroup(ctx context.Context, groupID string) ([]string, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT "deviceId" FROM "DeviceGroupMembership"
		WHERE "groupId" = $1 AND (expires_at IS NULL OR expires_at > NOW())
		UNION
		SELECT device_id FROM device_group_membership_cache WHERE group_id = $1
	`, groupID)
	if err != nil {
		return nil, fmt.Errorf("members of group %s: %w", groupID, err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var did string
		if err := rows.Scan(&did); err != nil {
			return nil, err
		}
		out = append(out, did)
	}
	return out, rows.Err()
}
