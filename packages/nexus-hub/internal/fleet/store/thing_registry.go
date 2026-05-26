package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ilikeEscaper escapes the three characters that Postgres ILIKE treats as
// metacharacters (%, _, and the default escape char \) so a user-supplied
// substring is matched literally rather than as a wildcard pattern.
var ilikeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// decodeJSONB decodes a JSONB column into target (which must be a
// pointer). NULL or empty columns are a no-op (legitimate Postgres
// state); a non-empty payload that fails to parse surfaces a wrapped
// error. The Thing registry is the source of truth driving config push
// to all four services, so a corrupt row must fail loudly instead of
// silently producing empty config.
func decodeJSONB(raw []byte, target any, column string) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode %s: %w", column, err)
	}
	return nil
}

// UpsertThingParams holds parameters for upserting a thing row.
type UpsertThingParams struct {
	ID           string
	Type         string
	Name         string
	Version      string
	Address      string
	EnrolledBy   string
	AuthType     string
	ConnProtocol string
	Status       string
	Metadata     map[string]any
	Desired      map[string]any
	// PhysicalID is the stable natural-key for this Thing. For agents
	// it's the hardware fingerprint (sha256 of IOPlatformUUID + serial
	// + MAC + cpu). For server services it's the yaml-configured id or
	// hostname+type+port (often == thing.id today). Empty string =>
	// not stored (kept NULL on insert / preserved on touch). The
	// partial UNIQUE index `thing_type_physical_id_uniq` enforces
	// one row per (type='agent', physical_id) at the DB level.
	PhysicalID string
}

// Thing is a row from the thing table — the canonical Thing identity +
// shadow snapshot, no override aggregates. Use ThingWithOverrideAgg
// when the override counts are needed (the list path); GetThing returns
// the bare Thing.
//
// ReportedOutcomes is the per-config-key outcome ledger carried over from
// the most recent shadow_report. AppliedAt / AppliedVersion describe the
// last KNOWN successful apply for that key; ApplyError, when present,
// describes the most recent failed attempt. Together with
// ProcessStartedAt — wall-clock the Thing's current process came online —
// they give operators a "is this node actually serving the config I see
// in Desired?" signal without crawling logs.
type Thing struct {
	ID               string                        `json:"id"`
	Type             string                        `json:"type"`
	Name             string                        `json:"name"`
	Version          string                        `json:"version"`
	Address          string                        `json:"address"`
	EnrolledBy       string                        `json:"enrolledBy"`
	AuthType         string                        `json:"authType"`
	ConnProtocol     string                        `json:"connProtocol"`
	Status           string                        `json:"status"`
	Desired          map[string]any                `json:"desired"`
	Reported         map[string]any                `json:"reported"`
	ReportedOutcomes map[string]ReportedKeyOutcome `json:"reportedOutcomes"`
	DesiredVer       int64                         `json:"desiredVer"`
	ReportedVer      int64                         `json:"reportedVer"`
	Metadata         map[string]any                `json:"metadata"`
	LastSeenAt       *time.Time                    `json:"lastSeenAt"`
	EnrolledAt       time.Time                     `json:"enrolledAt"`
	ProcessStartedAt *time.Time                    `json:"processStartedAt"`
	// Identity fields promoted out of metadata.staticInfo (migration
	// 20260522_thing_identity_columns). hostname / primary_ip / os /
	// os_version are populated by the heartbeat handler from staticInfo.
	// physical_id is set at enrollment time + on every heartbeat.
	Hostname   string `json:"hostname,omitempty"`
	PrimaryIP  string `json:"primaryIp,omitempty"`
	OS         string `json:"os,omitempty"`
	OSVersion  string `json:"osVersion,omitempty"`
	PhysicalID string `json:"physicalId,omitempty"`
	// Service-extension fields (left-joined from thing_service for server-side
	// Things; empty string for agents). MetricsURL is the /metrics endpoint
	// this Thing advertises via thingclient on register; surfaced here so the
	// Node Detail page can render a clickable link without a second API hop.
	MetricsURL string `json:"metricsUrl,omitempty"`
	// Bound user — currently-active DeviceAssignment for agent things.
	// LEFT JOINed at SELECT time so service Things naturally surface
	// empty strings. Phase 1 wire-up; surfaced into admin list/detail.
	BoundUserID          string `json:"boundUserId,omitempty"`
	BoundUserDisplayName string `json:"boundUserDisplayName,omitempty"`
	BoundUserEmail       string `json:"boundUserEmail,omitempty"`
}

// ReportedKeyOutcome mirrors thingclient.ApplyOutcome on the Hub side so
// it can be JOINed/serialised together with the rest of the Thing row.
// Fields are pointers to preserve the "unknown" / "fresh process, no apply
// yet" semantic across JSON round-trips.
type ReportedKeyOutcome struct {
	AppliedAt      *time.Time   `json:"appliedAt,omitempty"`
	AppliedVersion *int64       `json:"appliedVersion,omitempty"`
	ApplyError     *ApplyErrorV `json:"applyError,omitempty"`
}

// ApplyErrorV is the wire-compatible Hub-side mirror of
// thingclient.ApplyError. The trailing V is to avoid clashing with the
// (very common) ApplyError name used elsewhere in the codebase.
type ApplyErrorV struct {
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// ThingWithOverrideAgg is the row shape returned by ListThings — the bare
// Thing plus the override counts JOINed in. Only ListThings populates these,
// and now the type system enforces "you cannot read OverrideCount unless you
// asked for the aggregate". Hub /api/hub/things → CP /api/admin/nodes
// continues to emit the same JSON because ThingWithOverrideAgg embeds Thing
// (so all of Thing's fields show up in the wire shape).
//
// HasKillswitchBypass is true when the Thing has at least one active override
// row whose config_key='killswitch' AND emergency_override=true. This is the
// minimum signal the list page needs to render a red bypass marker on the
// row without paying the cost of fetching every override per Thing
// (AC14: a node bypassing the killswitch must be visually distinct from a
// node with non-emergency overrides).
type ThingWithOverrideAgg struct {
	Thing
	OverrideCount       int64 `json:"overrideCount"`
	OverrideStaleCount  int64 `json:"overrideStaleCount"`
	HasKillswitchBypass bool  `json:"hasKillswitchBypass"`
}

// UpsertThingEnrollment inserts or replaces a Thing during enrollment. It is
// the only place auth_type, conn_protocol, and enrolled_by are written after
// the initial row is created — subsequent reconnects must use TouchThingSession.
// Metadata is merged (jsonb ||) so existing keys are preserved unless overwritten.
func (s *Store) UpsertThingEnrollment(ctx context.Context, p UpsertThingParams) error {
	metadataJSON, err := json.Marshal(p.Metadata)
	if err != nil {
		metadataJSON = []byte("{}")
	}
	if p.Metadata == nil {
		metadataJSON = []byte("{}")
	}
	desiredJSON := []byte("{}")
	if p.Desired != nil {
		desiredJSON, err = json.Marshal(p.Desired)
		if err != nil {
			return fmt.Errorf("marshal desired: %w", err)
		}
	}

	authType := p.AuthType
	if authType == "" {
		authType = "bearer"
	}
	connProto := p.ConnProtocol
	if connProto == "" {
		connProto = "http"
	}

	_, err = s.db.Exec(ctx, `
		INSERT INTO thing (id, type, name, version, address, enrolled_by, auth_type, conn_protocol,
		                   status, metadata, desired, reported, desired_ver, reported_ver, last_seen_at, enrolled_at, updated_at,
		                   process_started_at, reported_outcomes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, '{}', 0, 0, NOW(), NOW(), NOW(),
		        CASE WHEN $9 = 'online' THEN NOW() ELSE NULL END, '{}'::jsonb)
		ON CONFLICT (id) DO UPDATE SET
			name          = COALESCE(EXCLUDED.name, thing.name),
			version       = EXCLUDED.version,
			address       = EXCLUDED.address,
			enrolled_by   = COALESCE(EXCLUDED.enrolled_by, thing.enrolled_by),
			auth_type     = EXCLUDED.auth_type,
			conn_protocol = EXCLUDED.conn_protocol,
			status        = EXCLUDED.status,
			metadata      = thing.metadata || EXCLUDED.metadata,
			-- Only overwrite desired when the caller actually supplied one.
			-- selfreg and other reconnect paths pass Desired=nil → marshal
			-- yields '{}', which would otherwise wipe live shadow state
			-- (e.g. observability pushed via Hub admin UI). Keeping the
			-- existing value on reconnect lets selfshadow re-apply on the
			-- next NOTIFY without an extra recovery pass.
			desired       = CASE
				WHEN EXCLUDED.desired = '{}'::jsonb THEN thing.desired
				ELSE EXCLUDED.desired
			END,
			last_seen_at  = NOW(),
			updated_at    = NOW(),
			-- Stamp a fresh process anchor + clear outcome ledger when the
			-- enrollment call is what flipped the row to online. Preserve
			-- existing values on conflicting upserts that stay non-online.
			process_started_at = CASE
				WHEN EXCLUDED.status = 'online' AND thing.status <> 'online' THEN NOW()
				ELSE thing.process_started_at
			END,
			reported_outcomes  = CASE
				WHEN EXCLUDED.status = 'online' AND thing.status <> 'online' THEN '{}'::jsonb
				ELSE thing.reported_outcomes
			END
	`, p.ID, p.Type, p.Name, p.Version, p.Address, p.EnrolledBy, authType, connProto, p.Status, metadataJSON, desiredJSON)
	if err != nil {
		return fmt.Errorf("upsert thing enrollment: %w", err)
	}
	return nil
}

// UpsertThingService inserts or updates the thing_service extension row
// for a service-type Thing (compliance-proxy, ai-gateway, control-plane,
// nexus-hub). Empty URL fields are allowed; role defaults to "default" if empty.
//
// Idempotent — safe to call from both first-time enrollment and the
// reconnect/touch path. The Hub register handler invokes this after the
// Thing row is upserted/touched so MetricsURL and ManagementURL flowing
// through the register payload (thingclient.Config) reach the DB.
func (s *Store) UpsertThingService(ctx context.Context, thingID, metricsURL, managementURL, role string) error {
	if role == "" {
		role = "default"
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO thing_service (thing_id, role, metrics_url, management_url)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''))
		ON CONFLICT (thing_id) DO UPDATE
			SET role           = EXCLUDED.role,
			    metrics_url    = COALESCE(EXCLUDED.metrics_url, thing_service.metrics_url),
			    management_url = COALESCE(EXCLUDED.management_url, thing_service.management_url)
	`, thingID, role, metricsURL, managementURL)
	return err
}

// GetThingManagementURL returns the management_url stored in thing_service for
// the given thingID. Returns ("", nil) if the thing exists but has no management URL.
// Returns ("", store.ErrNotFound) if the thing does not exist.
func (s *Store) GetThingManagementURL(ctx context.Context, thingID string) (string, error) {
	var managementURL *string
	err := s.db.QueryRow(ctx, `
		SELECT ts.management_url
		FROM thing t
		LEFT JOIN thing_service ts ON ts.thing_id = t.id
		WHERE t.id = $1
	`, thingID).Scan(&managementURL)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("get thing management url: %w", err)
	}
	if managementURL == nil {
		return "", nil
	}
	return *managementURL, nil
}

// UpsertThingEnrollmentWithDesiredVer inserts or replaces a Thing during first-time
// enrollment and sets a specific desired_ver atomically. Like UpsertThingEnrollment,
// it is the authoritative writer for auth_type, conn_protocol, and enrolled_by.
// Subsequent reconnects must use TouchThingSession, not this function.
// Metadata is merged (jsonb ||) so existing keys are preserved unless overwritten.
func (s *Store) UpsertThingEnrollmentWithDesiredVer(ctx context.Context, p UpsertThingParams, desiredVer int64) error {
	metadataJSON, err := json.Marshal(p.Metadata)
	if err != nil {
		metadataJSON = []byte("{}")
	}
	if p.Metadata == nil {
		metadataJSON = []byte("{}")
	}
	desiredJSON := []byte("{}")
	if p.Desired != nil {
		desiredJSON, err = json.Marshal(p.Desired)
		if err != nil {
			return fmt.Errorf("marshal desired: %w", err)
		}
	}

	authType := p.AuthType
	if authType == "" {
		authType = "bearer"
	}
	connProto := p.ConnProtocol
	if connProto == "" {
		connProto = "http"
	}

	// physicalID NULL → DB column stays NULL. Empty string is treated
	// the same as missing so callers don't have to choose between
	// `Param{...}` and `Param{PhysicalID: ""}`.
	var physicalID any
	if p.PhysicalID != "" {
		physicalID = p.PhysicalID
	}

	_, err = s.db.Exec(ctx, `
		INSERT INTO thing (id, type, name, version, address, enrolled_by, auth_type, conn_protocol,
		                   status, metadata, desired, reported, desired_ver, reported_ver, last_seen_at, enrolled_at, updated_at,
		                   process_started_at, reported_outcomes, physical_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, '{}', $12, 0, NOW(), NOW(), NOW(),
		        CASE WHEN $9 = 'online' THEN NOW() ELSE NULL END, '{}'::jsonb, $13)
		ON CONFLICT (id) DO UPDATE SET
			name          = COALESCE(EXCLUDED.name, thing.name),
			version       = EXCLUDED.version,
			address       = EXCLUDED.address,
			enrolled_by   = COALESCE(EXCLUDED.enrolled_by, thing.enrolled_by),
			auth_type     = EXCLUDED.auth_type,
			conn_protocol = EXCLUDED.conn_protocol,
			status        = EXCLUDED.status,
			metadata      = thing.metadata || EXCLUDED.metadata,
			-- Same preservation rule as UpsertThingEnrollment: empty
			-- '{}' on the incoming side means "caller didn't set
			-- Desired" — keep the live shadow state.
			desired       = CASE
				WHEN EXCLUDED.desired = '{}'::jsonb THEN thing.desired
				ELSE EXCLUDED.desired
			END,
			desired_ver   = EXCLUDED.desired_ver,
			last_seen_at  = NOW(),
			updated_at    = NOW(),
			-- physical_id is preserved if already set; an incoming NULL
			-- never overwrites a populated value. Keeps the dedupe key
			-- stable across reconnect/touch paths.
			physical_id   = COALESCE(EXCLUDED.physical_id, thing.physical_id),
			process_started_at = CASE
				WHEN EXCLUDED.status = 'online' AND thing.status <> 'online' THEN NOW()
				ELSE thing.process_started_at
			END,
			reported_outcomes  = CASE
				WHEN EXCLUDED.status = 'online' AND thing.status <> 'online' THEN '{}'::jsonb
				ELSE thing.reported_outcomes
			END
	`, p.ID, p.Type, p.Name, p.Version, p.Address, p.EnrolledBy, authType, connProto, p.Status, metadataJSON, desiredJSON, desiredVer, physicalID)
	if err != nil {
		return fmt.Errorf("upsert thing enrollment with ver: %w", err)
	}
	return nil
}

// UpdateLastSeen updates the last_seen_at timestamp for a Thing.
// SetPhysicalID writes the natural-key physical_id onto an existing
// thing row. Used by paths that create the thing first and learn the
// physical_id later (none today — UpsertThingEnrollmentWithDesiredVer
// already writes it inline — but kept for the explicit
// "re-stamp on heartbeat if it shifted" case the platform might want
// to add later).
func (s *Store) SetPhysicalID(ctx context.Context, thingID, physicalID string) error {
	if thingID == "" || physicalID == "" {
		return nil
	}
	_, err := s.db.Exec(ctx, `
		UPDATE thing
		SET physical_id = $2,
		    updated_at  = NOW()
		WHERE id = $1
	`, thingID, physicalID)
	if err != nil {
		return fmt.Errorf("set physical_id: %w", err)
	}
	return nil
}

// FindAgentByPhysicalID looks up an agent thing by its physical_id
// (the hardware fingerprint for agents). Returns "" + nil when no
// match — caller mints a new thing_id. Replaces the previous
// jsonb-buried lookup at `metadata.staticInfo.deviceFingerprint`.
func (s *Store) FindAgentByPhysicalID(ctx context.Context, physicalID string) (string, error) {
	if physicalID == "" {
		return "", nil
	}
	const q = `
		SELECT id FROM thing
		 WHERE type = 'agent'
		   AND physical_id = $1
		 ORDER BY last_seen_at DESC NULLS LAST, enrolled_at DESC
		 LIMIT 1
	`
	var id string
	row := s.db.QueryRow(ctx, q, physicalID)
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return id, nil
}

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

// UpdateThingStatus updates the status field for a Thing.
func (s *Store) UpdateThingStatus(ctx context.Context, id, status string) error {
	tag, err := s.db.Exec(ctx,
		`UPDATE thing SET status = $2, last_seen_at = NOW(), updated_at = NOW() WHERE id = $1`, id, status)
	if err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetThing retrieves a single Thing by ID.
func (s *Store) GetThing(ctx context.Context, id string) (*Thing, error) {
	t := &Thing{}
	var desiredRaw, reportedRaw, metadataRaw, outcomesRaw []byte
	err := s.db.QueryRow(ctx, `
		SELECT t.id, t.type, COALESCE(t.name,''), COALESCE(t.version,''), COALESCE(t.address,''),
		       COALESCE(t.enrolled_by,''), t.auth_type, t.conn_protocol,
		       t.status, t.desired, t.reported, t.desired_ver, t.reported_ver, t.metadata, t.last_seen_at, t.enrolled_at,
		       t.reported_outcomes, t.process_started_at,
		       COALESCE(t.hostname, ''), COALESCE(t.primary_ip, ''),
		       COALESCE(t.os, ''), COALESCE(t.os_version, ''),
		       COALESCE(t.physical_id, ''),
		       COALESCE(u.id, ''), COALESCE(u."displayName", ''), COALESCE(u.email, ''),
		       COALESCE(ts.metrics_url, '')
		FROM thing t
		LEFT JOIN "DeviceAssignment" da ON da."deviceId" = t.id AND da."releasedAt" IS NULL
		LEFT JOIN "NexusUser"        u  ON u.id = da."userId"
		LEFT JOIN thing_service      ts ON ts.thing_id = t.id
		WHERE t.id = $1
	`, id).Scan(
		&t.ID, &t.Type, &t.Name, &t.Version, &t.Address,
		&t.EnrolledBy, &t.AuthType, &t.ConnProtocol,
		&t.Status, &desiredRaw, &reportedRaw, &t.DesiredVer, &t.ReportedVer,
		&metadataRaw, &t.LastSeenAt, &t.EnrolledAt,
		&outcomesRaw, &t.ProcessStartedAt,
		&t.Hostname, &t.PrimaryIP, &t.OS, &t.OSVersion, &t.PhysicalID,
		&t.BoundUserID, &t.BoundUserDisplayName, &t.BoundUserEmail,
		&t.MetricsURL,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get thing: %w", err)
	}
	if err := decodeJSONB(desiredRaw, &t.Desired, "desired"); err != nil {
		return nil, err
	}
	if err := decodeJSONB(reportedRaw, &t.Reported, "reported"); err != nil {
		return nil, err
	}
	if err := decodeJSONB(metadataRaw, &t.Metadata, "metadata"); err != nil {
		return nil, err
	}
	if err := decodeJSONB(outcomesRaw, &t.ReportedOutcomes, "reported_outcomes"); err != nil {
		return nil, err
	}
	return t, nil
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

	var desiredVer int64
	var desiredRaw []byte
	err := s.db.QueryRow(ctx, `
		UPDATE thing
		SET status      = $2,
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

// UpdateShadowReport updates reported state, reported_ver and the per-key
// reported_outcomes ledger. Outcomes nil/empty is allowed — older callers
// (or shadow_reports from a process whose OutcomeTracker hasn't recorded
// anything yet) result in an empty ledger, not a wipe of prior state.
// Hub-internal correlation with process_started_at distinguishes "fresh
// process, no apply yet" from "stale ledger".
func (s *Store) UpdateShadowReport(ctx context.Context, id string, reported map[string]any, reportedVer int64, outcomes map[string]ReportedKeyOutcome) error {
	reportedJSON, err := json.Marshal(reported)
	if err != nil {
		return fmt.Errorf("marshal reported: %w", err)
	}
	outcomesJSON, err := json.Marshal(outcomes)
	if err != nil {
		return fmt.Errorf("marshal reported_outcomes: %w", err)
	}
	if len(outcomesJSON) == 0 || string(outcomesJSON) == "null" {
		outcomesJSON = []byte("{}")
	}

	tag, err := s.db.Exec(ctx, `
		UPDATE thing
		SET reported = $2, reported_ver = $3, reported_outcomes = $4::jsonb,
		    last_seen_at = NOW(), updated_at = NOW(),
		    status = CASE WHEN status = 'drift' AND $3 >= desired_ver THEN 'online' ELSE status END
		WHERE id = $1
	`, id, reportedJSON, reportedVer, outcomesJSON)
	if err != nil {
		return fmt.Errorf("update shadow: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

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

// DriftedThing represents a Thing with version mismatch or drift status.
type DriftedThing struct {
	ID          string     `json:"id"`
	Type        string     `json:"type"`
	Status      string     `json:"status"`
	DesiredVer  int64      `json:"desiredVer"`
	ReportedVer int64      `json:"reportedVer"`
	LastSeenAt  *time.Time `json:"lastSeenAt"`
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
func (s *Store) ListDriftedThings(ctx context.Context) ([]DriftedThing, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, type, status, desired_ver, reported_ver, last_seen_at
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
		if err := rows.Scan(&dt.ID, &dt.Type, &dt.Status, &dt.DesiredVer, &dt.ReportedVer, &dt.LastSeenAt); err != nil {
			return nil, fmt.Errorf("scan drifted: %w", err)
		}
		result = append(result, dt)
	}
	return result, nil
}

// UpdateDesiredForType updates desired JSON for all Things of a type (in a transaction)
// and bumps each row's desired_ver to a single new value shared across the type:
//
//	next = COALESCE(MAX(desired_ver) WHERE type=$thingType), 0) + 1
//
// Rationale: thing_config_template.version is per (type, config_key) and is not
// comparable across keys or to the Thing client's global reported_ver. The
// WebSocket config_changed fan-out is one payload per type, so every Thing of
// that type must see the same monotonic desired_ver that exceeds any prior
// reported_ver after the Thing has caught up.
//
// templateVer is the template row version from UpsertConfigTemplate; it is kept
// for call-site symmetry but is not written to thing.desired_ver.
func (s *Store) UpdateDesiredForType(ctx context.Context, tx pgx.Tx, thingType, configKey string, state any, templateVer int64) (rowsAffected int64, shadowDesiredVer int64, err error) {
	_ = templateVer
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return 0, 0, fmt.Errorf("marshal state: %w", err)
	}

	rows, err := tx.Query(ctx, `
WITH next AS (
	SELECT COALESCE(MAX(desired_ver), 0) + 1 AS v
	FROM thing
	WHERE type = $1
)
UPDATE thing AS t
SET desired = jsonb_set(COALESCE(t.desired, '{}'::jsonb), ARRAY[$2::text], $3::jsonb),
    desired_ver = next.v,
    updated_at = NOW()
FROM next
WHERE t.type = $1
RETURNING t.id, t.desired_ver
`, thingType, configKey, stateJSON)
	if err != nil {
		return 0, 0, fmt.Errorf("update desired for type: %w", err)
	}
	defer rows.Close()

	// Collect affected thing IDs so we can emit one NOTIFY per row in
	// the same tx. pg_notify is committed atomically with the UPDATE,
	// so a rollback discards both. Hub selfshadow listeners on each
	// affected Thing's instance filter by id and re-read thing.desired.
	var n int64
	notifyIDs := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id, &shadowDesiredVer); err != nil {
			return 0, 0, fmt.Errorf("scan desired_ver: %w", err)
		}
		notifyIDs = append(notifyIDs, id)
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate update results: %w", err)
	}
	// Close the rows cursor explicitly before issuing further queries
	// on the same tx. pgx serializes a single connection; leaving the
	// rows iterator open while Execing pg_notify would deadlock.
	rows.Close()
	for _, id := range notifyIDs {
		if err := notifyConfigChanged(ctx, tx, id); err != nil {
			return 0, 0, err
		}
	}
	return n, shadowDesiredVer, nil
}

// WriteDesiredAndBumpVer atomically replaces thing.desired with the supplied
// merged map and increments thing.desired_ver by one, returning the new
// desired_ver. Used by the override write/clear path (Hub Manager
// SetOverride / ClearOverride and the override-expiry job): the caller has
// already recomputed the merged shadow state (templates ⊕ overrides) and
// passes it in here so the SQL stays in one tx with the override row mutation.
//
// Unlike UpdateDesiredForType this is a single-Thing, not type-fanout, write —
// per-Thing overrides only affect one row. The caller must run this inside a
// pgx.Tx so the override CRUD and the merge cache write commit together.
func (s *Store) WriteDesiredAndBumpVer(ctx context.Context, tx pgx.Tx, thingID string, merged map[string]any) (int64, error) {
	mergedJSON, err := json.Marshal(merged)
	if err != nil {
		return 0, fmt.Errorf("marshal merged desired: %w", err)
	}
	if merged == nil {
		// json.Marshal(nil) returns "null"; we must store an empty JSON object so
		// downstream readers (thingclient pull, applied-config diff,
		// json.Unmarshal into map[string]any) get an empty map instead of a nil
		// map and surface a sane "no keys configured" state.
		mergedJSON = []byte("{}")
	}

	var newVer int64
	err = tx.QueryRow(ctx, `
		UPDATE thing
		   SET desired     = $2::jsonb,
		       desired_ver = desired_ver + 1,
		       updated_at  = NOW()
		 WHERE id = $1
		RETURNING desired_ver
	`, thingID, mergedJSON).Scan(&newVer)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("write desired and bump ver for %s: %w", thingID, err)
	}
	// Emit selfshadow notification inside the same tx so commit/rollback
	// stays atomic with the UPDATE. Hub instances LISTENing on
	// config_changed pick this up and apply the override delta.
	if err := notifyConfigChanged(ctx, tx, thingID); err != nil {
		return 0, err
	}
	return newVer, nil
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

// UpsertThingAgentParams holds parameters for inserting/updating a thing_agent row.
type UpsertThingAgentParams struct {
	ThingID       string
	Hostname      string
	OS            string
	OSVersion     string
	CertSerial    string
	CertExpiresAt *time.Time
}

// UpdateThingAgent inserts or updates the thing_agent extension row.
// As of Phase 6 of the thing-identity refactor, hostname/os/os_version
// live ONLY on thing.* (written by Heartbeat from staticInfo); this
// helper only owns the agent-cert columns now. The params struct still
// carries Hostname/OS/OSVersion for backward-compat with callers, but
// the helper writes them into thing.* directly so the move is atomic
// with respect to a single enrollment call site.
func (s *Store) UpdateThingAgent(ctx context.Context, p UpsertThingAgentParams) error {
	// Mirror Hostname/OS/OSVersion onto thing.* so identity is consistent
	// from the moment enrollment finishes (Heartbeat would normally do
	// this on the FIRST static_info push, but the device may not push
	// for several seconds; CP queries reading thing.hostname would see
	// NULL until then). Best-effort — failures don't abort enrollment.
	var hostname, osVal, osVersion any
	if p.Hostname != "" {
		hostname = p.Hostname
	}
	if p.OS != "" {
		osVal = p.OS
	}
	if p.OSVersion != "" {
		osVersion = p.OSVersion
	}
	if hostname != nil || osVal != nil || osVersion != nil {
		if _, err := s.db.Exec(ctx, `
			UPDATE thing
			SET hostname   = COALESCE($2, hostname),
			    os         = COALESCE($3, os),
			    os_version = COALESCE($4, os_version),
			    updated_at = NOW()
			WHERE id = $1
		`, p.ThingID, hostname, osVal, osVersion); err != nil {
			return fmt.Errorf("mirror thing identity: %w", err)
		}
	}

	_, err := s.db.Exec(ctx, `
		INSERT INTO thing_agent (thing_id, cert_serial, cert_expires_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (thing_id) DO UPDATE SET
			cert_serial     = EXCLUDED.cert_serial,
			cert_expires_at = EXCLUDED.cert_expires_at
	`, p.ThingID, p.CertSerial, p.CertExpiresAt)
	if err != nil {
		return fmt.Errorf("upsert thing_agent: %w", err)
	}
	return nil
}

// StoreAttestationPubKey persists the Ed25519 attestation public key
// + cert metadata into thing_agent.sysinfo for the given agent. Used
// by the enrollment handler right after agentca.SignAttestationCSR
// returns the signed cert; the compliance-proxy fetches the public key
// later via /api/internal/things/:id/attestation-pubkey to verify the
// X-Nexus-Attestation header on outbound traffic.
//
// The data lives inside the existing sysinfo JSONB column (jsonb_set merge).
// Schema:
//
//	sysinfo.attestation = {
//	    "publicKey":      "<base64 stdlib NoPadding>",
//	    "certSerial":     "<hex>",
//	    "certExpiresAt":  "<RFC3339>",
//	}
//
// Existing keys under sysinfo are preserved (jsonb_set merge).
func (s *Store) StoreAttestationPubKey(ctx context.Context, thingID string, publicKey []byte, certSerial string, certExpiresAt time.Time) error {
	payload := map[string]any{
		"publicKey":     base64.StdEncoding.EncodeToString(publicKey),
		"certSerial":    certSerial,
		"certExpiresAt": certExpiresAt.UTC().Format(time.RFC3339),
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal attestation payload: %w", err)
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE thing_agent
		SET sysinfo = jsonb_set(COALESCE(sysinfo, '{}'::jsonb), '{attestation}', $2::jsonb, true)
		WHERE thing_id = $1
	`, thingID, string(payloadJSON))
	if err != nil {
		return fmt.Errorf("store attestation pubkey: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetAttestationPubKey returns the Ed25519 attestation public key (raw
// 32 bytes) the compliance-proxy uses to verify the X-Nexus-Attestation
// header for the given agent. Returns ErrNotFound when the agent has
// not enrolled with attestation (older agent build, or enrollment partial
// when the Ed25519 CSR signing failed and the surrounding mTLS
// enrollment still succeeded). Empty publicKey is treated as ErrNotFound
// so callers can map "no attestation key on record" to a clean cache
// miss → MITM fallback at CP.
func (s *Store) GetAttestationPubKey(ctx context.Context, thingID string) ([]byte, error) {
	var encoded string
	err := s.db.QueryRow(ctx, `
		SELECT COALESCE(sysinfo->'attestation'->>'publicKey', '')
		FROM thing_agent
		WHERE thing_id = $1
	`, thingID).Scan(&encoded)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("load attestation pubkey: %w", err)
	}
	if encoded == "" {
		return nil, ErrNotFound
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode attestation pubkey: %w", err)
	}
	return raw, nil
}

// StoreDeviceTokenHash stores the SHA-256 hash of a device token in thing.metadata.
func (s *Store) StoreDeviceTokenHash(ctx context.Context, thingID, tokenHash string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE thing
		SET metadata = jsonb_set(COALESCE(metadata, '{}'), '{deviceTokenHash}', to_jsonb($2::text)),
		    updated_at = NOW()
		WHERE id = $1
	`, thingID, tokenHash)
	if err != nil {
		return fmt.Errorf("store device token hash: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ValidateDeviceToken checks a hashed device token against the stored hash.
// Returns the Thing if valid and not revoked.
func (s *Store) ValidateDeviceToken(ctx context.Context, thingID, tokenHash string) (*Thing, error) {
	t := &Thing{}
	var desiredRaw, reportedRaw, metadataRaw, outcomesRaw []byte
	err := s.db.QueryRow(ctx, `
		SELECT id, type, COALESCE(name,''), COALESCE(version,''), COALESCE(address,''),
		       COALESCE(enrolled_by,''), auth_type, conn_protocol,
		       status, desired, reported, desired_ver, reported_ver, metadata, last_seen_at, enrolled_at,
		       reported_outcomes, process_started_at
		FROM thing
		WHERE id = $1
		  AND metadata->>'deviceTokenHash' = $2
		  AND status != 'revoked'
	`, thingID, tokenHash).Scan(
		&t.ID, &t.Type, &t.Name, &t.Version, &t.Address,
		&t.EnrolledBy, &t.AuthType, &t.ConnProtocol,
		&t.Status, &desiredRaw, &reportedRaw, &t.DesiredVer, &t.ReportedVer,
		&metadataRaw, &t.LastSeenAt, &t.EnrolledAt,
		&outcomesRaw, &t.ProcessStartedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("validate device token: %w", err)
	}
	if err := decodeJSONB(desiredRaw, &t.Desired, "desired"); err != nil {
		return nil, err
	}
	if err := decodeJSONB(reportedRaw, &t.Reported, "reported"); err != nil {
		return nil, err
	}
	if err := decodeJSONB(metadataRaw, &t.Metadata, "metadata"); err != nil {
		return nil, err
	}
	if err := decodeJSONB(outcomesRaw, &t.ReportedOutcomes, "reported_outcomes"); err != nil {
		return nil, err
	}
	return t, nil
}
