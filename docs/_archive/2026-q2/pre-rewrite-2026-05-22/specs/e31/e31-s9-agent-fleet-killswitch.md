# E31 S9 — Agent fleet kill switch

**Epic:** 31
**Story:** 9
**Status:** Draft — 2026-04-27
**Requirements:** inline (gap A from e31-s7 introspection coverage matrix)

## User Story

As a compliance operator, I want to toggle the fleet-wide kill switch in the
CP UI Infrastructure → Kill Switch page and have **agents** stop intercepting
TLS traffic immediately, the same way compliance-proxy stops bumping today.
Without this, the kill switch only stops compliance-proxy MITM; agents on
end-user machines keep intercepting until each is restarted with new
config. That defeats the "kill switch" name.

## Background

The `killswitch` thingclient config_key is already produced by Hub's
`thingmgr.UpdateConfig` for any `thing_type` listed in the catalog. As of
2026-04-27 only compliance-proxy is registered as a consumer
(`packages/compliance-proxy/cmd/compliance-proxy/main.go` case
`"killswitch":` at the OnConfigChanged switch). Agent's
`configsync.Manager` has no `killswitch` applier (`packages/agent/core/sync/configsync/manager.go`
dispatch table covers `exemptions / policy_rules / interception_domains /
hook_config / payload_capture` only) so the key is logged as "unknown" and
silently dropped.

Surfaced by e31-s7 introspection as **gap A**.

## Scope

### In

- New package `packages/agent/core/control/killswitch/` with a `Switch` type
  mirroring `compliance-proxy/internal/runtimeapi/KillSwitch`'s essential
  surface (Toggle, IsEnabled, Snapshot, ApplyShadowState). Trimmed —
  agent does not need ForceClose (no in-process tunnel registry that the
  kill switch can drain) or break-glass HTTP API (CP→Hub→agent shadow
  push is the only mutator; no per-machine runtime override).
- `configsync.Manager` gains a `KillSwitch ShadowApplier` slot (Cat A
  inline, like `exemptions`) and dispatches the `killswitch` key.
- `agent/cmd/agent/main.go` constructs the Switch, wires it into the
  Manager and into `connectionBridge.killSwitch`.
- `connectionBridge.HandleConnection` returns
  `platform.DecisionPassthrough` immediately when `killSwitch.IsEnabled()`
  reports `false` (i.e. the switch is **engaged**), short-circuiting the
  policy engine + hook pipeline so no TLS bump or body buffering happens.
- A diagnostic log line at INFO when the switch flips, plus a structured
  audit entry on each engaged passthrough decision (rate-limited to one
  per host per minute to avoid log flooding).
- e31-s7 introspection source for the agent's switch (registered when
  e31-s12 lands; the SDD adds the Source struct + a TODO comment in
  main.go pointing to the registration call).

### Out

- **No new Hub or DB schema work.** The `killswitch` config_key is already
  produced by Hub for the `agent` thing_type via the existing
  `thingmgr.UpdateConfig` path (no per-thing-type allowlist). CP UI's
  current Infrastructure → Kill Switch page already lists agents as
  affected nodes; the click-to-toggle wiring on the CP side does not
  change.
- **No metrics endpoint changes.** Agent does not expose `/metrics`
  externally; the Switch tracks state internally and surfaces via
  introspection only (in e31-s12). A `killswitch.toggled_total` counter is
  added to the agent ops registry so existing telemetry pipelines pick it
  up without further wiring.
- **No platform shim changes.** The bypass is at `HandleConnection`, before
  the platform decides whether to hand the connection to the Go MITM
  relay. The shim already supports `DecisionPassthrough` (used by
  exemption-store hits today).

## Tasks

### T1. Agent killswitch package

`packages/agent/core/control/killswitch/killswitch.go`:

- Type `Switch` with internal `enabled atomic.Bool`, `mu sync.Mutex`,
  `lastChanged time.Time`, `changedBy string`, `*slog.Logger`.
- `New(logger *slog.Logger) *Switch` — defaults `enabled=true` (TLS bump
  on, kill switch disengaged).
- `IsEnabled() bool` — returns the atomic load. `true` means bump is
  allowed (normal operation), `false` means engaged.
- `Toggle(enabled bool, changedBy string) Snapshot` — sets state, records
  changed-by + timestamp, logs at INFO, returns the new snapshot.
- `Snapshot() Snapshot` — returns `{enabled, last_changed, changed_by}`
  for introspection.
- `ApplyShadowState(ctx, raw json.RawMessage) error` — decodes
  `configtypes.Killswitch`, calls `Toggle(state.Enabled, "hub-shadow")`
  if it differs from the current value (no-op when redundant). Empty /
  null payload is a no-op (per `ShadowApplier` contract — never a "clear
  everything" operation).

Unit tests covering: default state, toggle round-trip, redundant apply
no-op, malformed JSON returns error, concurrent Toggle/IsEnabled.

### T2. configsync.Manager wiring

`packages/agent/core/sync/configsync/manager.go`:

- Add `KillSwitch ShadowApplier` to `ManagerConfig`.
- Add `killSwitch ShadowApplier` field to `Manager`.
- Wire it through `NewManager`.
- Add `"killswitch": {applier: m.killSwitch, needsPull: false}` to the
  dispatch table (Cat A — state arrives inline).

Test in `manager_test.go`: a Toggle delivered through `ApplyDesired`
reaches the registered `KillSwitch` applier and is reported back.

### T3. main.go bring-up

`packages/agent/cmd/agent/main.go`:

- Construct `killSwitch := killswitch.New(logger)` after `exemptionStore`.
- Pass `KillSwitch: killSwitch` to `configsync.NewManager`.
- Add `killSwitch *killswitch.Switch` to `connectionBridge` struct.
- Pass it through the `connectionBridge` constructor at the platform-shim
  bring-up site.

### T4. connectionBridge gate

`HandleConnection` (currently lines 1029-1079) gains an early return:

```go
if b.killSwitch != nil && !b.killSwitch.IsEnabled() {
    slog.Info("kill switch engaged, passing through",
        "host", conn.DstHost, "flowId", conn.FlowID)
    b.killSwitchAuditOnce(conn.DstHost) // rate-limited audit emit
    return platform.DecisionPassthrough
}
```

Audit emit uses a per-host minute window (sync.Map keyed by host →
last-emit time) so a popular host doesn't flood the audit pipeline.

### T5. Tests

- killswitch_test.go: per T1.
- configsync manager_test.go: per T2.
- connection bridge: a unit test asserting that when `killSwitch.IsEnabled()`
  returns false, `HandleConnection` returns `DecisionPassthrough` without
  consulting the policy engine.

### T6. Verify

- `go test -race -count=1 ./packages/agent/...` PASS.
- Hand verification (deferred to post-restart): toggle kill switch from
  CP UI → Hub broadcasts to compliance-proxy AND agent → both stop
  bumping. Agent's local diag log shows the toggle and subsequent
  passthrough decisions.

## Acceptance Criteria

1. Agent receives the `killswitch` config_key from Hub shadow without
   logging "unknown shadow config key".
2. When the operator engages the kill switch in CP UI, both
   compliance-proxy and agent stop intercepting within one Hub broadcast
   round-trip (~1s).
3. With kill switch engaged, agent's `connectionBridge.HandleConnection`
   returns `DecisionPassthrough` for every host, regardless of policy
   engine result. Hook pipelines are not invoked.
4. Disengaging the switch restores normal interception immediately.
5. `go test -race -count=1 ./packages/agent/...` PASS, including new
   killswitch_test.go and the configsync.Manager dispatch test.
6. The introspection registration site in main.go has a TODO comment with
   the exact `Register(...)` call to be added in e31-s12 (so the agent
   local UI surfaces the same shape as compliance-proxy/ai-gateway).

## Risks

- **Audit-log flood.** A user behind a busy network with kill switch
  engaged could generate audit events for every TCP connection. The
  one-per-host-per-minute rate-limit caps fan-out; a malicious user can
  still bypass by varying hostnames, but they could already do that
  through normal traffic.
- **Race between Toggle and HandleConnection.** Resolved by `atomic.Bool`
  on the IsEnabled hot path; Toggle is rare (admin action). No locks held
  during connection handling.
- **Stale state across restart.** Agent re-fetches the shadow on each
  thingclient connect; the Hub side stores the canonical desired state.
  An offline agent will respect the last applied state until reconnect,
  which may be stale — acceptable for the kill switch use case (operators
  must accept that an unreachable agent cannot be stopped remotely).
