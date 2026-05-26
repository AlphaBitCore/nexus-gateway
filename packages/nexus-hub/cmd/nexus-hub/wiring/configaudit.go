package wiring

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
)

// RunConfigKeyAudit scans thing_config_template at startup and logs a WARN
// for every (type, key) row not registered in configkey.ValidByThingType.
// Boot is not failed — orphan rows may exist transiently during
// multi-PR configuration refactors.
func RunConfigKeyAudit(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) {
	if pool == nil || logger == nil {
		return
	}
	scanner := newConfigkeyDBAdapterFromPool(pool)
	orphans, err := configkey.AuditTemplateRows(ctx, scanner)
	if err != nil {
		logger.Error("config audit failed", "error", err)
		return
	}
	for _, o := range orphans {
		logger.Warn("orphan thing_config_template row (not in ValidByThingType)",
			"type", o.Type, "key", o.Key)
	}
	if len(orphans) == 0 {
		logger.Info("config audit clean", "rows_checked", "thing_config_template")
	}
}

// configkeyQueryer is the narrow pool interface configkeyDBAdapter uses.
// *pgxpool.Pool satisfies it; tests may inject a pgxmock via the seam constructor.
type configkeyQueryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// configkeyDBAdapter wraps a configkeyQueryer to satisfy configkey.DBScanner
// without leaking pgx types into the configkey package.
// Production code always passes a *pgxpool.Pool via RunConfigKeyAudit.
type configkeyDBAdapter struct {
	pool configkeyQueryer
}

// newConfigkeyDBAdapterFromPool wraps a *pgxpool.Pool (production path).
func newConfigkeyDBAdapterFromPool(pool *pgxpool.Pool) *configkeyDBAdapter {
	return &configkeyDBAdapter{pool: pool}
}

func (a *configkeyDBAdapter) Query(ctx context.Context, sql string, args ...any) (configkey.Rows, error) {
	rows, err := a.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &pgxRowsAdapter{rows: rows}, nil
}

// pgxRowsAdapter converts a pgx.Rows to a configkey.Rows. The pgx.Rows
// Close method returns nothing in v5; the iterator-style consumer in
// configkey ignores per-row scan errors after the first failure, so
// the surface matches.
type pgxRowsAdapter struct {
	rows pgx.Rows
}

func (p *pgxRowsAdapter) Next() bool                  { return p.rows.Next() }
func (p *pgxRowsAdapter) Scan(dest ...any) error      { return p.rows.Scan(dest...) }
func (p *pgxRowsAdapter) Close()                      { p.rows.Close() }
