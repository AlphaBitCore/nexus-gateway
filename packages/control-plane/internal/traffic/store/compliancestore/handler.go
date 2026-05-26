// Package compliancestore owns the compliance-specific query surface:
// matrix audit events, compliance audit events, coverage stats, hook
// health, trinity stats, and the full compliance overview dashboard.
// Split from store/misc_queries.go and store/compliance_dashboard.go so
// traffic/ handlers can depend directly on this narrow package instead of
// routing through the *store.DB god object.
package compliancestore

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/settings/store/metricsstore"
)

// PgxPool is the minimal pgx surface compliancestore needs for direct SQL.
type PgxPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store owns the compliance query surface.
// metrics is used by the rollup-cascade paths (GetComplianceCoverage,
// GetHookHealth, GetComplianceDashboard); it may be nil when only
// pure-SQL methods are needed (tests or callers that don't need rollup).
type Store struct {
	pool    PgxPool
	metrics *metricsstore.Store
}

// New constructs a Store from a pool. Rollup-based methods require
// metrics to be non-nil; pass nil only when rollup paths are not needed.
func New(pool PgxPool, metrics *metricsstore.Store) *Store {
	return &Store{pool: pool, metrics: metrics}
}
