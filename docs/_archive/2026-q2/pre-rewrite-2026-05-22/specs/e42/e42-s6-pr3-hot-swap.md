# E42-S6 — PR3 service hot-swap completion (SDD)

## User story

As an SRE I update the compliance-proxy source-IP allowlist, swap its
streaming mode + per-hook timeouts, and shift the agent's heartbeat /
audit drain cadence — all from the Control Plane UI, all without
restarting a single binary.

## Tasks

### compliance-proxy `access_control` (source-IP allowlist)
- `access.Checker.ipAllowlist` migrates from a plain pointer to
  `atomic.Pointer[IPAllowlist]`.
- New `SwapSourceIPAllowlist(cidrs []string, logger)` method validates
  the new CIDR list via `NewIPAllowlist`; on parse failure it logs a
  warning and keeps the previous allowlist active (avoiding operator
  lockout).
- main.go's thingclient `OnConfigChanged` switch gains
  `case "access_control":` that decodes
  `{"sourceIpAllowlist": [...]}` and calls SwapSourceIPAllowlist.

### compliance-proxy `compliance_streaming`
- `ProxyServer` collapses `streamingMode`, `perHookTimeout`,
  `totalTimeout` into a single `streamingTuningSnapshot` held in
  `atomic.Pointer`. Each bumped CONNECT loads the snapshot once so the
  hot path stays branch-free.
- New `ProxyServer.SetStreamingTuning(mode, perHookTimeout,
  totalTimeout)` merges non-zero fields into the current snapshot
  before atomic swap. Empty / zero fields preserve the previous
  value so a partial shadow payload behaves intuitively.
- main.go's `OnConfigChanged` gains
  `case "compliance_streaming":` that decodes
  `{"mode": "live|buffer|passthrough", "perHookTimeoutMs": ..., "totalTimeoutMs": ...}`
  and calls SetStreamingTuning.

### agent `timing_intervals`
- `shared/thingclient.Client` gains:
  - `heartbeatIntervalNS atomic.Int64`
  - `heartbeatKick atomic.Pointer[chan struct{}]`
  - `SetHeartbeatInterval(d)` updates both
  - `CurrentHeartbeatInterval()` accessor
- Both `runMetricsTicker` and the `runHTTPFallback` heartbeat loop
  rewire from `time.NewTicker(cfg.HeartbeatInterval)` to a
  `time.NewTimer(CurrentHeartbeatInterval())` that re-arms every
  iteration. A `select` branch on the kick channel breaks the in-flight
  wait so SetHeartbeatInterval takes effect immediately, not at the
  next natural tick.
- agent main.go declares `drainIntervalNS atomic.Int64` and
  `drainKickCh chan struct{}` BEFORE building the configsync manager.
- New supervisor goroutine wraps `auditQueue.DrainLoop` in a
  child-context lifecycle: every iteration spawns DrainLoop with the
  current interval, then `select { <-ctx.Done() | <-drainKickCh }`.
  On kick the child cancels (DrainLoop's deferred final drain runs)
  and the supervisor re-loops with the new interval.
- `configsync.ManagerConfig` adds a `TimingIntervals ShadowApplier`
  slot dispatched on the `timing_intervals` key (Cat A inline).
- The applier in agent main.go decodes
  `{"heartbeatIntervalSec": ..., "auditDrainIntervalSec": ...}` and
  calls `tc.SetHeartbeatInterval` + updates `drainIntervalNS` +
  signals `drainKickCh`.

### Migration — 3 new template rows
- `compliance-proxy / access_control` — default state
  `{"sourceIpAllowlist": []}` (empty list = allow all sources, matching
  the YAML default behavior).
- `compliance-proxy / compliance_streaming` — default state
  `{"mode": "live", "perHookTimeoutMs": 0, "totalTimeoutMs": 0}` (zero
  ms means "use service-internal default"; admins set non-zero to
  override).
- `agent / timing_intervals` — default state
  `{"heartbeatIntervalSec": 0, "auditDrainIntervalSec": 0}` (zero =
  use YAML default; admins set non-zero to override).
- Existing things' desired backfilled per the established CASE-when-
  absent + per-type desired_ver bump pattern.

## Out of scope

- ai-gateway `cache_config`, `cors_config`, `http_client_timeouts.*` —
  cache layer / middleware chain / per-client http.Client all require
  rebuilds the existing surfaces do not support cleanly. Belongs to a
  future epic with its own architecture work.
- compliance-proxy `audit_config` / `siem_forwarder` — same shape:
  forwarder captures config at construction; clean hot-swap needs
  refactoring the writer pipeline.
- control-plane `ai_guard_timeout_sec` / `http_client_timeouts` — each
  client is a fully-built http.Client; refactor needed.

## Acceptance criteria

- [ ] `go test ./packages/compliance-proxy/internal/access/...
      ./packages/compliance-proxy/internal/proxy/...
      ./packages/shared/transport/thingclient/...
      ./packages/agent/core/sync/configsync/... -race -count=1` passes.
- [ ] Setting an override on
      `<compliance-proxy-id>/access_control = {"sourceIpAllowlist": ["10.0.0.0/8"]}`
      causes the next CONNECT from a non-allowlisted source to be
      rejected with `ErrIPDenied`; clearing the override returns to
      the YAML allowlist.
- [ ] Setting an override on
      `<compliance-proxy-id>/compliance_streaming = {"mode": "buffer"}`
      causes the next bumped CONNECT's resolveStreamingMode to return
      "buffer".
- [ ] Setting an override on
      `<agent-id>/timing_intervals = {"heartbeatIntervalSec": 5}`
      causes the thingclient's next heartbeat to fire on a 5-second
      cadence instead of waiting out the previous 15 s window.
- [ ] `SELECT type, config_key FROM thing_config_template WHERE config_key IN ('access_control','compliance_streaming','timing_intervals');`
      returns 3 rows.
