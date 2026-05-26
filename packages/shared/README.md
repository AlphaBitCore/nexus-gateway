# @nexus-gateway/shared (Go)

Shared Go packages consumed by every Go service (`nexus-hub`,
`control-plane`, `ai-gateway`, `compliance-proxy`, `agent`). Linked via
`go.work` at the repo root using `replace` directives — every consumer
pins `v0.0.0` and points at this sibling directory, never at a real
pseudo-version (see CLAUDE.md "replace directives — workspace-sibling
contract" binding).

## Stability contract

API-stable additive-only once shipped in a released Agent binary. A
removed symbol is a binary-breaking change for the Agent fleet.

## Layout

The tree is grouped into seven domain buckets so the 36 subpackages
that used to live flat at the root are now discoverable. Three
historically-load-bearing trees stay at the top level for stable
import paths.

```
packages/shared/
├── audit/          (4 files)   — audit event types + body helpers (kept at root: cross-service consumer)
├── compliance/     (15 files)  — hook executor / pipeline engine (kept at root: heavily referenced)
├── traffic/        (19 files + adapters/)  — traffic_event row builder + per-provider Tier-1 adapters (kept at root)
│
├── runtime/        (7)  — process-lifetime observability + boot primitives
│   ├── bootenv/             .env autoloader (dev) + systemd-aware no-op (prod). Every service main.go calls LoadFromRepoRoot first.
│   ├── logging/             slog adapter + level swap
│   ├── diag/                diagnostic event sink (in-process tee)
│   ├── telemetry/           OpenTelemetry setup
│   ├── metrics/             Prometheus helpers (NewCounter / NewHistogram / namespaced registry)
│   ├── opsmetrics/          Ops dashboard metric primitives (per-service ops surface)
│   └── runtimeintrospect/   /debug/runtime HTTP introspection surface
│
├── transport/      (9)  — network I/O primitives and request-shape transforms
│   ├── httpclient/          Outbound HTTP transport (caller-ID, retries, request-id propagation)
│   ├── mq/                  NATS / JetStream client wrapper
│   ├── thingclient/         Hub-facing Thing client (WS primary, HTTP fallback) — every server service registers via this
│   ├── tlsbump/             TLS-bump engine shared by Compliance Proxy and the macOS Agent
│   ├── streaming/           SSE / NDJSON accumulator + stream-policy helpers
│   ├── bufconn/             In-memory net.Conn for tests / Wails bindings
│   ├── responseio/          Response-body reader helpers
│   ├── normalize/           Canonical request/response shape + extractors (Tier-2 detector framework)
│   └── wirerewrite/         Byte-level upstream wire rewriter (Anthropic / OpenAI / Azure / DeepSeek rules)
│
├── storage/        (5)  — durable + cache layers
│   ├── spillstore/          Out-of-band body storage (S3 / localfs / spillfactory)
│   ├── spillupload/         Upload helpers paired with spillstore
│   ├── cacheconfig/         Cache config schema
│   ├── configcache/         Config snapshot cache (atomic.Pointer swap)
│   └── configstore/         KV config store abstraction
│
├── security/       (3)  — auth + crypto primitives
│   ├── iam/                 NRN building, policy types, RBAC verbs
│   ├── pkce/                OAuth PKCE flow helpers
│   └── rstokenauth/         X-RS-Token verifier (cross-service token auth)
│
├── policy/         (5)  — hook rules + policy data primitives
│   ├── hooks/               Hook registry + built-in hooks (pii, keyword, rate-limit, …)
│   ├── rulepack/            Versioned rule-pack loader + starter packs
│   ├── payloadcapture/      Payload spill helper invoked by capture hooks
│   ├── domainpolicy/        Per-domain policy resolution
│   └── devicepredicate/     Device-matching predicate evaluator
│
└── schemas/        (4)  — config + type definitions
    ├── configtypes/         Cat-B config-key schemas (allowlists, hooks, observability, killswitch, …)
    ├── thingtype/           Thing model schemas (kind, capabilities)
    ├── credstate/           Credential state machine schema
    └── domain/              DNS / domain matching primitives
```

## Dependency tier policy

| Tier | Where | Examples |
|------|-------|----------|
| **Core (always)** | The root `shared/` tree | `log/slog`, `pgx`, `prometheus/client_golang`, `tidwall/gjson`+`sjson`, `gopkg.in/yaml.v3`, `go.opentelemetry.io/otel*`, `golang.org/x/{net,sync}` |
| **Driver-scoped** | Lives only inside the relevant subpackage | `nats.go` → `transport/mq/natsmq`; `aws-sdk-go-v2` → `storage/spillstore/s3`; `redis/go-redis/v9` → `storage/configcache`, `storage/spillstore/redis`; `coder/websocket` → `transport/thingclient`; `golang-jwt/v5` → `security/iam`; `golang-lru/v2` → `storage/configcache`, cert-cache; `bloom/v3` → `compliance`; `klauspost/compress` → `transport/normalize`, `transport/tlsbump`; `google.golang.org/protobuf` → `traffic/adapters/cursor/`, `transport/normalize/extract/`; `joho/godotenv` → `runtime/bootenv`; `labstack/echo/v4` → any subpackage that exposes a Handler |

Adding a new dependency to the core tier requires explicit user
approval. The intent is that the root `shared` builds with a tiny dep
set; driver-scoped deps living in the umbrella `go.mod` is a known
shortcut that we'd split into per-module `go.mod`s when bandwidth
permits.

## Build / test

```bash
go build ./packages/shared/...
go test -race -count=1 ./packages/shared/...
```

There is no separate `make` target; `make build-all` includes shared
transitively via each service's build.

## Architecture references

- `docs/developers/workflow/conventions.md` — module-path / replace-directive rules.
- `docs/developers/architecture/cross-cutting/foundation/thing-model.md` — what `transport/thingclient` actually does at the wire level.
- `docs/developers/architecture/cross-cutting/shared/shared-package-architecture.md` — the canonical shared-package doc.
