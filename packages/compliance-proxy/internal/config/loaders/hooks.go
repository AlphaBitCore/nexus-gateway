// Package loaders loads config data from the Prisma-migrated PostgreSQL
// database for consumption by the compliance-proxy in-memory config cache
// (packages/compliance-proxy/internal/config/cache).
//
// The compliance-proxy caches hook configurations in memory and invalidates
// them via the Hub WebSocket control channel — Hub broadcasts a config
// change, the local thingclient.OnConfigChanged callback fires, and this
// package's loaders refresh the affected cache entries. Redis is pure cache
// only on the data plane; there is no Redis pub/sub on this path.
//
// All queries use hand-written SQL (database/sql with the pgx/v5/stdlib
// driver) against the Prisma-migrated schema. No query generator is used —
// hand-written SQL was chosen to avoid adding a code-generation dependency
// to the compliance-proxy build.
package loaders

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// stringArray implements sql.Scanner for PostgreSQL text[] columns when
// using database/sql (pgx/v5 stdlib). PostgreSQL transmits text arrays
// in the format {val1,val2,...} — this type parses that representation
// into a Go []string without requiring an additional dependency like
// lib/pq.
type stringArray []string

func (a *stringArray) Scan(src any) error {
	if src == nil {
		*a = nil
		return nil
	}
	var s string
	switch v := src.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		return fmt.Errorf("stringArray.Scan: unsupported type %T", src)
	}
	// Trim surrounding braces: "{ALL,COMPLIANCE_PROXY}" → "ALL,COMPLIANCE_PROXY"
	s = strings.TrimSpace(s)
	if s == "{}" || s == "" {
		*a = []string{}
		return nil
	}
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return fmt.Errorf("stringArray.Scan: unexpected format %q", s)
	}
	inner := s[1 : len(s)-1]
	*a = strings.Split(inner, ",")
	return nil
}

// hookConfigQuery is the hand-written SELECT that drives the cache refresh
// for the hooks category. It targets the Prisma model `HookConfig` which is
// emitted by Prisma as a double-quoted camelCase table, so every identifier
// that contains an uppercase letter must be quoted on the Postgres side.
//
// Filtering:
//   - Only enabled rows are returned; the cache never holds disabled core.
//
// Ordering:
//   - Primary: priority ASC (lower priority runs earlier in the pipeline).
//   - Tiebreaker: createdAt ASC so the ordering is deterministic for two
//     hooks that share the same priority.
const hookConfigQuery = `
	SELECT id, name, type, "implementationId", stage,
	       category, endpoint, script, config,
	       priority, "timeoutMs", "failBehavior", enabled,
	       "applicableIngress"
	FROM "HookConfig"
	WHERE enabled = true
	ORDER BY priority ASC, "createdAt" ASC
`

// HookConfigRow is the raw row shape produced by hookConfigQuery. It sits
// in this package (not in compliance) because it is an implementation
// detail of the DB loader — the compliance package knows nothing about
// the Prisma schema and should not gain a dependency on it.
//
// Exported for testability: buildHookConfig is unit-tested via direct
// construction of HookConfigRow values without a live *sql.Rows.
type HookConfigRow struct {
	ID                string
	Name              string
	Type              string
	ImplementationID  string
	Stage             string
	Category          sql.NullString
	Endpoint          sql.NullString
	Script            sql.NullString
	Config            sql.NullString // jsonb as text
	Priority          int
	TimeoutMs         int
	FailBehavior      string
	Enabled           bool
	ApplicableIngress stringArray
}

// LoadedHookConfig is an alias for core.HookConfig. The loader returns
// this type directly so callers no longer need a field-mapping loop.
type LoadedHookConfig = core.HookConfig

// LoadHookConfigs runs hookConfigQuery against db and maps every row to
// a LoadedHookConfig via buildHookConfig. It returns the whole slice on
// success or the first unrecoverable error encountered.
//
// A per-row parse failure (e.g. malformed JSON in the `config` column)
// aborts the entire load; a partial load would let the compliance-proxy run
// with a silently-truncated hook set, which is worse than failing the
// reload and keeping the previously cached value.
//
// Row → LoadedHookConfig mapping + abort-on-first-error semantics live in
// buildHookConfigsFromRows so they can be unit-tested without a live
// *sql.DB.
func LoadHookConfigs(ctx context.Context, db *sql.DB) ([]LoadedHookConfig, error) {
	rows, err := db.QueryContext(ctx, hookConfigQuery)
	if err != nil {
		return nil, fmt.Errorf("configloader: query HookConfig: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var scanned []HookConfigRow
	for rows.Next() {
		var row HookConfigRow
		if err := rows.Scan(
			&row.ID, &row.Name, &row.Type, &row.ImplementationID, &row.Stage,
			&row.Category, &row.Endpoint, &row.Script, &row.Config,
			&row.Priority, &row.TimeoutMs, &row.FailBehavior, &row.Enabled,
			&row.ApplicableIngress,
		); err != nil {
			return nil, fmt.Errorf("configloader: scan HookConfig row: %w", err)
		}
		scanned = append(scanned, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("configloader: iterate HookConfig: %w", err)
	}
	return buildHookConfigsFromRows(scanned)
}

// buildHookConfigsFromRows converts a slice of scanned rows into the
// final hook-config slice. A malformed JSON config aborts the whole
// build with a wrapped error so the compliance-proxy does not silently
// run with a hook missing its `config` payload (it would default to
// "fail-open + empty config" and behave like a no-op).
func buildHookConfigsFromRows(rows []HookConfigRow) ([]LoadedHookConfig, error) {
	var result []LoadedHookConfig
	for _, row := range rows {
		hc, err := buildHookConfig(row)
		if err != nil {
			return nil, fmt.Errorf("configloader: build HookConfig %q: %w", row.ID, err)
		}
		result = append(result, hc)
	}
	return result, nil
}

// buildHookConfig converts a raw row to the compliance shape.
//
// NULL semantics:
//   - category / endpoint / script: pass-through as empty strings. The
//     compliance layer does not currently expose these fields; they are
//     kept here so that adding them later is a one-line change.
//   - config: NULL or empty string becomes an empty map so hook factories
//     can always call len(cfg.Config) and map lookups without a nil guard.
//   - A malformed JSON string in config is a HARD error — a row with
//     corrupt config data should not silently be replaced with empty, or
//     the hook would run in an unexpected mode.
//
// ApplicableIngress:
//
//	Scanned from the DB `applicableIngress` text[] column. The column
//	defaults to {"ALL"} so existing rows behave identically to the
//	previous hardcoded default.
func buildHookConfig(row HookConfigRow) (LoadedHookConfig, error) {
	sharedRow := core.HookConfigRow{
		ID:                row.ID,
		Name:              row.Name,
		ImplementationID:  row.ImplementationID,
		Stage:             row.Stage,
		Enabled:           row.Enabled,
		Priority:          row.Priority,
		TimeoutMs:         row.TimeoutMs,
		FailBehavior:      row.FailBehavior,
		ConfigJSON:        row.Config.String,
		Endpoint:          row.Endpoint.String,
		ApplicableIngress: []string(row.ApplicableIngress),
	}
	return core.BuildHookConfig(sharedRow)
}
