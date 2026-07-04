# Nexus Gateway

[![CI](https://github.com/AlphaBitCore/nexus-gateway/actions/workflows/ci.yml/badge.svg?branch=main)](.github/workflows/ci.yml)
[![Go CI](https://github.com/AlphaBitCore/nexus-gateway/actions/workflows/go-ci.yml/badge.svg?branch=main)](.github/workflows/go-ci.yml)
[![Coverage gate](https://img.shields.io/badge/coverage-%E2%89%A595%25%20per%20package-brightgreen)](./scripts/check-go-coverage.sh)
[![Status: 1.1.0](https://img.shields.io/badge/status-1.1.0-brightgreen)](./CHANGELOG.md)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](./LICENSE)

> **Make AI safe to use across the enterprise.**

> [!IMPORTANT]
> **Upgrading from 1.0.x?** This **1.1.0** release changes how captured traffic
> bodies are stored — `traffic_event_payload.inline_*_body` is now raw `BYTEA`
> — and collapses the hook `onMatch` model. Fresh installs (the AMI appliance, or `prisma db push` on an empty
> database) need nothing. **Existing deployments that retain traffic history
> must run the one-time body re-encode migration, and direct `traffic_event`
> database consumers must update their readers.** Full migration steps:
> **[CHANGELOG → 1.1.0](./CHANGELOG.md)**.

Nexus Gateway intercepts enterprise LLM traffic at three layers and runs all of it through one compliance engine, one audit pipeline, and one control plane.

| Mode | Where it intercepts | Code |
|---|---|---|
| 🔑 **AI Gateway** | SDK layer — virtual keys on `/v1/chat/*`, `/v1/responses`, `/v1/embeddings`, `/v1/messages` | `packages/ai-gateway/` |
| 🌐 **Compliance Proxy** | Network layer — transparent TLS bump (`CONNECT` + MITM) | `packages/compliance-proxy/` |
| 💻 **Desktop Agent** | OS layer — macOS, Linux, and Windows GA | `packages/agent/platform/{darwin,linux,windows}/` |

The three pipes are independent: AI Gateway, Compliance Proxy, and Agent each run the **full hooks pipeline on their own traffic** (`packages/shared/policy/hooks/`, plus the per-service compliance pipeline — e.g. `packages/agent/internal/compliance/pipeline.go`). The Agent always egresses directly to the upstream provider — it does **not** care whether enterprise network policy then routes that traffic through the Compliance Proxy.

When it does — Agent stamps an Ed25519-signed `X-Nexus-Attestation` header on the outbound request (`packages/agent/internal/identity/attestation/`). The Compliance Proxy peeks this header *before* the TLS bump (`packages/shared/transport/tlsbump/forward_handler.go`); if the signature verifies, the CONNECT becomes pure passthrough — no MITM, no hooks, no audit on that flow, since the Agent already ran them.

---

## See it in action

The web **Console** to configure and inspect everything, the captured-traffic
drawer that shows each AI request end to end, and the **Desktop Agent** on the
endpoint:

| Console — dashboard | Captured AI request — payloads | Desktop Agent |
|---|---|---|
| ![Console dashboard](docs/assets/screenshots/console/overview/dashboard.png) | ![Captured request payloads](docs/assets/screenshots/console/overview/traffic-detail-payloads.png) | ![Desktop Agent](docs/assets/screenshots/agent/overview.png) |

**▶ [Full product tour](docs/users/product/tour.md)** — Console, Desktop Agent, and Chat with Nexus, screen by screen.

---

## What Nexus does

### 🔁 Write once in OpenAI shape, route to 20 in-tree adapter codecs

Applications speak the OpenAI SDK. Nexus normalises every request to a canonical OpenAI shape, then translates wire format on the way to the actual provider. Shipped adapter codecs today (`packages/ai-gateway/internal/providers/specs/`):

- **First-class codecs (11):** `openai`, `anthropic`, `gemini`, `vertex`, `azure`, `bedrock`, `cohere`, `minimax`, `glm`, `replicate`, `voyage`.
- **OpenAI-compatible passthrough (9):** `deepseek`, `moonshot`, `mistral`, `groq`, `fireworks`, `together`, `perplexity`, `xai`, `huggingface` — all under `packages/ai-gateway/internal/providers/specs/compat/`.

Reasoning tokens, function calls, vision inputs, structured outputs are carried through the translation. Adding a new provider is a documented procedure under `.claude/skills/add-provider-adapter/`.

### 🧊 Multi-tier cache

- **Exact-match response cache** — Valkey-backed, Redis-wire-compatible.
- **Provider-native cache accounting** — surfaces Anthropic `cached_tokens` and Gemini `cachedContentTokenCount` in billing when the provider reports them.
- **Semantic vector cache** via the `valkey-search` module — `packages/ai-gateway/internal/cache/semantic/` (lookup, writer, client, circuit breaker, singleflight, poison guard, index lifecycle).
- **In-flight singleflight** — concurrent identical prompts fold into one upstream call.

### 💰 Cost & quota control

- **Multi-axis quotas** — per organization, per virtual key, per provider, per model. Each axis has its own budget and sliding-window enforcement.
- **Token-based or USD-based budgets.**
- **Hard limits and soft limits** — soft fires an alert; hard rejects with 429.
- **Real-time accounting** — counters update on every traffic event, no batch lag.
- **Routing strategies** in `packages/ai-gateway/internal/routing/strategies/`: `single`, `fallback`, `loadbalance`, `conditional`, `absplit`, `policy`, `smart`.

### 🛡 Compliance pipeline

PII detection · data classification · keyword filtering · content safety · rate limiting · IP allowlists · request-size validation · webhook forwarders · per-stage audit (request hooks and response hooks recorded independently) · body capture (256 KiB inline + spillstore for the rest, see `packages/shared/storage/spillstore/`) · SIEM forwarder (`packages/control-plane/internal/observability/siem/` and `packages/nexus-hub/internal/observability/siem/`) · three-tier kill switch · emergency passthrough (`bypassHooks` / `bypassCache` / `bypassNormalize`).

### 🎨 Modalities

Chat · Embeddings · Structured outputs · Function / tool calling · Vision input · Reasoning tokens. Multimodal in development.

### 🏢 Enterprise governance

- **IAM** — RBAC + ABAC with an NRN resource model (`packages/shared/identity/iam/`).
- **Virtual keys** with per-key model scope.
- **OIDC federation** with JIT user provisioning (`packages/control-plane/internal/identity/authserver/login/oidc.go`, JIT flag in `scim_store.go`).
- **Organization / project hierarchy** with per-org quota.
- **Credential vault** — AES-256-GCM (`packages/control-plane/internal/platform/crypto/aes_gcm.go`, `packages/ai-gateway/internal/credentials/decrypt/decrypt.go`) with key rotation.
- **Agent fleet management** — Hub CA, Thing-based config sync, drift detection.

---

## Performance

Nexus is designed so that the compliance layer adds as little latency as possible to the request path. The key architectural decisions that make this work:

**Audit pipeline is fully async.** Request/response bodies are queued in-memory, shipped to NATS JetStream, and bulk-inserted into Postgres via `COPY` — none of this blocks the response to the caller. Captured bodies are stored as raw `BYTEA`, so PostgreSQL skips per-row parse and validation overhead and avoids base64 size inflation. Under load, the audit pipeline runs at ~0% throughput cost even with full body capture enabled.

**Compliance scanning uses Vectorscan.** The hook pipeline runs a SIMD-accelerated multi-pattern scanner (Vectorscan) instead of sequential regex evaluation. All 423 rules scan in a single pass over the payload. Scan cost scales with payload size — roughly 30% overhead at short context (128 tokens), up to 55% at long context (12k tokens). For applications where compliance scanning overhead is a concern, the scan pipeline is configurable per-route.

**Upstream connection pool is sized for throughput.** The default pool is 5,000 connections per upstream target, eliminating connection establishment as a bottleneck under sustained load.

**Quota accounting is write-behind.** Per-request quota costs accumulate in-process and flush to Redis on a 250ms interval, removing the synchronous Redis round-trip from the hot path. Configurable back to synchronous mode (`NEXUS_QUOTA_WRITE_BEHIND=0`) for strict accounting requirements.

### Benchmarking & load-testing toolkit

The numbers above are produced and re-verified by a three-repo toolkit, each maintained as its own standalone repository. The load generator was previously the in-tree `tools/loadtest` and was extracted to `nexus-loadtest` (see [CHANGELOG](./CHANGELOG.md)).

| Repo | Role | Key docs |
|---|---|---|
| **[llm-gateway-benchmark](https://github.com/AlphaBitCore/llm-gateway-benchmark)** | On-demand AWS rig (CloudFormation + Ansible) that benchmarks Nexus head-to-head against 5 other gateways (Bifrost, LiteLLM, Kong, Portkey, TensorZero) — each isolated on its own box, all hitting one shared mock upstream. Deploy to run, `delete-stack` to tear down. | [ARCHITECTURE](https://github.com/AlphaBitCore/llm-gateway-benchmark/blob/main/ARCHITECTURE.md) · [LOADTEST-RUNBOOK](https://github.com/AlphaBitCore/llm-gateway-benchmark/blob/main/docs/LOADTEST-RUNBOOK.md) · [CONTROL-BOX-RUNBOOK](https://github.com/AlphaBitCore/llm-gateway-benchmark/blob/main/docs/CONTROL-BOX-RUNBOOK.md) |
| **[nexus-mock-provider](https://github.com/AlphaBitCore/nexus-mock-provider)** | High-performance mock upstream that speaks the real OpenAI / Gemini / Anthropic wire formats (streaming + non-streaming) and echoes requests back with plausible token usage. Removes the real, paid, rate-limited provider from the measurement so you benchmark the gateway, not the model. Listens on `:3062`. | [README](https://github.com/AlphaBitCore/nexus-mock-provider/blob/main/README.md) · [CONFIGURE-NEXUS](https://github.com/AlphaBitCore/nexus-mock-provider/blob/main/CONFIGURE-NEXUS.md) |
| **[nexus-loadtest](https://github.com/AlphaBitCore/nexus-loadtest)** | Scenario-driven load generator for any OpenAI- or Anthropic-compatible endpoint. Simulates realistic, weighted, multi-turn traffic and reports TTFT, inter-token latency, and token throughput. Scales to tens of thousands of concurrent virtual users from one host. | [README](https://github.com/AlphaBitCore/nexus-loadtest/blob/main/README.md) · [DESIGN](https://github.com/AlphaBitCore/nexus-loadtest/blob/main/DESIGN.md) |

#### Run a quick local benchmark (single gateway)

Measure your local AI Gateway (`:3050`) against the mock upstream — no real provider, no cost:

1. **Start the mock upstream** (in a `nexus-mock-provider` checkout):
   ```bash
   make run            # serves :3062, all three specs (OpenAI / Gemini / Anthropic)
   ```
2. **Point Nexus at the mock.** Add a provider credential / routing rule whose upstream base URL is `http://localhost:3062` (the mock accepts any API key). See [`CONFIGURE-NEXUS.md`](https://github.com/AlphaBitCore/nexus-mock-provider/blob/main/CONFIGURE-NEXUS.md) for the exact routing setup.
3. **Run the load generator** (in a `nexus-loadtest` checkout) against the gateway, authenticating with a Nexus virtual key:
   ```bash
   go run ./cmd/loadtest -config profiles/realistic.json \
     -target http://localhost:3050 -vk <your-virtual-key> -out runs/
   ```
4. **Read the report** at `runs/<run-id>/` — TTFT, inter-token latency, throughput, and per-tier breakdown. Use `-compare` against an earlier `summary.json` to gate regressions.

#### Run the full head-to-head matrix (AWS)

To compare Nexus against the other gateways on isolated boxes, use the `llm-gateway-benchmark` rig (binaries ship prebuilt in `artifacts/` — nothing compiles on-box):

```bash
# 1. Deploy infra
aws cloudformation deploy --stack-name nexus-perf-matrix \
  --template-file cloudformation/perf-matrix-stack.yaml --capabilities CAPABILITY_IAM \
  --parameter-overrides KeyName=<your-key> AdminCidr=<your.ip>/32
# 2. Provision every box (host-native)
scripts/gen-inventory.sh nexus-perf-matrix ~/.ssh/<your-key>.pem <region>
cd ansible && ansible-playbook -i inventory.ini site.yml
# 3. Run the benchmark (one gateway, all tiers → a report each)
GATEWAY=nexus scripts/bench/run-tiers.sh
# 4. Tear down when idle (on-demand, cost control)
aws cloudformation delete-stack --stack-name nexus-perf-matrix
```

Compare gateways by **TTFT delta** — the shared mock's latency cancels out, so the difference is the gateway's own overhead. Full procedure and the report-validity gate are in the rig's [LOADTEST-RUNBOOK](https://github.com/AlphaBitCore/llm-gateway-benchmark/blob/main/docs/LOADTEST-RUNBOOK.md).

---

## Architecture in one minute

Five Go services + one React control console. The diagram below shows **only the traffic plane** — the three independent intercept pipes and where each one egresses. Control plane (Hub-centric) and storage are summarized in the component table immediately after.

```mermaid
flowchart TB
    SDK["SDK app<br/>(OpenAI SDK)"]
    HTTPS["HTTPS app<br/>(network-proxied)"]
    Endpoint["Developer endpoint<br/>(Cursor / Claude Code / …)"]

    AIGW["AI Gateway :3050<br/>routing · cache · quota<br/>+ hooks pipeline"]
    CPProxy["Compliance Proxy :3128<br/>MITM TLS<br/>+ hooks pipeline"]
    Agent["Desktop Agent · local<br/>OS-level intercept<br/>+ hooks pipeline"]

    Provider["LLM Provider<br/>(OpenAI / Anthropic / Gemini / …)"]

    SDK ==>|"/v1 + VK"| AIGW
    HTTPS ==>|HTTPS via proxy| CPProxy
    Endpoint ==>|OS-level capture| Agent

    AIGW ==> Provider
    CPProxy ==> Provider
    Agent ==> Provider

    Agent -. "X-Nexus-Attestation verified<br/>→ passthrough" .-> CPProxy
```

The lateral dotted arrow is the **attestation handoff**: the Agent always egresses directly, but when enterprise network policy happens to route Agent traffic through the Compliance Proxy, the Agent's Ed25519-signed `X-Nexus-Attestation` header (`packages/agent/internal/identity/attestation/`) is verified at TLS-bump time (`packages/shared/transport/tlsbump/forward_handler.go`); on success the CONNECT becomes pure passthrough — no MITM, no hooks, no audit on that flow, since the Agent already ran them on its end.

**Control plane (out-of-band).** All four Go services register with **Nexus Hub** as Things via `packages/shared/transport/thingclient/` (WebSocket primary, HTTP fallback) and pull configuration from the Hub's device shadow on boot and on change-signal — the Hub never pushes full state. The Control Plane admin API (`:3001`) and the React UI (`:3000`) sit alongside, talking to the Hub the same way.

| Component | Port | Code |
|---|---|---|
| **Nexus Hub** | 3060 | `packages/nexus-hub/` — Thing Registry, Device Shadow, config sync, jobs, agent CA, SIEM bridge |
| **Control Plane** | 3001 | `packages/control-plane/` (Echo) — admin API / BFF, IAM, SSO, analytics |
| **AI Gateway** | 3050 | `packages/ai-gateway/` — `/v1` AI traffic, provider adapters, routing, quota |
| **Compliance Proxy** | 3128 | `packages/compliance-proxy/` — CONNECT, MITM, compliance pipeline |
| **Agent** | local | `packages/agent/` — macOS intercepts via the `NETransparentProxyProvider` system extension (`packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/`), the sole macOS intercept path; Linux uses `iptables`; Windows uses the `NexusWFP` kernel driver (Windows Filtering Platform, transparent TCP connect-redirect). macOS, Linux, and Windows are GA. |
| **Control Plane UI** | 3000 | `packages/control-plane-ui/` — React + Vite + TypeScript |

**Storage stack**

- **PostgreSQL 16** — durable storage. Prisma schema in `tools/db-migrate/` is the source of truth for dev-time migrations; runtime code reads via hand-written SQL + `pgx` (no `sqlc`).
- **Valkey 8** — Redis-wire-compatible, pinned to `valkey/valkey-bundle:8-trixie` in `docker-compose.yml` for BSD-license parity; the `valkey-search` module ships in the bundle image and backs the semantic vector cache. Pure cache only — no pub/sub.
- **NATS JetStream** — event streaming and Hub coordination via `packages/shared/transport/mq/`.

> **Roadmap — pluggable traffic storage.** The traffic / observability storage
> layer is being reworked into an operator-selectable backend. The first step
> makes the high-frequency `traffic_event` store switchable between
> **PostgreSQL and ClickHouse**, so the audit firehose can move off the
> single-box disk-write wall at high request rates while transactional OLTP
> state stays on PostgreSQL. In active development; not yet shipped.

---

## Deployment

| Form factor | How | Status |
|---|---|---|
| **AWS Marketplace AMI / single-instance appliance** | `cd nexus-ami && ./build.sh` — bakes binaries + UI + Prisma + nginx + Postgres + Valkey + NATS into one AL2023 image via Packer | [`nexus-ami/README.md`](./nexus-ami/README.md) for build steps, [`docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md`](./docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md) for design |
| **Local development** | docker-compose + `./scripts/dev-start.sh` (Postgres + Valkey + NATS) and per-service `go run ./cmd/<svc>/` | See **Quick start** below |
| **Docker (dev / demo)** | `./scripts/compose-init.sh && docker compose -f docker-compose.full.yml --env-file deploy/docker/.env.compose up -d` — full stack from the published `alphabitcore/nexus-*` images | See **Docker images** below |
| **Kubernetes / Helm (production)** | `helm install nexus deploy/helm/nexus-gateway` — 4 service deployments + embedded or external Postgres/Valkey/NATS | [`deploy/helm/nexus-gateway/README.md`](./deploy/helm/nexus-gateway/README.md) |
| **VMware / KVM image / bare-metal appliance** | Reuses the same `install.sh` + `harden.sh` from `nexus-ami/scripts/` under a different Packer builder | Future |

### Docker images

Four images ship to Docker Hub under the `alphabitcore` org (tag-triggered by
`.github/workflows/docker-publish.yml`; dependency stores — Postgres, Valkey,
NATS — are pulled upstream, never rebuilt):

| Image | Contents | Ports |
|---|---|---|
| `alphabitcore/nexus-console` | nginx + control-plane + admin UI (the console) | 80, 443, 3001 |
| `alphabitcore/nexus-hub` | Hub (node lifecycle, config sync, MQ consumers) | 3060 |
| `alphabitcore/nexus-ai-gateway` | AI Gateway — Vectorscan-accelerated compliance scanning | 3050 |
| `alphabitcore/nexus-compliance-proxy` | TLS-bump compliance proxy — Vectorscan-accelerated | 3128, 3040, 9090 |

```bash
docker pull alphabitcore/nexus-ai-gateway:latest
# verify the scanning engine inside the shipped image (never trust "build succeeded"):
docker run --rm --entrypoint hs-selfcheck alphabitcore/nexus-ai-gateway:latest
# single service against your own infra (vars documented in .env.example):
docker run --env-file ai-gateway.env -p 3050:3050 alphabitcore/nexus-ai-gateway:latest
```

The ai-gateway and compliance-proxy images build libhs from source per-arch
with `FAT_RUNTIME=OFF` + `BUILD_AVX512=OFF` (portable baseline; see the
warning below) and hard-fail the build if the engine didn't link or its
self-test fails — a silent pure-Go RE2 fallback image can never ship.

> **⚠ Building from source — Vectorscan binaries are CPU-microarchitecture-specific.**
> The compliance scanner links **libhs (Vectorscan) statically with `FAT_RUNTIME=OFF`** —
> a single-microarchitecture build with no runtime CPU dispatch (this sidesteps a
> known `hs_alloc_scratch` IFUNC dispatch bug). One consequence: **a compiled binary
> is not portable across CPU generations.**
> - Building libhs with `-DBUILD_AVX512VBMI=ON` yields the fastest scanner, but the
>   binary then runs **only** on CPUs with AVX512VBMI (Intel Ice Lake / Sapphire
>   Rapids and newer). On an older CPU it **SIGILLs (illegal instruction)** at the
>   first scan.
> - For an older deployment target (e.g. Cascade Lake / Skylake-SP, no AVX512VBMI),
>   build a **baseline** libhs with no `-DBUILD_AVX512*` flags.
> - Match the target's glibc too — compile on, or in a container matching, the
>   deployment OS (e.g. `amazonlinux:2023`, glibc 2.34).
> - Verify a build: `strings nexus-ai-gateway | grep -c hs_alloc_scratch` (>0 →
>   Vectorscan linked) and `ldd nexus-ai-gateway | grep libhs` (empty → statically
>   linked, correct).
>
> `nexus-ami/build.sh` already builds libhs matched to the AMI's target ISA — this
> only matters when you compile the binaries yourself for a specific host.

---

## Quick start (local development)

### Prerequisites

| Tool | Version | Notes |
|---|---|---|
| Node.js | **20+** | npm workspaces require npm 10+ |
| Go | **1.25+** | All Go modules share `go.work` at the repo root |
| Docker | any recent | Hosts PostgreSQL, Valkey, NATS via `docker-compose.yml` |

### One-shot bootstrap

```bash
./scripts/dev-start.sh
```

The script:

1. Verifies prerequisites (Node 20+, Go 1.25+, Docker, OpenSSL).
2. Auto-creates **repo-root `.env`** from `.env.example` with safe dev defaults for `CHANGE_ME_*` secrets (`INTERNAL_SERVICE_TOKEN`, `ADMIN_KEY_HMAC_SECRET`, `CREDENTIAL_ENCRYPTION_KEY` = `openssl rand -hex 32`, …). All four Go services read this via `packages/shared/core/bootenv/` at boot.
3. Starts PostgreSQL + Valkey + NATS via `docker-compose.yml`.
4. Runs `npm install`.
5. Auto-creates **`tools/db-migrate/.env`** and propagates `CREDENTIAL_ENCRYPTION_KEY` into it so `prisma db seed` can re-encrypt the seed credentials.
6. Applies the Prisma schema (`db push`) and seed under `tools/db-migrate/`.
7. Auto-generates the **Compliance Proxy dev CA** at `packages/compliance-proxy/dev-certs/{ca.crt,ca.key}` so the TLS-bump cert issuer can boot.
8. Prints the per-service `go run … -config <svc>.dev.yaml` commands.
9. Finally starts the Control Plane UI dev server.

Flags:

- `--force-reset` — DESTRUCTIVE: wipe local Postgres / Valkey / NATS volumes + the entire `nexus_gateway` database before re-applying the schema.
- `--no-dev` — bootstrap only; print the per-service commands and exit instead of starting the UI dev server.

### Start the services

Open one terminal per Go service after the bootstrap finishes:

```bash
cd packages/nexus-hub         && go run ./cmd/nexus-hub/         -config nexus-hub.dev.yaml          # port 3060
cd packages/control-plane     && go run ./cmd/control-plane/     -config control-plane.dev.yaml      # port 3001
cd packages/ai-gateway        && go run ./cmd/ai-gateway/        -config ai-gateway.dev.yaml         # port 3050
cd packages/compliance-proxy  && go run ./cmd/compliance-proxy/  -config compliance-proxy.dev.yaml   # port 3128
npm run dev:control-plane-ui                                                                          # port 3000
```

The `-config <svc>.dev.yaml` flag is **required** — each binary defaults to `<svc>.config.yaml`, which is the prod-shape template and is intentionally missing dev-only fields like `hub.id`. Without the flag the service fails fast at boot.

Each Go service tees logs to `packages/<service>/logs/<service>.log` in dev mode (configured in the service's `*.dev.yaml`). Override the path with `LOG_FILE=/path/to/file`.

### Open the console

Browse to <http://localhost:3000> and sign in as the seeded super-admin:

```
admin@nexus.ai / admin123
```

Additional seeded roles (`alice@nexus.ai`, `carol@nexus.ai`, `bob@nexus.ai`, `diana@nexus.ai`) are defined in `tools/db-migrate/seed/seed.ts`.

### Try it

After the stack is up, walk through [`examples/01-hello-world/`](./examples/01-hello-world/) — a 3-minute curl-through-the-gateway demo that ends with you reading the resulting `traffic_event` Postgres row.

### Admin-API debugging from the shell

The Control Plane uses OAuth + PKCE bearer tokens. Helpers wrap the flow:

```bash
cp tests/.env.local.example tests/.env.local      # gitignored; edit if you need to override defaults
source tests/lib/loadenv.sh local                  # picks up tests/.env.local + tests/.env.local.example defaults
source tests/lib/auth.sh

cp_login                                       # idempotent; caches token at /tmp/nexus_test_token_local
cp_curl /api/admin/analytics/cost?groupBy=device
cp_curl -X POST /api/admin/routing-rules -d @rule.json
```

For direct DB inspection in dev:

```bash
docker exec $(docker ps --filter "name=postgres" -q | head -1) \
  psql -U postgres -d nexus_gateway -c "SELECT ..."
```

---

## Repository layout

```
packages/
  nexus-hub/         Go — Thing Registry, Shadow, config sync, jobs, SIEM bridge, agent CA
  control-plane/     Go + Echo — admin API / BFF, IAM, SSO, analytics
  ai-gateway/        Go — /v1 AI traffic, provider adapters, routing, quota
  compliance-proxy/  Go — transparent TLS proxy, CONNECT, compliance pipeline
  agent/             Go — desktop traffic interception
                     (macOS + Linux + Windows GA)
  shared/            Go — cross-service business logic (hooks, traffic, configtypes,
                     mq, thingclient, cache, …)
  control-plane-ui/  React + Vite + TypeScript — admin dashboard
  ui-shared/         Shared design tokens, chart colors, i18n bundles

tools/db-migrate/    Prisma schema + migrations + seed (dev-time only)

scripts/             dev-start.sh + check-* lint scripts
tests/               Test harnesses, .env.local.example, auth.sh helper, smoke scripts
examples/            Self-contained demos (01-hello-world, …)

docker-compose.yml   Local PostgreSQL + Valkey + NATS
go.work              Go workspace (one module per package + tools)
Makefile             build / test targets per service
```

---

## Tech stack

- **Go services** — Go 1.25+ with `go.work`; Echo on Control Plane / Nexus Hub / AI Gateway (`labstack/echo/v4 v4.15.2`); structured logging via `log/slog`; metrics via Prometheus `promauto`; Redis-wire client `redis/go-redis/v9 v9.19.0`; WebSocket via `coder/websocket v1.8.14`.
- **Control Plane UI** — React + Vite + TypeScript (strict mode); React Query via the `useApi` hook; layered design tokens in `packages/ui-shared/src/styles/` (`global.css` raw → `light.css` / `dark.css` semantic, flipped by `data-theme`); i18n with `react-i18next` (`en` / `zh` / `es` under `packages/control-plane-ui/public/locales/` and `src/i18n/locales/`); tests via Vitest.
- **Database** — PostgreSQL 16. Prisma is the dev-time source of truth (`tools/db-migrate/`); runtime queries use hand-written SQL + `pgx`.
- **Cache** — Valkey 8 (Redis-wire-compatible, BSD-licensed `valkey/valkey-bundle:8-trixie` image). Pure cache only — no pub/sub anywhere.
- **MQ** — NATS JetStream behind the `packages/shared/transport/mq/` interface.
- **Monorepo** — npm workspaces (`packages/control-plane-ui`, `packages/agent/ui/frontend`, `tools/db-migrate`) + `go.work` for Go.

### Go workspace — what every build context must carry

Every Go module under `packages/` references its sibling workspace packages by `require github.com/AlphaBitCore/nexus-gateway/packages/<sibling> v0.0.0-<timestamp>-<commit>`. Those pseudo-version `require`s are only there to make each module syntactically valid on its own — real resolution comes from `go.work` at the repo root.

This has one consequence: if `go.work` is missing from the build context, Go falls back to the literal pseudo-version in `require` and tries to fetch the module from GitHub instead of using the local source tree. The build "succeeds" against an old remote snapshot, masking local changes.

Rules for every build environment:

- **Fresh clone** — `git clone` already includes the committed `go.work` and `go.work.sum`. Run `go build` from inside the repo.
- **Docker** — copy `go.work` + `go.work.sum` **and every `packages/<module>` directory the service transitively depends on**, not just the service's own folder. Minimum viable layout:
  ```dockerfile
  WORKDIR /build
  COPY go.work go.work.sum ./
  COPY packages/shared       packages/shared
  COPY packages/<svc>        packages/<svc>
  WORKDIR /build/packages/<svc>
  RUN go build -o /out/<svc> ./cmd/<svc>/
  ```
- **CI** — use full `actions/checkout` (default fetch-depth, no sparse-checkout).
- **Sanity probe** — `GOWORK=off go build ./cmd/<svc>/` from inside a workspace package should refuse to build or pull a remote snapshot.

If a contributor reports "Go keeps downloading our own modules from GitHub", the answer is always: their build context is missing `go.work` (or they have `GOWORK=off` set).

---

## Common commands

| Command | Purpose |
|---|---|
| `./scripts/dev-start.sh` | One-shot bootstrap (Docker + DB + seed + UI) |
| `npm run dev:control-plane-ui` | Start the UI dev server only |
| `make build-all` | Build the Go services + UI. Go binaries land in `dist/bin/<service>/<binary>`. |
| `make test-all` | Run `go test -race -count=1` for every Go module + UI Vitest |
| `make clean` | Remove `dist/bin/` and `packages/control-plane-ui/dist/`. Platform agent packages under `dist/{macos,linux,windows}/` are preserved — clean those via the per-platform targets (`agent-clean-macos`, `agent-clean-windows`). |
| `npm run check:all` | Run every pre-commit lint (i18n parity, design tokens, terminology, migration timestamps, useApi keys, sidebar icons, …). CI runs the same set. |
| `npm run db:migrate` | Create a new Prisma migration in `tools/db-migrate/` |

To build, sign, notarize, or package the macOS Agent (`.app` / `.pkg`), always invoke the `build-agent` Claude Code skill — not the raw `wails` / `codesign` / `notarytool` commands. See `CLAUDE.md` → "macOS Agent builds MUST go through `Skill('build-agent')`" binding rule for why.

---

## Authoritative documents

1. **[`CLAUDE.md`](./CLAUDE.md)** — binding charter. Plan + Todo gate, English-only artifacts, IAM impact review, macOS NE fail-open, pre-edit reading, completion-time self-audit, real-implementation-only, development-phase greenfield policy.
2. **[`CONTRIBUTING.md`](./CONTRIBUTING.md)** — workflow summary, pre-commit checks, high-blast-radius surfaces, review pointers.

---

## Acknowledgments

- **Steve** — the original idea behind Nexus Gateway came from him, and he stayed hands-on throughout: code, tests, design reviews, architectural decisions.
- **The wider team** — engineers, code reviewers, QA, design folks, and the people running prod. The architecture decisions, design reviews, code-review catches, and prod incidents that shaped this codebase all came from team collaboration.
- **[Claude Code](https://claude.com/claude-code)** — Anthropic's CLI assistant did the lion's share of the implementation work, side-by-side with the human maintainers.

---

*AI is already here. Keep learning, keep adapting.*
