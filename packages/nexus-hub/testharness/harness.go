//go:build e2e

// Package testharness provides the Nexus Hub e2e test harness. It is a
// public package so e2e tests in other modules (compliance-proxy, ai-
// gateway) can build a live Hub for in-process integration testing.
// Compiled only when the e2e build tag is active.
package testharness

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
	emw "github.com/labstack/echo/v4/middleware"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine/rules"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/alerts/engine/senders"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/fleet/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/handler"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/agentca"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/enrollment"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/observability/opsmetrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/ws"
	sharedops "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// harnessOpsRegistry returns a fresh opsmetrics.Registry backed by a private
// prometheus.Registry. Each NewForTest invocation gets its own registry so
// metric registrations from concurrent e2e tests cannot collide on the
// process-global prometheus.DefaultRegisterer.
func harnessOpsRegistry(t *testing.T) *sharedops.Registry {
	t.Helper()
	return sharedops.NewRegistry(prometheus.NewRegistry())
}

const (
	testServiceToken = "e2e-test-service-token"

	// E2EActorID is the actor_id stamped on config_change_event rows produced
	// by the e2e harness. Tests MUST use this value when calling UpdateConfig
	// so the t.Cleanup hook can scope its DELETE without affecting unrelated
	// rows. It is exported solely for the e2e test package.
	E2EActorID = "e2e-harness"
)

// Option is a functional option for NewForTest.
type Option func(*harnessOpts)

type harnessOpts struct {
	withAlerting   bool
	withOpsMetrics bool
}

// WithAlerting opts in to building the full alerting stack (Store, Raiser,
// Dispatcher, webhook sender, rulesReg) and wiring it into SetupRoutes.
// Without this option, the alerting-related routes (/api/v1/alerts/*,
// /api/v1/admin/alerts/*) are not registered — matching the default
// behaviour that keeps non-alerting tests unaffected.
func WithAlerting() Option { return func(o *harnessOpts) { o.withAlerting = true } }

// WithOpsMetrics opts in to building the opsmetrics ingestion pipeline
// (Writer, DiagWriter, Handler) and registering the diag-events:batch
// HTTP route. Tests that exercise metrics_sample / diag_event WS messages
// or the crash-buffer drain endpoint must pass this option; without it
// those messages are silently dropped at the WS layer and the HTTP route
// is unmounted.
func WithOpsMetrics() Option { return func(o *harnessOpts) { o.withOpsMetrics = true } }

// Harness is the test harness for end-to-end tests. It wires the same
// components as the production nexus-hub but with no-op Redis and MQ.
type Harness struct {
	pool       *pgxpool.Pool
	st         *store.Store
	mgr        *manager.Manager
	wsServer   *ws.Server
	echo       *echo.Echo
	logger     *slog.Logger
	alertStore *alerting.Store  // non-nil only when WithAlerting() was passed
	raiser     *alerting.Raiser // non-nil only when WithAlerting() was passed
}

// NewForTest creates a Harness connected to DATABASE_URL.
// It panics (via t.Fatal) if the database is unreachable or any dependency
// fails to initialise. Redis and MQ are nil (no-op); the scheduler, selfreg,
// and agentCA are all omitted — only the components required for the
// requested tests are wired.
//
// Pass WithAlerting() to additionally wire the alerting stack and register
// the alert HTTP routes. Callers that do not pass this option receive the
// same harness as before (no alerting routes).
func NewForTest(t *testing.T, opts ...Option) *Harness {
	t.Helper()

	var ho harnessOpts
	for _, o := range opts {
		o(&ho)
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Fatal("DATABASE_URL must be set when running e2e tests")
	}

	ctx := context.Background()

	poolCfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	poolCfg.MaxConns = 10

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("create pgxpool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("database ping: %v", err)
	}

	t.Cleanup(func() { pool.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	st := store.New(pool)

	// WSPool and Manager — Redis and MQ are nil (no-op; both are nil-guarded
	// throughout the codebase: cacheDesired, publishHubSignal, etc. all check
	// for nil before use). The opsmetrics registry is unique per harness
	// instance so multiple tests in the same process do not collide on
	// Prometheus registration.
	wsPool := ws.NewPool(harnessOpsRegistry(t), logger)
	mgr := manager.New(st, nil, nil, wsPool, "e2e-hub", logger)

	wsServer := ws.NewServer(wsPool, mgr, "e2e-hub", testServiceToken, nil, logger)

	// Optional ops-metrics ingestion pipeline. Built only when
	// WithOpsMetrics() is set so legacy harness tests don't pay the
	// extra goroutine cost. Stop is registered via t.Cleanup so writers
	// drain before the test exits and the test pool closes.
	var opsDiagPool *pgxpool.Pool
	if ho.withOpsMetrics {
		opsDiagPool = pool
		opsWriter := opsmetrics.NewWriter(pool, logger, 1000, 50*time.Millisecond)
		opsDiagWriter := opsmetrics.NewDiagWriter(pool, logger, 1000, 50*time.Millisecond)
		opsStaticWriter := opsmetrics.NewStaticInfoWriter(pool)
		opsHandler := opsmetrics.NewHandler(opsWriter, opsDiagWriter, opsStaticWriter, logger)
		wsServer.SetOpsMetricsHandler(opsHandler)
		t.Cleanup(func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = opsWriter.Stop(stopCtx)
			_ = opsDiagWriter.Stop(stopCtx)
		})
	}

	enrollSvc := enrollment.NewService(st)

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Use(emw.Recover())

	// Alerting stack — built only when WithAlerting() is requested.
	var (
		alertStore   *alerting.Store
		raiser       *alerting.Raiser
		alertRules   alerting.RuleRegistry
		alertSenders alerting.SenderRegistry
	)
	if ho.withAlerting {
		alertStore = alerting.NewStore(pool)
		reg := senders.NewRegistry()
		reg.Register("webhook", senders.NewWebhook(nil))
		adapter := harnessSenderRegAdapter{r: reg}
		dispatcher := alerting.NewDispatcher(alertStore, adapter, logger)
		raiser = alerting.NewRaiser(pool, alertStore, dispatcher, logger)
		rulesReg := rules.NewRegistry(rules.BuiltinRules)
		alertRules = harnessRulesRegAdapter{r: rulesReg}
		alertSenders = adapter
	}

	handler.SetupRoutes(handler.RouteConfig{
		Echo:         e,
		Mgr:          mgr,
		WSServer:     wsServer,
		Scheduler:    nil,
		Enrollment:   enrollSvc,
		MQProducer:   nil,
		ServiceToken: testServiceToken,
		Store:        st,
		AgentCA:      nil, // enrollment API not needed for this test
		Raiser:       raiser,
		AlertStore:   alertStore,
		AlertRules:   alertRules,
		AlertSenders: alertSenders,
		OpsDiagPool:  opsDiagPool,
		OpsLogger:    logger,
	})

	return &Harness{
		pool:       pool,
		st:         st,
		mgr:        mgr,
		wsServer:   wsServer,
		echo:       e,
		logger:     logger,
		alertStore: alertStore,
		raiser:     raiser,
	}
}

// Handler returns the http.Handler backed by the Echo instance.
// Pass this to httptest.NewServer.
func (h *Harness) Handler() http.Handler {
	return h.echo
}

// Mgr returns the Thing Manager for direct state manipulation in tests.
func (h *Harness) Mgr() *manager.Manager {
	return h.mgr
}

// Store returns the underlying store for direct fixture setup and assertions.
func (h *Harness) Store() *store.Store {
	return h.st
}

// IssueEnrollmentToken pre-creates a Thing row for thingID with type "agent"
// and generates a device token whose hash is stored in
// thing.metadata.deviceTokenHash. Returns the plaintext token for use in
// thingclient.Config.Token. The Thing row is cleaned up by a t.Cleanup hook.
//
// Equivalent to IssueEnrollmentTokenOfType(t, thingID, "agent").
func (h *Harness) IssueEnrollmentToken(t *testing.T, thingID string) string {
	return h.IssueEnrollmentTokenOfType(t, thingID, "agent")
}

// IssueEnrollmentTokenOfType pre-creates a Thing row for thingID with the
// given thingType and generates a device token. Returns the plaintext token.
// Callers must use this variant when the Thing is not an agent (e.g.
// "compliance-proxy" or "ai-gateway") so broadcast routing and typed config
// updates target the correct Thing type.
//
// The Thing row and any config_change_event rows tagged with E2EActorID are
// cleaned up by a t.Cleanup hook.
func (h *Harness) IssueEnrollmentTokenOfType(t *testing.T, thingID, thingType string) string {
	t.Helper()

	ctx := context.Background()

	// Ensure the Thing row exists. Auth type is "bearer" (schema constraint
	// allows bearer/mtls/apikey); the device token is stored separately in
	// thing.metadata.deviceTokenHash. If the row already exists from a
	// previous run, UpsertThingEnrollment merges/updates it.
	if err := h.st.RegistryStore().UpsertThingEnrollment(ctx, store.UpsertThingParams{
		ID:           thingID,
		Type:         thingType,
		Name:         thingID,
		AuthType:     "bearer",
		ConnProtocol: "websocket",
		Status:       "offline",
	}); err != nil {
		t.Fatalf("IssueEnrollmentTokenOfType: upsert thing: %v", err)
	}

	plaintext, hashed, err := agentca.GenerateDeviceToken()
	if err != nil {
		t.Fatalf("IssueEnrollmentTokenOfType: generate token: %v", err)
	}

	if err := h.st.RegistryStore().StoreDeviceTokenHash(ctx, thingID, hashed); err != nil {
		t.Fatalf("IssueEnrollmentTokenOfType: store hash: %v", err)
	}

	t.Cleanup(func() {
		// Best-effort cleanup: remove the test thing row so subsequent runs
		// start from a clean state.
		cleanCtx := context.Background()
		_, _ = h.pool.Exec(cleanCtx, `DELETE FROM thing WHERE id = $1`, thingID)
		_, _ = h.pool.Exec(cleanCtx, `DELETE FROM config_change_event WHERE actor_id = $1`, E2EActorID)
	})

	return plaintext
}

// ServiceToken returns the internal service token the harness uses for the
// /api/hub/* ServiceAuth middleware. Tests that exercise the CP→Hub notify
// path over HTTP must attach this as `Authorization: Bearer <token>`.
func (h *Harness) ServiceToken() string {
	return testServiceToken
}

// Pool returns the underlying pgxpool so tests can run raw SQL for fixture
// setup and assertions.
func (h *Harness) Pool() *pgxpool.Pool { return h.pool }

// Raiser returns the alerting.Raiser built when WithAlerting() was passed.
// Returns nil when alerting was not requested.
func (h *Harness) Raiser() *alerting.Raiser { return h.raiser }

// AlertStore returns the alerting.Store built when WithAlerting() was passed.
// Returns nil when alerting was not requested.
func (h *Harness) AlertStore() *alerting.Store { return h.alertStore }

// These mirror the shims in cmd/nexus-hub/main.go; they live here because
// main is package main and cannot be imported.

type harnessSenderRegAdapter struct{ r *senders.Registry }

func (a harnessSenderRegAdapter) Get(channelType string) (alerting.Sender, error) {
	s, err := a.r.Get(channelType)
	if err != nil {
		return nil, err
	}
	return harnessSenderShim{s}, nil
}

type harnessSenderShim struct{ s senders.Sender }

func (w harnessSenderShim) Send(ctx context.Context, ch alerting.Channel, a alerting.Alert) (int, error) {
	return w.s.Send(ctx, ch, a)
}

// harnessRulesRegAdapter bridges *rules.Registry → alerting.RuleRegistry.
type harnessRulesRegAdapter struct{ r *rules.Registry }

func (a harnessRulesRegAdapter) Lookup(id string) (alerting.RuleDefault, bool) {
	d, ok := a.r.Lookup(id)
	if !ok {
		return alerting.RuleDefault{}, false
	}
	return alerting.RuleDefault{
		ID:              d.ID,
		DisplayName:     d.DisplayName,
		DefaultSeverity: d.DefaultSeverity,
		RequiresAck:     d.RequiresAck,
		Enabled:         d.Enabled,
		CooldownSec:     d.CooldownSec,
		Params:          d.Params,
		ParamsSchema:    d.ParamsSchema,
	}, true
}
