// Package store provides the central Store facade for Nexus Hub database access.
// Sub-store packages have been redistributed to bounded-context directories:
// identity/store/{authstore,enrollstore,userstore}, fleet/store/, fleet/shadow/,
// fleet/overrides/, fleet/smartgroup/, traffic/store/, compliance/catbagent/.
// Access them via the accessor methods below.
package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/overrides"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/shadow"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/smartgroup"
	fleetstore "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/store/authstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/store/enrollstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/store/userstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/hubstore"
	trafficstore "github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/traffic/store"
)

// ErrNotFound is returned when a query matches zero rows.
// Alias for hubstore.ErrNotFound — the shared sentinel across all sub-stores.
var ErrNotFound = hubstore.ErrNotFound

// ErrAmbiguous is returned by lookups whose match key alone cannot
// uniquely identify a row — currently raised by
// FindActiveAssignmentByIPAndTime when 2+ active DeviceAssignment rows
// share the same ip_address (NAT-shared egress). The IdentityEnricher
// stamps identity.status="ambiguous" in this case rather than guessing
// one user; misattribution is a worse outcome than no attribution.
var ErrAmbiguous = hubstore.ErrAmbiguous

// PgxPool is the minimum pgx pool surface the Store needs across all
// methods. *pgxpool.Pool satisfies it in production; pgxmock.PgxPoolIface
// satisfies it in tests, letting (s *Store) methods be unit-tested
// against the mock without touching a live Postgres. Mirrors the
// pgxQuerier convention the Cat B loaders already follow.
type PgxPool interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store provides database access methods for the Nexus Hub.
//
// db is interface-typed so tests can inject pgxmock; pool keeps the
// concrete reference for Pool() callers who need it for fleet/manager /
// handler transactions that bypass the Store API.
type Store struct {
	db   PgxPool
	pool *pgxpool.Pool
}

// New creates a Store backed by the given connection pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{db: pool, pool: pool}
}

// NewWithPgxPool is the test-only constructor accepting any PgxPool —
// production code goes through New. The pool field stays nil so
// callers asking for Pool() get a clear nil rather than a half-real
// store; tests should not call methods that need transactions.
func NewWithPgxPool(db PgxPool) *Store {
	return &Store{db: db}
}

// Pool returns the underlying connection pool (for transactions).
// Returns nil when constructed via NewWithPgxPool (test-only path);
// production callers always go through New so pool is set.
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// AuthStore returns the auth sub-store (revoked/refresh tokens + session touch).
// Callers: identity/enrollment, jobs/expiry, ws/server token validation.
func (s *Store) AuthStore() *authstore.Store { return authstore.New(s.db) }

// EnrollStore returns the enrollment sub-store (tokens + device assignments).
// Callers: identity/enrollment, identity/handler/enroll, jobs/expiry.
func (s *Store) EnrollStore() *enrollstore.Store { return enrollstore.New(s.db) }

// UserStore returns the user sub-store (nexus users + org info).
// Callers: identity/handler/enroll, identity/handler/bootstrap.
func (s *Store) UserStore() *userstore.Store { return userstore.New(s.db) }

// RegistryStore returns the thing registry sub-store.
// Callers: fleet/manager, fleet/handler/hubapi, ws/server, jobs/drift.
func (s *Store) RegistryStore() *fleetstore.Store { return fleetstore.New(s.db) }

// ConfigStore returns the config sub-store (templates + change events + resolve).
// Callers: fleet/manager, self/shadow, fleet/handler/hubapi.
func (s *Store) ConfigStore() *shadow.Store { return shadow.New(s.db) }

// OverrideStore returns the override sub-store (thing config overrides).
// Callers: fleet/manager, fleet/handler/hubapi, jobs/expiry.
func (s *Store) OverrideStore() *overrides.Store { return overrides.New(s.db) }

// SmartGroupStore returns the smart group sub-store.
// Callers: jobs/drift smart_group_recompute.
func (s *Store) SmartGroupStore() *smartgroup.Store { return smartgroup.New(s.db) }

// TrafficStore returns the traffic event sub-store (identity enrichment).
// Callers: jobs/drift identity_enrichment.
func (s *Store) TrafficStore() *trafficstore.Store { return trafficstore.New(s.db) }

// Re-exported sentinel errors and type aliases for sub-store packages
// that callers still reference via the store package name.
// These allow a clean transition without breaking every call site at once.

// Re-exported type aliases from sub-stores for callers that reference them
// via the store package. Migrate callers to import sub-store packages directly.

type (
	// identity/store sub-stores
	TouchSessionParams = authstore.TouchSessionParams

	EnrollmentToken              = enrollstore.EnrollmentToken
	InsertEnrollmentTokenParams  = enrollstore.InsertEnrollmentTokenParams
	ThingAgentRecord             = enrollstore.ThingAgentRecord
	UpsertDeviceAssignmentParams = enrollstore.UpsertDeviceAssignmentParams
	DeviceAssignmentSource       = enrollstore.DeviceAssignmentSource
	DeviceAssignmentMatch        = trafficstore.DeviceAssignmentMatch
	AgentByIP                    = trafficstore.AgentByIP

	NexusUserInfo = userstore.NexusUserInfo
	OrgInfo       = userstore.OrgInfo

	// fleet/store (fleetstore alias — package name is "store", aliased to avoid collision)
	Thing                  = fleetstore.Thing
	HeartbeatResult        = fleetstore.HeartbeatResult
	ReportedKeyOutcome     = fleetstore.ReportedKeyOutcome
	ListThingsParams       = fleetstore.ListThingsParams
	ListThingsResult       = fleetstore.ListThingsResult
	DriftedThing           = fleetstore.DriftedThing
	UpsertThingParams      = fleetstore.UpsertThingParams
	UpsertThingAgentParams = fleetstore.UpsertThingAgentParams
	ThingWithOverrideAgg   = fleetstore.ThingWithOverrideAgg
	ApplyErrorV            = fleetstore.ApplyErrorV

	// fleet/shadow
	ConfigChangeEvent          = shadow.ConfigChangeEvent
	ListConfigHistoryParams    = shadow.ListConfigHistoryParams
	ListConfigHistoryResult    = shadow.ListConfigHistoryResult
	ConfigTemplate             = shadow.ConfigTemplate
	ConfigTemplateCatalogEntry = shadow.ConfigTemplateCatalogEntry

	// fleet/overrides
	ThingConfigOverride                  = overrides.ThingConfigOverride
	ThingConfigOverrideWithStale         = overrides.ThingConfigOverrideWithStale
	ThingConfigOverrideWithStaleAndThing = overrides.ThingConfigOverrideWithStaleAndThing
	ListOverridesFilter                  = overrides.ListOverridesFilter
	ListOverridesSummary                 = overrides.ListOverridesSummary
	OverrideState                        = overrides.OverrideState

	// fleet/smartgroup
	SmartGroupSnapshot = smartgroup.SmartGroupSnapshot

	// traffic/store (trafficstore alias — package name is "store", aliased to avoid collision)
	PendingIdentityEvent      = trafficstore.PendingIdentityEvent
	MatchedEventByTraceID     = trafficstore.MatchedEventByTraceID
	UpdateEventIdentityParams = trafficstore.UpdateEventIdentityParams
)

// Re-exported constants.
const (
	ConfigChangedChannel      = shadow.ConfigChangedChannel
	DeviceAssignmentSourceSSO = enrollstore.DeviceAssignmentSourceSSO
)

// NewOverrideState re-exports the overrides constructor for callers
// that still use store.NewOverrideState.
func NewOverrideState(b []byte) (OverrideState, error) { return overrides.NewOverrideState(b) }
