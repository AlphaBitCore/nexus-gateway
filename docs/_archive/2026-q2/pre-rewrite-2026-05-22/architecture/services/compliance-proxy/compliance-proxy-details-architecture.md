---
doc: compliance-proxy-details-architecture
area: service
service: compliance-proxy
tier: 1
---

# Compliance Proxy — Detailed Subsystem Architecture

> **Tier 2 architecture doc.** Companion to `compliance-pipeline-architecture.md` (Tier 1 — covers the phase model + hooks integration). This doc drills into the compliance proxy's internal subsystems: cert minting, access control, exemption manager, runtime API.

The compliance proxy has more moving parts than the AI Gateway because it transparently bumps TLS and handles arbitrary destinations. This doc maps the internals so anyone touching `packages/compliance-proxy/internal/` can find the right area fast.

---

## 1. Subsystem map

| Subsystem | Path | Role |
|---|---|---|
| CONNECT listener | `internal/proxy/server/` | `server.go` accepts CONNECT |
| Bumped tunnel driver | `internal/proxy/connect/` | `tunnel.go` drives the bumped tunnel + streaming relay |
| Bumped-traffic forward | `internal/proxy/forward/` | `forward.go` carries bumped HTTPS requests upstream after access + pinning checks |
| Connection mgr | `internal/proxy/conn/` | Per-conn lifecycle, TLS bump driver, pool (`lifecycle.go` + `manager.go` + `pool.go`) |
| Access control | `internal/access/` | Source IP allowlist (YAML), domain allowlist (hot-swapped), private-IP guard, SNI check, per-source concurrency |
| Cert mint + cache | `internal/tls/{cache,issuer,kms,pinning}/` | Leaf cert minting (LRU + Redis 2-tier; optional KMS signing); pinning detection |
| Compliance pipeline | `internal/compliance/` | Hook pipeline construction + invocation (counterpart to AI Gateway's hook wiring) |
| Bootstrap config | `internal/config/` | YAML loader at startup + shadow integration tree |
| Config cache | `internal/config/cache/` | Hot-swappable in-process config snapshot (from Hub shadow) |
| Config loaders | `internal/config/loaders/` | Hub initial pull + change-signal handlers |
| Shadow integration | `internal/config/shadow/` | Shadow-blob → resolved runtime structures |
| Exemption manager | `internal/exemption/` | Pinning exemptions rebuilt from the shadow blob into an in-memory `Store` |
| Local audit producer | `internal/audit/` | MQ writer + NDJSON fallback; emits to `nexus.event.compliance` |
| Health | `internal/health/` | Liveness / readiness probes |
| Metrics | `internal/metrics/` | Prometheus counters / histograms (dotted opsmetrics names; no `nexus_compliance_proxy_*` prefix) |
| SIEM emitter | `internal/siem/` | Direct in-line emit (low-latency path) |
| Runtime API | `internal/runtime/{auth,breakglass,config,handler,killswitch,server}/` | Localhost ops API (slimmed surface — see §5) |

## 2. Cert minting in detail

The proxy bumps TLS by minting a short-lived leaf cert for every intercepted host. The cert is signed by a local CA whose private key the proxy controls.

### Two-tier cert cache

```
Request for HOST
  → look up HOST in L1 (in-process LRU, 256 entries — wired in `packages/compliance-proxy/cmd/compliance-proxy/wiring/cert.go:87`)
    ✓ hit → return                                   # microseconds
    ✗ miss
  → look up HOST in L2 (Redis, cross-instance; size bounded by Redis maxmemory + cert TTL eviction)
    ✓ hit → promote to L1, return                    # low ms
    ✗ miss
  → mint a leaf cert (signed by local CA)
    → store in L1 + L2
    → return
```

The L2 layer matters for multi-instance deployments: a new instance doesn't have to re-mint everything on cold start.

### Cert lifetime

- Leaf cert: ~24h validity. Short to limit blast radius if a leaf leaks.
- Local CA: long-lived; rotation is an admin operation.

### KMS integration (optional)

For high-security deployments, the local CA private key can live in KMS with remote signing. The proxy submits the leaf's TBS bytes to KMS, gets back the signature, assembles the cert. Adds a KMS round-trip per mint; reduces blast radius further. KMS wiring lives in `packages/compliance-proxy/internal/tls/kms/`.

## 3. Access control

Three lists, evaluated in order:

1. **Source IP allowlist** — YAML-only, boot-fixed. Lives in `compliance-proxy.yaml`'s `accessControl:` block and is *not* hot-swapped. See the load-bearing comment at `packages/compliance-proxy/internal/access/checker.go:21`. A CONNECT from a non-allowlisted IP is rejected with `403 Forbidden`.
2. **Domain allowlist** — per-instance. Built by merging the YAML static entries with DB-driven `InterceptionDomain` rows; hot-swapped via `Checker.SwapDomainAllowlist` when Hub broadcasts a config change. A CONNECT to a non-allowlisted domain is rejected with `403`.
3. **Concurrency limits** — per-source-IP concurrent connections. Excess is rejected with `429 Too Many Requests`.

Each rejection is recorded as a `traffic_event` row written via the local audit producer (no separate `audit.proxy.connect_denied` event type exists — investigators filter rejected attempts via the status / hook-decision columns on `traffic_event`).

## 4. Exemption manager

When a TLS bump fails (client pins the upstream cert and rejects our leaf), the proxy detects the failure and auto-exempts the destination:

- Observes the client's TLS handshake failure (the shared `tlsbump.PinningTracker` records the failure — see `packages/shared/transport/tlsbump/pinning.go`).
- Counts failures inside a sliding window.
- When the failure count crosses `audit.pinning.autoExempt.failureThreshold` within `windowSeconds`, the tracker writes an in-memory auto-exemption that expires after `exemptionDurationSeconds`. All three values come from `compliance-proxy.yaml` (see `packages/compliance-proxy/cmd/compliance-proxy/config/config.go:235-244`); the sample config in `compliance-proxy.config.yaml` ships `failureThreshold: 3`, `windowSeconds: 3600`, `exemptionDurationSeconds: 86400`.
- Future flows to that destination bypass TLS bump — relay unbumped (no content inspection).

There is no `interception_exemption` Prisma table; admin-managed exemption state is rebuilt from the Hub-pushed shadow blob via `exemption.Store.Rebuild` (`packages/compliance-proxy/internal/exemption/store.go:87`). Admin creates exemptions through the CP admin API (which writes the shadow). The auto-exempt set lives in the in-process `tlsbump.PinningTracker` rather than the shadow.

### Exemption expiry

Auto-applied exemptions expire after `exemptionDurationSeconds`. After expiry, the next flow re-attempts bump; if it fails again, the tracker reopens the counter.

Admin exemptions carry their own `ExpiresAt` (set when the row is created in the shadow) and are removed when the shadow no longer lists them.

## 5. Runtime API

The proxy exposes a **localhost-only** HTTP API for on-host ops. The surface is deliberately small — every mutating operation funnels through the shadow-aligned break-glass PUT path. From `packages/compliance-proxy/internal/runtime/server/server.go`:

```
GET  /healthz                       → process liveness (no auth)
GET  /metrics                       → Prometheus scrape
GET  /connections                   → active CONNECT tunnels snapshot
GET  /runtime/config                → composite snapshot (killswitch, exemptions, log level, …)
GET  /runtime/config/{key}          → per-key snapshot
PUT  /runtime/config/{key}          → break-glass write (only `killswitch` and `exemptions` accept writes; other keys return 400)
GET  /runtime/sync-status           → shadow cursor (desired / reported versions)
GET  /runtime/health                → per-subsystem liveness
```

The surface above is the complete runtime API. Legacy routes (`/runtime/exemptions`, `/runtime/killswitch`, `/runtime/cert-cache/*`, `/runtime/cpu-profile`, `/runtime/goroutine-dump`, `/alerts/*`) are not mounted; the compliance gate is `TestDeletedRoutes_Return404` in `packages/compliance-proxy/internal/runtime/server/deleted_routes_test.go`.

Local kill switch is independent from the Hub-managed kill switch (`kill-switch-architecture.md` §10): a PUT to `/runtime/config/killswitch` writes the local override and spools an event for Hub on reconnect. Useful when Hub is unreachable and an instance needs to be drained.

## 6. Config cache pattern

```go
var policyResolver atomic.Pointer[PolicyResolver]

// On change-signal:
newResolver := buildResolverFromShadow(snapshot)
policyResolver.Store(newResolver)

// In hot path:
resolver := policyResolver.Load()
decision := resolver.Decide(transaction)
```

Atomic pointer swap means in-flight requests keep using the previous resolver; new requests pick up the new one. Zero coordination, zero blocking.

The same pattern is used for routing rules in AI Gateway, hooks in agent, etc. (cross-ref `thing-config-sync-architecture.md` §5).

## 7. Metrics emitted

Registered in `packages/compliance-proxy/internal/metrics/prometheus.go` via the dotted-name `registry` package (the canonical naming is enforced in `prometheus-naming-architecture.md`):

Tunnel + connection:

- `tunnels.active` — gauge.
- `tunnels.total{result}` — counter.

Cert cache + minting:

- `cert_cache.hits_total{layer}` — counter (layer = `l1` LRU / `l2` Redis).
- `cert_cache.misses_total` — counter.
- `cert_cache.size` — gauge.
- `cert_sign_ms` — histogram.
- `cert_prewarm.duration_ms` — gauge.

Pinning + killswitch + redis liveness:

- `pinning.passthrough_total{status}` — counter.
- `killswitch.active` — gauge.
- `redis.available` — gauge.
- `attestation.verify_total{outcome}` — counter.

## 8. SIEM emitter direct path

The compliance proxy ships an in-process SIEM forwarder (`packages/compliance-proxy/internal/siem/forwarder.go`) that the audit batch writer tees into. It classifies each event via `ClassifyAuditEvent` (`internal/siem/classify.go:17`) — only **blocked** events produce a non-empty classification:

- block + reason `rate_limited`    → `traffic.rate_limited`
- block + reason `budget_exceeded` → `traffic.budget_exceeded`
- block + any other reason         → `traffic.request_blocked`
- non-block events                 → no classification, not forwarded

`FilterAuditEvents` then keeps only the classifications listed in the configured allowlist before delivering to the pluggable `Sink` (HTTPS / syslog / file). Other events continue to flow through MQ → Hub → SIEM bridge.

Cross-ref `siem-bridge-architecture.md` for the broader SIEM picture.

## 9. Operational concerns

- **Cert cache hot key** — single-tenant heavy traffic to one domain heats L1 effectively; multi-domain workloads benefit from larger L1 (configurable).
- **Pinning auto-exemption can hide a misconfiguration** — pf rules / agent settings may be wrong. Periodic review of auto-exemptions is recommended; the UI flags new ones.
- **KMS latency spike** — if KMS pauses (e.g., key rotation in progress), cert mint latency spikes; the L1+L2 cache absorbs most of it but a cold cache during KMS pause is slow.

## 10. Sources

- `packages/compliance-proxy/internal/` — all subsystems above.
- `packages/compliance-proxy/internal/tls/cache/` — cert cache primitives (L1 LRU + L2 Redis tier).
- `packages/compliance-proxy/internal/siem/` — SIEM emitter (classifier + formatter + forwarder + sinks).
- `packages/compliance-proxy/internal/audit/` — local audit producer (MQ writer + NDJSON fallback).
- `packages/shared/policy/domain/` — host + path match engine consumed by `internal/proxy/forward/`.
- `packages/shared/traffic/` — adapter + extract + normalize libraries.
- `packages/shared/transport/mq/` — NATS JetStream producer used by `internal/audit/`.
- `docs/operators/ops/runbooks/compliance-proxy-smoke.md` — operational runbook.

## 11. Cross-references

- `compliance-pipeline-architecture.md` — Tier 1 parent doc (phase model).
- `agent-forwarder-architecture.md` — sibling pipeline.
- `kill-switch-architecture.md` §10 — local kill switch interaction.
- `siem-bridge-architecture.md` — SIEM picture.
- `cache-multi-tier-architecture.md` — cert cache in the multi-tier catalog.
