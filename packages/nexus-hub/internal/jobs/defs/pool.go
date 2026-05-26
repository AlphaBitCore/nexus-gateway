// Package jobs — shared PgxPool seam.
//
// Most jobs in this package take a *pgxpool.Pool directly in their
// constructors. To make Run() / helper methods unit-testable without a
// live Postgres, each job's internal pool field is typed against this
// PgxPool interface — the production *pgxpool.Pool satisfies it (the
// concrete type already exposes Exec / Query / QueryRow / Begin /
// SendBatch), and pgxmock.PgxPoolIface satisfies it in tests.
//
// Why a shared interface in this package:
//
//   - Most jobs use the same subset of pgx surface — defining it once
//     keeps each prod file's import block trim.
//   - Establishes the package-level convention so a reader who sees
//     `pool PgxPool` in any job file knows it's the same shape.
//
// Why constructors still take *pgxpool.Pool:
//
//   - All production wiring in `cmd/nexus-hub/main.go` already passes
//     a *pgxpool.Pool; widening the parameter to PgxPool would force
//     every call site to import this package's interface type.
//   - The field-only seam keeps the call site unchanged while still
//     allowing tests to swap the field via a test-only setter or
//     by exposing the field unexported within the same package.
//
// Mirrors the PgxPool convention in:
//   - packages/nexus-hub/internal/observability/siem/bridge.go (d09a373f8)
//   - packages/nexus-hub/internal/storage/store/store.go
//   - packages/control-plane/internal/store/store.go
package defs

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PgxPool is the minimum pgx pool surface shared across jobs/defs prod
// files. Tests inject pgxmock.PgxPoolIface; production uses
// *pgxpool.Pool. Methods are the same shape as the corresponding
// *pgxpool.Pool methods so the concrete satisfies the interface
// without an adapter.
//
// SendBatch and Begin are included because rollup and merge jobs use
// transactions and pipelined writes; jobs that don't use them simply
// don't call those methods.
type PgxPool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}
