# E42-S2 — Hub Self-Shadow (SDD)

## User story

As an operator, I can change `nexus-hub`'s OTEL configuration from the
Control Plane UI (via either a template edit or a per-Hub override) and
have the running Hub apply the change within seconds without me
restarting the binary or editing YAML on the host.

## Background

`nexus-hub` is registered as a Thing by `packages/nexus-hub/internal/self/reg`,
which performs a direct DB upsert and runs a heartbeat loop. Hub does
not run `thingclient` pointed at itself — it IS the WebSocket broker,
and a self-WebSocket loop would be circular.

To close the loop, this story adds a self-shadow consumer that learns of
its own desired-state changes via PostgreSQL `LISTEN`/`NOTIFY` and
dispatches in-process callbacks. PR1 wires the mechanism end-to-end
through a single key (`observability` → OTEL Reconfigure); adding more
keys is a follow-up that touches only this manager's handler registry.

## Tasks

1. **New package `packages/nexus-hub/internal/self/shadow/`.**
   - `Manager` struct with fields: `instanceID string`, `pool *pgxpool.Pool`,
     `store thingShadowReader`, `logger *slog.Logger`,
     `handlers map[string]ReloadHandler`, `lastVer atomic.Int64`,
     `cancel context.CancelFunc`, `wg sync.WaitGroup`.
   - `ReloadHandler` interface: `Apply(ctx context.Context, state json.RawMessage) error`.
   - `New(instanceID string, pool *pgxpool.Pool, store *store.Store, logger *slog.Logger) *Manager`.
   - `Register(key string, handler ReloadHandler)` — registers a reload
     callback. Idempotent; second registration of the same key replaces
     the previous handler.
   - `Start(ctx context.Context) error` — opens a dedicated `pgx.Conn`
     from the pool (LISTEN must not share with a request-serving conn),
     issues `LISTEN config_changed`, runs a `WaitForNotification` loop
     in a goroutine, calls `applyAll` once on start so any state changes
     that happened between the last process exit and now are picked up.
   - `Stop(ctx context.Context) error` — cancels the goroutine, closes
     the LISTEN connection, waits for the goroutine to exit.
   - On every NOTIFY: if the payload (a `thing_id` string) matches
     `instanceID`, call `applyAll`. Drop notifications for other IDs.
   - `applyAll(ctx)`: read `thing.desired` and `thing.desired_ver` for
     `instanceID`; if `desired_ver <= lastVer`, no-op; otherwise unmarshal
     the desired map and for every key present in both the desired map
     AND `handlers`, call `handler.Apply(ctx, state)`. Track applied
     `desired_ver`; on success, update `thing.reported` and
     `thing.reported_ver` so the UI's inSync logic continues to work.
     A panic inside a handler is recovered, logged, and does NOT abort
     the remaining handlers in the same dispatch round.
2. **NOTIFY emission from shadow write paths.** Inside
   `packages/nexus-hub/internal/storage/store/thing.go` (and any other path that
   writes `thing.desired`), wrap the existing `UPDATE` so that after a
   successful commit the same transaction has issued `pg_notify` with the
   thing_id. The new helper is `notifyConfigChanged(ctx pgx.Tx, thingID string) error`;
   each write path calls it before commit so rollback also discards the
   notification. Audit the following call sites and patch:
   - `thingmgr.UpdateDesiredForType` (template push fan-out)
   - `store.SetThingOverride` / `store.ClearThingOverride` (per-Thing override CRUD)
   - `store.UpdateThingDesired` (direct admin shadow write, if used)
3. **Reload handler for `observability`.** Adapt the existing OTEL
   reconfiguration logic in `cmd/nexus-hub/main.go` into a function
   `reconfigureOTEL(cfg observabilityConfig) error` that takes a struct
   matching the existing fields (`enabled`, `endpoint`, `serviceName`,
   `samplingRate`). Implement a `ReloadHandler` that unmarshals the
   desired-state JSON into that struct, validates basic shape, and
   invokes `reconfigureOTEL`. Wire this handler into
   `selfshadow.Manager.Register("observability", obsHandler)` inside
   `cmd/nexus-hub/main.go` after selfreg has registered the Hub row.
4. **Tests** in `selfshadow_test.go`:
   - `TestManager_AppliesOnStart` — pre-seed a fake store with desired
     `{"observability": {...}}` and `desired_ver=5`, start the manager,
     assert the handler was called once with the JSON and that
     `lastVer == 5`.
   - `TestManager_IgnoresOlderVersion` — seed `desired_ver=3` after
     manager has already applied `desired_ver=5`, send NOTIFY, assert
     handler NOT called.
   - `TestManager_FilterByInstanceID` — send NOTIFY for a different
     `thing_id`, assert handler NOT called.
   - `TestManager_HandlerPanicRecovered` — handler that panics; assert
     manager logs at Error and remains running (a subsequent NOTIFY
     still triggers another applyAll).
   The test uses a fake `thingShadowReader` and bypasses real `pgx`
   listener; the NOTIFY plumbing is tested separately as an integration
   smoke when the migration is applied and Hub is restarted.
5. **Wiring** in `cmd/nexus-hub/main.go`:
   - After `selfreg.Register`, instantiate `selfshadow.New(cfg.Hub.ID, dbPool, store, logger)`.
   - Call `mgr.Register("observability", obsHandler)`.
   - Call `mgr.Start(ctx)` and defer `mgr.Stop(shutdownCtx)`.
   - Stop ordering: `selfshadow.Stop` → `selfreg.Deregister` →
     `dbPool.Close`. The LISTEN connection must close before the pool
     drains.

## Out of scope

- Adding more shadow keys to Hub (`log_level`, `scheduler_intervals`,
  `scheduler_retention`, `consumers_config` — PR2 of E42).
- Cluster fan-out from one Hub admin write to every Hub row (`type='nexus-hub'`).
  PR1 ships per-instance only; production multi-Hub support is PR2 design.
- Replacing the existing `selfreg` heartbeat path. The two subsystems
  cooperate: selfreg owns Identity (Hub row exists, status, last_seen);
  selfshadow owns Convergence (desired → applied).
- A schema migration for `thing_config_template` — covered by E42-S1.

## Acceptance criteria

- [ ] `go test ./packages/nexus-hub/internal/self/shadow/...` passes with
      `-race -count=1`.
- [ ] On a clean dev startup, the Hub logs include
      `selfshadow: applied desired_ver=N for hub-dev` exactly once.
- [ ] Setting an override on `hub-dev / observability` via
      `POST /api/admin/nodes/hub-dev/overrides` produces:
      (a) a `NOTIFY` observed by `pg_notify('config_changed', 'hub-dev')`,
      (b) the Hub's OTEL exporter being reconfigured within 2 seconds,
      (c) `thing.reported` updated to include the applied observability
      payload on the next applyAll cycle.
- [ ] Killing the Hub process while an unsynced desired change is
      pending and starting it again causes the change to be applied on
      first run (the `applyAll` on `Start` path).
- [ ] No regression: `selfreg` heartbeat still runs every 15 s and the
      Hub row continues to receive `last_seen_at` updates.
