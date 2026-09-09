# Agent architecture

The Nexus Agent is the per-device enforcement point of the gateway. It runs as
a background daemon on each managed endpoint (macOS, Windows, Linux), intercepts
the device's outbound HTTPS traffic, and applies the same compliance pipeline the
server-side Compliance Proxy applies — locally, before the request leaves the
machine. Structurally it is a client-side sibling of the Compliance Proxy: a
forwarding proxy that terminates TLS for monitored hosts, runs hooks across the
request/response lifecycle, measures upstream timing, and emits an audit record
per flow.

This document is the front door for the agent. It covers the daemon's shape,
its process lifecycle, how it pulls configuration from Hub, and the lifecycle
events it reports — then indexes the per-concern documents that own each
subsystem in depth.

## Role and boundaries

The agent is a Thing: it enrolls with Nexus Hub, holds a device certificate, and
pulls its policy from Hub's shadow. It is provider-agnostic — it does not know
"OpenAI" versus "Anthropic" as first-class concepts; it recognises monitored
hosts from the `interception_domains` config and runs the shared traffic adapters
against whatever it bumps.

Enforcement is real and local. The agent runs the shared hook pipeline
(`packages/shared/policy/pipeline`), a host-matching `domain.Engine`, and any
admin-bound rule packs directly on the device. A blocked request is blocked at
the endpoint; it never reaches the provider. The agent measures upstream TTFB and
total time on its forward path, the same instrumentation the Compliance Proxy
uses, so cross-service rows stitch by `external_request_id` at query time.

Two processes cooperate per install:

- **The daemon** (`packages/agent/cmd/agent`, binary `nexus-agent`) — runs as a
  privileged service (macOS LaunchDaemon, Linux systemd unit, Windows service).
  It owns network interception, the compliance pipeline, the audit queue, and the
  Hub connection.
- **The menu-bar / tray host app** — a per-user GUI that talks to the daemon over
  a local IPC socket for status display, sign-in, and pause/quit affordances. The
  daemon↔tray protocol is documented in
  [agent-tray-ipc-architecture.md](agent-tray-ipc-architecture.md).

## Command surface

`packages/agent/cmd/agent/main.go` dispatches the daemon's subcommands:

| Command | Purpose |
| --- | --- |
| `run` | Start the daemon (the long-running service entry point). |
| `enroll` | Enroll the device with a one-time token. |
| `enroll-sso` | Enroll via the interactive SSO / PKCE flow. |
| `unenroll` | Remove local enrollment state. |
| `install-ca` | Linux-only install-time helper (invoked by the deb/rpm postinstall as root). |
| `install-wfp-check` | Windows-only MSI custom-action helper run after the WFP service starts. |
| `version` / `versions` | Print build identity; `versions` adds the full macOS bundle inventory. |

Platform-specific commands (for example Windows service control) are dispatched
ahead of this switch through a platform shim, so the same `main` serves all three
operating systems.

## Process lifecycle

The `run` command (`packages/agent/cmd/agent/cmd_run.go`) drives the daemon's
whole life. It has two modes selected by enrollment state.

### Pending-enrollment (cold boot, no device cert)

When no device certificate exists on disk, `run` enters pending-enrollment mode.
It starts only the status IPC server and waits for the menu-bar app to drive
either a token enrollment or the SSO flow. Once enrollment completes, the daemon
exits so the service manager restarts it with the full stack. A user-quit flag
on disk short-circuits this entirely: if present, every service-manager respawn
exits immediately until the menu-bar app clears it.

### Full stack (enrolled)

An enrolled daemon brings up, in order: the Hub HTTP client (mTLS, device-cert
pinned), the enrollment manager, the Hub WebSocket client (`thingclient`,
WebSocket primary with HTTP fallback), OpenTelemetry, the compliance + policy
subsystem, the shadow config appliers, the SQLCipher-encrypted audit queue, the
diagnostics subsystem, and a set of background goroutines (audit drain, audit
prune, local rollup, exemption cleanup + upload, auto-updater, diag dedup). It
then starts the status IPC server and, last, the platform interception layer.

On startup the daemon writes its own PID to a well-known path so the interception
layer can pass through the daemon's own outbound traffic (self-intercept guard).

### Shutdown

Shutdown is triggered by a termination signal, the runtime user-quit flag
watcher, or a status-IPC shutdown request. Every trigger emits an
`agent.shutdown` lifecycle event with a brief flush window before cancelling the
root context, then waits up to ten seconds for the audit queue to drain before
exiting. On macOS the daemon flushes the DNS cache and restarts mDNSResponder on
both startup and shutdown so the user's name resolution returns cleanly to native
routing when interception is not running.

## Configuration sync

All agent policy originates in Hub's shadow and is pulled by the agent — Hub
never pushes full desired state. This is the fleet-wide pull model described in
[thing-config-sync-architecture.md](../../cross-cutting/foundation/thing-config-sync-architecture.md);
the agent's wiring of it lives in `packages/agent/cmd/agent/configdispatch.go`
and `packages/agent/internal/sync`.

A change signal arrives over the `thingclient` WebSocket
(`OnConfigChanged`), which hands the desired key set to a shared
`configloader.Loader`. Each shadow key is registered with a per-key applier in
one of two categories:

- **Category A — inline.** Hub pushes the full state bytes in the shadow; the
  applier consumes them directly. The agent's Category A keys are `killswitch`
  and `agent_settings`.
- **Category B — pull.** Hub pushes minimal state bytes over WS as a pull signal
  only; the client's `RegisterRawPull` flag (not any `{needsPull:true}` marker in
  the payload) drives the fetch. The Loader discards the pushed bytes and
  issues an authenticated HTTP GET to Hub
  (`/api/internal/things/config/<key>?type=agent`, Bearer device token plus an
  `X-Thing-Id` header) to fetch the live bytes before applying. The agent's
  Category B keys are `exemptions`, `interception_domains`, `hooks`,
  `payload_capture`, `streaming_compliance`, `installed_rule_packs`, and
  `user_context`.

Applied config lives in memory — each apply atomically swaps the live hook
resolver and domain snapshot — and is also mirrored, per shadow key, into a
local SQLCipher-backed `config_cache` table
(`packages/agent/internal/sync/shadow/cache.go`) on the audit queue's database.
At boot the daemon replays that cache through the same per-key appliers
(`packages/agent/cmd/agent/configcache.go`), so an agent that starts while Hub is
unreachable enforces its last-known policy instead of starting with empty
resolvers; a successful Hub pull later in startup supersedes the restored values.
Cached config older than a grace period is still applied — staleness fails open,
never closed — and emits a warning so operators can see the node has lost contact
with Hub. The local `agent.yaml` (`packages/agent/internal/sync/schema/config.go`)
supplies the non-shadow defaults the daemon needs before any of this runs. The
per-key catalog and the Category A/B classification rules are owned by
[configuration-architecture.md](../../cross-cutting/foundation/configuration-architecture.md).

Before enrollment is possible, the agent reads deployment-wide settings (Control
Plane URL, device-auth mode) from Hub's unauthenticated
`/api/public/agent-bootstrap` endpoint (`packages/agent/internal/lifecycle/bootstrap`).
This endpoint uses system TLS roots rather than the mTLS-pinned client, because
Hub serves it on its public hostname; the response is cached for sixty seconds.

## Local compliance enforcement

The agent's compliance pipeline (`packages/agent/internal/compliance/pipeline.go`)
wraps the same shared hook resolver used by the AI Gateway and Compliance Proxy.
It holds a `pipeline.HookConfigCache` with a zero TTL, so the resolver is swapped
only on an explicit shadow apply — never on a background timer. Three pieces of
state move together as a unit on each apply:

- the **hook configs**, staged and reloaded atomically;
- the **`DomainSnapshot`** and a priority-ordered host-match `domain.Engine`,
  rebuilt from the `interception_domains` shadow, which drive per-host
  PROCESS / PASSTHROUGH / BLOCK decisions and adapter resolution;
- the **rule-pack registry**, indexed by the hook each pack is bound to. On hook
  reload the agent injects `_rulePackInstalls` into the matching hook's config so
  the shared keyword-filter factory routes to the rule-pack engine. A pack not
  bound to a hook is visible to the device but enforces nothing.

The pipeline runs at the connection stage (before the relay, keyed on target host
and SNI) and at the request stage (after TLS bump, on the decrypted request). On
any infrastructure error — resolver build failure, pipeline build error, timeout
— the agent fails open, matching the AI Gateway and Compliance Proxy. The forward
path that drives these stages is documented in
[agent-forwarder-architecture.md](agent-forwarder-architecture.md); the engine
and exemption mechanics are in
[agent-policy-eval-architecture.md](agent-policy-eval-architecture.md).

## Lifecycle events

The agent reports user- and system-level lifecycle events
(`packages/agent/internal/lifecycle/state`): `agent.startup`, `agent.shutdown`,
`agent.paused`, `agent.resumed`, `agent.sso_login`, and `agent.sso_logout`. Each
event fans out to two independent sinks:

- a **Hub diag-event push** over the `thingclient` WebSocket, which lands in the
  Control Plane's infrastructure view; and
- a **local SQLCipher mirror** in the agent's `lifecycle_event` table, which the
  Dashboard's Activity page reads without querying Hub.

Both paths are best-effort and independent: a Hub outage must not leave the local
Activity timeline empty, and a local write failure must not block the Hub push.
Crashes do not emit here — they are captured separately as fatal diag events by
the recovery path.

## Subsystem index

Each concern below has its own document. The code globs match the architecture
trigger table.

| Concern | Anchor packages | Document |
| --- | --- | --- |
| Network forward / intercept path (intercept → req_hooks → upstream_ttfb → upstream_total → resp_hooks), HTTP/2 + QUIC handling | `packages/agent/internal/network/**` | [agent-forwarder-architecture.md](agent-forwarder-architecture.md) |
| macOS NE provider fail-open rules (safety-critical) | `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/**` | [agent-ne-fail-open-architecture.md](agent-ne-fail-open-architecture.md) |
| macOS platform: system-extension ↔ daemon IPC, NE interception, mode selection | `packages/agent/internal/platform/darwin/**`, `packages/agent/platform/darwin/**` | [agent-macos-platform-architecture.md](agent-macos-platform-architecture.md) |
| Windows platform: WFP driver interception, Service Control Manager, named-pipe IPC | `packages/agent/internal/platform/windows/**`, `packages/agent/platform/windows/**` | [agent-windows-platform-architecture.md](agent-windows-platform-architecture.md) |
| Linux platform: iptables REDIRECT + `SO_ORIGINAL_DST` interception, systemd integration | `packages/agent/internal/platform/linux/**`, `packages/agent/platform/linux/**` | [agent-linux-platform-architecture.md](agent-linux-platform-architecture.md) |
| macOS build / signing / notarization | (driven by the `build-agent` skill) | [macos-build-signing-architecture.md](macos-build-signing-architecture.md) |
| Platform path abstraction | `packages/agent/internal/platform/paths/**` | [agent-paths-abstraction-architecture.md](agent-paths-abstraction-architecture.md) |
| Platform key storage (SQLCipher DB-key, mTLS key) | `packages/agent/internal/identity/keystore/**`, `packages/agent/internal/identity/secretstore/**` | [agent-keystore-architecture.md](agent-keystore-architecture.md) |
| Policy engine, host exemptions, protection pause | `packages/agent/internal/policy/{core,policies,exemption}/**`, `packages/agent/internal/lifecycle/protectionpause/**` | [agent-policy-eval-architecture.md](agent-policy-eval-architecture.md) |
| Device enrollment, attestation header, SSO / PKCE, browser callback | `packages/agent/internal/identity/{enrollment,attestation,auth}/**`, `packages/agent/internal/host/openbrowser/**` | [agent-identity-enrollment-architecture.md](agent-identity-enrollment-architecture.md) |
| Audit-upload queue, OTel tracing, backpressure rollup | `packages/agent/internal/observability/{audit,telemetry,backpressure}/**` | [agent-observability-architecture.md](agent-observability-architecture.md) |
| Binary auto-update (manifest polling, signature verification) | `packages/agent/internal/host/updater/**` | [agent-autoupdater-architecture.md](agent-autoupdater-architecture.md) |
| Daemon ↔ tray IPC protocol | `packages/agent/internal/host/trayipc/**` | [agent-tray-ipc-architecture.md](agent-tray-ipc-architecture.md) |

## Related cross-cutting documents

The agent implements or consumes several concerns owned elsewhere; it references
them rather than redefining them:

- [thing-model.md](../../cross-cutting/foundation/thing-model.md) — the Thing /
  shadow model the agent registers under.
- [thing-config-sync-architecture.md](../../cross-cutting/foundation/thing-config-sync-architecture.md)
  and [configuration-architecture.md](../../cross-cutting/foundation/configuration-architecture.md)
  — the pull model and per-key config catalog.
- [kill-switch-architecture.md](../../cross-cutting/safety/kill-switch-architecture.md)
  and [emergency-passthrough-architecture.md](../../cross-cutting/safety/emergency-passthrough-architecture.md)
  — the passthrough behaviour the `killswitch` shadow key drives.
- [credentials-architecture.md](../../cross-cutting/safety/credentials-architecture.md)
  — credential handling the agent never sees in plaintext.
- [audit-pipeline-architecture.md](../../cross-cutting/observability/audit-pipeline-architecture.md)
  and [alerting-architecture.md](../../cross-cutting/observability/alerting-architecture.md)
  — where the agent's uploaded audit events and lifecycle diag events land.

## References

- `packages/agent/cmd/agent/main.go` — command dispatch and build identity
- `packages/agent/cmd/agent/cmd_run.go` — daemon lifecycle: the ordered subsystem boot sequence and shutdown sequencing
- `packages/agent/cmd/agent/wiring/` — per-subsystem constructors `cmdRun` calls in boot order (logger, Hub clients, compliance, audit queue, diag, status server, platform interception)
- `packages/agent/cmd/agent/configdispatch.go` — shadow config loader, Category A/B registration, Hub pull
- `packages/agent/cmd/agent/configcache.go` — persist-on-apply and boot replay of the offline config cache
- `packages/agent/internal/sync/shadow/cache.go` — per-key offline config cache (`config_cache` table)
- `packages/agent/internal/sync/schema/config.go` — agent config schema and merge
- `packages/agent/internal/lifecycle/bootstrap/bootstrap.go` — pre-enrollment bootstrap client
- `packages/agent/internal/lifecycle/state/lifecycle.go` — lifecycle event emitter
- `packages/agent/internal/compliance/pipeline.go` — local hook pipeline, domain engine, rule-pack injection
- `packages/agent/internal/network/` — forward / intercept path
- `packages/agent/internal/observability/` — audit queue, telemetry, backpressure
