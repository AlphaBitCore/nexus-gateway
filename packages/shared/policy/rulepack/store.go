// Package rulepack: store.go — DB-backed persistence for packs, installs, overrides.
package rulepack

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	core "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// PgxPool is the minimum pgx pool surface Store needs. The concrete
// *pgxpool.Pool satisfies it in production; pgxmock's PgxPoolIface
// satisfies it in tests so the SQL paths can be unit-tested without a
// live Postgres. Mirrors the PgxPool seam in
// packages/control-plane/internal/store and packages/nexus-hub/internal/store.
type PgxPool interface {
	BeginTx(ctx context.Context, txOptions pgx.TxOptions) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is the DB-backed persistence layer for rule packs. Methods take
// a ctx + pool-aware connection; no implicit transactions beyond what
// each method declares internally.
type Store struct{ pool PgxPool }

// NewStore returns a Store backed by pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// NewStoreWithPgxPool is the test-only constructor. Production callers
// go through NewStore; tests pass a pgxmock pool here so individual
// store methods can be unit-tested without a live Postgres.
func NewStoreWithPgxPool(pool PgxPool) *Store { return &Store{pool: pool} }

// ErrDuplicatePackVersion is returned when an import hits the (name, version) unique constraint.
var ErrDuplicatePackVersion = errors.New("rulepack: (name, version) already exists")

// pgUniqueViolation is the PostgreSQL SQLSTATE for a unique_violation.
const pgUniqueViolation = "23505"

// validSeverities is the closed authoring-severity enum. Mirrors the set
// enforced by ValidatePack (yaml.go); kept here so the persistence layer can
// reject a bad severity even when the caller bypassed LoadYAML/ValidatePack
// (e.g. a direct JSON admin-form write). A severity typo silently downgrades
// a blocking rule at runtime (severityToDecision maps any unknown value to
// Approve), so this gate is a correctness backstop, not cosmetics.
var validSeverities = map[string]struct{}{
	"hard": {}, "soft": {}, "warn": {},
}

// RuleError describes one invalid rule found during persistence validation.
// Index is the rule's position in the incoming slice; RuleID is its authored
// id (may be empty if the rule omitted one); Reason is the human-readable
// cause (bad regex, bad severity).
type RuleError struct {
	Index  int    `json:"index"`
	RuleID string `json:"ruleId"`
	Reason string `json:"reason"`
}

// InvalidRulesError aggregates every RuleError found in a single
// ImportPack/UpdatePack call so the admin API can report ALL invalid rules
// at once instead of one-at-a-time round trips. No rules are persisted when
// this error is returned.
type InvalidRulesError struct {
	Errors []RuleError
}

func (e *InvalidRulesError) Error() string {
	parts := make([]string, 0, len(e.Errors))
	for _, re := range e.Errors {
		parts = append(parts, fmt.Sprintf("rule[%d] %q: %s", re.Index, re.RuleID, re.Reason))
	}
	return "rulepack: invalid rules: " + strings.Join(parts, "; ")
}

// validateRules checks every rule's pattern (via core.CompilePattern, the same
// cache-backed compiler the runtime evaluator uses, so a pattern that passes
// here is guaranteed compilable at evaluation time) and severity (against the
// closed authoring enum). Returns *InvalidRulesError listing every offending
// rule, or nil when all rules are valid. Invalid rules MUST NOT be persisted.
func validateRules(rules []Rule) error {
	var bad []RuleError
	for i, r := range rules {
		if _, ok := validSeverities[r.Severity]; !ok {
			bad = append(bad, RuleError{
				Index:  i,
				RuleID: r.RuleID,
				Reason: fmt.Sprintf("invalid severity %q (want hard|soft|warn)", r.Severity),
			})
		}
		if r.Pattern == "" {
			bad = append(bad, RuleError{
				Index: i, RuleID: r.RuleID, Reason: "empty pattern",
			})
			continue
		}
		if _, err := core.CompilePattern(r.Pattern, r.Flags); err != nil {
			bad = append(bad, RuleError{
				Index:  i,
				RuleID: r.RuleID,
				Reason: fmt.Sprintf("invalid pattern: %v", err),
			})
		}
	}
	if len(bad) > 0 {
		return &InvalidRulesError{Errors: bad}
	}
	return nil
}

// ImportPack inserts Pack + Rules transactionally. Returns the Pack with
// populated IDs.
//
// Every rule's pattern and severity are validated (validateRules) before any
// write; on a validation failure the method returns *InvalidRulesError listing
// every bad rule and persists nothing. This closes the gap where a direct
// JSON admin-form write bypassed LoadYAML/ValidatePack and a typo'd severity
// or uncompilable regex landed in the DB to silently never fire at runtime.
//
// Returns ErrDuplicatePackVersion (wrapped) on (name, version) collision.
func (s *Store) ImportPack(ctx context.Context, p *Pack) (*Pack, error) {
	if err := validateRules(p.Rules); err != nil {
		return nil, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("rulepack.ImportPack: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var newID string
	err = tx.QueryRow(ctx,
		`INSERT INTO "rule_pack" (id, name, version, maintainer, description)
		 VALUES (gen_random_uuid(), $1, $2, $3, NULLIF($4,''))
		 RETURNING id`,
		p.Name, p.Version, p.Maintainer, p.Description,
	).Scan(&newID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return nil, fmt.Errorf("rulepack.ImportPack: %w", ErrDuplicatePackVersion)
		}
		return nil, fmt.Errorf("rulepack.ImportPack: insert pack: %w", err)
	}
	p.ID = newID

	for i := range p.Rules {
		r := &p.Rules[i]
		err := tx.QueryRow(ctx,
			`INSERT INTO "rule" (id, "packId", "ruleId", category, severity, pattern, flags, description, labels)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, NULLIF($6,''), NULLIF($7,''), $8)
			 RETURNING id`,
			newID, r.RuleID, r.Category, r.Severity, r.Pattern, r.Flags, r.Description, r.Labels,
		).Scan(&r.ID)
		if err != nil {
			return nil, fmt.Errorf("rulepack.ImportPack: insert rule %q: %w", r.RuleID, err)
		}
		r.PackID = newID
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("rulepack.ImportPack: commit: %w", err)
	}
	return p, nil
}

// ListPacks returns every pack ordered by (name asc, version desc).
// Pure metadata — rules are NOT included (GetPack returns those).
func (s *Store) ListPacks(ctx context.Context) ([]Pack, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id, p.name, p.version, p.maintainer, COALESCE(p.description, ''),
		       p."createdAt"
		FROM "rule_pack" p
		ORDER BY p.name ASC, p.version DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pack
	for rows.Next() {
		var p Pack
		if err := rows.Scan(&p.ID, &p.Name, &p.Version, &p.Maintainer,
			&p.Description, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPack returns a pack with its full rule list.
func (s *Store) GetPack(ctx context.Context, id string) (*Pack, error) {
	var p Pack
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, version, maintainer, COALESCE(description, ''),
		       "createdAt"
		FROM "rule_pack" WHERE id = $1`, id,
	).Scan(&p.ID, &p.Name, &p.Version, &p.Maintainer, &p.Description, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, "ruleId", category, severity, pattern, COALESCE(flags, ''),
		       COALESCE(description, ''), labels
		FROM "rule" WHERE "packId" = $1
		ORDER BY "ruleId"`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r Rule
		if err := rows.Scan(&r.ID, &r.RuleID, &r.Category, &r.Severity,
			&r.Pattern, &r.Flags, &r.Description, &r.Labels); err != nil {
			return nil, err
		}
		r.PackID = id
		p.Rules = append(p.Rules, r)
	}
	return &p, rows.Err()
}

// Install creates a RulePackInstall row and populates PackName for convenience.
func (s *Store) Install(ctx context.Context, in Install) (*Install, error) {
	err := s.pool.QueryRow(ctx,
		`INSERT INTO "rule_pack_install" (id, "packId", "pinVersion", "boundHookId", enabled)
		 VALUES (gen_random_uuid(), $1, $2, $3, $4)
		 RETURNING id, "installedAt"`,
		in.PackID, in.PinVersion, in.BoundHookID, in.Enabled,
	).Scan(&in.ID, &in.InstalledAt)
	if err != nil {
		return nil, fmt.Errorf("rulepack.Install: %w", err)
	}
	// Populate PackName for convenience — non-fatal if it fails (PackID is still canonical).
	_ = s.pool.QueryRow(ctx, `SELECT name FROM "rule_pack" WHERE id = $1`, in.PackID).Scan(&in.PackName)
	return &in, nil
}

// UpsertOverrides replaces per-rule overrides for an install. Rules not
// mentioned in the incoming list retain their existing override state.
// Pass Disabled=false + SeverityOverride="" to clear a specific override's
// severity swap while keeping the row.
func (s *Store) UpsertOverrides(ctx context.Context, installID string, overrides []Override) error {
	for _, o := range overrides {
		_, err := s.pool.Exec(ctx,
			`INSERT INTO "rule_override" (id, "installId", "ruleLocalId", disabled, "severityOverride", "updatedAt")
			 VALUES (gen_random_uuid(), $1, $2, $3, NULLIF($4,''), NOW())
			 ON CONFLICT ("installId", "ruleLocalId") DO UPDATE SET
			     disabled = EXCLUDED.disabled,
			     "severityOverride" = EXCLUDED."severityOverride",
			     "updatedAt" = NOW()`,
			installID, o.RuleLocalID, o.Disabled, o.SeverityOverride)
		if err != nil {
			return fmt.Errorf("rulepack.UpsertOverrides: %w", err)
		}
	}
	return nil
}

// ErrPackNotFound is returned when an admin CRUD operation references
// a pack id that does not exist. Admin handlers translate this to 404.
var ErrPackNotFound = errors.New("rulepack: pack not found")

// ErrInstallNotFound is returned when an admin CRUD operation references
// a rule_pack_install id that does not exist.
var ErrInstallNotFound = errors.New("rulepack: install not found")

// PackUpdate is the partial-update payload for UpdatePack. Nil pointer
// fields are skipped; empty strings are treated as "clear this field"
// for the nullable description column.
type PackUpdate struct {
	Maintainer  *string
	Description *string
	// Rules, when non-nil, fully replaces the pack's rule list in the same
	// transaction as the metadata update. Passing nil keeps existing rules.
	Rules *[]Rule
}

// UpdatePack applies metadata and optional rule-list updates to packID
// transactionally. Returns ErrPackNotFound when no row matched.
//
// Rule replacement is wholesale — existing rows are deleted and new ones
// inserted. This keeps the UX simple (the UI edits the full list) and
// avoids the complexity of per-rule diffing. The caller must preserve
// RuleID stability if it needs override rows to keep resolving.
//
// When u.Rules is non-nil, every incoming rule's pattern and severity is
// validated (validateRules) before any write; a validation failure returns
// *InvalidRulesError and persists nothing (no partial rule replacement). This
// matches ImportPack so neither authoring path can land an uncompilable regex
// or a typo'd severity that silently never fires at runtime.
func (s *Store) UpdatePack(ctx context.Context, packID string, u PackUpdate) error {
	if u.Rules != nil {
		if err := validateRules(*u.Rules); err != nil {
			return err
		}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rulepack.UpdatePack: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if u.Maintainer != nil || u.Description != nil {
		res, err := tx.Exec(ctx, `
			UPDATE "rule_pack" SET
			    maintainer = COALESCE($2, maintainer),
			    description = CASE WHEN $3::boolean THEN NULLIF($4, '') ELSE description END
			WHERE id = $1`,
			packID,
			u.Maintainer,
			u.Description != nil, stringOr(u.Description),
		)
		if err != nil {
			return fmt.Errorf("rulepack.UpdatePack: metadata: %w", err)
		}
		if res.RowsAffected() == 0 {
			return ErrPackNotFound
		}
	} else {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM "rule_pack" WHERE id = $1)`, packID).Scan(&exists); err != nil {
			return fmt.Errorf("rulepack.UpdatePack: exists: %w", err)
		}
		if !exists {
			return ErrPackNotFound
		}
	}

	if u.Rules != nil {
		if _, err := tx.Exec(ctx, `DELETE FROM "rule" WHERE "packId" = $1`, packID); err != nil {
			return fmt.Errorf("rulepack.UpdatePack: delete rules: %w", err)
		}
		for _, r := range *u.Rules {
			_, err := tx.Exec(ctx,
				`INSERT INTO "rule" (id, "packId", "ruleId", category, severity, pattern, flags, description, labels)
				 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, NULLIF($6,''), NULLIF($7,''), $8)`,
				packID, r.RuleID, r.Category, r.Severity, r.Pattern, r.Flags, r.Description, r.Labels,
			)
			if err != nil {
				return fmt.Errorf("rulepack.UpdatePack: insert rule %q: %w", r.RuleID, err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rulepack.UpdatePack: commit: %w", err)
	}
	return nil
}

// stringOr returns the pointed-to string or "" if the pointer is nil.
// Used by UpdatePack's NULLIF-driven CASE WHEN expressions.
func stringOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// DeletePack removes a pack and its rules. CASCADE on the rule table
// handles rule rows. Installs that reference the pack are blocked by
// the FK (no CASCADE on rule_pack_install.packId); callers must remove
// installs first — the method surfaces that constraint as a regular
// error so the admin UI can explain it.
func (s *Store) DeletePack(ctx context.Context, packID string) error {
	res, err := s.pool.Exec(ctx, `DELETE FROM "rule_pack" WHERE id = $1`, packID)
	if err != nil {
		return fmt.Errorf("rulepack.DeletePack: %w", err)
	}
	if res.RowsAffected() == 0 {
		return ErrPackNotFound
	}
	return nil
}

// UpdateInstall patches mutable fields on a rule_pack_install row. For
// the current scope only `enabled` can be toggled; pin-version changes
// require an uninstall + fresh install so operators are never surprised
// by a silent binding swap.
func (s *Store) UpdateInstall(ctx context.Context, installID string, enabled bool) error {
	res, err := s.pool.Exec(ctx,
		`UPDATE "rule_pack_install" SET enabled = $2 WHERE id = $1`,
		installID, enabled)
	if err != nil {
		return fmt.Errorf("rulepack.UpdateInstall: %w", err)
	}
	if res.RowsAffected() == 0 {
		return ErrInstallNotFound
	}
	return nil
}

// DeleteInstall removes a rule_pack_install. Overrides are removed via
// ON DELETE CASCADE defined on rule_override.installId.
func (s *Store) DeleteInstall(ctx context.Context, installID string) error {
	res, err := s.pool.Exec(ctx, `DELETE FROM "rule_pack_install" WHERE id = $1`, installID)
	if err != nil {
		return fmt.Errorf("rulepack.DeleteInstall: %w", err)
	}
	if res.RowsAffected() == 0 {
		return ErrInstallNotFound
	}
	return nil
}

// ListInstallsForHook returns every rule_pack_install bound to hookID,
// including the pack name for convenience. Disabled installs are included
// (callers decide whether to evaluate them; the admin UI needs visibility).
// Ordered by installedAt ASC so the rule-pack engine scans in install order.
func (s *Store) ListInstallsForHook(ctx context.Context, hookID string) ([]Install, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i."packId", p.name, i."pinVersion", i."boundHookId", i.enabled, i."installedAt"
		FROM "rule_pack_install" i
		JOIN "rule_pack" p ON p.id = i."packId"
		WHERE i."boundHookId" = $1
		ORDER BY i."installedAt" ASC`, hookID)
	if err != nil {
		return nil, fmt.Errorf("rulepack.ListInstallsForHook: %w", err)
	}
	defer rows.Close()
	var out []Install
	for rows.Next() {
		var inst Install
		if err := rows.Scan(&inst.ID, &inst.PackID, &inst.PackName, &inst.PinVersion,
			&inst.BoundHookID, &inst.Enabled, &inst.InstalledAt); err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, rows.Err()
}

// LoadEffectiveSetsForHook returns every enabled install bound to hookID,
// each resolved to its post-override EffectiveRuleSet. Disabled installs
// are filtered out — the runtime engine never sees them. This is the
// method the data-plane config loader calls when enriching HookConfig
// with its bound rule packs.
func (s *Store) LoadEffectiveSetsForHook(ctx context.Context, hookID string) ([]EffectiveRuleSet, error) {
	installs, err := s.ListInstallsForHook(ctx, hookID)
	if err != nil {
		return nil, err
	}
	out := make([]EffectiveRuleSet, 0, len(installs))
	for _, inst := range installs {
		if !inst.Enabled {
			continue
		}
		eff, err := s.LoadForInstall(ctx, inst.ID)
		if err != nil {
			return nil, fmt.Errorf("rulepack.LoadEffectiveSetsForHook: install %s: %w", inst.ID, err)
		}
		out = append(out, *eff)
	}
	return out, nil
}

// LoadForInstall returns the post-override rule list for a given install.
// The returned EffectiveRuleSet is cache-friendly — hook factories call
// this at pipeline-build time and hold the reference for the pipeline's
// lifetime.
func (s *Store) LoadForInstall(ctx context.Context, installID string) (*EffectiveRuleSet, error) {
	var inst Install
	err := s.pool.QueryRow(ctx, `
		SELECT i.id, i."packId", p.name, i."pinVersion", i."boundHookId", i.enabled, i."installedAt"
		FROM "rule_pack_install" i
		JOIN "rule_pack" p ON p.id = i."packId"
		WHERE i.id = $1`, installID,
	).Scan(&inst.ID, &inst.PackID, &inst.PackName, &inst.PinVersion, &inst.BoundHookID, &inst.Enabled, &inst.InstalledAt)
	if err != nil {
		return nil, fmt.Errorf("rulepack.LoadForInstall: load install: %w", err)
	}
	pack, err := s.GetPack(ctx, inst.PackID)
	if err != nil {
		return nil, fmt.Errorf("rulepack.LoadForInstall: load pack: %w", err)
	}
	// Load overrides for this install.
	rows, err := s.pool.Query(ctx, `
		SELECT "ruleLocalId", disabled, COALESCE("severityOverride", '')
		FROM "rule_override" WHERE "installId" = $1`, installID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	overrides := map[string]Override{}
	for rows.Next() {
		var o Override
		if err := rows.Scan(&o.RuleLocalID, &o.Disabled, &o.SeverityOverride); err != nil {
			return nil, err
		}
		overrides[o.RuleLocalID] = o
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Apply overrides: filter disabled rules, swap severity.
	effective := make([]Rule, 0, len(pack.Rules))
	for _, r := range pack.Rules {
		if o, ok := overrides[r.RuleID]; ok {
			if o.Disabled {
				continue
			}
			if o.SeverityOverride != "" {
				r.Severity = o.SeverityOverride
			}
		}
		effective = append(effective, r)
	}
	pack.Rules = effective
	return &EffectiveRuleSet{Install: inst, Pack: *pack}, nil
}
