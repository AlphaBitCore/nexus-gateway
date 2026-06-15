package store

import (
	"context"
	"fmt"
	"strings"
)

// ilikeEscaper escapes the three characters that Postgres ILIKE treats as
// metacharacters (%, _, and the default escape char \) so a user-supplied
// substring is matched literally rather than as a wildcard pattern.
var ilikeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// ListThingsParams holds filters for listing Things.
type ListThingsParams struct {
	Type   string
	Status string
	// Search is a case-insensitive substring filter applied to id, name, and
	// address. Empty string disables the filter.
	Search string
	// HasOverrides filters by whether the Thing has at least one active row
	// in thing_config_override. nil = no filter, true = only Things with
	// overrides, false = only Things without overrides. Drives the admin
	// "Has overrides" toggle on InfraNodesPage.
	HasOverrides *bool
	Page         int
	PageSize     int
}

// ListThingsResult is a paginated list of Things plus their override
// aggregates. The list path is the only Thing-fetcher that JOINs the
// override counts; the per-row shape carries the aggregates so callers
// can render the Overrides column and HasOverrides filter without a
// second round-trip per Thing.
type ListThingsResult struct {
	Things []ThingWithOverrideAgg `json:"things"`
	Total  int                    `json:"total"`
}

// normalizeListThingsParams applies defaults for pagination.
func normalizeListThingsParams(p *ListThingsParams) {
	if p.Page < 1 {
		p.Page = 1
	}
	if p.PageSize < 1 || p.PageSize > 200 {
		p.PageSize = 50
	}
}

// ListThings returns a filtered, paginated list of Things.
//
// Each row carries an aggregate from thing_config_override:
//
//	overrideCount      — total active override rows for this Thing.
//	overrideStaleCount — subset where the Thing's template has bumped past
//	                     template_ver_at_set (admin should review).
//
// The aggregate JOIN is computed from thing_config_override ⨝
// thing_config_template on (thing.type, override.config_key) so a stale
// template version on the matching template row marks the override stale.
//
// p.HasOverrides, when set, filters by override_count > 0 (true) or
// override_count = 0 (false). Both the count query and the list query use
// the same `thing_with_overrides` CTE so paging and total-count agree.
func (s *Store) ListThings(ctx context.Context, p ListThingsParams) (*ListThingsResult, error) {
	normalizeListThingsParams(&p)
	offset := (p.Page - 1) * p.PageSize

	// The CTE materializes the override aggregate once and is reused by both
	// the COUNT and the LIMIT/OFFSET-paged list query, so total and page rows
	// always agree on the HasOverrides filter outcome.
	// LEFT JOIN against thing_config_template so an orphan override (whose
	// matching template row has been deleted out from under it) still
	// contributes to overrideCount instead of vanishing from the aggregate.
	// Stale counting uses COALESCE(tct.version, 0) > tco.template_ver_at_set —
	// an orphan template (NULL version) is therefore not stale (you cannot
	// move "past" a row that no longer exists; the admin needs to clear the
	// orphan, not be told it's "stale"). Same COALESCE pattern is used by
	// thing_config_override.go (per-Thing list and global list queries).
	cte := `
WITH override_agg AS (
	SELECT tco.thing_id,
	       COUNT(*)::bigint AS cnt,
	       SUM(CASE WHEN COALESCE(tct.version, 0) > tco.template_ver_at_set THEN 1 ELSE 0 END)::bigint AS stale_cnt,
	       BOOL_OR(tco.config_key = 'killswitch' AND tco.emergency_override) AS has_killswitch_bypass
	  FROM thing_config_override tco
	  JOIN thing tt ON tt.id = tco.thing_id
	  LEFT JOIN thing_config_template tct ON tct.type = tt.type AND tct.config_key = tco.config_key
	 GROUP BY tco.thing_id
),
thing_with_overrides AS (
	SELECT t.id, t.type, t.name, t.version, t.address,
	       t.enrolled_by, t.auth_type, t.conn_protocol,
	       t.status, t.desired, t.reported, t.desired_ver, t.reported_ver,
	       t.metadata, t.last_seen_at, t.enrolled_at,
	       t.reported_outcomes, t.process_started_at,
	       t.hostname, t.primary_ip, t.os, t.os_version, t.physical_id,
	       COALESCE(oa.cnt, 0)                       AS override_count,
	       COALESCE(oa.stale_cnt, 0)                 AS override_stale_count,
	       COALESCE(oa.has_killswitch_bypass, false) AS has_killswitch_bypass,
	       u.id                                       AS bound_user_id,
	       u."displayName"                            AS bound_user_display_name,
	       u.email                                    AS bound_user_email
	  FROM thing t
	  LEFT JOIN override_agg oa ON oa.thing_id = t.id
	  LEFT JOIN "DeviceAssignment" da ON da."deviceId" = t.id AND da."releasedAt" IS NULL
	  LEFT JOIN "NexusUser"        u  ON u.id = da."userId"
)`

	where := "WHERE 1=1"
	args := []any{}
	argIdx := 1

	if p.Type != "" {
		where += fmt.Sprintf(" AND type = $%d", argIdx)
		args = append(args, p.Type)
		argIdx++
	}
	if p.Status != "" {
		where += fmt.Sprintf(" AND status = $%d", argIdx)
		args = append(args, p.Status)
		argIdx++
	}
	if p.Search != "" {
		where += fmt.Sprintf(" AND (id ILIKE $%d OR COALESCE(name,'') ILIKE $%d OR COALESCE(address,'') ILIKE $%d)", argIdx, argIdx, argIdx)
		args = append(args, "%"+ilikeEscaper.Replace(p.Search)+"%")
		argIdx++
	}
	if p.HasOverrides != nil {
		if *p.HasOverrides {
			where += " AND override_count > 0"
		} else {
			where += " AND override_count = 0"
		}
	}

	countQuery := cte + " SELECT COUNT(*) FROM thing_with_overrides " + where
	var total int
	if err := s.db.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("count things: %w", err)
	}

	listQuery := fmt.Sprintf(`%s
		SELECT id, type, COALESCE(name,''), COALESCE(version,''), COALESCE(address,''),
		       COALESCE(enrolled_by,''), auth_type, conn_protocol,
		       status, desired, reported, desired_ver, reported_ver, metadata, last_seen_at, enrolled_at,
		       reported_outcomes, process_started_at,
		       COALESCE(hostname,''), COALESCE(primary_ip,''),
		       COALESCE(os,''), COALESCE(os_version,''), COALESCE(physical_id,''),
		       COALESCE(bound_user_id,''), COALESCE(bound_user_display_name,''), COALESCE(bound_user_email,''),
		       override_count, override_stale_count, has_killswitch_bypass
		FROM thing_with_overrides %s
		ORDER BY last_seen_at DESC NULLS LAST
		LIMIT $%d OFFSET $%d
	`, cte, where, argIdx, argIdx+1)
	args = append(args, p.PageSize, offset)

	rows, err := s.db.Query(ctx, listQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("list things: %w", err)
	}
	defer rows.Close()

	var things []ThingWithOverrideAgg
	for rows.Next() {
		var t ThingWithOverrideAgg
		var desiredRaw, reportedRaw, metaRaw, outcomesRaw []byte
		if err := rows.Scan(
			&t.ID, &t.Type, &t.Name, &t.Version, &t.Address,
			&t.EnrolledBy, &t.AuthType, &t.ConnProtocol,
			&t.Status, &desiredRaw, &reportedRaw, &t.DesiredVer, &t.ReportedVer,
			&metaRaw, &t.LastSeenAt, &t.EnrolledAt,
			&outcomesRaw, &t.ProcessStartedAt,
			&t.Hostname, &t.PrimaryIP, &t.OS, &t.OSVersion, &t.PhysicalID,
			&t.BoundUserID, &t.BoundUserDisplayName, &t.BoundUserEmail,
			&t.OverrideCount, &t.OverrideStaleCount, &t.HasKillswitchBypass,
		); err != nil {
			return nil, fmt.Errorf("scan thing: %w", err)
		}
		if err := decodeJSONB(desiredRaw, &t.Desired, "desired"); err != nil {
			return nil, err
		}
		if err := decodeJSONB(reportedRaw, &t.Reported, "reported"); err != nil {
			return nil, err
		}
		if err := decodeJSONB(metaRaw, &t.Metadata, "metadata"); err != nil {
			return nil, err
		}
		if err := decodeJSONB(outcomesRaw, &t.ReportedOutcomes, "reported_outcomes"); err != nil {
			return nil, err
		}
		things = append(things, t)
	}
	if things == nil {
		things = []ThingWithOverrideAgg{}
	}
	return &ListThingsResult{Things: things, Total: total}, nil
}
