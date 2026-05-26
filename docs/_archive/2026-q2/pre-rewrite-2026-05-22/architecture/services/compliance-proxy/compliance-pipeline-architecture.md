---
doc: compliance-pipeline-architecture
area: service
service: compliance-proxy
tier: 1
---

# Compliance Proxy Pipeline Architecture

> **Tier 1 architecture doc.** Read this before touching `packages/compliance-proxy/internal/**`. Agent-side forward pipeline is a sibling doc at `agent-forwarder-architecture.md`. Hooks that this pipeline invokes are in `hook-architecture.md`. The text-first normalizer is part of this pipeline.

The Compliance Proxy is a **transparent TLS forward proxy** with a compliance pipeline. Applications point their HTTPS proxy at it; the proxy bumps TLS, runs hooks, and forwards upstream.

---

## 1. Phase model

```
CONNECT ── access control ── connection mgmt ── pinning detect ── TLS bump ──
   ──► domain/path match (interception_domain) ── content extract ── req_hooks ──
   ──► upstream HTTPS ── upstream TTFB / total ── stream session ── resp_hooks ──
   ──► relay to client (per streaming mode) ── audit emit
```

Each phase produces a stamped `phase_*` timestamp on the traffic event (cross-ref the latency phase taxonomy now folded into the audit pipeline doc). Phase boundaries are how we attribute latency.

## 2. CONNECT + access control

A CONNECT request arrives with `Host: api.openai.com:443` (or equivalent). The proxy:

1. Checks **source IP allowlist** (YAML-only, boot-fixed). Reject if outside. The IP allowlist is parsed from `compliance-proxy.yaml`'s `accessControl:` block at startup and is *not* hot-swapped; restart the proxy to change it. See the load-bearing comment at `packages/compliance-proxy/internal/access/checker.go:21`.
2. Checks **domain allowlist** (per-instance, merged from YAML static + DB-driven `InterceptionDomain` rows). The DB-driven half is hot-swapped via `SwapDomainAllowlist` when Hub broadcasts a config change. Reject if outside.
3. Checks **connection mgr** quota (per-source concurrency, total concurrent).
4. Begins TLS bump.

Reject decisions surface as a `traffic_event` row written via the audit pipeline (compliance-proxy/internal/audit emits to the `nexus.event.compliance` NATS subject). There is no separate `audit.proxy.connect_denied` event type — investigators filter rejected attempts via `source = 'compliance-proxy'` (the `TrafficEvent.source` discriminator pinned by the migration referenced at `tools/db-migrate/schema.prisma:1281-1289`) plus the status / hook-decision columns on `traffic_event`.

## 3. Pinning detection + auto-exemption

Some clients pin the upstream cert and would reject Nexus's minted leaf. The proxy detects pinning by observing the client's TLS Alert (e.g., `bad_certificate`) and **auto-exempts** the destination on N consecutive failures.

Auto-exemptions:

- Live in the Hub-pushed shadow blob; the proxy rebuilds an in-memory `exemption.Store` from the shadow on every config change (see `packages/compliance-proxy/internal/exemption/store.go` — `Rebuild`). There is no `interception_exemption` Prisma table; admin reads/writes flow through the shadow surface, not direct SQL.
- Surface in the CP "Exemptions" page (which reads the resolved shadow state).
- Apply per destination domain on this instance.
- Expire after configurable window (admin-tunable).

A pinned destination flows through unbumped — no content inspection, no body capture. The pinning state is also a signal that the destination is "interesting" — alerting can fire on auto-exemption count.

## 4. TLS bump (cert minting)

For non-pinned, allowlisted destinations:

1. Look up cached leaf cert for the destination host.
2. Cache miss → mint a fresh leaf from the local CA. The leaf is short-lived (~24h); cached in LRU (in-process) + Redis (cross-instance).
3. Optional **KMS integration** — CA private key can live in KMS with remote signing for high-security deployments; default is local file with restrictive perms.
4. Serve the leaf to the client; complete TLS to the upstream with a fresh client TLS context.

The cert cache is two-tier: LRU hits never touch Redis; Redis hits avoid mint cost; mint hits are rare in steady-state. Memory `project_prod_compliance_proxy_debug` records prod operational tunings.

## 5. Domain / path match (InterceptionDomain rules)

Once TLS is established, the proxy reads the inbound HTTP request and matches:

- **Domain** — full host or wildcard (e.g., `*.openai.com`).
- **Path** — exact or pattern (e.g., `/v1/chat/*`).
- **Method** — typically POST for AI traffic.

The match selects the **traffic adapter** (cross-ref `provider-adapter-architecture.md`): which provider format this destination speaks. The adapter is what produces the canonical payload for hook evaluation.

## 6. Content extraction (text-first normalizer)

For consumer-surface traffic — `chatgpt-web`, `claude-web`, `cursor`, … — the normalizer's required output is **readable text**. Losing token / usage stats at this stage is **acceptable**. The reason: consumer surfaces don't expose token counts in the wire format consistently; insisting on full canonical structure produces fragile adapters. Memory `feedback_compliance_proxy_text_first` is binding.

For API-surface traffic (the same provider via SDK), the same adapter typically produces both text and structured usage — but only the **text** is required for the hook decision. The structured fields are nice-to-have for analytics.

The Tier-2 NonJSONDetector framework (cross-ref adapter doc §7) handles binary / multipart / gRPC-Web cases. Tier-1 adapters delegate to detectors rather than reimplementing per-host.

## 7. Hook pipeline (request + response)

Same shared Go code as the AI Gateway and the Agent. The proxy constructs an `InterceptedTransaction` from the extracted content + request metadata, dispatches to the request-stage hook pipeline, then forwards upstream (or returns 451 on hard reject).

After upstream returns:

- Non-streaming → buffer entire body, extract, run response-stage hooks, then relay.
- Streaming → per the route's streaming compliance mode (`passthrough` / `buffer_full_block` / `chunked_async`).

Cross-ref `hook-architecture.md` for hook semantics.

## 8. Streaming compliance modes

| Mode | Effect |
|---|---|
| `passthrough` | Relay bytes raw; no hook, no capture. |
| `buffer_full_block` | Accumulate full extracted text before forwarding any byte; hook runs at end; 451 on hard reject (upstream body never reaches client). |
| `chunked_async` | Relay in real time; hook runs per-chunk; cannot stop sent bytes; complete audit + post-hoc alerting. |

Configured per `interception_domain` (compliance-proxy) or per `Provider` (AI Gateway).

## 9. Exemption manager + Runtime API

The proxy exposes a localhost **runtime API** for ops (`packages/compliance-proxy/internal/runtime/server/server.go`). The surface is deliberately small and shadow-aligned — every mutating operation funnels through the break-glass PUT path:

- `GET  /healthz` — process liveness (no auth).
- `GET  /metrics` — Prometheus scrape.
- `GET  /connections` — active CONNECT tunnels snapshot.
- `GET  /runtime/config` — composite snapshot of all runtime config keys (killswitch, exemptions, log level, …).
- `GET  /runtime/config/{key}` — per-key snapshot.
- `PUT  /runtime/config/{key}` — break-glass write. Only `killswitch` and `exemptions` accept writes; other keys return 400. The PUT writes are spooled and replayed on Hub reconnect so the shadow eventually carries them.
- `GET  /runtime/sync-status` — shadow cursor (desired / reported versions).
- `GET  /runtime/health` — per-subsystem liveness (cert cache, MQ, Hub link).

The surface above is the complete runtime API. Legacy routes (`/runtime/exemptions`, `/runtime/killswitch`, `/runtime/cert-cache/*`, `/runtime/cpu-profile`, `/runtime/goroutine-dump`, `/alerts/*`) are not mounted; the compliance gate is `TestDeletedRoutes_Return404` in `packages/compliance-proxy/internal/runtime/server/deleted_routes_test.go`. Admin operations go through the CP admin API → Hub → shadow push, not the runtime API.

## 10. SIEM bridge + Audit emission

Every completed CONNECT → response cycle emits one `traffic_event` row, batched and shipped to the `nexus.event.compliance` NATS subject via the MQ batch writer (`packages/compliance-proxy/internal/audit/mq_writer.go`) with an NDJSON spill-to-disk fallback (`packages/compliance-proxy/internal/audit/ndjson.go`) when MQ is unavailable.

Events flow:

- `traffic_event` → MQ → Hub audit-sink → Postgres + spillstore.
- SIEM bridge (`packages/compliance-proxy/internal/siem/`) optionally forwards a subset to an external SIEM (HTTPS webhook or syslog) — see `siem-bridge-architecture.md`.

Compliance proxy does not write `AdminAuditLog` rows or emit `audit.*` admin event types; admin-action audit lives in the Control Plane. Cross-ref `audit-pipeline-architecture.md` and `mq-architecture.md`.

## 11. Config & shadow integration

The proxy is a Thing. Its shadow includes:

- `interception_domain` ruleset (Cat B, pulled on change).
- `hook_config` (Cat B).
- Allowlists (Cat A inline for fast lookup).
- Kill switch (Cat A).
- SIEM channel config (Cat B).

Pull-on-signal applies (cross-ref `thing-config-sync-architecture.md`). Hot-swap via `atomic.Pointer` on the policy resolver.

## 12. Failure modes

| Failure | Behavior |
|---|---|
| Upstream unreachable | Return 503 with `reason=upstream_unreachable`; record on traffic_event. |
| Hook timeout | Per `fail_behavior`: `fail_open` (relay) or `fail_close` (451). |
| TLS bump fails (no leaf) | Auto-exempt; relay unbumped. |
| Cert cache empty + KMS slow | Mint inline (latency hit); cache for next request. |
| Killswitch active | Local kill (process-wide refusal) or shadow-managed kill (per E48). |
| Body > spillstore threshold | Overflow to S3; audit row stores reference. |

## 13. Sources

- `packages/compliance-proxy/internal/proxy/server/` — CONNECT listener (`server.go`).
- `packages/compliance-proxy/internal/proxy/connect/` — bumped CONNECT tunnel driver (`tunnel.go`).
- `packages/compliance-proxy/internal/proxy/conn/` — connection lifecycle + manager + pool.
- `packages/compliance-proxy/internal/proxy/forward/` — bumped-traffic forward handler.
- `packages/compliance-proxy/internal/access/` — IP + domain + private-IP + SNI access checks.
- `packages/compliance-proxy/internal/tls/{cache,issuer,kms,pinning}/` — cert minting, two-tier cache, optional KMS signer, pinning detection.
- `packages/compliance-proxy/internal/config/{cache,loaders,shadow}/` — config cache + loaders + shadow integration.
- `packages/compliance-proxy/internal/exemption/` — exemption store (rebuilt from shadow).
- `packages/compliance-proxy/internal/runtime/{auth,breakglass,config,handler,killswitch,server}/` — runtime API.
- `packages/compliance-proxy/internal/siem/` — SIEM bridge.
- `packages/compliance-proxy/internal/audit/` — local audit producer (MQ writer + NDJSON fallback) that emits to `nexus.event.compliance`.
- `packages/shared/traffic/` — shared adapter + extract + normalize code.
- `packages/shared/policy/domain/` — host + path match engine (Engine.Swap / MatchHost / PathAction).

## 14. Cross-references

- `agent-forwarder-architecture.md` — sibling pipeline (agent runs structurally the same model).
- `hook-architecture.md` — hook invocation semantics.
- `provider-adapter-architecture.md` — content extraction.
- `normalization-architecture.md` — existing normalize-pipeline detail.
- `audit-pipeline-architecture.md` — where traffic_events and audit events go.
- `error-taxonomy-architecture.md` — 5xx / timeout / 429 classification.
