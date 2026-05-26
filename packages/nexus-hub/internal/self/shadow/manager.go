// Package selfshadow consumes Hub's own row in the thing table via
// PostgreSQL LISTEN/NOTIFY.
//
// Unlike every other Thing in the platform, Hub does not run thingclient
// pointed at itself — it IS the WebSocket broker. To still participate
// in the same UI-driven, audited config flow as ai-gateway, agent, etc.,
// this manager LISTENs on a dedicated channel that Hub writers emit
// NOTIFY on whenever thing.desired changes. On a matching notification
// (filtered by Hub's instanceID), the manager re-reads the row,
// dispatches per-key ReloadHandlers, and writes back thing.reported so
// the Configuration tab's inSync column converges.
//
// Why LISTEN/NOTIFY: it's the only mechanism Hub already has in-process
// (single connection to the same Postgres) that fans out atomically to
// every Hub replica. A polling loop would either burn CPU or add
// latency; an in-process callback only fires on the writer Hub and
// strands the other replicas with stale config.
package selfshadow

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// Channel is the postgres LISTEN channel name. Writers MUST emit
// pg_notify(Channel, thing_id) in the same transaction that updates
// thing.desired, so listeners only observe committed state. The
// canonical name lives in store.ConfigChangedChannel so writer and
// reader cannot drift.
var Channel = store.ConfigChangedChannel

// reacquireBackoff is the wait between attempts to re-acquire a pooled
// connection after the listener loop loses its conn (e.g. server
// restart, network blip). Declared as a var (not const) so unit tests
// can compress it to keep test runtime bounded; production code never
// mutates it. Same pattern as the test-only seams in auth/apikey.go.
var reacquireBackoff = time.Second

// ReloadHandler is invoked when the desired state for a registered key
// on the Hub's own row changes. state is the raw JSON value of
// thing.desired[key], passed through unmodified so handlers can
// json.Unmarshal into their own struct.
type ReloadHandler interface {
	Apply(ctx context.Context, state json.RawMessage) error
}

// HandlerFunc adapts a plain function to the ReloadHandler interface.
type HandlerFunc func(ctx context.Context, state json.RawMessage) error

// Apply implements ReloadHandler.
func (f HandlerFunc) Apply(ctx context.Context, state json.RawMessage) error {
	return f(ctx, state)
}

// shadowReader is the read seam the Manager uses to fetch the Hub's
// own row and write back the reported state. *store.Store satisfies it
// in production; tests inject a fake without touching Postgres.
type shadowReader interface {
	GetThing(ctx context.Context, id string) (*store.Thing, error)
	UpdateShadowReport(ctx context.Context, id string, reported map[string]any, reportedVer int64, outcomes map[string]store.ReportedKeyOutcome) error
}

// notifier is the LISTEN seam the Manager uses to acquire a dedicated
// pooled connection. *pgxpool.Pool satisfies it; tests can supply a
// no-op stub so unit tests don't need a real Postgres listener.
type notifier interface {
	Acquire(ctx context.Context) (pooledListener, error)
}

// pooledListener wraps the operations the listen loop performs on a
// pooled connection. Decoupling from *pgxpool.Conn lets tests bypass
// LISTEN entirely while still exercising applyAll.
type pooledListener interface {
	Exec(ctx context.Context, sql string) error
	WaitForNotification(ctx context.Context) (*pgconnNotification, error)
	Release()
}

// pgconnNotification is a lightweight projection of *pgconn.Notification
// so tests don't need to import pgconn. We only consume Channel and
// Payload.
type pgconnNotification struct {
	Channel string
	Payload string
}

// Manager subscribes to LISTEN config_changed, filters notifications by
// instanceID, and dispatches registered reload handlers.
type Manager struct {
	instanceID string
	notifier   notifier
	store      shadowReader
	logger     *slog.Logger

	mu       sync.Mutex
	handlers map[string]ReloadHandler

	// outcomes is the per-key apply-outcome ledger, mirrored on every
	// successful applyAll into thing.reported_outcomes. Same semantics as
	// thingclient.OutcomeTracker on the data-plane side: AppliedAt /
	// AppliedVersion track the LAST KNOWN successful apply (preserved
	// across failures); ApplyError carries the most recent failure and is
	// cleared on the next success. Process-scoped — reset on Hub restart
	// when the Manager is reconstructed.
	outcomesMu sync.Mutex
	outcomes   map[string]store.ReportedKeyOutcome

	appliedVer atomic.Int64
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

// recordOutcome updates the per-key ledger after a single Apply attempt.
// On success the AppliedAt / AppliedVersion advance and any prior error
// is cleared. On failure ApplyError is set with the message + timestamp
// while AppliedAt / AppliedVersion are preserved from the previous
// successful tick — operators see "still serving vN, latest attempt vN+1
// failed" rather than losing track of what the Hub last applied.
func (m *Manager) recordOutcome(key string, ver int64, err error) {
	if key == "" {
		return
	}
	m.outcomesMu.Lock()
	defer m.outcomesMu.Unlock()
	if m.outcomes == nil {
		m.outcomes = map[string]store.ReportedKeyOutcome{}
	}
	prev := m.outcomes[key]
	now := time.Now().UTC()
	if err == nil {
		prev.AppliedAt = &now
		v := ver
		prev.AppliedVersion = &v
		prev.ApplyError = nil
	} else {
		prev.ApplyError = &store.ApplyErrorV{Message: err.Error(), At: now}
	}
	m.outcomes[key] = prev
}

// outcomesSnapshot returns a stable copy for serialisation. Returns an
// empty (non-nil) map so the wire shape stays `{}` rather than `null`.
func (m *Manager) outcomesSnapshot() map[string]store.ReportedKeyOutcome {
	m.outcomesMu.Lock()
	defer m.outcomesMu.Unlock()
	out := make(map[string]store.ReportedKeyOutcome, len(m.outcomes))
	for k, v := range m.outcomes {
		out[k] = v
	}
	return out
}

// New builds a Manager bound to the production *pgxpool.Pool and
// *store.Store. The instanceID MUST match the Hub row's `thing.id`
// (config.Hub.ID); only notifications whose payload equals this string
// are dispatched.
func New(instanceID string, pool *pgxpool.Pool, st *store.Store, logger *slog.Logger) *Manager {
	var sr shadowReader
	if st != nil {
		sr = st.RegistryStore()
	}
	return newManager(instanceID, &poolAdapter{pool: pool}, sr, logger)
}

// newManager is the test-friendly constructor that accepts pre-wrapped
// seams. Exported tests call this with fakes.
func newManager(instanceID string, n notifier, st shadowReader, logger *slog.Logger) *Manager {
	return &Manager{
		instanceID: instanceID,
		notifier:   n,
		store:      st,
		logger:     logger.With("component", "selfshadow"),
		handlers:   map[string]ReloadHandler{},
	}
}

// Register associates a ReloadHandler with a config_key. Idempotent
// (second registration replaces the previous handler). Safe to call
// before or after Start.
func (m *Manager) Register(key string, h ReloadHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers[key] = h
}

// Start runs applyAll once synchronously (so any desired-state changes
// that happened while Hub was down are picked up), then launches the
// LISTEN loop in a goroutine. Stop must be called to drain the
// goroutine on shutdown.
func (m *Manager) Start(ctx context.Context) error {
	if err := m.applyAll(ctx); err != nil {
		// Don't fail Start; we want Hub to come up even if the row
		// isn't ready yet. The listener will catch up on first NOTIFY.
		m.logger.Warn("selfshadow initial applyAll failed", "error", err)
	}

	listenCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.wg.Add(1)
	go m.listen(listenCtx)

	m.logger.Info("selfshadow started", "instanceId", m.instanceID, "channel", Channel)
	return nil
}

// Stop cancels the listener loop and waits for it to exit. Safe to
// call multiple times.
func (m *Manager) Stop(_ context.Context) error {
	if m.cancel == nil {
		return nil
	}
	m.cancel()
	m.cancel = nil
	m.wg.Wait()
	m.logger.Info("selfshadow stopped", "instanceId", m.instanceID)
	return nil
}

// listen is the long-running LISTEN loop. It acquires a pooled
// connection, issues LISTEN, and then blocks on WaitForNotification.
// On disconnect or error it logs, releases the conn, and re-acquires.
// Notifications whose payload != instanceID are dropped early.
func (m *Manager) listen(ctx context.Context) {
	defer m.wg.Done()
	for {
		if ctx.Err() != nil {
			return
		}
		conn, err := m.notifier.Acquire(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.logger.Warn("selfshadow acquire conn", "error", err)
			if !sleepCtx(ctx, reacquireBackoff) {
				return
			}
			continue
		}
		// Tight inner scope so we Release on every exit path.
		func() {
			defer conn.Release()
			if err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
				m.logger.Warn("selfshadow LISTEN failed", "error", err)
				return
			}
			for {
				notif, err := conn.WaitForNotification(ctx)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					// Network blip / server restart — bail out and let
					// the outer loop reacquire.
					m.logger.Warn("selfshadow wait notification", "error", err)
					return
				}
				if notif == nil || notif.Channel != Channel {
					continue
				}
				if notif.Payload != m.instanceID {
					// Notification for another Thing — drop. Other Hub
					// replicas are independently filtering on their own
					// instanceID; this is the per-replica
					// fan-out point.
					continue
				}
				if err := m.applyAll(ctx); err != nil {
					m.logger.Warn("selfshadow applyAll on notify",
						"instanceId", m.instanceID, "error", err)
				}
			}
		}()
	}
}

// applyAll reads the Hub's own row, walks every key present in
// thing.desired AND registered in handlers, and dispatches Apply.
// Versions older than the previously applied desired_ver are skipped
// (idempotent re-fires from duplicate NOTIFY events do no work).
// A panic inside any single handler is recovered and logged; siblings
// in the same dispatch round still run.
func (m *Manager) applyAll(ctx context.Context) error {
	thing, err := m.store.GetThing(ctx, m.instanceID)
	if err != nil {
		return fmt.Errorf("get thing: %w", err)
	}
	if thing.DesiredVer <= m.appliedVer.Load() {
		return nil
	}

	// Snapshot the handler map under the lock so handlers that re-Register
	// from inside Apply don't deadlock or mutate mid-dispatch.
	m.mu.Lock()
	keys := make([]string, 0, len(m.handlers))
	handlersCopy := make(map[string]ReloadHandler, len(m.handlers))
	for k, h := range m.handlers {
		keys = append(keys, k)
		handlersCopy[k] = h
	}
	m.mu.Unlock()

	reported := map[string]any{}
	if thing.Reported != nil {
		for k, v := range thing.Reported {
			reported[k] = v
		}
	}

	for _, key := range keys {
		raw, ok := thing.Desired[key]
		if !ok {
			continue
		}
		stateJSON, err := json.Marshal(raw)
		if err != nil {
			m.logger.Warn("selfshadow marshal desired key",
				"instanceId", m.instanceID, "key", key, "error", err)
			m.recordOutcome(key, thing.DesiredVer, err)
			continue
		}
		h := handlersCopy[key]
		applyErr := m.dispatchOne(ctx, key, h, stateJSON)
		m.recordOutcome(key, thing.DesiredVer, applyErr)
		// Echo the desired value into reported so the inSync diff in the
		// Configuration tab converges once Apply has run. We mirror the
		// raw desired value rather than a handler-returned value because
		// the wire shape is whatever Hub pushed; per-key normalization
		// belongs in the handler.
		reported[key] = raw
	}

	if err := m.store.UpdateShadowReport(ctx, m.instanceID, reported, thing.DesiredVer, m.outcomesSnapshot()); err != nil {
		return fmt.Errorf("update shadow report: %w", err)
	}
	m.appliedVer.Store(thing.DesiredVer)
	m.logger.Info("selfshadow applied",
		"instanceId", m.instanceID, "desiredVer", thing.DesiredVer, "keys", len(keys))
	return nil
}

// dispatchOne wraps Apply with a panic recover so one bad handler does
// not abort the round. The Apply error (if any) is returned so the
// caller can mirror it into the outcome ledger; panics are converted to
// errors via a named return so the ledger still records a failure
// (otherwise a panicking handler would look "applied successfully" to
// the operator).
//
// There is no retry on the failing payload: the next NOTIFY will trigger
// another applyAll, which is a no-op until desired_ver advances.
func (m *Manager) dispatchOne(ctx context.Context, key string, h ReloadHandler, state json.RawMessage) (applyErr error) {
	defer func() {
		if r := recover(); r != nil {
			m.logger.Error("selfshadow handler panic",
				"instanceId", m.instanceID, "key", key, "panic", r)
			applyErr = fmt.Errorf("handler panic: %v", r)
		}
	}()
	if h == nil {
		return nil
	}
	if err := h.Apply(ctx, state); err != nil {
		m.logger.Warn("selfshadow handler error",
			"instanceId", m.instanceID, "key", key, "error", err)
		return err
	}
	return nil
}

// sleepCtx is a context-aware sleep. Returns false if ctx was cancelled
// during the sleep so callers can exit cleanly.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// pooledConn is the minimum *pgxpool.Conn surface poolConnAdapter
// depends on. Decoupling from the concrete pool conn type lets unit
// tests inject a fake that returns canned pgconn notifications and
// errors without standing up a real Postgres listener. Same seam
// pattern used by thingmgr.PgxPool and the cp/store pgxmock harness.
type pooledConn interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	WaitForNotification(ctx context.Context) (*pgconn.Notification, error)
	Release()
}

// pgxpoolConnWrapper adapts *pgxpool.Conn to the pooledConn interface.
// The wrapper exists because *pgxpool.Conn exposes WaitForNotification
// indirectly via Conn() (the underlying *pgx.Conn) rather than on the
// pool conn itself; flattening that hop here keeps the seam uniform.
type pgxpoolConnWrapper struct {
	conn *pgxpool.Conn
}

// Exec implements pooledConn.
func (w *pgxpoolConnWrapper) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	return w.conn.Exec(ctx, sql, arguments...)
}

// WaitForNotification implements pooledConn.
func (w *pgxpoolConnWrapper) WaitForNotification(ctx context.Context) (*pgconn.Notification, error) {
	return w.conn.Conn().WaitForNotification(ctx)
}

// Release implements pooledConn.
func (w *pgxpoolConnWrapper) Release() { w.conn.Release() }

// poolAdapter wraps *pgxpool.Pool to satisfy the notifier seam.
type poolAdapter struct {
	pool *pgxpool.Pool
}

// Acquire implements notifier.
func (a *poolAdapter) Acquire(ctx context.Context) (pooledListener, error) {
	conn, err := a.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	return &poolConnAdapter{conn: &pgxpoolConnWrapper{conn: conn}}, nil
}

// poolConnAdapter wraps a pooledConn so the listener loop can speak
// to it through the pooledListener interface. In production the
// underlying pooledConn is a *pgxpoolConnWrapper around *pgxpool.Conn;
// in unit tests it's a fake conn supplying canned notifications and
// errors.
type poolConnAdapter struct {
	conn pooledConn
}

// Exec implements pooledListener.
func (c *poolConnAdapter) Exec(ctx context.Context, sql string) error {
	_, err := c.conn.Exec(ctx, sql)
	return err
}

// WaitForNotification implements pooledListener.
func (c *poolConnAdapter) WaitForNotification(ctx context.Context) (*pgconnNotification, error) {
	n, err := c.conn.WaitForNotification(ctx)
	if err != nil {
		return nil, err
	}
	return &pgconnNotification{Channel: n.Channel, Payload: n.Payload}, nil
}

// Release implements pooledListener.
func (c *poolConnAdapter) Release() { c.conn.Release() }

// Compile-time assertions: the production wiring satisfies the seams.
var (
	_ notifier       = (*poolAdapter)(nil)
	_ pooledListener = (*poolConnAdapter)(nil)
	_ pooledConn     = (*pgxpoolConnWrapper)(nil)
)
