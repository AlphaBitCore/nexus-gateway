// Package manager implements the fleet Thing lifecycle management layer.
// It coordinates between HTTP/WebSocket handlers, the database store,
// Redis cache, MQ producer, and WebSocket connection pool.
package manager

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/mq"
)

// WSPool is the interface the Thing Manager uses to push messages to connected Things.
// Implemented by ws.Pool.
type WSPool interface {
	Send(thingID string, msg []byte) bool
	Broadcast(thingType string, msg []byte) int
	IsConnected(thingID string) bool
}

// PgxPool is the minimum pgx pool surface the Thing Manager needs for
// transaction-bound flows (UpdateConfig, SetOverride, ClearOverride,
// handleBreakGlassReport). *pgxpool.Pool satisfies it in production via
// m.store.Pool(); pgxmock.PgxPoolIface satisfies it in tests, letting
// (m *Manager) tx-bound methods be unit-tested against the mock without
// touching a live Postgres. Mirrors the PgxPool convention from
// packages/nexus-hub/internal/storage/store, packages/nexus-hub/internal/observability/siem,
// and packages/nexus-hub/internal/alerts/engine.
type PgxPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Manager is the central business logic component for Thing lifecycle.
type Manager struct {
	store *store.Store
	redis redis.UniversalClient
	mq    mq.Producer
	ws    WSPool
	// pool overrides m.store.Pool() at the 4 tx-bound call sites when set.
	// Production code constructs via New, where pool stays nil and the
	// tx helpers fall through to m.store.Pool(); test code uses
	// NewWithPool to inject a pgxmock pool directly.
	pool   PgxPool
	logger *slog.Logger
	hubID  string

	// signalSecret is the Hub-to-Hub HMAC key for nexus.hub.signal frames.
	// nil = unsigned (tests); production wiring installs it via
	// SetSignalSecret so a data-plane producer cannot forge a fleet-wide config
	// injection over the message bus.
	signalSecret []byte

	// fanoutFailed counts post-commit config fan-out failures, labelled by
	// path (ws|nats). nil until SetFanoutMetrics wires a registry, so the
	// increment helper is a no-op in tests / scheduler-disabled mode. See
	// incFanoutFailed.
	fanoutFailed *opsmetrics.Counter
}

// New creates a new Thing Manager.
func New(
	st *store.Store,
	rdb redis.UniversalClient,
	mqProducer mq.Producer,
	ws WSPool,
	hubID string,
	logger *slog.Logger,
) *Manager {
	return &Manager{
		store:  st,
		redis:  rdb,
		mq:     mqProducer,
		ws:     ws,
		hubID:  hubID,
		logger: logger.With("component", "fleet.manager"),
	}
}

// NewWithPool is the test-only constructor that overrides the tx pool with an
// arbitrary PgxPool (typically pgxmock.PgxPoolIface). Production code MUST go
// through New; this constructor exists solely so transaction-bound flows can
// be unit-tested against a mock. The store is still passed in so non-tx store
// methods continue to work (the store itself wraps the same pgxmock via
// store.NewWithPgxPool in test setup).
func NewWithPool(
	st *store.Store,
	pool PgxPool,
	rdb redis.UniversalClient,
	mqProducer mq.Producer,
	ws WSPool,
	hubID string,
	logger *slog.Logger,
) *Manager {
	return &Manager{
		store:  st,
		pool:   pool,
		redis:  rdb,
		mq:     mqProducer,
		ws:     ws,
		hubID:  hubID,
		logger: logger.With("component", "fleet.manager"),
	}
}

// SetFanoutMetrics wires the ops-metrics registry used for the
// config_fanout_failed_total{path} counter. Called once from the fleet
// wiring after construction (mirrors wsServer.SetOpsMetricsHandler) so the
// Manager constructor signature — and every test call site — stays untouched.
// reg nil is tolerated: the counter stays nil and incFanoutFailed no-ops.
func (m *Manager) SetFanoutMetrics(reg *opsmetrics.Registry) {
	if reg == nil {
		return
	}
	m.fanoutFailed = reg.NewCounter("config.fanout_failed_total", []string{"path"})
}

// incFanoutFailed records one post-commit fan-out failure on the given path
// ("ws" or "nats"). nil-safe so unit tests and registry-less modes skip it.
func (m *Manager) incFanoutFailed(path string) {
	if m.fanoutFailed == nil {
		return
	}
	m.fanoutFailed.With(path).Inc()
}

// Store returns the underlying store (for handler access).
func (m *Manager) Store() *store.Store {
	return m.store
}

// txPool returns the pool used for transaction-bound flows. Returns the
// test-injected pool when set (via NewWithPool); otherwise returns the
// store's *pgxpool.Pool. The return type is interface PgxPool so callers
// don't depend on the concrete pgxpool type.
func (m *Manager) txPool() PgxPool {
	if m.pool != nil {
		return m.pool
	}
	return m.store.Pool()
}
