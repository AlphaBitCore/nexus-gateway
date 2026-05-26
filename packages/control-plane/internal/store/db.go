// Package store provides the pgx database layer for the control-plane.
// All queries are hand-written SQL matching the Prisma schema.
package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	cpgx "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/pgx"
	metrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/instruments"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/fleet/store/agentstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/fleet/store/fleetstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/apikeystore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/iamstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/users/userstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/diag/diagstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/opsmetrics/opsstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/settings/store/metricsstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/analytics/analyticsstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/trafficstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/modelstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/ai/providers/credstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/scim/scimstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/observability/thingstats/thingstore"
)

// PgxPool is the shared pool interface for control-plane store packages.
// It is an alias of platform/pgx.PgxPool — the concrete *pgxpool.Pool
// satisfies it in production; pgxmock's PgxPoolIface satisfies it in
// tests. Defined once in platform/pgx to avoid duplication.
type PgxPool = cpgx.PgxPool

// PoolConfig is an alias of platform/pgx.PoolConfig for backward
// compatibility with callers that reference store.PoolConfig.
type PoolConfig = cpgx.PoolConfig

// DB wraps a pgx connection pool.
//
// Pool is exposed as the concrete *pgxpool.Pool because handlers and
// cmd code wire it into authserver_store / revocation / rulepack /
// configstore constructors that take *pgxpool.Pool directly — and
// because production tx code paths need the concrete pool's
// AcquireFunc/BeginTxFunc surface.
//
// pool is the internal interface-typed view that (db *DB) methods
// use for SQL. Tests construct *DB via NewWithPgxPool with a
// pgxmock pool — that sets pool only; Pool stays nil so any
// accidental handler path is a clear nil-deref.
type DB struct {
	Pool *pgxpool.Pool
	pool PgxPool
}

// New creates a DB from a connection string with optional pool tuning.
// Pool construction and connectivity check are delegated to platform/pgx.New.
func New(ctx context.Context, dsn string, opts ...PoolConfig) (*DB, error) {
	pool, err := cpgx.New(ctx, dsn, opts...)
	if err != nil {
		return nil, err
	}
	return &DB{Pool: pool, pool: pool}, nil
}

// Close closes the connection pool.
func (db *DB) Close() {
	db.Pool.Close()
}

// QueryRollupCascade delegates to metricsstore for aggregate rollup queries.
// Used by compliance_dashboard.go cross-boundary queries.
func (db *DB) QueryRollupCascade(ctx context.Context, q metrics.MetricsQuery) ([]metrics.RollupRow, error) {
	return metricsstore.New(db.pool).QueryRollupCascade(ctx, q)
}

// AuditEventRow is re-exported from fleetstore for cross-path governance queries.
type AuditEventRow = fleetstore.AuditEventRow

// NexusUserSafe is re-exported from userstore for cross-path governance queries.
type NexusUserSafe = userstore.NexusUserSafe

// Model, Credential and their create-param types are re-exported from their
// respective sub-stores so that provider.go (same package) can reference them
// without an explicit package prefix.
type Model = modelstore.Model
type CreateModelParams = modelstore.CreateModelParams
type Credential = credstore.Credential
type CreateCredentialParams = credstore.CreateCredentialParams

// CreateIdentityProviderParams is re-exported from scimstore for idp_migrate.go.
type CreateIdentityProviderParams = scimstore.CreateIdentityProviderParams

// CreateIdentityProvider delegates to scimstore for idp_migrate.go cross-boundary call.
func (db *DB) CreateIdentityProvider(ctx context.Context, p CreateIdentityProviderParams) (*scimstore.IdentityProviderRecord, error) {
	return scimstore.New(db.pool).CreateIdentityProvider(ctx, p)
}

// GetThingTypeSummaries delegates to thingstore for service_instance.go cross-boundary call.
func (db *DB) GetThingTypeSummaries(ctx context.Context) ([]thingstore.ThingTypeSummary, error) {
	return thingstore.New(db.pool).GetThingTypeSummaries(ctx)
}

// ThingRegistry, ThingConfigTemplate, and ConfigChangeEvent are re-exported
// from thingstore for the AdminHandler applied-config surface
// (handler/admin_things_applied_config.go).
type ThingRegistry = thingstore.ThingRegistry
type ThingConfigTemplate = thingstore.ThingConfigTemplate
type ConfigChangeEvent = thingstore.ConfigChangeEvent

// GetThing delegates to thingstore.
func (db *DB) GetThing(ctx context.Context, id string) (*ThingRegistry, error) {
	return thingstore.New(db.pool).GetThing(ctx, id)
}

// ListTemplatesByType delegates to thingstore.
func (db *DB) ListTemplatesByType(ctx context.Context, thingType string) ([]ThingConfigTemplate, error) {
	return thingstore.New(db.pool).ListTemplatesByType(ctx, thingType)
}

// GetLatestConfigChangeEvent delegates to thingstore.
func (db *DB) GetLatestConfigChangeEvent(ctx context.Context, thingType, configKey string) (*ConfigChangeEvent, error) {
	return thingstore.New(db.pool).GetLatestConfigChangeEvent(ctx, thingType, configKey)
}

// QueryRollupAware delegates to metricsstore for time-series-aware rollup queries.
// Used by AdminHandler.queryMetricsOrFallback in handler/helpers.go.
func (db *DB) QueryRollupAware(ctx context.Context, q metrics.MetricsQuery) ([]metrics.RollupRow, error) {
	return metricsstore.New(db.pool).QueryRollupAware(ctx, q)
}

// AdminAuditLogListParams is re-exported from trafficstore for the AdminHandler
// audit-log surface (handler/helpers.go, my_routes.go, traffic handler).
type AdminAuditLogListParams = trafficstore.AdminAuditLogListParams

// ListGroupNamesForPrincipal delegates to iamstore for the AdminHandler
// isSuperAdmin check in handler/helpers.go.
func (db *DB) ListGroupNamesForPrincipal(ctx context.Context, principalType, principalID string) ([]string, error) {
	return iamstore.New(db.pool).ListGroupNamesForPrincipal(ctx, principalType, principalID)
}

// NewWithPgxPool is the test-only constructor. Production callers go
// through New(); tests pass a pgxmock pool here so individual store
// methods can be unit-tested without a live Postgres. Pool stays nil
// so any handler path that demands the concrete type fails loudly
// instead of silently using the mock.
func NewWithPgxPool(pool PgxPool) *DB {
	return &DB{pool: pool}
}

// InternalPool returns the underlying PgxPool interface (which may be
// a pgxmock in test contexts). Sub-stores that migrate away from
// cross_aliases use this to construct themselves from a *DB in tests
// where Pool (*pgxpool.Pool) is nil. Production handlers use DB.Pool
// directly (which is always a *pgxpool.Pool).
func (db *DB) InternalPool() PgxPool { return db.pool }

// APIKeyWithOwner is re-exported from apikeystore.
type APIKeyWithOwner = apikeystore.APIKeyWithOwner

// ThingNodeInfo is re-exported from agentstore.
type ThingNodeInfo = agentstore.ThingNodeInfo

// DiagSilence is re-exported from diagstore.
type DiagSilence = diagstore.DiagSilence

// DiagEvent is re-exported from opsstore.
type DiagEvent = opsstore.DiagEvent

// DiagGroup is re-exported from opsstore.
type DiagGroup = opsstore.DiagGroup

// DiagModeWindow is re-exported from opsstore.
type DiagModeWindow = opsstore.DiagModeWindow

// DeviceAssignmentDetail is re-exported from fleetstore.
type DeviceAssignmentDetail = fleetstore.DeviceAssignmentDetail

// GroupByResult is re-exported from analyticsstore.
type GroupByResult = analyticsstore.GroupByResult

// ModelListParams is re-exported from modelstore.
type ModelListParams = modelstore.ModelListParams

// GetWatermark delegates to metricsstore. Called by store-package integration
// tests (metrics_rollup_aware_test.go).
func (db *DB) GetWatermark(ctx context.Context, jobName string) (time.Time, error) {
	return metricsstore.New(db.pool).GetWatermark(ctx, jobName)
}

// ListModelsFlat delegates to modelstore. Called by store-package integration
// tests (model_test.go).
func (db *DB) ListModelsFlat(ctx context.Context, p ModelListParams) ([]Model, int, error) {
	return modelstore.New(db.pool).ListModelsFlat(ctx, p)
}

// LookupThingNodeByCertSerial delegates to agentstore. Called by middleware tests
// that pass *store.DB as middleware.ThingNodeLookup.
func (db *DB) LookupThingNodeByCertSerial(ctx context.Context, serial string) (*ThingNodeInfo, error) {
	return agentstore.New(db.pool).LookupThingNodeByCertSerial(ctx, serial)
}

