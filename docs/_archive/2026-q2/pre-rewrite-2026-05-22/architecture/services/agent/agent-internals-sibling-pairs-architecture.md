---
doc: agent-internals-sibling-pairs-architecture
area: service
service: agent
tier: 1
updated: 2026-05-20
---

# `packages/agent/internal/` — Sibling-Pair Pattern + Small Subpackages

> **Tier 3 architecture doc.** Documents the "runtime engine vs UI exposer" sibling-pair pattern used inside `packages/agent/internal/`, plus brief notes on small subpackages without their own dedicated arch doc. The pattern can read like a dedup target at first glance — it isn't; see §"Why it's not dedup" below.

This doc does **not** cover the agent subpackages that already have their own arch docs (`agent-forwarder-architecture.md`, `agent-keystore-architecture.md`, `agent-tray-ipc-architecture.md`, `agent-autoupdater-architecture.md`, `agent-paths-abstraction-architecture.md`, `agent-policy-eval-architecture.md`, `agent-protection-pause-architecture.md`, `agent-browser-opener-architecture.md`, `agent-telemetry-architecture.md`, `agent-sso-enrollment-architecture.md`, `agent-backpressure-rollup-architecture.md`, `agent-exemption-grants-architecture.md`, `agent-enrollment-architecture.md`, `agent-ne-fail-open-architecture.md`, `agent-{macos,windows,linux}-platform-architecture.md`, `diag-event-triage-architecture.md`).

## The sibling-pair pattern

Three pairs in `packages/agent/internal/` use a deliberate decomposition that looks like a dedup target at first glance but isn't:

| Runtime engine | Adjacent package | What the adjacent does |
|---|---|---|
| `policy/core/` (`engine.go`) — runtime domain-policy engine (glob evaluation, consults `exemption.Store` and the interception-domains snapshot) | `policy/policies/` (`applied.go`, `snapshot_cache.go`) | Builds the **AppliedConfig snapshot** the Dashboard's "Policies" page renders — a unified view of every admin-pushed config the device honours. Pure decoder over the shadow snapshot. |
| `observability/diag/` (`drain.go`, `local_buffer.go`) — runtime diag-event drain (local-buffer + upload mechanism) | `observability/diagnostics/` (`diagnostics.go`) | Builds the data the Dashboard's "Diagnostics" page renders (log tail, Hub reachability, device cert path). Pure UI render data, best-effort. |

**Counter-example — single-package status.** `packages/agent/internal/sync/status/` keeps both the data structures (`status.go`, `config_summary.go`) and the localhost HTTP API (`statusapi_server.go`, `statusapi_listen_other.go`, `statusapi_listen_windows.go`) in one package. The two shapes do not evolve independently enough to justify a split — illustrating the decision rule below ("if shapes are identical, keep them in one package").

**Why it's not dedup**: each runtime engine has internal types optimised for its work; each UI exposer has wire-shape types optimised for the dashboard render. Putting them in the same package would entangle the runtime's internal evolution with the UI's wire format. The split keeps both free to evolve.

**Naming convention**: when you find a third pair like this in agent code, the pattern is consistent — the **runtime engine** is named for what it does (verb / noun-of-the-mechanism); the **UI exposer** is named for the page or surface it feeds (plural noun matching the dashboard label).

**When NOT to introduce a new pair**: if the UI render shape and the runtime shape are genuinely identical, keep them in one package. The pair is justified only when the two shapes evolve at different cadences.

## Small subpackages

### `network/bridge/`

Daemon ↔ tray IPC connection bridge. Not the same as `trayipc/` (which is the IPC *protocol*; bridge is the connection establishment).

### `lifecycle/bootstrap/`

Agent boot sequence: load YAML bootstrap config, derive paths via `platform.DefaultPaths()`, mount the SQLCipher store, start the lifecycle coordinator, hand off to the runtime. Single-file orchestrator.

### `lifecycle/killswitch/`

Kill-switch reader: consumes the Hub-pushed Cat A inline kill-switch shadow keys and exposes them to the forwarder phase model. Companion to `passthrough/` in the AI Gateway.

### `lifecycle/protectionpause/` — has its own arch doc

User-initiated pause toggling the killswitch. Cross-ref `agent-protection-pause-architecture.md` (Tier 3).

### `lifecycle/state/`

Service-lifecycle state coordination — shutdown ordering, startup gating. Wires `context.Context` cancellation through every long-running goroutine so a SIGTERM converges cleanly.

### `network/tls/`

Agent-side TLS handling for the bumped traffic. The cert minting + bumping itself happens in `shared/tlsbump/` and `compliance-proxy/internal/cert/`; this package wires the agent's local pieces (CA load, leaf cache eviction signalling).

### `compliance/`

Wires the shared hook pipeline (`packages/shared/policy/hooks/`) for the agent's forwarder. Mirror of `ai-gateway/internal/hooks/` and `compliance-proxy/internal/compliance/`.

## When you change one of these

- **Adding a new sibling pair**: add a row to the §"sibling-pair pattern" table above; do not introduce the pair if the runtime and UI shapes are identical.
- **Promoting one to its own Tier-2/Tier-3 doc**: remove the entry from this card; add a row to `architecture-doc-triggers.md`.

## Sources

- Sibling pairs: `packages/agent/internal/policy/core/` ↔ `packages/agent/internal/policy/policies/`; `packages/agent/internal/observability/diag/` ↔ `packages/agent/internal/observability/diagnostics/`.
- Merged single-package (was sibling pair): `packages/agent/internal/sync/status/`.
- Small subpackages: `packages/agent/internal/network/bridge/`, `packages/agent/internal/network/tls/`, `packages/agent/internal/lifecycle/bootstrap/`, `packages/agent/internal/lifecycle/killswitch/`, `packages/agent/internal/lifecycle/protectionpause/`, `packages/agent/internal/lifecycle/state/`, `packages/agent/internal/compliance/`.
