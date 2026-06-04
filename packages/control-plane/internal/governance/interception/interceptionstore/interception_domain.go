package interceptionstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// InterceptionDomainRow represents an InterceptionDomain row with its paths.
// Used by both the agent Cat B configuration path (read-only, enabled-only)
// and the admin CRUD API (full table — includes disabled rows).
//
// The field layout is the canonical shape for anything serialised out of the
// control-plane store layer; any new admin/agent surface that consumes
// domains + paths should keep the JSON tags below stable.
type InterceptionDomainRow struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	Description       *string         `json:"description,omitempty"`
	HostPattern       string          `json:"hostPattern"`
	HostMatchType     string          `json:"hostMatchType"`
	AdapterID         string          `json:"adapterId"`
	AdapterConfig     json.RawMessage `json:"adapterConfig,omitempty"`
	Enabled           bool            `json:"enabled"`
	Priority          int             `json:"priority"`
	DefaultPathAction string          `json:"defaultPathAction"`
	OnAdapterError    string          `json:"onAdapterError"`
	NetworkZone       string          `json:"networkZone"`
	Source            string          `json:"source,omitempty"`
	// ApplicableEndpoints is the endpoint filter. When non-empty, CP only
	// applies this domain rule to traffic whose classified EndpointType is
	// in the list. Empty list = all endpoints.
	ApplicableEndpoints []string              `json:"applicableEndpoints,omitempty"`
	CreatedAt           time.Time             `json:"createdAt,omitempty"`
	UpdatedAt           time.Time             `json:"updatedAt,omitempty"`
	CreatedBy           *string               `json:"createdBy,omitempty"`
	Paths               []InterceptionPathRow `json:"paths"`
}

// InterceptionPathRow represents a path rule within an InterceptionDomain.
type InterceptionPathRow struct {
	ID          string    `json:"id"`
	DomainID    string    `json:"domainId,omitempty"`
	PathPattern []string  `json:"pathPattern"`
	MatchType   string    `json:"matchType"`
	Action      string    `json:"action"`
	Priority    int       `json:"priority"`
	Description *string   `json:"description,omitempty"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"createdAt,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt,omitempty"`
}

// idColumns layout matches ListEnabledInterceptionDomains / GetInterceptionDomain /
// scanInterceptionDomain. Kept as a single source of truth so every read
// path stays byte-identical with the admin CRUD response shape.
const idDomainColumns = `id, name, description, host_pattern, host_match_type,
	adapter_id, adapter_config, enabled, priority, default_path_action,
	on_adapter_error, network_zone, source, applicable_endpoints,
	created_at, updated_at, created_by`

const idPathColumns = `id, domain_id, path_pattern, match_type, action,
	priority, description, enabled, created_at, updated_at`

// scanDomain decodes a single interception_domain row using idDomainColumns.
// paths is not populated here — callers must attach paths separately.
func scanDomain(row pgx.Row) (*InterceptionDomainRow, error) {
	var d InterceptionDomainRow
	err := row.Scan(
		&d.ID, &d.Name, &d.Description, &d.HostPattern, &d.HostMatchType,
		&d.AdapterID, &d.AdapterConfig, &d.Enabled, &d.Priority,
		&d.DefaultPathAction, &d.OnAdapterError, &d.NetworkZone,
		&d.Source, &d.ApplicableEndpoints,
		&d.CreatedAt, &d.UpdatedAt, &d.CreatedBy,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.Paths = []InterceptionPathRow{}
	return &d, nil
}

// scanPath decodes a single interception_path row using idPathColumns.
func scanPath(row pgx.Row) (*InterceptionPathRow, error) {
	var p InterceptionPathRow
	err := row.Scan(
		&p.ID, &p.DomainID, &p.PathPattern, &p.MatchType, &p.Action,
		&p.Priority, &p.Description, &p.Enabled, &p.CreatedAt, &p.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// attachPaths loads every path row for the given domain IDs in a single query
// and attaches them to the matching domain in domains, preserving the caller
// ORDER BY. onlyEnabled controls whether disabled paths are included.
func (store *Store) attachPaths(ctx context.Context, domains []InterceptionDomainRow, onlyEnabled bool) error {
	if len(domains) == 0 {
		return nil
	}
	ids := make([]string, len(domains))
	for i, d := range domains {
		ids[i] = d.ID
	}
	where := `WHERE domain_id = ANY($1)`
	if onlyEnabled {
		where += ` AND enabled = true`
	}
	q := fmt.Sprintf(`
		SELECT %s
		FROM interception_path
		%s
		ORDER BY priority DESC, created_at ASC
	`, idPathColumns, where)

	rows, err := store.pool.Query(ctx, q, ids)
	if err != nil {
		return fmt.Errorf("list interception paths: %w", err)
	}
	defer rows.Close()

	idx := make(map[string]int, len(domains))
	for i := range domains {
		idx[domains[i].ID] = i
	}
	for rows.Next() {
		var p InterceptionPathRow
		if err := rows.Scan(
			&p.ID, &p.DomainID, &p.PathPattern, &p.MatchType, &p.Action,
			&p.Priority, &p.Description, &p.Enabled, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return fmt.Errorf("scan interception path: %w", err)
		}
		if i, ok := idx[p.DomainID]; ok {
			domains[i].Paths = append(domains[i].Paths, p)
		}
	}
	return rows.Err()
}

// ListEnabledInterceptionDomains returns all enabled interception domains
// with their enabled paths. Used by the agent config endpoint and any other
// read-only consumer that only needs the live ruleset.
//
// Signature MUST stay stable — the Cat B pipeline and any in-tree admin list
// helpers depend on this shape byte-for-byte.
func (store *Store) ListEnabledInterceptionDomains(ctx context.Context) ([]InterceptionDomainRow, error) {
	q := fmt.Sprintf(`
		SELECT %s
		FROM "interception_domain"
		WHERE enabled = true
		ORDER BY priority DESC, created_at ASC
	`, idDomainColumns)

	rows, err := store.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list interception domains: %w", err)
	}
	defer rows.Close()

	var domains []InterceptionDomainRow
	for rows.Next() {
		var d InterceptionDomainRow
		if err := rows.Scan(
			&d.ID, &d.Name, &d.Description, &d.HostPattern, &d.HostMatchType,
			&d.AdapterID, &d.AdapterConfig, &d.Enabled, &d.Priority,
			&d.DefaultPathAction, &d.OnAdapterError, &d.NetworkZone,
			&d.Source, &d.ApplicableEndpoints,
			&d.CreatedAt, &d.UpdatedAt, &d.CreatedBy,
		); err != nil {
			return nil, fmt.Errorf("scan interception domain: %w", err)
		}
		d.Paths = []InterceptionPathRow{}
		domains = append(domains, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate interception domains: %w", err)
	}
	if len(domains) == 0 {
		return domains, nil
	}
	if err := store.attachPaths(ctx, domains, true); err != nil {
		return nil, err
	}
	return domains, nil
}

// admin CRUD: InterceptionDomain

// InterceptionDomainListParams drives ListInterceptionDomains.
type InterceptionDomainListParams struct {
	Search  string // matches name / host_pattern / adapter_id
	Enabled *bool  // nil => include all
	Limit   int
	Offset  int
}

// ListInterceptionDomainsResult is the paginated response envelope.
type ListInterceptionDomainsResult struct {
	Domains []InterceptionDomainRow
	Total   int
}

// ListInterceptionDomains returns every interception domain (enabled or not)
// matching the filters, plus the absolute total matching count.
func (store *Store) ListInterceptionDomains(ctx context.Context, p InterceptionDomainListParams) (*ListInterceptionDomainsResult, error) {
	if p.Limit <= 0 {
		p.Limit = 50
	}

	where := "WHERE 1=1"
	args := []any{}
	n := 1

	if p.Enabled != nil {
		where += fmt.Sprintf(" AND enabled = $%d", n)
		args = append(args, *p.Enabled)
		n++
	}
	if p.Search != "" {
		where += fmt.Sprintf(
			" AND (name ILIKE $%d OR host_pattern ILIKE $%d OR adapter_id ILIKE $%d)",
			n, n, n,
		)
		args = append(args, "%"+escapeILIKE(p.Search)+"%")
		n++
	}

	var total int
	if err := store.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM "interception_domain" `+where, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("count interception domains: %w", err)
	}

	q := fmt.Sprintf(`
		SELECT %s
		FROM "interception_domain"
		%s
		ORDER BY priority DESC, created_at ASC
		LIMIT $%d OFFSET $%d
	`, idDomainColumns, where, n, n+1)
	args = append(args, p.Limit, p.Offset)

	rows, err := store.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list interception domains: %w", err)
	}
	defer rows.Close()

	domains := []InterceptionDomainRow{}
	for rows.Next() {
		var d InterceptionDomainRow
		if err := rows.Scan(
			&d.ID, &d.Name, &d.Description, &d.HostPattern, &d.HostMatchType,
			&d.AdapterID, &d.AdapterConfig, &d.Enabled, &d.Priority,
			&d.DefaultPathAction, &d.OnAdapterError, &d.NetworkZone,
			&d.Source, &d.ApplicableEndpoints,
			&d.CreatedAt, &d.UpdatedAt, &d.CreatedBy,
		); err != nil {
			return nil, fmt.Errorf("scan interception domain: %w", err)
		}
		d.Paths = []InterceptionPathRow{}
		domains = append(domains, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate interception domains: %w", err)
	}
	// Admin list view includes disabled paths so counts / detail navigation
	// work uniformly (toggling a path's enabled flag should not make it
	// disappear from the list page).
	if err := store.attachPaths(ctx, domains, false); err != nil {
		return nil, err
	}
	return &ListInterceptionDomainsResult{Domains: domains, Total: total}, nil
}

// GetInterceptionDomain returns a single domain (with paths) by id, or
// (nil, nil) when no row exists.
func (store *Store) GetInterceptionDomain(ctx context.Context, id string) (*InterceptionDomainRow, error) {
	q := fmt.Sprintf(`SELECT %s FROM "interception_domain" WHERE id = $1`, idDomainColumns)
	d, err := scanDomain(store.pool.QueryRow(ctx, q, id))
	if err != nil {
		return nil, fmt.Errorf("get interception domain: %w", err)
	}
	if d == nil {
		return nil, nil
	}
	paths, err := store.listPathsForDomain(ctx, d.ID, false)
	if err != nil {
		return nil, err
	}
	d.Paths = paths
	return d, nil
}

// listPathsForDomain fetches every path row for a single domain. onlyEnabled
// filters out disabled paths when true.
func (store *Store) listPathsForDomain(ctx context.Context, domainID string, onlyEnabled bool) ([]InterceptionPathRow, error) {
	where := `WHERE domain_id = $1`
	if onlyEnabled {
		where += ` AND enabled = true`
	}
	q := fmt.Sprintf(`
		SELECT %s
		FROM interception_path
		%s
		ORDER BY priority DESC, created_at ASC
	`, idPathColumns, where)
	rows, err := store.pool.Query(ctx, q, domainID)
	if err != nil {
		return nil, fmt.Errorf("list paths for domain: %w", err)
	}
	defer rows.Close()
	out := []InterceptionPathRow{}
	for rows.Next() {
		var p InterceptionPathRow
		if err := rows.Scan(
			&p.ID, &p.DomainID, &p.PathPattern, &p.MatchType, &p.Action,
			&p.Priority, &p.Description, &p.Enabled, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan path: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CreateInterceptionDomainInput carries the full field set for a new domain
// plus an optional list of paths created in the same transaction.
type CreateInterceptionDomainInput struct {
	Name              string
	Description       *string
	HostPattern       string
	HostMatchType     string
	AdapterID         string
	AdapterConfig     json.RawMessage
	Enabled           *bool // nil => DB default (true)
	Priority          int
	DefaultPathAction string // empty => DB default (PROCESS)
	OnAdapterError    string // empty => DB default (FAIL_OPEN)
	NetworkZone       string // empty => DB default (PUBLIC)
	Source            string // empty => DB default (admin)
	// ApplicableEndpoints is the endpoint filter.
	// nil or empty slice => DB default (ARRAY[]::TEXT[], all endpoints).
	ApplicableEndpoints []string
	CreatedBy           *string

	Paths []CreateInterceptionPathInput
}

// CreateInterceptionPathInput carries the fields for a new path under an
// existing domain. DomainID is not captured here; the caller supplies it.
type CreateInterceptionPathInput struct {
	PathPattern []string
	MatchType   string // empty => DB default (PREFIX)
	Action      string // required (no DB default)
	Priority    int
	Description *string
	Enabled     *bool // nil => DB default (true)
}

// CreateInterceptionDomain inserts a domain row, plus every nested path in a
// single transaction, and returns the hydrated domain (with paths).
func (store *Store) CreateInterceptionDomain(ctx context.Context, in CreateInterceptionDomainInput) (*InterceptionDomainRow, error) {
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	hostMatch := firstNonEmptyString(in.HostMatchType, "EXACT")
	defaultAction := firstNonEmptyString(in.DefaultPathAction, "PROCESS")
	onErr := firstNonEmptyString(in.OnAdapterError, "FAIL_OPEN")
	zone := firstNonEmptyString(in.NetworkZone, "PUBLIC")
	source := firstNonEmptyString(in.Source, "admin")

	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	q := fmt.Sprintf(`
		INSERT INTO "interception_domain" (
			id, name, description, host_pattern, host_match_type,
			adapter_id, adapter_config, enabled, priority, default_path_action,
			on_adapter_error, network_zone, source, created_at, updated_at, created_by
		) VALUES (
			gen_random_uuid()::text, $1, $2, $3, $4::"HostMatchType",
			$5, $6, $7, $8, $9::"DefaultPathAction",
			$10::"FailureAction", $11::"NetworkZone", $12, NOW(), NOW(), $13
		)
		RETURNING %s
	`, idDomainColumns)

	d, err := scanDomain(tx.QueryRow(ctx, q,
		in.Name, in.Description, in.HostPattern, hostMatch,
		in.AdapterID, in.AdapterConfig, enabled, in.Priority, defaultAction,
		onErr, zone, source, in.CreatedBy,
	))
	if err != nil {
		return nil, fmt.Errorf("insert interception domain: %w", err)
	}
	if d == nil {
		return nil, fmt.Errorf("insert interception domain: no row returned")
	}

	paths := []InterceptionPathRow{}
	for i, p := range in.Paths {
		row, err := insertPath(ctx, tx, d.ID, p)
		if err != nil {
			return nil, fmt.Errorf("insert path[%d]: %w", i, err)
		}
		paths = append(paths, *row)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}
	d.Paths = paths
	return d, nil
}

// insertPath inserts a single interception_path row under the given domain
// within the supplied tx. It uses the DB defaults for unset enum fields.
func insertPath(ctx context.Context, tx pgx.Tx, domainID string, p CreateInterceptionPathInput) (*InterceptionPathRow, error) {
	enabled := true
	if p.Enabled != nil {
		enabled = *p.Enabled
	}
	matchType := firstNonEmptyString(p.MatchType, "PREFIX")
	if p.Action == "" {
		return nil, fmt.Errorf("path action is required")
	}
	if p.PathPattern == nil {
		p.PathPattern = []string{}
	}
	q := fmt.Sprintf(`
		INSERT INTO "interception_path" (
			id, domain_id, path_pattern, match_type, action,
			priority, description, enabled, created_at, updated_at
		) VALUES (
			gen_random_uuid()::text, $1, $2, $3::"PathMatchType", $4::"PathAction",
			$5, $6, $7, NOW(), NOW()
		)
		RETURNING %s
	`, idPathColumns)
	return scanPath(tx.QueryRow(ctx, q,
		domainID, p.PathPattern, matchType, p.Action, p.Priority, p.Description, enabled,
	))
}

// UpdateInterceptionDomainInput uses nil pointers as "leave column unchanged".
// A non-nil AdapterConfig sets the column to that JSON value; to leave it
// unchanged, omit the field (send nil). Clearing the column to SQL NULL is
// not currently supported — admins can replace with a specific config instead.
type UpdateInterceptionDomainInput struct {
	Name              *string
	Description       *string
	HostPattern       *string
	HostMatchType     *string
	AdapterID         *string
	AdapterConfig     json.RawMessage // nil => no change; []byte("null") => set NULL
	Enabled           *bool
	Priority          *int
	DefaultPathAction *string
	OnAdapterError    *string
	NetworkZone       *string
	Source            *string
}

// UpdateInterceptionDomain applies a partial update using COALESCE so nil
// pointers preserve the existing column value. Returns the refreshed row
// with paths attached.
func (store *Store) UpdateInterceptionDomain(ctx context.Context, id string, in UpdateInterceptionDomainInput) (*InterceptionDomainRow, error) {
	q := fmt.Sprintf(`
		UPDATE "interception_domain" SET
			name                = COALESCE($2, name),
			description         = COALESCE($3, description),
			host_pattern        = COALESCE($4, host_pattern),
			host_match_type     = COALESCE($5::"HostMatchType", host_match_type),
			adapter_id          = COALESCE($6, adapter_id),
			adapter_config      = COALESCE($7::jsonb, adapter_config),
			enabled             = COALESCE($8, enabled),
			priority            = COALESCE($9, priority),
			default_path_action = COALESCE($10::"DefaultPathAction", default_path_action),
			on_adapter_error    = COALESCE($11::"FailureAction", on_adapter_error),
			network_zone        = COALESCE($12::"NetworkZone", network_zone),
			source              = COALESCE($13, source),
			updated_at          = NOW()
		WHERE id = $1
		RETURNING %s
	`, idDomainColumns)

	d, err := scanDomain(store.pool.QueryRow(ctx, q, id,
		in.Name, in.Description, in.HostPattern, in.HostMatchType,
		in.AdapterID, in.AdapterConfig, in.Enabled, in.Priority,
		in.DefaultPathAction, in.OnAdapterError, in.NetworkZone, in.Source,
	))
	if err != nil {
		return nil, fmt.Errorf("update interception domain: %w", err)
	}
	if d == nil {
		return nil, nil
	}
	paths, err := store.listPathsForDomain(ctx, id, false)
	if err != nil {
		return nil, err
	}
	d.Paths = paths
	return d, nil
}

// DeleteInterceptionDomain removes a domain by id. Paths cascade via the
// interception_path FK's ON DELETE CASCADE. Returns pgx.ErrNoRows when no
// row matched so the caller can map to 404.
func (store *Store) DeleteInterceptionDomain(ctx context.Context, id string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "interception_domain" WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete interception domain: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// admin CRUD: InterceptionPath

// GetInterceptionPath returns a path by id, or (nil, nil) when no row exists.
// The returned row carries DomainID so handlers can verify URL ownership.
func (store *Store) GetInterceptionPath(ctx context.Context, id string) (*InterceptionPathRow, error) {
	q := fmt.Sprintf(`SELECT %s FROM "interception_path" WHERE id = $1`, idPathColumns)
	p, err := scanPath(store.pool.QueryRow(ctx, q, id))
	if err != nil {
		return nil, fmt.Errorf("get interception path: %w", err)
	}
	return p, nil
}

// CreateInterceptionPath inserts a new path under the supplied domain id.
// Returns the hydrated row.
func (store *Store) CreateInterceptionPath(ctx context.Context, domainID string, in CreateInterceptionPathInput) (*InterceptionPathRow, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	row, err := insertPath(ctx, tx, domainID, in)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}
	return row, nil
}

// UpdateInterceptionPathInput uses nil pointers as "leave unchanged". A nil
// PathPattern skips the column; an explicit empty slice sets path_pattern to
// an empty ARRAY[] which is a legitimate "match nothing" value.
type UpdateInterceptionPathInput struct {
	PathPattern []string // nil => no change
	MatchType   *string
	Action      *string
	Priority    *int
	Description *string
	Enabled     *bool
}

// UpdateInterceptionPath applies a partial update to a path row.
func (store *Store) UpdateInterceptionPath(ctx context.Context, id string, in UpdateInterceptionPathInput) (*InterceptionPathRow, error) {
	// path_pattern uses a sentinel trick: pgx binds nil []string as SQL NULL,
	// which COALESCE would then keep unchanged — exactly what we want.
	q := fmt.Sprintf(`
		UPDATE "interception_path" SET
			path_pattern = COALESCE($2, path_pattern),
			match_type   = COALESCE($3::"PathMatchType", match_type),
			action       = COALESCE($4::"PathAction", action),
			priority     = COALESCE($5, priority),
			description  = COALESCE($6, description),
			enabled      = COALESCE($7, enabled),
			updated_at   = NOW()
		WHERE id = $1
		RETURNING %s
	`, idPathColumns)
	return scanPath(store.pool.QueryRow(ctx, q, id,
		in.PathPattern, in.MatchType, in.Action, in.Priority, in.Description, in.Enabled,
	))
}

// DeleteInterceptionPath removes a path by id.
func (store *Store) DeleteInterceptionPath(ctx context.Context, id string) error {
	tag, err := store.pool.Exec(ctx, `DELETE FROM "interception_path" WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete interception path: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// firstNonEmptyString returns v if non-empty, otherwise fallback. Local to
// this file to avoid stuttering a generic helper in sqlutil.go; the store
// package already owns pgx idiom polish there.
func firstNonEmptyString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
