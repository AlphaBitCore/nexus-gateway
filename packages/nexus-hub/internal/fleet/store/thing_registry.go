package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"time"

	"github.com/jackc/pgx/v5"
)

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
	// empty strings. Surfaced into admin list/detail.
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

// GetThingStatus returns just the status field of a Thing. Returns ErrNotFound
// when the id is unknown. Used by the WS service-token auth path to reject
// reconnects from revoked service Things without loading the full Thing row.
func (s *Store) GetThingStatus(ctx context.Context, id string) (string, error) {
	var status string
	err := s.db.QueryRow(ctx, `SELECT status FROM thing WHERE id = $1`, id).Scan(&status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("get thing status: %w", err)
	}
	return status, nil
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
