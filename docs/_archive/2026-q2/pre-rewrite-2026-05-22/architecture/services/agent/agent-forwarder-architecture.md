---
doc: agent-forwarder-architecture
area: service
service: agent
tier: 1
updated: 2026-05-21
---

# Agent Forwarder Architecture

> **Tier 1 architecture doc.** Read this before touching `packages/agent/internal/` forwarder paths, the phase model in agent code, agent-side hook execution, or the `agent_settings` traffic-upload level. The macOS-specific NE provider has its own safety-critical doc at `agent-ne-fail-open-architecture.md`. Enrollment / mTLS lives in `agent-enrollment-architecture.md`. Compliance-proxy sibling pipeline lives in `compliance-pipeline-architecture.md`.

The Agent is **structurally a client-side compliance-proxy**, not a passive sniffer. It intercepts at the OS, runs the same Go forwarder pipeline, measures its own upstream phases, and emits the same traffic event shape. Memory `feedback_agent_is_pure_forwarder` is binding on this point.

---

## 1. The "agent ≈ compliance-proxy" invariant

Agent is provider-agnostic in the routing sense (it does not pick a provider — applications already chose one), but it **does** instrument and enforce the same forwarder model:

```
intercept ── req_hooks ── upstream_ttfb ── upstream_total ── resp_hooks
```

Phase fields on the agent's audit event row are **durations in milliseconds**, not wall-clock timestamps (the row already carries `started_at` / `completed_at` for absolute time):

- `request_hooks_ms` — total time spent in request-stage hooks.
- `upstream_ttfb_ms` — time from request dispatch to first byte from upstream.
- `upstream_total_ms` — time from request dispatch to last byte from upstream.
- `response_hooks_ms` — total time spent in response-stage hooks.
- `duration_ms` — end-to-end intercept-to-emit duration.

`packages/agent/internal/network/proxy/proxy.go` calls `ProxySession.AddBreakdown("response_hooks_ms", ...)` (and siblings) as each phase closes; the queue writer in `packages/agent/internal/observability/audit/queue/queue.go` persists them into the nullable `*_ms` columns on `event.Event`.

Cross-service stitching uses `trace_id` (cross-ref `trace-id-propagation-architecture.md`). When the same logical user request hits both agent and an upstream Nexus AI GW, the timeline UI joins on `trace_id`.

## 2. Platform intercept layers

The shipping intercept paths today, all dispatching into the shared Go forwarder.

> **Status legend.** "Shipping" in this table means **code path is wired and compiles** — the platform-specific entry point exists, the build tag selects it, and integration tests exercise it where harness coverage permits. "Production-validated" is a separate, stricter bar tracked in `docs/developers/roadmap.md`; today only macOS qualifies. Downstream user-facing docs (`docs/users/features/deployment-models.md`, `docs/users/product/features.md`, `docs/users/product/overview.md`, repo root `README.md`) anchor back to this section for the definitive platform reality.

| Platform | Mechanism | Status |
|---|---|---|
| macOS | `NETransparentProxyProvider` (Network Extension) at `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/` | Shipping **and production-validated** — this is the only platform currently running in real customer production. Safety-critical fail-open contract — see `agent-ne-fail-open-architecture.md`. |
| Linux | `iptables` NAT-redirect transparent proxy. Per-platform code at `packages/agent/internal/platform/linux/` (4 `.go` files / ~1015 LOC: `iptables_linux.go`, `linux_linux.go`, `marker_linux.go`, `reconciler_linux.go`). | **Wired and compiles**; not yet production-validated. The path passes unit + integration tests under the `linux` build tag, but has not been exercised on a fleet of real customer endpoints. Treat any Linux rollout as a staged pilot until the roadmap moves this row to "production-validated". |
| Windows | — | Not yet wired. `packages/agent/platform/platform.go` + the per-platform stubs exist as build seams; no kernel-mode driver (WinDivert or otherwise) ships today. |

The Go forwarder above the platform layer is shared. Platform-specific entry points live in `packages/agent/platform/` (build seams) and `packages/agent/internal/platform/<os>/` (concrete implementations).

## 3. The local pipeline

Once the OS has handed a flow:

1. **Resolve destination** — agent reads SNI / first bytes to identify the destination host.
2. **Decide intercept policy** — domain glob/regex matching against admin-pushed allowlist. If not in scope, relay unmodified.
3. **TLS bump** (where supported) — Linux / Windows perform full MITM with admin-trusted cert. macOS does not (NE only sees metadata).
4. **Content extract** — Tier-1 adapter via `ExtractText` (cross-ref `provider-adapter-architecture.md`).
5. **Request-stage hooks** — same `shared/hooks` set; same `HookConfig` shape; same decisions.
6. **Forward** to upstream (the real provider, not Nexus).
7. **Receive** upstream response.
8. **Response-stage hooks**.
9. **Audit-event emit** to local SQLCipher queue.
10. **Drain** to Hub on connectivity (mTLS upload).

macOS NE limitations: today's `NETransparentProxyProvider` only surfaces connection metadata (host, IP, port, process) to the Go agent. The agent on macOS performs policy decisions and metadata-level audit, but does **not** TLS-bump or run content-aware hooks. Windows / Linux can.

## 4. Audit upload (mTLS, with HTTP fallback to MQ)

- Primary: WS to Hub for live audit upload.
- Fallback: HTTP POST to Hub `/api/internal/things/agent-audit` (mTLS + `Authorization: Bearer <device-token>`). Registered at `packages/nexus-hub/internal/handler/routes.go` → `things.POST("/agent-audit", agentAuditAPI.UploadAgentAudit)`; agent-side client lives at `packages/agent/internal/observability/audit/hub/hub_client.go`.
- Last resort: persistent local SQLCipher queue, drained when connectivity returns.

The local SQLCipher store is encrypted at rest using the audit DB key fetched from the platform keystore (`packages/agent/internal/identity/keystore/`, see `agent-keystore-architecture.md`).

**Empty-string stamping (binding).** The agent's audit-event serializer always sets string fields including `""`. Hub's `AuditUpload` handler must stamp-unconditionally or strip-empty for any CHECK-constrained column or the audit pipeline stalls silently. Memory `feedback_agent_audit_empty_string_stripping` is binding.

## 5. Traffic-upload level (binding)

`agent_settings.trafficUploadLevel` is a Cat B shadow key with enum `{all, processed, blocked}`, default `processed`:

| Level | Emit |
|---|---|
| `all` | Every traffic event. |
| `processed` | Only events the agent inspected (in-scope domain, hooks ran). Out-of-scope relays do not emit. |
| `blocked` | Only events with a hook hard reject. |

**Filtering at emit time, not DB-side.** Agent decides per event whether to emit. `deny` / `block` / `error` outcomes **always** bypass the filter — those are auditable regardless of level (memory `feedback_agent_traffic_upload_level`).

This control matters because endpoints can generate a lot of traffic. Default `processed` is the right balance: investigators see what we touched without drowning in unrelated TCP.

## 6. HTTP/2 + QUIC + keep-alive (Cursor incident retrospective)

Cursor's IDE protocol uses `http2.connect()` for StreamChat — which bypasses HTTP CONNECT semantics, so a CONNECT-proxy-only agent never sees those streams. Memory `project_cursor_capture_investigation` records the investigation; the structural fix is a `pf` transparent proxy (Linux / macOS), which captures regardless of protocol negotiation.

Verification of Cursor StreamChat through the macOS NE agent is parked (memory `project_ne_cursor_streamchat_verification`) and will run once the current session and the next prod update are verified.

Lessons (binding):

- Do not assume "CONNECT proxy" covers all the relevant traffic. Verify each high-priority surface.
- HTTP/2 multiplexing, QUIC, and keep-alive intermingling all change what "a request" looks like; the agent has to detect at the OS layer, not the application layer.
- Each new protocol family that surfaces on the endpoint deserves a smoke test in `test-cursor-adapter` / `test-geminiweb-adapter` / etc., not just a unit test.

## 7. Hook execution + exemptions + rule packs

Local hook execution runs the same 3 stages as the server-side compliance proxy, and both shadow-delivered policies are now consumed end-to-end (wired 2026-05-18/19 — supersedes the 2026-05-14 "vanity surface" finding):

- **Exemptions** — `policy/core/Engine.SetExemptionStore` is wired at agent boot. `Engine.Evaluate(host)` checks `exemptionStore.IsExempt(host)` **first** and returns `{Action: "passthrough", MatchedPattern: "exempt:<reason>"}` on hit, short-circuiting the interception-domains lookup. Source: `packages/agent/internal/policy/core/engine.go` Evaluate path.
- **Rule packs** — the `installed_rule_packs` Cat B shadow key is consumed by `compliance.AgentPipeline.ApplyRulePacksShadowState`, which writes to the in-memory `rulePacksByHookID` registry. On every hook-config reload, `injectRulePacks` rewrites the cached `core.HookConfig` slice so each matching `HookConfig.Config` map carries `_rulePackInstalls`; the pipeline runs against that enriched config. Source: `packages/agent/internal/compliance/pipeline.go` (`ApplyRulePacksShadowState`, `injectRulePacks`).

The CP "Policies → Exemptions" and rule-pack surfaces are therefore the operative policy contract on agent-originated traffic; they are not server-side-only.

## 8. Platform paths abstraction (binding)

All agent filesystem paths come from `paths.DefaultPaths()`. Never hardcode `/Library/`, `/var/`, `/etc/`, `/tmp/`, or `C:\` strings. The 2026-05-13 QuitFlag incident shipped a hardcoded macOS path; review caught it. Memory `feedback_agent_platform_paths_abstraction` is binding.

The fields exposed today are `StateDir`, `ConfigDir`, `ConfigFile`, `LogDir`, `SocketPath`, `FlagsDir`, `UserQuitFlagPath`, `DaemonUnitPath`. See `agent-paths-abstraction-architecture.md` for the canonical per-platform values; adding a new field requires updating the struct + all three `paths_<goos>.go` files + tests.

## 9. Failure modes

| Failure | Behavior |
|---|---|
| Hub unreachable | Forwarder continues with cached config. Audit queues locally. |
| MQ down | HTTP fallback for audit. |
| SQLCipher full | Rotate queue; drop oldest non-blocked events (alert fires). |
| Hook timeout | Per `fail_behavior` (typically `fail_open` for agent). |
| Cert revoked | Re-enrollment required; agent enters minimal-functionality mode. |
| Update binary tampered | Ed25519 signature check fails; agent refuses to switch (auto-updater detail in Tier-2 updater doc). |
| Local DB encryption key lost | Wipe local queue; fresh start; report incident to Hub. |

## 10. Sources

- `packages/agent/internal/network/proxy/proxy.go` — Go forwarder, phase-duration stamping (`AddBreakdown`).
- `packages/agent/internal/policy/core/engine.go` — exemption + interception-domain decision engine.
- `packages/agent/internal/compliance/pipeline.go` — `AgentPipeline`, `ApplyRulePacksShadowState`, `injectRulePacks`.
- `packages/agent/internal/observability/audit/` — audit event types (`event/event.go`) + SQLCipher queue (`queue/queue.go`) + Hub uploader (`hub/hub_client.go` posting to `/api/internal/things/agent-audit`).
- `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/` — macOS NE provider.
- `packages/agent/platform/` (root + per-OS files) — Linux iptables hook + Windows build stub.
- `packages/shared/traffic/`, `packages/shared/policy/hooks/` — shared forwarder + hook code.
- `packages/agent/internal/identity/{secretstore,keystore}/` — local DB encryption + key mgmt.

## 11. Cross-references

- `agent-ne-fail-open-architecture.md` — macOS NE provider safety-critical rules.
- `agent-enrollment-architecture.md` — enrollment / mTLS / re-enrollment.
- `compliance-pipeline-architecture.md` — sibling pipeline (compliance-proxy).
- `hook-architecture.md` — hook semantics (agent reality §7).
- `audit-pipeline-architecture.md` — audit upload and storage.
- `trace-id-propagation-architecture.md` — `trace_id` across agent ↔ server.
