package alerting

import (
	"context"
	"errors"
	"fmt"
	"github.com/goccy/go-json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when an alert is not found or is in an unexpected state.
var ErrNotFound = errors.New("alert not found or not in expected state")

// PgxPool is the minimum pgx pool surface the alerting Store needs across all
// methods. *pgxpool.Pool satisfies it in production; pgxmock.PgxPoolIface
// satisfies it in tests, letting (s *Store) methods be unit-tested against
// the mock without touching a live Postgres. Mirrors the PgxPool convention
// from packages/nexus-hub/internal/observability/siem and packages/nexus-hub/internal/store.
type PgxPool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store provides persistence operations for the alerting subsystem.
//
// pool is interface-typed (PgxPool) so tests can inject pgxmock without a
// live Postgres. Production code constructs via NewStore which holds a real
// *pgxpool.Pool. The Raiser still requires a concrete *pgxpool.Pool for
// pgx.BeginFunc / row-locking transactions and gets it separately.
type Store struct {
	pool   PgxPool
	secret *ChannelSecretCipher // nil = passthrough; prod never reaches this (InitAlerts fails closed, FU-1)
}

// NewStore creates a Store backed by the given connection pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// NewStoreWithPgxPool is the test-only constructor accepting any PgxPool —
// production code goes through NewStore. Methods that take a pgx.Tx are
// unaffected by this seam (they operate on the tx parameter, not the pool).
func NewStoreWithPgxPool(pool PgxPool) *Store { return &Store{pool: pool} }

// WithChannelSecretCipher attaches the cipher used to encrypt channel config
// secrets at rest. Returns the same Store for chaining. A nil cipher leaves the
// Store in passthrough mode (secrets stored as cleartext); production never
// reaches that state because InitAlerts fails the hub closed when the key is
// unset (FU-1), so the passthrough mode exists only for unit tests that
// construct a Store directly.
func (s *Store) WithChannelSecretCipher(c *ChannelSecretCipher) *Store {
	s.secret = c
	return s
}

// dbSeverity maps our Severity type to the DB enum (uppercase).
func dbSeverity(s Severity) string { return strings.ToUpper(string(s)) }

// dbState maps our State type to the DB enum (uppercase).
func dbState(st State) string { return strings.ToUpper(string(st)) }

// goSeverity converts a DB uppercase enum value to our Severity type via
// the typed-enum ParseLoose helper. A value that doesn't parse falls
// through to the raw lowercase string — the call site for AlertRule /
// Alert reads from a Prisma `AlertSeverity` enum column which constrains
// values to the five known set, so this branch only fires if a future
// migration adds an enum value without updating Go. We keep the value so
// downstream code (e.g. dispatcher matchesSeverity) can still operate;
// callers that need to reject unknowns should use Parse on their inputs.
func goSeverity(s string) Severity {
	if sev, err := ParseLoose(s); err == nil {
		return sev
	}
	return Severity(strings.ToLower(s))
}

// dbSeverityList maps a []Severity to the []string the pgx driver writes
// into the `AlertChannel.severities` text[] column. Unlike Alert.severity
// (a typed Prisma `AlertSeverity` enum stored uppercase), AlertChannel
// .severities is a free-form String[] historically populated by the
// admin UI in lowercase — so we write the canonical lowercase form to
// keep dispatcher.matchesSeverity comparisons case-stable across rows
// written before and after the typed enum landed. Empty input yields an
// empty (not nil) slice so the Postgres array literal is `{}`, matching
// the initialise-on-create contract in CreateChannel.
func dbSeverityList(in []Severity) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = s.String() // lowercase canonical form
	}
	return out
}

// goSeverityList is the inverse of dbSeverityList. It is applied at the
// store-row-scan boundary so the rest of the codebase only sees typed
// Severity values; rows that pre-date the typed enum (or were written by
// a future migration that adds a value) survive via ParseLoose's fallback.
func goSeverityList(in []string) []Severity {
	out := make([]Severity, len(in))
	for i, s := range in {
		out[i] = goSeverity(s)
	}
	return out
}

// goState converts a DB uppercase enum value to our State type.
func goState(s string) State { return State(strings.ToLower(s)) }

// InsertAlert inserts a new alert row and returns its generated UUID.
func (s *Store) InsertAlert(ctx context.Context, a Alert) (string, error) {
	details, err := json.Marshal(a.Details)
	if err != nil {
		return "", fmt.Errorf("marshal details: %w", err)
	}

	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO "Alert" (
			id, "ruleId", "sourceType", "targetKey", "targetLabel",
			severity, state, message, details,
			"firedAt", "lastSeenAt", "duplicateCount"
		) VALUES (
			gen_random_uuid()::text,
			$1,$2,$3,$4,
			$5::"AlertSeverity", $6::"AlertState", $7,$8,
			$9,$10,$11
		)
		RETURNING id`,
		a.RuleID, a.SourceType, a.TargetKey, a.TargetLabel,
		dbSeverity(a.Severity), dbState(a.State), a.Message, details,
		a.FiredAt, a.LastSeenAt, a.DuplicateCount,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert alert: %w", err)
	}
	return id, nil
}

// UpdateFiringDuplicate increments duplicateCount and sets lastSeenAt.
func (s *Store) UpdateFiringDuplicate(ctx context.Context, id string, now time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE "Alert"
		SET "duplicateCount" = "duplicateCount" + 1,
		    "lastSeenAt"     = $2
		WHERE id = $1 AND state = 'FIRING'::"AlertState"`,
		id, now,
	)
	if err != nil {
		return fmt.Errorf("update firing duplicate: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// FindLatestByRuleTarget returns the newest FIRING or ACKNOWLEDGED alert for the
// given rule+target pair, or nil if none exists.
func (s *Store) FindLatestByRuleTarget(ctx context.Context, ruleID, targetKey string) (*Alert, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, "ruleId", "sourceType", "targetKey", "targetLabel",
		       severity, state, message, details,
		       "firedAt", "lastSeenAt", "duplicateCount",
		       "acknowledgedBy", "acknowledgedAt",
		       "resolvedAt", "resolvedBy", "resolvedReason"
		FROM "Alert"
		WHERE "ruleId"    = $1
		  AND "targetKey" = $2
		  AND state IN ('FIRING'::"AlertState", 'ACKNOWLEDGED'::"AlertState")
		ORDER BY "firedAt" DESC
		LIMIT 1`,
		ruleID, targetKey,
	)
	if err != nil {
		return nil, fmt.Errorf("query latest alert: %w", err)
	}
	alerts, err := scanAlertRows(rows)
	if err != nil {
		return nil, err
	}
	if len(alerts) == 0 {
		return nil, nil
	}
	return &alerts[0], nil
}

// FindLatestByRuleTargetAnyStateTx returns the newest alert for the given
// (rule, target) regardless of state — FIRING, ACKNOWLEDGED, or RESOLVED.
// Caller takes SELECT FOR UPDATE on the row so concurrent Raise transactions
// observe a consistent "latest" while computing cooldown windows.
func (s *Store) FindLatestByRuleTargetAnyStateTx(ctx context.Context, tx pgx.Tx, ruleID, targetKey string) (*Alert, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, "ruleId", "sourceType", "targetKey", "targetLabel",
		       severity, state, message, details,
		       "firedAt", "lastSeenAt", "duplicateCount",
		       "acknowledgedBy", "acknowledgedAt",
		       "resolvedAt", "resolvedBy", "resolvedReason"
		FROM "Alert"
		WHERE "ruleId"    = $1
		  AND "targetKey" = $2
		ORDER BY "firedAt" DESC
		LIMIT 1
		FOR UPDATE`,
		ruleID, targetKey,
	)
	if err != nil {
		return nil, fmt.Errorf("query latest alert (any state) tx: %w", err)
	}
	alerts, err := scanAlertRows(rows)
	if err != nil {
		return nil, err
	}
	if len(alerts) == 0 {
		return nil, nil
	}
	return &alerts[0], nil
}

// GetAlert returns a single alert by its ID.
func (s *Store) GetAlert(ctx context.Context, id string) (*Alert, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, "ruleId", "sourceType", "targetKey", "targetLabel",
		       severity, state, message, details,
		       "firedAt", "lastSeenAt", "duplicateCount",
		       "acknowledgedBy", "acknowledgedAt",
		       "resolvedAt", "resolvedBy", "resolvedReason"
		FROM "Alert"
		WHERE id = $1`,
		id,
	)
	if err != nil {
		return nil, fmt.Errorf("get alert: %w", err)
	}
	alerts, err := scanAlertRows(rows)
	if err != nil {
		return nil, err
	}
	if len(alerts) == 0 {
		return nil, ErrNotFound
	}
	return &alerts[0], nil
}

// AcknowledgeAlert transitions a FIRING alert to ACKNOWLEDGED.
// Returns ErrNotFound if the alert does not exist or is not FIRING.
func (s *Store) AcknowledgeAlert(ctx context.Context, id, by, _ string) error {
	now := time.Now().UTC()
	tag, err := s.pool.Exec(ctx, `
		UPDATE "Alert"
		SET state             = 'ACKNOWLEDGED'::"AlertState",
		    "acknowledgedBy"  = $2,
		    "acknowledgedAt"  = $3
		WHERE id = $1 AND state = 'FIRING'::"AlertState"`,
		id, by, now,
	)
	if err != nil {
		return fmt.Errorf("acknowledge alert: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ResolveAlert transitions a FIRING or ACKNOWLEDGED alert to RESOLVED.
// Returns ErrNotFound if the alert does not exist or is already resolved.
func (s *Store) ResolveAlert(ctx context.Context, id, by, reason string) error {
	now := time.Now().UTC()
	tag, err := s.pool.Exec(ctx, `
		UPDATE "Alert"
		SET state             = 'RESOLVED'::"AlertState",
		    "resolvedAt"      = $2,
		    "resolvedBy"      = $3,
		    "resolvedReason"  = $4
		WHERE id = $1 AND state IN ('FIRING'::"AlertState", 'ACKNOWLEDGED'::"AlertState")`,
		id, now, by, reason,
	)
	if err != nil {
		return fmt.Errorf("resolve alert: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ResolveByRuleTarget resolves all FIRING/ACKNOWLEDGED alerts for the given
// rule+target. Returns the number of rows affected.
func (s *Store) ResolveByRuleTarget(ctx context.Context, ruleID, targetKey, reason string) (int, error) {
	now := time.Now().UTC()
	tag, err := s.pool.Exec(ctx, `
		UPDATE "Alert"
		SET state            = 'RESOLVED'::"AlertState",
		    "resolvedAt"     = $3,
		    "resolvedBy"     = 'system',
		    "resolvedReason" = $4
		WHERE "ruleId"    = $1
		  AND "targetKey" = $2
		  AND state IN ('FIRING'::"AlertState", 'ACKNOWLEDGED'::"AlertState")`,
		ruleID, targetKey, now, reason,
	)
	if err != nil {
		return 0, fmt.Errorf("resolve by rule+target: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ResolveByRuleTargetIfAcknowledged resolves ONLY the ACKNOWLEDGED alerts for
// the given rule+target, leaving any FIRING rows untouched. It backs the
// requiresAck auto-resolve path: when a rule sets requiresAck=true the
// producer's "condition cleared" signal may auto-clear an alert a human has
// already triaged (ACKNOWLEDGED), but must NOT silently clear a FIRING alert
// that no human has seen yet — that one waits for an explicit operator resolve.
// Returns the number of rows affected (i.e. ACKNOWLEDGED rows resolved).
func (s *Store) ResolveByRuleTargetIfAcknowledged(ctx context.Context, ruleID, targetKey, reason string) (int, error) {
	now := time.Now().UTC()
	tag, err := s.pool.Exec(ctx, `
		UPDATE "Alert"
		SET state            = 'RESOLVED'::"AlertState",
		    "resolvedAt"     = $3,
		    "resolvedBy"     = 'system',
		    "resolvedReason" = $4
		WHERE "ruleId"    = $1
		  AND "targetKey" = $2
		  AND state IN ('ACKNOWLEDGED'::"AlertState")`,
		ruleID, targetKey, now, reason,
	)
	if err != nil {
		return 0, fmt.Errorf("resolve by rule+target (acknowledged only): %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// CountFiringByRuleTarget returns how many FIRING rows exist for the given
// rule+target. Used by the requiresAck auto-resolve path to log how many
// FIRING alerts were deliberately skipped (left waiting for a human ack).
func (s *Store) CountFiringByRuleTarget(ctx context.Context, ruleID, targetKey string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM "Alert"
		WHERE "ruleId"    = $1
		  AND "targetKey" = $2
		  AND state = 'FIRING'::"AlertState"`,
		ruleID, targetKey,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count firing by rule+target: %w", err)
	}
	return n, nil
}

// CountChannels returns the total number of configured alert channels.
// Alerting is org-scoped by construction in this codebase (a single tenant per
// deployment), so a plain table COUNT is the org channel count. The UpdateRule
// admin handler uses it to warn when a rule is enabled but no channel exists to
// deliver its notifications.
func (s *Store) CountChannels(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM "AlertChannel"`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count channels: %w", err)
	}
	return n, nil
}

// ListAlerts returns a paginated, filtered list of alerts plus total count.
func (s *Store) ListAlerts(ctx context.Context, f ListFilter) ([]Alert, int, error) {
	where, args := buildListWhere(f)

	countSQL := `SELECT COUNT(*) FROM "Alert"` + where
	var total int
	if err := s.pool.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count alerts: %w", err)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	offset := f.Offset

	selectSQL := `SELECT id, "ruleId", "sourceType", "targetKey", "targetLabel",
		       severity, state, message, details,
		       "firedAt", "lastSeenAt", "duplicateCount",
		       "acknowledgedBy", "acknowledgedAt",
		       "resolvedAt", "resolvedBy", "resolvedReason"
		FROM "Alert"` + where +
		fmt.Sprintf(` ORDER BY "firedAt" DESC LIMIT %d OFFSET %d`, limit, offset)

	rows, err := s.pool.Query(ctx, selectSQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list alerts: %w", err)
	}
	alerts, err := scanAlertRows(rows)
	if err != nil {
		return nil, 0, err
	}
	return alerts, total, nil
}

// buildListWhere constructs a parameterised WHERE clause from a ListFilter.
//
// Each of the four categorical slice fields (State, Severity, SourceType,
// RuleID) emits a single `col = ANY($n::type[])` predicate when non-empty;
// dimensions are AND'd together. State and Severity values are case-folded
// to uppercase to match the DB enums; SourceType and RuleID pass through
// unchanged. A single-element slice behaves identically to the old `col = $n`
// form, so existing single-value callers keep working.
func buildListWhere(f ListFilter) (string, []any) {
	var clauses []string
	var args []any
	idx := 1

	if len(f.State) > 0 {
		upper := make([]string, len(f.State))
		for i, s := range f.State {
			upper[i] = strings.ToUpper(s)
		}
		clauses = append(clauses, fmt.Sprintf(`state = ANY($%d::"AlertState"[])`, idx))
		args = append(args, upper)
		idx++
	}
	if len(f.Severity) > 0 {
		upper := make([]string, len(f.Severity))
		for i, s := range f.Severity {
			upper[i] = strings.ToUpper(s)
		}
		clauses = append(clauses, fmt.Sprintf(`severity = ANY($%d::"AlertSeverity"[])`, idx))
		args = append(args, upper)
		idx++
	}
	if len(f.SourceType) > 0 {
		clauses = append(clauses, fmt.Sprintf(`"sourceType" = ANY($%d::text[])`, idx))
		args = append(args, f.SourceType)
		idx++
	}
	if len(f.RuleID) > 0 {
		clauses = append(clauses, fmt.Sprintf(`"ruleId" = ANY($%d::text[])`, idx))
		args = append(args, f.RuleID)
		idx++
	}
	if f.Since != nil {
		clauses = append(clauses, fmt.Sprintf(`"firedAt" >= $%d`, idx))
		args = append(args, *f.Since)
		idx++
	}
	if f.Until != nil {
		clauses = append(clauses, fmt.Sprintf(`"firedAt" <= $%d`, idx))
		args = append(args, *f.Until)
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func scanAlertRows(rows pgx.Rows) ([]Alert, error) {
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func scanAlert(s pgx.Row) (Alert, error) {
	var a Alert
	var detailsRaw []byte
	var sev, state string

	err := s.Scan(
		&a.ID, &a.RuleID, &a.SourceType, &a.TargetKey, &a.TargetLabel,
		&sev, &state, &a.Message, &detailsRaw,
		&a.FiredAt, &a.LastSeenAt, &a.DuplicateCount,
		&a.AcknowledgedBy, &a.AcknowledgedAt,
		&a.ResolvedAt, &a.ResolvedBy, &a.ResolvedReason,
	)
	if err != nil {
		return Alert{}, fmt.Errorf("scan alert: %w", err)
	}
	a.Severity = goSeverity(sev)
	a.State = goState(state)
	if len(detailsRaw) > 0 {
		if err := json.Unmarshal(detailsRaw, &a.Details); err != nil {
			return Alert{}, fmt.Errorf("unmarshal details: %w", err)
		}
	}
	return a, nil
}

// ListRulesParams carries the filter + pagination inputs for ListRules.
// Each filter is optional; an empty / nil value means "no filter".
type ListRulesParams struct {
	// Search matches id or displayName (case-insensitive substring).
	Search string
	// Enabled, when non-nil, restricts to rules with that enabled value.
	Enabled *bool
	// Severity, when set, filters to that defaultSeverity (case-insensitive).
	Severity string
	// SourceType, when set, filters to that sourceType (case-insensitive).
	SourceType string
	// Pagination. Limit ≤ 0 defaults to 50; Offset < 0 is treated as 0.
	Limit  int
	Offset int
}

// ListRules returns paginated alert rules matching the filter struct,
// ordered by updatedAt desc, id asc (newest-edited first, then stable).
// Returns rules + total count of MATCHING rows (not the global table count).
func (s *Store) ListRules(ctx context.Context, p ListRulesParams) ([]AlertRule, int, error) {
	if p.Limit <= 0 {
		p.Limit = 50
	}
	if p.Offset < 0 {
		p.Offset = 0
	}

	// Build dynamic WHERE clause. Args slice grows in lockstep with the
	// placeholder index so the SQL we emit always matches positional args.
	clauses := []string{}
	args := []any{}
	argN := 0
	add := func(arg any) string {
		args = append(args, arg)
		argN++
		return fmt.Sprintf("$%d", argN)
	}
	if q := strings.TrimSpace(p.Search); q != "" {
		// id and displayName are short — ILIKE is fine without a trigram index.
		pat := "%" + q + "%"
		ph := add(pat)
		clauses = append(clauses, fmt.Sprintf(`(id ILIKE %s OR "displayName" ILIKE %s)`, ph, ph))
	}
	if p.Enabled != nil {
		clauses = append(clauses, fmt.Sprintf("enabled = %s", add(*p.Enabled)))
	}
	if sev := strings.TrimSpace(p.Severity); sev != "" {
		clauses = append(clauses, fmt.Sprintf(`"defaultSeverity" = %s::"AlertSeverity"`, add(sev)))
	}
	if st := strings.TrimSpace(p.SourceType); st != "" {
		clauses = append(clauses, fmt.Sprintf(`"sourceType" = %s`, add(st)))
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}

	var total int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM "AlertRule"`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count rules: %w", err)
	}

	dataArgs := append([]any{}, args...)
	limitPh := fmt.Sprintf("$%d", argN+1)
	offsetPh := fmt.Sprintf("$%d", argN+2)
	dataArgs = append(dataArgs, p.Limit, p.Offset)

	rows, err := s.pool.Query(ctx, `
		SELECT id, "displayName", "sourceType", "defaultSeverity",
		       "requiresAck", enabled, params, "paramsSchema", "cooldownSec",
		       group_id_filter,
		       "createdAt", "updatedAt"
		FROM "AlertRule"`+where+`
		ORDER BY "updatedAt" DESC, id ASC
		LIMIT `+limitPh+` OFFSET `+offsetPh, dataArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("list rules: %w", err)
	}
	rules, err := scanRuleRows(rows)
	return rules, total, err
}

// GetRule returns a single alert rule by ID.
func (s *Store) GetRule(ctx context.Context, id string) (*AlertRule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, "displayName", "sourceType", "defaultSeverity",
		       "requiresAck", enabled, params, "paramsSchema", "cooldownSec",
		       group_id_filter,
		       "createdAt", "updatedAt"
		FROM "AlertRule"
		WHERE id = $1`,
		id,
	)
	if err != nil {
		return nil, fmt.Errorf("get rule: %w", err)
	}
	rules, err := scanRuleRows(rows)
	if err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return nil, ErrNotFound
	}
	return &rules[0], nil
}

// UpdateRule updates the mutable fields of an alert rule.
func (s *Store) UpdateRule(ctx context.Context, r AlertRule) error {
	params, err := json.Marshal(r.Params)
	if err != nil {
		return fmt.Errorf("marshal params: %w", err)
	}
	paramsSchema, err := json.Marshal(r.ParamsSchema)
	if err != nil {
		return fmt.Errorf("marshal params_schema: %w", err)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE "AlertRule"
		SET "displayName"     = $2,
		    "defaultSeverity" = $3::"AlertSeverity",
		    "requiresAck"     = $4,
		    enabled           = $5,
		    params            = $6,
		    "paramsSchema"    = $7,
		    "cooldownSec"     = $8,
		    group_id_filter   = $9,
		    "updatedAt"       = NOW()
		WHERE id = $1`,
		r.ID, r.DisplayName, dbSeverity(r.DefaultSeverity),
		r.RequiresAck, r.Enabled, params, paramsSchema, r.CooldownSec,
		r.GroupIDFilter,
	)
	if err != nil {
		return fmt.Errorf("update rule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanRuleRows(rows pgx.Rows) ([]AlertRule, error) {
	defer rows.Close()
	var out []AlertRule
	for rows.Next() {
		var r AlertRule
		var sev string
		var paramsRaw, paramsSchemaRaw []byte
		err := rows.Scan(
			&r.ID, &r.DisplayName, &r.SourceType, &sev,
			&r.RequiresAck, &r.Enabled, &paramsRaw, &paramsSchemaRaw, &r.CooldownSec,
			&r.GroupIDFilter,
			&r.CreatedAt, &r.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan rule: %w", err)
		}
		r.DefaultSeverity = goSeverity(sev)
		if len(paramsRaw) > 0 {
			if err := json.Unmarshal(paramsRaw, &r.Params); err != nil {
				return nil, fmt.Errorf("unmarshal params: %w", err)
			}
		}
		if len(paramsSchemaRaw) > 0 {
			if err := json.Unmarshal(paramsSchemaRaw, &r.ParamsSchema); err != nil {
				return nil, fmt.Errorf("unmarshal params_schema: %w", err)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InsertChannel inserts a new alert channel and returns its generated UUID.
// Secret-valued config fields are encrypted at rest before persistence.
func (s *Store) InsertChannel(ctx context.Context, c Channel) (string, error) {
	encConfig, err := s.secret.encryptConfig(c.Config)
	if err != nil {
		return "", fmt.Errorf("encrypt config: %w", err)
	}
	cfg, err := json.Marshal(encConfig)
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}

	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO "AlertChannel" (id, name, type, enabled, severities, "sourceTypes", config, "updatedAt")
		VALUES (gen_random_uuid()::text, $1,$2,$3,$4,$5,$6, NOW())
		RETURNING id`,
		c.Name, c.Type, c.Enabled, dbSeverityList(c.Severities), c.SourceTypes, cfg,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert channel: %w", err)
	}
	return id, nil
}

// GetChannel returns a single channel by ID.
func (s *Store) GetChannel(ctx context.Context, id string) (*Channel, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, type, enabled, severities, "sourceTypes", config, "createdAt", "updatedAt"
		FROM "AlertChannel"
		WHERE id = $1`,
		id,
	)
	if err != nil {
		return nil, fmt.Errorf("get channel: %w", err)
	}
	chs, err := scanChannelRows(rows)
	if err != nil {
		return nil, err
	}
	if len(chs) == 0 {
		return nil, ErrNotFound
	}
	if err := s.decryptChannels(chs); err != nil {
		return nil, err
	}
	return &chs[0], nil
}

// ListChannels returns all channels ordered by name.
func (s *Store) ListChannels(ctx context.Context) ([]Channel, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, type, enabled, severities, "sourceTypes", config, "createdAt", "updatedAt"
		FROM "AlertChannel"
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	chs, err := scanChannelRows(rows)
	if err != nil {
		return nil, err
	}
	if err := s.decryptChannels(chs); err != nil {
		return nil, err
	}
	return chs, nil
}

// ListEnabledChannels returns only enabled channels.
func (s *Store) ListEnabledChannels(ctx context.Context) ([]Channel, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, type, enabled, severities, "sourceTypes", config, "createdAt", "updatedAt"
		FROM "AlertChannel"
		WHERE enabled = true
		ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list enabled channels: %w", err)
	}
	chs, err := scanChannelRows(rows)
	if err != nil {
		return nil, err
	}
	if err := s.decryptChannels(chs); err != nil {
		return nil, err
	}
	return chs, nil
}

// decryptChannels decrypts the secret-valued config fields of each channel in
// place so callers (the dispatcher's senders, and admin reads before masking)
// see cleartext. A nil cipher is a no-op.
func (s *Store) decryptChannels(chs []Channel) error {
	for i := range chs {
		dec, err := s.secret.decryptConfig(chs[i].Config)
		if err != nil {
			return fmt.Errorf("decrypt channel config: %w", err)
		}
		chs[i].Config = dec
	}
	return nil
}

// UpdateChannel updates the mutable fields of a channel. Secret-valued config
// fields are encrypted at rest before persistence.
func (s *Store) UpdateChannel(ctx context.Context, c Channel) error {
	encConfig, err := s.secret.encryptConfig(c.Config)
	if err != nil {
		return fmt.Errorf("encrypt config: %w", err)
	}
	cfg, err := json.Marshal(encConfig)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE "AlertChannel"
		SET name          = $2,
		    type          = $3,
		    enabled       = $4,
		    severities    = $5,
		    "sourceTypes" = $6,
		    config        = $7,
		    "updatedAt"   = NOW()
		WHERE id = $1`,
		c.ID, c.Name, c.Type, c.Enabled, dbSeverityList(c.Severities), c.SourceTypes, cfg,
	)
	if err != nil {
		return fmt.Errorf("update channel: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteChannel removes a channel by ID.
func (s *Store) DeleteChannel(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM "AlertChannel" WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete channel: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanChannelRows(rows pgx.Rows) ([]Channel, error) {
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		var c Channel
		var cfgRaw []byte
		// Severities lives in a free-form text[] column historically
		// populated by the admin UI in lowercase. Scan into a string
		// slice and then funnel through goSeverityList so the rest of
		// the codebase only ever sees typed Severity values.
		var sevsRaw []string
		err := rows.Scan(
			&c.ID, &c.Name, &c.Type, &c.Enabled,
			&sevsRaw, &c.SourceTypes, &cfgRaw,
			&c.CreatedAt, &c.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan channel: %w", err)
		}
		c.Severities = goSeverityList(sevsRaw)
		if len(cfgRaw) > 0 {
			if err := json.Unmarshal(cfgRaw, &c.Config); err != nil {
				return nil, fmt.Errorf("unmarshal config: %w", err)
			}
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// InsertDispatch records a single delivery attempt and returns its generated UUID.
func (s *Store) InsertDispatch(ctx context.Context, d Dispatch) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO "AlertDispatch" (id, "alertId", "channelId", "channelName", success, "statusCode", "errorMsg", "attemptedAt")
		VALUES (gen_random_uuid()::text, $1,$2,$3,$4,$5,$6,$7)
		RETURNING id`,
		d.AlertID, d.ChannelID, d.ChannelName, d.Success, d.StatusCode, d.ErrorMsg, d.AttemptedAt,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert dispatch: %w", err)
	}
	return id, nil
}

// ListDispatchesByAlert returns all dispatch records for a given alert, newest first.
func (s *Store) ListDispatchesByAlert(ctx context.Context, alertID string) ([]Dispatch, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, "alertId", "channelId", "channelName", success, "statusCode", "errorMsg", "attemptedAt"
		FROM "AlertDispatch"
		WHERE "alertId" = $1
		ORDER BY "attemptedAt" DESC`,
		alertID,
	)
	if err != nil {
		return nil, fmt.Errorf("list dispatches: %w", err)
	}
	defer rows.Close()

	var out []Dispatch
	for rows.Next() {
		var d Dispatch
		if err := rows.Scan(
			&d.ID, &d.AlertID, &d.ChannelID, &d.ChannelName,
			&d.Success, &d.StatusCode, &d.ErrorMsg, &d.AttemptedAt,
		); err != nil {
			return nil, fmt.Errorf("scan dispatch: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
