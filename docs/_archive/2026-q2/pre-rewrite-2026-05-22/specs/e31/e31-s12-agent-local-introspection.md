# E31 S12 — Agent local runtime introspection (NexusAgentUI Runtime tab)

**Epic:** 31
**Story:** 12
**Status:** Draft — 2026-04-27 (agent side complete; NexusAgentUI side TBD in the
swift repo)
**Requirements:** inline (closes the agent leg of e31-s7's "every service
exposes its runtime to operators" goal)

## User Story

As an end user (or an internal support engineer working with that user)
I want to open NexusAgentUI's Runtime tab on my own machine and see
exactly what the agent has applied right now: kill switch state, hooks
loaded, exemptions in effect, payload-capture knobs, OTEL endpoint.
The same shape as compliance-proxy / ai-gateway / hub expose to the CP
UI Nodes detail page in e31-s7 — but the agent never round-trips
through Hub because user machines sit behind NAT.

## Background

e31-s7 deliberately put **agent introspection out of scope** because
the bridge path (CP UI → CP API → Hub bridge → reverse-HTTP to thing's
metrics URL) cannot reach an agent through NAT. The fix is to surface
the same data via the agent's existing local IPC channel
(`packages/agent/core/sync/statusapi/`), which the Swift NexusAgentUI
already uses to read status, query events, sync config, etc.

This story closes the loop: agent registers a `runtimeintrospect.Registry`
and exposes the snapshot via a new `GET_RUNTIME` IPC command; the Swift
GUI grows a Runtime tab that issues the command and renders the same
JSON shape as the CP UI Runtime State tab does for the other services.

## Scope

### In — agent side (this repo)

- Agent embeds a `shared/runtimeintrospect.Registry` constructed at
  startup with `service="agent"`, `thing_id=deviceID`, `thing_version=version`.
- Initial sources registered: `config.killswitch` (via the e31-s9
  `killSwitch.SnapshotState()` accessor) and `config.payload_capture`
  (via `payloadCaptureStore.Get()`). More sources land here in
  follow-ups as accessors mature on the underlying types (exemption
  store, hook list, observability config, policy engine state).
- `statusapi.Server` gains a typed `RuntimeFn` and a `SetRuntimeFn`
  setter (additive — does not break existing callers/tests). The
  dispatcher handles `GET_RUNTIME` by calling the function with a 5-second
  context timeout.
- main.go wires `statusServer.SetRuntimeFn(func(ctx) { return reg.Snapshot(ctx) })`
  after construction.

### In — NexusAgentUI side (separate Swift repo, not touched here)

Documented for handoff — the implementation lands in the Swift project:

- New `RuntimePane.swift` view registered as the third tab on the
  agent's main window (alongside Status / Events).
- Sends `GET_RUNTIME\n` over the existing Unix-domain-socket /
  named-pipe IPC, reads one JSON line, decodes using the
  `runtimeintrospect.Response` shape from the SDD's Snapshot Schema
  section (mirrors the TS types in
  `packages/control-plane-ui/src/api/services/nodeRuntime.ts`).
- Renders the same three-column-per-source layout as the CP UI tab
  (source name, status badge, JSON-pretty value). No three-way
  desired/reported/applied diff because the agent has no Hub-side
  meta — only its own snapshot.
- A "Refresh" button + 10-second auto-refresh toggle (off by default).

### Out

- **Every Source not yet wired.** The agent has many internal stores
  (`exemption.Store`, `agentPipeline` hook list, policy engine rules,
  observability config) that don't yet expose snapshot accessors.
  Adding one Source per such store is a strict superset of this story
  and lands as small follow-ups (each adds an `IntrospectSnapshot()`
  method on the relevant type plus a `Register` call here).
- **Auth on the IPC socket.** The Unix socket is already bound with
  permissions only the local user can open. Same trust boundary as
  `GET_STATUS` and `QUERY_EVENTS`. No bearer token needed.
- **Cross-platform parity for NexusAgentUI.** The Swift work is
  macOS-first; Windows / Linux GUIs catch up as their respective
  apps land.

## Tasks

### T1. Agent: runtimeintrospect.Registry construction (DONE)

`packages/agent/cmd/agent/main.go` constructs the registry after
`killSwitch` is built and registers the initial Sources. Aliased import
`sharedintro` keeps the call sites concise without colliding with the
agent's own `intercept` package.

### T2. statusapi: GET_RUNTIME command + SetRuntimeFn (DONE)

`packages/agent/core/sync/statusapi/server.go` adds:

- `RuntimeFn func(ctx context.Context) any` type.
- `runtimeFn` field on `Server` and `SetRuntimeFn(fn)` method.
- `case "GET_RUNTIME"` in `dispatch` with a 5s timeout context;
  returns `{"error": "runtime introspection not configured"}` when the
  function is unset.

### T3. main.go: wire SetRuntimeFn (DONE)

After `statusServer` construction:

```go
statusServer.SetRuntimeFn(func(ctx context.Context) any {
    return introspectReg.Snapshot(ctx)
})
```

### T4. NexusAgentUI Swift: Runtime tab (TBD — separate repo)

Tracked here for visibility; lands in the macOS/iOS repo. Outline:

1. Add a `RuntimeIntrospectClient` wrapper around the existing IPC
   transport — `func runtime() async throws -> RuntimeResponse`.
2. Decode `RuntimeResponse` matching the JSON envelope; the TS
   reference is at
   `packages/control-plane-ui/src/api/services/nodeRuntime.ts:RuntimeSnapshot`.
3. New `RuntimeView` listing each source as a card: source name +
   green/red badge + collapsible pretty-printed JSON. No edits.
4. Hook into the existing tab strip on the main window.

### T5. Tests (this repo)

- `statusapi/server_test.go` extends the dispatch coverage with a
  GET_RUNTIME case: when `SetRuntimeFn` returns a fake snapshot, the
  server replies with the marshalled JSON; when unset, the server
  replies with the configured-error placeholder. Existing tests pass
  unchanged.

### T6. Verify

- `go test -race -count=1 ./packages/agent/...` PASS — covered by
  the existing agent test suite plus the GET_RUNTIME case (T5).
- Hand smoke (deferred): once NexusAgentUI lands the Runtime tab,
  toggle kill switch from CP UI, observe the agent's local UI reflect
  it within one shadow apply round-trip.

## Acceptance Criteria

1. The agent process exposes a `GET_RUNTIME` command over its existing
   IPC channel returning a JSON envelope with the
   `runtimeintrospect.Response` shape.
2. Initial sources `config.killswitch` and `config.payload_capture`
   return live values; absent sources fail per-source (`{ok: false,
   error: ...}`) without blocking the rest.
3. `statusapi.Server.SetRuntimeFn` is optional; pre-existing callers
   that never invoke it keep working.
4. `go test -race -count=1 ./packages/agent/...` PASS.
5. The Swift Runtime tab implementation is unblocked: the JSON shape
   referenced in T4 matches the agent output exactly, so it can land
   in the macOS repo without further coordination.

## Risks

- **Source crash bringing down the IPC.** Mitigated by
  `runtimeintrospect.Registry.Snapshot`'s per-source recover wrapper —
  a panicking Source returns an error result, the rest still serve.
- **5s timeout too tight on a busy agent.** If a Source is doing
  blocking work it's already a violation of the package contract
  (snapshots must be O(in-memory state)). Operators see a partial
  snapshot with the offending source marked errored — actionable
  signal.
- **Swift / Go JSON shape drift.** Mitigated by the TS reference in
  `nodeRuntime.ts` being the single source of truth for both UIs.
  When a new source lands, both clients can keep using
  `additionalProperties: true` without recompiling.
