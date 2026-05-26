package agentstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DeviceGroup represents a row from the DeviceGroup table.
type DeviceGroup struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description *string   `json:"description"`
	CreatedBy   *string   `json:"createdBy"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	MemberCount *int      `json:"memberCount,omitempty"`
}

const dgColumns = `id, name, description, "createdBy", "createdAt", "updatedAt"`

// DeviceGroupListParams holds filter/pagination.
type DeviceGroupListParams struct {
	Q      string
	Limit  int
	Offset int
}

// ListDeviceGroups returns device groups with member/policy counts.
func (store *Store) ListDeviceGroups(ctx context.Context, p DeviceGroupListParams) ([]DeviceGroup, int, error) {
	where := "WHERE 1=1"
	args := []any{}
	argIdx := 1

	if p.Q != "" {
		where += fmt.Sprintf(` AND name ILIKE $%d`, argIdx)
		args = append(args, "%"+escapeILIKE(p.Q)+"%")
		argIdx++
	}

	var total int
	if err := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM "DeviceGroup" %s`, where), args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := fmt.Sprintf(`
		SELECT g.%s,
			(SELECT COUNT(*) FROM "DeviceGroupMembership" m WHERE m."groupId" = g.id) AS member_count
		FROM "DeviceGroup" g
		%s ORDER BY g."createdAt" DESC LIMIT $%d OFFSET $%d
	`, dgColumns, where, argIdx, argIdx+1)
	args = append(args, p.Limit, p.Offset)

	rows, err := store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	groups := []DeviceGroup{}
	for rows.Next() {
		var g DeviceGroup
		var mc int
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.CreatedBy,
			&g.CreatedAt, &g.UpdatedAt, &mc); err != nil {
			return nil, 0, err
		}
		g.MemberCount = &mc
		groups = append(groups, g)
	}
	return groups, total, rows.Err()
}

// GetDeviceGroup returns a device group by ID.
func (store *Store) GetDeviceGroup(ctx context.Context, id string) (*DeviceGroup, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`SELECT %s FROM "DeviceGroup" WHERE id = $1`, dgColumns), id)
	var g DeviceGroup
	err := row.Scan(&g.ID, &g.Name, &g.Description, &g.CreatedBy, &g.CreatedAt, &g.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &g, err
}

// CreateDeviceGroup inserts a new device group.
func (store *Store) CreateDeviceGroup(ctx context.Context, name string, description *string, createdBy string) (*DeviceGroup, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO "DeviceGroup" (id, name, description, "createdBy", "createdAt", "updatedAt")
		VALUES (gen_random_uuid(), $1, $2, $3, NOW(), NOW())
		RETURNING %s
	`, dgColumns), name, description, createdBy)
	var g DeviceGroup
	err := row.Scan(&g.ID, &g.Name, &g.Description, &g.CreatedBy, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// UpdateDeviceGroupParams holds optional fields for updating a device group.
type UpdateDeviceGroupParams struct {
	Name        *string
	Description *string
}

// UpdateDeviceGroup updates a device group using COALESCE.
func (store *Store) UpdateDeviceGroup(ctx context.Context, id string, p UpdateDeviceGroupParams) (*DeviceGroup, error) {
	row := store.pool.QueryRow(ctx, fmt.Sprintf(`UPDATE "DeviceGroup" SET
		name = COALESCE($2, name), description = COALESCE($3, description), "updatedAt" = NOW()
	WHERE id = $1 RETURNING %s`, dgColumns), id, p.Name, p.Description)
	var g DeviceGroup
	err := row.Scan(&g.ID, &g.Name, &g.Description, &g.CreatedBy, &g.CreatedAt, &g.UpdatedAt)
	return &g, err
}

// SetSmartGroupQuery sets or clears the membership_query on a group.
// A non-nil query flips the group to smart mode; nil clears
// it back to static. When switching from smart → static, the cached
// memberships are also wiped (callers that want to materialize the
// cache as static rows should do that BEFORE calling this, otherwise
// the cache rows are simply dropped). When switching static → smart,
// the existing DeviceGroupMembership rows are left untouched but
// have no effect — GroupsOfDevice now reads from the cache table.
//
// Returns the updated group row. Returns nil + ErrNotFound when the
// group doesn't exist.
func (store *Store) SetSmartGroupQuery(ctx context.Context, id string, query []byte) (*DeviceGroup, error) {
	// Wipe stale cache when going smart → static. Done first so a
	// failed update doesn't leave the cache dangling.
	if query == nil {
		if _, err := store.pool.Exec(ctx, `DELETE FROM device_group_membership_cache WHERE group_id = $1`, id); err != nil {
			return nil, fmt.Errorf("wipe cache for %s: %w", id, err)
		}
	}

	row := store.pool.QueryRow(ctx, fmt.Sprintf(`UPDATE "DeviceGroup" SET
		membership_query = $2, "updatedAt" = NOW()
	WHERE id = $1 RETURNING %s`, dgColumns), id, query)
	var g DeviceGroup
	if err := row.Scan(&g.ID, &g.Name, &g.Description, &g.CreatedBy, &g.CreatedAt, &g.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // not found — caller surfaces 404
		}
		return nil, fmt.Errorf("set smart query for %s: %w", id, err)
	}
	return &g, nil
}

// DeleteDeviceGroup deletes a device group (cascade memberships and rules).
func (store *Store) DeleteDeviceGroup(ctx context.Context, id string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "DeviceGroup" WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// AddDeviceToGroup adds a device membership. expiresAt is optional —
// nil means permanent; non-nil scopes
// the membership to a window after which it's evicted by the
// SmartGroupRecompute tick AND filtered at read time.
//
// ON CONFLICT keeps the original (groupId, deviceId) row but lets
// callers update its expires_at — useful for "extend the quarantine
// window by another 4h" without dropping/re-adding.
func (store *Store) AddDeviceToGroup(ctx context.Context, groupID, deviceID string, expiresAt *time.Time) (string, error) {
	var id string
	err := store.pool.QueryRow(ctx, `
		INSERT INTO "DeviceGroupMembership" (id, "groupId", "deviceId", "createdAt", expires_at)
		VALUES (gen_random_uuid(), $1, $2, NOW(), $3)
		ON CONFLICT ("groupId", "deviceId") DO UPDATE SET expires_at = EXCLUDED.expires_at
		RETURNING id
	`, groupID, deviceID, expiresAt).Scan(&id)
	return id, err
}

// RemoveDeviceFromGroup removes a device membership.
func (store *Store) RemoveDeviceFromGroup(ctx context.Context, groupID, deviceID string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "DeviceGroupMembership" WHERE "groupId" = $1 AND "deviceId" = $2`, groupID, deviceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// DeviceGroupMemberDevice is the embedded device view attached to each
// membership row. hostname / os come from the thing_agent 1:1 extension
// (populated at agent enrollment); non-agent Things would yield empty
// strings, but device groups are agent-only so the LEFT JOIN is a safety
// net rather than a design allowance.
type DeviceGroupMemberDevice struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Status   string `json:"status"`
}

// DeviceGroupMembershipDetail is a membership row with the joined device
// view the admin UI reads directly (m.device.hostname, m.device.os, ...).
type DeviceGroupMembershipDetail struct {
	ID        string    `json:"id"`
	GroupID   string    `json:"groupId"`
	DeviceID  string    `json:"deviceId"`
	CreatedAt time.Time `json:"createdAt"`
	// Auto-expiry. NULL = permanent. Surfaced on the admin API so the
	// UI can render a Badge + countdown.
	ExpiresAt *time.Time              `json:"expiresAt,omitempty"`
	Device    DeviceGroupMemberDevice `json:"device"`
}

// GroupsOfDevice returns the IDs of every DeviceGroup the given device
// is a member of, drawn from static memberships AND the smart-group
// cache. Used by the request-time IAM middleware
// (RequireIAMPermissionForDevice) to expand candidate NRNs for scope
// enforcement. Returns an empty slice (not nil) when the device is in
// no groups — that's a valid state, not an error.
//
// The UNION DISTINCT collapses any (group_id, device_id) pair that
// appears in both tables, which shouldn't happen by contract (a
// group is either smart OR static) but guards against transient
// inconsistency during a mode switch.
func (store *Store) GroupsOfDevice(ctx context.Context, deviceID string) ([]string, error) {
	// Filter expired memberships (expires_at <= NOW()). NULL expires_at
	// is the permanent case and continues to match. Smart-cache rows
	// never carry expiry — they're recomputed from a live predicate each
	// tick.
	rows, err := store.pool.Query(ctx, `
		SELECT "groupId" FROM "DeviceGroupMembership"
		WHERE "deviceId" = $1 AND (expires_at IS NULL OR expires_at > NOW())
		UNION
		SELECT group_id FROM device_group_membership_cache WHERE device_id = $1
	`, deviceID)
	if err != nil {
		return nil, fmt.Errorf("groups of device %s: %w", deviceID, err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, fmt.Errorf("scan group id: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ListDeviceGroupMemberships returns memberships with the joined device
// view needed by the admin Device Group detail page. Ordered by join time
// so the UI reflects the add-order.
func (store *Store) ListDeviceGroupMemberships(ctx context.Context, groupID string) ([]DeviceGroupMembershipDetail, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT m.id, m."groupId", m."deviceId", m."createdAt", m.expires_at,
		       t.id, t.status,
		       COALESCE(t.hostname, ''), COALESCE(t.os, '')
		FROM "DeviceGroupMembership" m
		JOIN thing t ON t.id = m."deviceId"
		WHERE m."groupId" = $1
		ORDER BY m."createdAt" ASC
	`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []DeviceGroupMembershipDetail{}
	for rows.Next() {
		var m DeviceGroupMembershipDetail
		if err := rows.Scan(
			&m.ID, &m.GroupID, &m.DeviceID, &m.CreatedAt, &m.ExpiresAt,
			&m.Device.ID, &m.Device.Status,
			&m.Device.Hostname, &m.Device.OS,
		); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
