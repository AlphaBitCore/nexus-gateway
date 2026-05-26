# E31 S11 — Agent dynamic OTEL config

**Epic:** 31
**Story:** 11
**Status:** Draft — 2026-04-27
**Requirements:** inline (gap D from e31-s7 introspection coverage matrix)

## Compliance review (must read first)

Today the agent reads OTEL settings from its local YAML at startup. Making
the OTEL endpoint dynamically reconfigurable from the operator console
means: an admin (or a compromised CP/Hub) could redirect the agent's
telemetry stream to an arbitrary endpoint. For Nexus's deployment posture
(large-enterprise on-prem, single-tenant per
`memory/project_product_positioning.md`), the operator console **already**
governs every other data-plane behavior on the agent (hooks, exemptions,
domains, payload capture). OTEL is no different in trust scope: the same
operator who could push a malicious hook can already redirect data via
many other means. **Conclusion: the residual risk does not justify
making OTEL the lone YAML-only knob.** Implementing the dynamic path.

If a future deployment requires hard isolation (regulated industry,
compliance officer different from CP admin), the operator can leave the
local YAML's OTEL block in place; the dynamic applier respects an empty
shadow payload as a no-op (per `ShadowApplier` contract), so as long as
no one pushes an `observability` config_key to that agent, behavior is
unchanged.

## User Story

As an operator changing the global observability target (OTLP endpoint,
sampling rate, enable/disable) from the CP UI, I want **agents** to pick
up the new setting without each user having to restart their agent —
the same way ai-gateway and compliance-proxy already do today.

## Background

`packages/agent/cmd/agent/main.go:220` initializes a
`shared/telemetry.SwappableTracerProvider` at startup from local
YAML-derived `cfg.Otel*` fields. The provider already supports
hot-swap via `tp.Reconfigure(cfg)`. The piece that's missing is the
shadow-config applier: agent's `configsync.Manager` has no
`observability` slot, so the key is logged as "unknown shadow config
key" and dropped (gap D).

## Scope

### In

- `configsync.Manager` gains an `Observability ShadowApplier` slot
  (Cat A inline, like `exemptions` and `killswitch`) and dispatches the
  `observability` key.
- `agent/cmd/agent/main.go` constructs an inline adapter that parses
  the shadow payload as `telemetry.Config` and calls
  `tp.Reconfigure`. The adapter is no-op when `tp` is nil (agent ran
  with telemetry disabled at start) and on empty / null payload (per
  `ShadowApplier` contract).
- TODO comment at the introspection registration site flagging that
  this gives e31-s12 a `config.observability` source to expose.

### Out

- **No CP UI changes.** A dedicated "settings/observability" page is a
  separate operational ergonomic story. Today, ai-gateway and
  compliance-proxy receive `observability` via the existing config
  channel; the agent inclusion in the future-thing-broadcast is an
  operational decision that can be made independently of this code.
- **No Hub broadcast wiring.** Hub already accepts the
  `observability` config_key for any thing type. The day operators
  decide to push it to agents, the receive side will work.
- **No DB schema changes.** The `telemetry.Config` struct is
  pre-existing.
- **No on-disk config persistence.** Reconfigure swaps the in-memory
  provider; on agent restart, the local YAML wins again (intended —
  ensures the "boot-time floor" remains operator-controlled).

## Tasks

### T1. configsync.Manager wiring

`packages/agent/core/sync/configsync/manager.go`:

- Add `Observability ShadowApplier` to `ManagerConfig`.
- Add `observability ShadowApplier` field to `Manager`.
- Wire it through `NewManager`.
- Add `"observability": {applier: m.observability, needsPull: false}`
  to the dispatch table (Cat A — inline state).

### T2. agent main.go adapter

After `tp` is constructed, build and pass an adapter to
`configsync.NewManager`:

```go
observabilityApplier := configsync.AdapterFunc(func(ctx context.Context, raw json.RawMessage) error {
    if len(raw) == 0 || string(raw) == "null" {
        return nil
    }
    var oc telemetry.Config
    if err := json.Unmarshal(raw, &oc); err != nil {
        return fmt.Errorf("decode observability: %w", err)
    }
    if tp == nil {
        return nil
    }
    return tp.Reconfigure(oc)
})
```

Then `Observability: observabilityApplier` in the `ManagerConfig`.

### T3. Tests

- Extend `configsync/manager_test.go` to assert that an `observability`
  key delivered through `ApplyDesired` reaches the registered applier
  and is reported back.
- A focused unit test in agent `main_test.go` (or a new file) is
  optional; the adapter is small and the configsync test covers
  dispatch.

### T4. Verify

- `go test -race -count=1 ./packages/agent/...` PASS.
- Hand smoke (deferred): bring up agent with telemetry enabled, push
  an `observability` shadow with a different endpoint, confirm
  subsequent spans land at the new endpoint without restart.

## Acceptance Criteria

1. Agent's `configsync.Manager` no longer logs "unknown shadow config
   key" for `observability`.
2. A shadow-pushed `telemetry.Config` triggers
   `tp.Reconfigure(cfg)`; subsequent traces emit to the new endpoint
   while in-flight spans complete on the previous provider (covered by
   `SwappableTracerProvider`'s existing semantics).
3. Empty / null payload is a no-op (does not flip the provider into a
   degraded state).
4. Tests in T3 pass; existing tests do not regress.
5. The compliance-review section of this SDD is preserved verbatim in
   the architecture doc (`docs/users/product/architecture.md` Runtime
   Introspection section gap D entry already references this) so the
   trade-off is auditable.

## Risks

- **Telemetry endpoint redirect via compromised Hub.** Documented in
  the compliance-review section. Mitigation: operators wishing hard
  isolation simply do not push `observability` to agents.
- **Reconfigure errors.** `SwappableTracerProvider.Reconfigure` returns
  an error path; the applier surfaces it. The provider remains on the
  previous configuration (no torn state) — confirmed by the
  package-level `Reconfigure` implementation.
- **Build-time agent compiled without telemetry.** When `tp` is nil
  (telemetry disabled at boot), the applier is a no-op. Operators see
  the shadow apply succeed but the local provider remains disabled.
  Acceptable — matches how ai-gateway behaves when its OTEL init
  fails.
