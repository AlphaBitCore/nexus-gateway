---
doc: shared-package-architecture
area: cross-cutting
service: shared
tier: 1
updated: 2026-05-20
---

# `packages/shared` Architecture

> **Tier 1 architecture doc.** Read this before adding a new subpackage to `packages/shared`, taking a new dependency in any existing subpackage, or making an API change to a subpackage already consumed by a released Agent binary.

The `shared` module is the common Go library used by Nexus Hub, Control Plane, AI Gateway, Compliance Proxy, and Agent. It encodes the platform's domain primitives so all five Things behave the same way on the wire and in their pipelines.

---

## 1. Boundaries (what `shared` is and is not)

**Is:** the canonical implementation of cross-service concerns (Thing client, config types, hooks, traffic adapters, MQ abstraction, audit, telemetry).

**Is not:** a kitchen sink. Service-specific logic belongs in `packages/<service>/internal/*`, not in `shared`. If only one service ever calls it, it should not live here.

## 2. Subpackage catalogue (8 buckets)

`packages/shared/` is organised into **8 top-level buckets**, each holding one or more subpackages. Reference for the canonical tree: `find packages/shared -maxdepth 3 -type d`. The buckets are:

```
packages/shared/
  audit/          (1 leaf)        — audit event schema + emitters
  core/           (5 leaves)      — diag, logging, metrics, telemetry, bootenv
  identity/       (3 leaves)      — iam, pkce, rstokenauth
  policy/         (7 leaves)      — hooks/*, rulepack, domain, decision, pipeline, payloadcapture, device
  schemas/        (5 leaves)      — configtypes/*, configkey, credstate, domain, thingtype
  storage/        (6 leaves)      — configcache, configstore, cacheconfig, redisfactory, spillstore/*, spillupload
  traffic/        (2 leaves)      — adapters/*, classify (+ root files: adapter.go, detect.go, markers.go, phasetimer.go, tracing.go)
  transport/      (10 leaves)     — thingclient, mq, normalize/*, wirerewrite, streaming/*, tlsbump, responseio, bufconn, http, configloader, inputstaging
```

Each row below names a real subpackage path. **When you add or remove a subpackage, update this catalogue in the same PR — the trigger lockstep below (§3) does not catch catalogue drift.**

### `audit/`

| Subpackage | Purpose | Consumers |
|---|---|---|
| `audit/` | Audit event schema + emitters | AI GW, Compliance Proxy, Agent, Hub |

### `core/`

| Subpackage | Purpose | Consumers |
|---|---|---|
| `core/bootenv/` | Env-file loader (repo-root `.env` + `tests/.env.<target>` loader contract) | All five services |
| `core/diag/` | Diag event triage; hosts the `slog` → diag pipeline sink (`slog_sink.go`) | All five services |
| `core/diag/runtimeintrospect/` | `/runtime/*` introspection endpoints (Echo handler) | All four server services |
| `core/logging/` | `slog`-based logger factory + file sink | All five services |
| `core/metrics/` | Per-service Prometheus metric registration helpers (with subdirs `instruments/`, `platform/`, `registry/`) | Every server service |
| `core/telemetry/` | OTel setup + span helpers | All five services |

### `identity/`

| Subpackage | Purpose | Consumers |
|---|---|---|
| `identity/iam/` | NRN builder + action catalog | CP, Hub |
| `identity/pkce/` | PKCE primitives | CP authserver |
| `identity/rstokenauth/` | RS256 token issue/verify | CP, Hub |

### `policy/`

| Subpackage | Purpose | Consumers |
|---|---|---|
| `policy/hooks/` (subdirs `access/`, `builtins/`, `contract/`, `core/`, `ratelimit/`, `validators/`, `webhook/`) | Hook interface, built-in implementations (PII, keyword, rate-limit, …), pipeline, `HookConfigRow` + `BuildHookConfig` | AI GW, Compliance Proxy, Agent |
| `policy/rulepack/` | Server-side rule-pack types (built-in keyword / PII rule bundles) | Hub, AI GW |
| `policy/domain/` | Domain glob / regex matcher | Compliance Proxy, Agent |
| `policy/decision/` | Allow / inspect / passthrough decision primitives consumed by per-domain policy lookup | Agent, Compliance Proxy |
| `policy/pipeline/` | Shared hook-pipeline orchestration primitives | AI GW, Compliance Proxy, Agent |
| `policy/payloadcapture/` | Body capture primitives for audit + spillstore upload | AI GW, Agent, Compliance Proxy |
| `policy/device/` | Device-level predicate evaluator | Agent |

### `schemas/`

| Subpackage | Purpose | Consumers |
|---|---|---|
| `schemas/configtypes/` (subdirs `enums/`, `identity/`, `interception/`, `observability/`, `policy/`) | Hand-maintained Go structs mirroring Prisma models | All five services |
| `schemas/configkey/` | Cat A / Cat B shadow-key constants + `ValidByThingType` + `TypedRegistry` | All five services |
| `schemas/credstate/` | Per-credential runtime-state constants (Redis keys, circuit/health enums, default thresholds) — single source of truth | AI GW, CP, Hub |
| `schemas/domain/` | Domain matcher input types shared between policy/domain and consumer code | Compliance Proxy, Agent |
| `schemas/thingtype/` | Canonical Thing-type enum + helpers | All five services |

### `storage/`

| Subpackage | Purpose | Consumers |
|---|---|---|
| `storage/configcache/` | In-process snapshot of shadow keys (Valkey-backed) | All five services |
| `storage/configstore/` | DB-backed loader interface used by per-service configloaders | AI GW, Compliance Proxy |
| `storage/cacheconfig/` | E38 prompt-cache config blob types (Cat B keys) | AI GW |
| `storage/redisfactory/` | Common Valkey/Redis client factory (TLS, sentinel, observability) | All Valkey-using services |
| `storage/spillstore/` (subdirs `s3/`, `localfs/`, `spillfactory/`) | Body overflow storage interface + S3 / localfs drivers | Hub presign + AI GW + Compliance Proxy + Agent uploader |
| `storage/spillupload/` | Agent-side spill uploader consuming Hub-issued presigned URLs | Agent |

### `traffic/`

| Subpackage | Purpose | Consumers |
|---|---|---|
| `traffic/` (root files: `adapter.go`, `detect.go`, `markers.go`, `phasetimer.go`, `tracing.go`) | Adapter interface, body extraction, Nexus response markers, phase timing | AI GW, Compliance Proxy, Agent |
| `traffic/adapters/` (subdirs `api/`, `generic/`, `ide/`, `web/`) | Provider format adapters (Tier-1 IDE + Web + API surface) | AI GW, Compliance Proxy, Agent |
| `traffic/classify/` | Traffic classification helpers (kind, detected-spec inference) | Compliance Proxy, Hub normalize pipeline |

### `transport/`

| Subpackage | Purpose | Consumers |
|---|---|---|
| `transport/thingclient/` | Connect, heartbeat, pull config, report state | All five services |
| `transport/mq/` | MQ interface + `natsmq/` JetStream driver | All five services |
| `transport/normalize/` (subdirs `codecs/`, `core/`, `extract/`) | Canonical request/response shape framework + `extract/NonJSONDetector` | AI GW, Compliance Proxy, Hub |
| `transport/wirerewrite/` | Byte-level wire rewriter (Anthropic / Bedrock cache_control strip, OpenAI / Azure / DeepSeek / xAI etc. field-order normalisation) with circuit breaker; runs before upstream send. **Distinct from `normalize/`.** | AI GW, CP (admin cache preview) |
| `transport/streaming/` (subdirs `extract/`, `policy/`) | SSE parsing, incremental JSON, buffer mgmt | AI GW, Compliance Proxy, Agent |
| `transport/tlsbump/` | TLS-bump primitives (SNI peek, cert minting hook, forward path) | Compliance Proxy, Agent, policy/domain |
| `transport/responseio/` | JSON / SSE / error response writers | AI GW, CP, Hub |
| `transport/bufconn/` | In-memory net.Conn for proxy/bridge testability | Agent, Compliance Proxy |
| `transport/http/` | Telemetry-aware HTTP client + helpers (retry, OTel, logging, query-param value redaction) | AI GW, Compliance Proxy, Agent |
| `transport/configloader/` | Service-side config loader plumbing on top of `storage/configstore/` | AI GW, Compliance Proxy |
| `transport/inputstaging/` | Request-side input staging primitives (chunked body buffering) | AI GW, Compliance Proxy |

## 3. Dependency tiers (binding)

Two tiers govern third-party imports:

### Core (always allowed)

- `log/slog` (stdlib)
- `pgx`
- `prometheus/client_golang`
- `tidwall/gjson`, `tidwall/sjson`
- `gopkg.in/yaml.v3`
- `go.opentelemetry.io/otel*`
- `golang.org/x/net`, `golang.org/x/sync`

Foundational pieces every shared subpackage may pull. The intent is to keep the **root** `shared` module buildable on Go environments without external service dependencies.

### Driver-scoped (allowed only in the listed subpackage)

| Dependency | Subpackage(s) |
|---|---|
| `nats-io/nats.go` | `transport/mq/natsmq` |
| `aws-sdk-go-v2*` | `storage/spillstore/s3` |
| `redis/go-redis/v9` | `storage/cacheconfig/`, `storage/configcache/`, `storage/redisfactory/`, `storage/spillstore/`, quota counters |
| `coder/websocket` | `transport/thingclient/` |
| `golang-jwt/jwt/v5` | `identity/rstokenauth/`, auth + token-verifier subpackages |
| `hashicorp/golang-lru/v2` | cache / cert-cache subpackages |
| `bits-and-blooms/bloom/v3` | compliance dedup, audit dedup |
| `labstack/echo/v4` | only when a shared subpackage exposes a mountable Handler (`core/diag/runtimeintrospect/`) |
| `klauspost/compress` | `transport/normalize/`, `transport/tlsbump/` — HTTP-level gzip on bumped proxy traffic |
| `google.golang.org/protobuf` | `traffic/adapters/ide/cursor/`, `transport/normalize/extract/` — Cursor IDE Tier-1 protobuf framing |

A new dependency outside this set requires explicit user approval (review for licence, maintenance, surface area, and whether it belongs in `shared` at all). The CLAUDE.md mirror of this table is the binding source; this row list must stay in lockstep.

## 4. Architectural intent — umbrella `go.mod` (no split)

`shared` is a single Go module. Splitting it into `shared/core` (zero-infra-deps for the Agent) + `shared/infra` (server-side heavy deps) has been considered and is **not** the chosen shape:

- The dep-tier rule (§3) + code review enforces the property a split would enforce at module-boundary level (Agent's transitive heavy deps come from `thingclient/` → `mq/` and `telemetry/`, which is accepted).
- The binary-size argument is theoretical until a customer-visible budget surfaces.

Driver-scoped deps living in the umbrella `go.mod` is therefore the **chosen** shape, not a known shortcut. Re-opening this question requires a concrete trigger (binary-size budget, supply-chain SBOM constraint, contributor friction).

Don't fight the umbrella `go.mod` in a feature PR; do follow the dep-tier rule.

## 5. API stability (binding)

Once a `shared` subpackage has shipped in a **released Agent binary**, all changes to its public API must be **additive** (new exported symbols, new methods on existing types, new optional fields in structs). Breaking changes — renamed symbols, removed methods, changed signatures, repurposed fields — are forbidden.

Reason: in-the-wild Agents pin against a specific `shared` import; breaking the API forces an Agent re-rollout. We treat the Agent binary fleet as a long-running consumer with its own update cadence.

The same rule applies to MQ envelope formats, audit event JSON shapes, and HTTP API request/response shapes consumed by Agents.

## 6. Conventions inside `shared`

- **Package naming** — lowercase, short, no underscores (`configtypes`, `configcache`, `configloader`). Package names must not stutter (`hooks.Hook` ok; `hooks.HooksRegistry` stutters).
- **Logging** — `*slog.Logger` passed as constructor parameter, not a global.
- **Metrics** — `promauto` for registration. Constructors that register metrics accept a `namespace string` parameter.
- **Concurrency** — `sync.Mutex` / `sync.RWMutex` for shared state; `atomic.Pointer` for hot-swappable snapshots; `sync.Pool` for high-frequency allocations.
- **Errors** — return errors, do not panic in library code. Sentinel errors via `errors.New`; wrap with `fmt.Errorf("context: %w", err)`.
- **Tests** — `go test -race -count=1`; table-driven where appropriate.
- **No `replace` directives** in subpackage `go.mod` files; CI enforces this.
- **No `sqlc`** — hand-written SQL + Prisma → Go codegen. Subpackages that need DB types import from `configtypes`.

## 7. The SlogSink DI invariant (binding)

After wiring `SlogSink` + `slog.SetDefault(...)`, you MUST also reassign any module-scope `logger` that was captured before the default was set:

```go
logger = slog.Default()
```

Otherwise DI-injected loggers silently bypass the diag pipeline and the live introspection surface looks empty.

## 8. Server / client symmetry

Several subpackages have server and client sides (Thing client ↔ Hub Thing registry; MQ producers ↔ consumers; audit emitters ↔ Hub audit sink). When you change a wire format, change both sides in the same PR — asymmetric updates can break the pipeline (e.g., an empty-string sender vs a CHECK-constrained receiver stalls audit silently).

## 9. Sources

- `packages/shared/` (the whole tree).
- CLAUDE.md "Go" section — Go conventions, dependency tiers (canonical statement; mirrored in §3).

## 10. Cross-references

- `mq-architecture.md` — MQ subpackage in detail.
- `cache-multi-tier-architecture.md` — cache subpackage in detail.
- `thing-config-sync-architecture.md` — `thingclient` and `configreconcile` in detail.
- `audit-pipeline-architecture.md` — `audit` subpackage in detail.
- `agent-forwarder-architecture.md` — `traffic/*` and `streaming/*` consumption.
