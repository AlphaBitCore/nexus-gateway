# Agent Overview

*Audience: evaluators deciding whether to deploy the Desktop Agent; contributors who will work on agent code.*

The Desktop Agent is a Go daemon that intercepts AI traffic at the OS level on managed endpoints — running the same forwarder pipeline as the Compliance Proxy but from the client side rather than the network path. It captures traffic from apps like Cursor, Claude Code, and ChatGPT Desktop before it leaves the machine, applies the same hook-based compliance pipeline, and drains audit events to Hub over mTLS. The key consequence of this design: the 5-service architecture delivers OS-level endpoint governance without requiring proxy configuration in each application.

---

## The agent-as-forwarder model

The agent is structurally a client-side Compliance Proxy. It intercepts at the OS, runs the identical Go forwarder pipeline, measures its own upstream phases, and emits the same `traffic_event` shape. Provider-agnosticism holds in the routing sense — the agent does not pick a provider, applications already chose one — but instrumentation is full:

```
intercept → req_hooks → upstream_ttfb → upstream_total → resp_hooks
```

Phase fields on each audit event row are durations in milliseconds:

| Field | Meaning |
|---|---|
| `request_hooks_ms` | Total time in request-stage hooks |
| `upstream_ttfb_ms` | Request dispatch to first byte from upstream |
| `upstream_total_ms` | Request dispatch to last byte from upstream |
| `response_hooks_ms` | Total time in response-stage hooks |
| `duration_ms` | End-to-end intercept-to-emit duration |

Cross-service stitching uses `trace_id`: when the same logical request hits both the agent and an upstream Nexus AI Gateway, the Traffic timeline joins on `trace_id`. The phase model is stamped in `packages/agent/internal/network/proxy/proxy.go` via `ProxySession.AddBreakdown`.

## Platform intercept layers

The agent ships three intercept paths, all dispatching into the shared Go forwarder:

| Platform | Mechanism | Notes |
|---|---|---|
| macOS | `NETransparentProxyProvider` (Network Extension) | Shipping. Safety-critical fail-open contract — see [Agent macOS NE Architecture](Agent-macOS-NE-Architecture). |
| Linux | `iptables` NAT-redirect transparent proxy | Shipping. See [Agent Linux Platform](Agent-Linux-Platform). |
| Windows | WinDivert user-mode packet capture | Shipping. See [Agent Windows Status](Agent-Windows-Status). |

On macOS the NE only surfaces connection metadata (host, port, process bundle ID) — TLS bump and content-aware hooks run only on Linux and Windows. macOS provides policy decisions and metadata-level audit.

## Internal package structure (sibling-pair pattern)

`packages/agent/internal/` follows a deliberate "runtime engine + UI exposer" decomposition across three pairs:

| Runtime engine | Adjacent exposer | Purpose of the split |
|---|---|---|
| `policy/core/` — runtime glob/regex evaluator | `policy/policies/` — builds the `AppliedConfig` snapshot the Dashboard's Policies page renders | Runtime internal types evolve independently from UI wire shapes |
| `observability/diag/` — live diag-event drain | `observability/diagnostics/` — builds the Diagnostics page render data | Diagnostic render data is best-effort; runtime drain is not |

A merged `sync/status/` package (previously split) holds both data structures and the localhost HTTP API; the two shapes did not evolve independently and were merged to eliminate the artificial boundary.

This pattern is not a dedup target. Each pair exists because the runtime engine and its UI exposer evolve at different cadences. New pairs require a clear divergence rationale before being introduced.

## Audit upload and local queue

The agent persists events to a local SQLCipher queue (encrypted at rest using a key from the platform keystore), then drains over three channels in priority order:

1. **WebSocket to Hub** — primary live upload.
2. **HTTP POST to Hub `/api/internal/things/agent-audit`** — fallback when WS is unavailable, using mTLS + device token.
3. **SQLCipher queue** — persistent buffer during connectivity outages; drained when Hub is reachable again.

The local store is encrypted with the audit DB key fetched from `packages/agent/internal/identity/keystore/`.

## Traffic upload level

`agent_settings.trafficUploadLevel` controls which events reach Hub:

| Level | Events emitted |
|---|---|
| `all` | Every intercepted event |
| `processed` (default) | Only events the agent inspected (in-scope domain, hooks ran). Out-of-scope relays do not emit. |
| `blocked` | Only events with a hook hard reject |

Filtering happens at emit time on the agent, not DB-side. `deny`, `block`, and `error` outcomes bypass the filter regardless of level — those are always auditable.

## Local pipeline steps

Once the OS hands a flow to the agent, the Go forwarder runs the following sequence:

1. **Resolve destination** — read SNI or first bytes to identify the target host.
2. **Decide intercept policy** — domain glob/regex matching against the admin-pushed allowlist. Out-of-scope flows relay unmodified.
3. **TLS bump** (Linux/Windows only) — full MITM with an admin-trusted cert. macOS NE sees only connection metadata, no body bytes.
4. **Content extract** — Tier-1 adapter via `ExtractText` for structured AI payloads.
5. **Request-stage hooks** — same `shared/hooks` pipeline; same `HookConfig` shape; same allow/redact/block decisions.
6. **Forward** to the upstream provider (the real provider, not via Nexus).
7. **Receive** upstream response.
8. **Response-stage hooks**.
9. **Audit-event emit** to local SQLCipher queue.
10. **Drain** to Hub on connectivity via mTLS.

Content-aware steps (3, 4, 5, 8) run only on Linux and Windows. macOS records metadata and policy decisions only.

## HTTP/2 and QUIC considerations

Cursor's IDE protocol uses `http2.connect()` for streaming chat — which bypasses HTTP CONNECT semantics. A CONNECT-proxy-only agent never sees those streams. The structural fix for this class of protocol is a transparent-proxy intercept at the OS layer (`pf` on macOS, `iptables` on Linux), which captures regardless of application-layer protocol negotiation.

Similarly, QUIC traffic rides on UDP and requires explicit handling in the macOS NE (the daemon writes the bundle allowlist per the file-only Rule 3). On Linux and Windows, iptables/WinDivert capture at the TCP layer and most QUIC traffic that falls back to TCP is covered; raw UDP QUIC from specific high-priority bundles requires the same file-driven allowlist approach.

## Failure modes

| Failure | Behavior |
|---|---|
| Hub unreachable | Forwarder continues with cached config; audit queues locally |
| Hook timeout | Per `fail_behavior` (typically `fail_open` for agent) |
| Cert revoked | Agent enters minimal-functionality mode; re-enrollment required |
| Update binary tampered | Ed25519 signature check fails; agent refuses to apply |
| Local DB encryption key lost | Wipe local queue; fresh start; report incident to Hub |
| SQLCipher queue full | Rotate queue; drop oldest non-blocked events; alert fires |

---

## Canonical docs

- [`agent-forwarder-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-forwarder-architecture.md) — Forwarder phase model, audit upload, traffic upload level, hook and exemption wiring
- [`agent-internals-sibling-pairs-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-internals-sibling-pairs-architecture.md) — Sibling-pair decomposition pattern inside `packages/agent/internal/`

**Adjacent wiki pages**: [Agent macOS NE Architecture](Agent-macOS-NE-Architecture) · [Agent Linux Platform](Agent-Linux-Platform) · [Agent Windows Status](Agent-Windows-Status) · [Agent Enrollment Attestation](Agent-Enrollment-Attestation) · [Agent Policy Evaluation](Agent-Policy-Evaluation) · [The Five Services](The-Five-Services)
