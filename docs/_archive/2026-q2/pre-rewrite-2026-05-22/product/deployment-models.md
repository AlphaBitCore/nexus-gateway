# Deployment Models

Nexus Gateway provides three independent traffic interception paths that can
be deployed individually or in combination depending on the enterprise's
network topology and compliance requirements.

## Traffic Paths at a Glance

```
                    ┌─────────────────────┐
                    │   Nexus Hub +       │  Node registry, config sync,
                    │   Control Plane     │  config push, IAM, audit,
                    │   (Go + Echo)       │  alerts, scheduled jobs
                    └──────┬──────────────┘
                           │  Hub WS push  (pull-only config-sync model)
          ┌────────────────┼────────────────┐
          │                │                │
    ┌─────▼──────┐   ┌────▼─────┐   ┌─────▼──────┐
    │ AI Gateway │   │Compliance│   │  Desktop   │
    │ (explicit) │   │  Proxy   │   │  Agent     │
    │            │   │(network) │   │ (endpoint) │
    └─────┬──────┘   └────┬─────┘   └─────┬──────┘
          │               │               │
     App → VK auth   Network CONNECT   Local intercept
     → route → AI    → TLS bump        → policy eval
     provider         → hooks           → TLS MITM
                      → upstream        → hooks → audit
```

## Path A — AI Gateway (Explicit Proxy)

**Use case**: Applications that integrate directly with the Nexus API.

Applications send requests to the AI Gateway instead of directly to AI
providers. Each request carries a Virtual Key (VK) for authentication.

**Capabilities**:
- Virtual Key authentication and per-key rate limiting
- Multi-provider routing engine (7 strategies: single, load-balance,
  fallback, conditional, A/B split, policy narrowing, smart LLM-dispatch)
- Two-phase quota management: pre-request `Engine.Check` returns an estimate-based allow/deny/downgrade decision, post-response `Engine.Reconcile` writes actual usage to the counters (`packages/ai-gateway/internal/policy/quota/enforcement.go:83` `Check`, line 172 `Reconcile`)
- Full compliance hook pipeline (request + response, including live
  streaming checkpoint-based inspection)
- Provider adapter translation (OpenAI, Anthropic, Gemini, Moonshot,
  DeepSeek production-validated; Azure OpenAI, GLM, MiniMax adapter shipped
  but not yet production-validated — see roadmap E72)
- Async batch audit logging to PostgreSQL

**When to choose**: SDK-integrated applications, API-gateway-style
deployments, cost tracking and budget enforcement per team/project.

## Path B — Compliance Proxy (Network-Level)

**Use case**: Intercept all AI traffic traversing the network perimeter.

Deployed as a forward proxy (typically at the gateway/firewall). All HTTPS
traffic to AI provider domains passes through via HTTP CONNECT tunnels.

**Capabilities**:
- Transparent TLS interception with dynamic certificate issuance
- Two-layer certificate cache (LRU + Redis, AES-256-GCM encrypted)
- KMS integration for CA key protection (remote signing mode available)
- IP and domain allowlists (YAML static + DB-driven dynamic)
- Parallel compliance hook execution with per-hook/total timeouts
- SSE streaming compliance with checkpoint-based inspection
- Certificate pinning detection with auto-exemption
- Runtime API for kill switch, alerts, temporary exemptions
- SIEM log forwarding
- Cross-instance state replication via Redis

**When to choose**: Network-perimeter deployments, transparent interception
without application changes, environments where all AI traffic must pass
through a single compliance chokepoint.

## Path C — Desktop Agent (Endpoint-Level)

**Use case**: Intercept AI traffic on end-user devices.

Installed on a developer workstation. **macOS is the only
production-validated platform today.** Linux (iptables) and Windows
(WinDivert) backends are scaffolds in development and not yet
production-ready — see
`docs/developers/architecture/services/agent/agent-forwarder-architecture.md`
§"Platform support matrix" (lines 41–46) for the authoritative status.
The agent intercepts outbound HTTPS traffic to configured AI domains at
the OS level.

**Capabilities** (macOS shipping; Linux / Windows scaffolds noted):
- Platform-native traffic interception — macOS pf-mode (E74,
  production); Linux transparent proxy via iptables (scaffold,
  ~1015 LOC, not validated); Windows CONNECT proxy via WinDivert
  (scaffold, ~1048 LOC, not validated)
- mTLS enrollment with CSR-based certificate lifecycle
- Local policy engine (domain glob/regex matching)
- TLS MITM with compliance hook pipeline (macOS today; Linux/Windows
  wired in the scaffolds, not validated end-to-end)
- Encrypted local audit queue (SQLCipher, platform keystore for key)
- Heartbeat, config sync, and audit upload to Control Plane
- Auto-updater with Ed25519 signature verification
- Auto-exemption for TLS pinning failures
- Status API for native GUI (Unix socket / named pipe)
- OpenTelemetry instrumentation

**When to choose**: BYOD or managed-device environments, remote workers,
endpoint-level compliance enforcement, environments where network-level
interception is not feasible.

### macOS limitation

On macOS, the Network Extension intercepts traffic at the network layer
and only surfaces connection metadata (host, IP, port, process) to the Go
agent. The agent does not perform TLS MITM or content inspection on macOS;
it provides policy-based allow/deny decisions and metadata-level auditing.
Full content inspection with compliance hooks is available on Windows and
Linux.

## Combining Paths

The three paths are **independent and parallel** — traffic intercepted by
one path does not flow through another. This means:

- An application using the AI Gateway (Path A) does not also pass through
  the Compliance Proxy (Path B) or the Desktop Agent (Path C).
- An admin configuring compliance hooks in the Control Plane applies those
  hooks to all three paths (subject to `applicableIngress` filtering).
- Audit events from all three paths are queryable in the unified audit
  timeline (`GET /audit/unified`).

**Typical combinations**:

| Scenario | Paths | Rationale |
|----------|-------|-----------|
| API-first with SDK integration | A only | Full routing + quota control |
| Network perimeter compliance | B only | Transparent, no app changes |
| Remote workforce | C only | Per-device enforcement |
| Hybrid: APIs + network | A + B | SDKs use Gateway, browser/CLI uses proxy |
| Hybrid: APIs + endpoints | A + C | SDKs use Gateway, device traffic audited |
| Full coverage | A + B + C | All traffic paths governed |

## Configuration Flow

All three paths receive their configuration from the Nexus Hub via the unified
**node / config-sync** model. The Control Plane fronts the admin UI and
forwards writes to the Hub; the Hub maintains target vs applied config per
node and signals each node to **pull** the updated config on change. Redis
is used only for caching — it is **not** the config invalidation channel.

| Path | Config mechanism | Latency |
|------|-----------------|---------|
| AI Gateway | Hub WebSocket change-signal → AI Gateway pulls keys → hot-swaps via `atomic.Pointer` | Sub-second |
| Compliance Proxy | Hub WebSocket change-signal → Compliance Proxy pulls keys → hot-swaps `PolicyResolver` | Sub-second |
| Desktop Agent | Hub WebSocket change-signal → Agent pulls keys → applies locally | Seconds (online); resumes on reconnect |

When an admin creates or updates a hook, routing rule, or interception domain in
the Control Plane UI, the change is persisted to Postgres via the Hub, the
Hub updates each affected node's target config, and a change-signal
prompts each node to pull only the keys that changed. Each node reports its
applied-config version back to the Hub so out-of-sync state is observable.

## Multi-instance topology

Today's production runs as a single EC2 node co-locating all server-side
services (Hub + Control Plane + AI Gateway + Compliance Proxy + Postgres +
Redis + NATS, fronted by nginx) plus per-endpoint Agent installs. The
node / config-sync model and the stateless service tier are designed for
future multi-instance and region-split deployment without changing the
data-plane contracts.
