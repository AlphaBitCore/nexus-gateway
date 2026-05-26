# AI Gateway

Inbound `/v1/*` AI traffic gateway. Authenticates client virtual keys,
applies hooks (compliance, quota, routing), translates between ingress
formats (OpenAI Chat Completions / OpenAI Responses / Anthropic Messages /
Gemini), executes the request against the resolved provider, normalises
the response, caches what it can, and emits a `traffic_event` row per
call. Plus the cache layer, rate limiter, and request-context plumbing
that every other AI Gateway feature builds on.

## Where it sits

| | |
|---|---|
| **Port** | `3050` (HTTP) |
| **DB** | PostgreSQL (`traffic_event`, `traffic_event_normalized`, `RoutingRule`, `Provider`, `Credential`) |
| **Cache** | Redis (response cache, quota counters, rate limit) |
| **Upstream** | Every AI provider listed under `internal/providers/spec_*` |
| **Registers as Thing** | `type=ai-gateway`; receives config shadows for `routing`, `hook_config`, `observability`, `kill_switch` |

## Build

```bash
make ai-gateway-build      # outputs to dist/bin/ai-gateway/ai-gateway
# or
cd packages/ai-gateway && go run ./cmd/ai-gateway/ -config ai-gateway.dev.yaml
```

## Test

```bash
make ai-gateway-test       # go test -race -count=1 ./...
```

## Key directories

| Path | Purpose |
|---|---|
| `cmd/ai-gateway/` | Process entry. Wires routing, providers, hooks, the `/v1/*` HTTP surface, and the `/runtime/*` ops surface. |
| `internal/providers/spec_*/` | One subpackage per provider format (OpenAI, Anthropic, Gemini, Bedrock, Vertex, Azure OpenAI, Cohere, Groq, etc.). Each owns its full canonical↔wire codec and stream-session implementation. |
| `internal/router/` | Smart routing engine + rule matcher + `routerllm` LLM-based routing. |
| `internal/quota/` | Quota policy cache + usage counter (Redis-backed). |
| `internal/ratelimit/` | Token-bucket per-VK rate limiter. |
| `internal/streamcache/` | Two-turn response cache for SSE-class workloads. |
| `internal/runtimeapi/` | Operator-facing `/runtime/*` GETs (gated by `AI_GATEWAY_API_TOKEN`). |
| `internal/store/` | Hand-written SQL + pgx for runtime reads (provider lookup, VK auth, routing rule fetch). |

## Configuration

- `ai-gateway.dev.yaml` — local boot defaults.
- `ai-gateway.prod.yaml.example` — production template.
- Secrets via env: `ADMIN_KEY_HMAC_SECRET` (must match CP),
  `CREDENTIAL_ENCRYPTION_KEY` (must match CP),
  `INTERNAL_SERVICE_TOKEN` (must match Hub),
  `AI_GATEWAY_API_TOKEN` (ops-only).

## Architecture references

- `docs/developers/architecture/services/ai-gateway/ai-gateway-internals-architecture.md` — module layout.
- `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` — the binding §3a rules
  every adapter codec must follow.
- `docs/developers/architecture/services/ai-gateway/normalization-architecture.md` — canonical payload + how
  ingress formats translate.
- `docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md` — the response-cache tiers.
- Smoke test: `tests/scripts/smoke-gateway.py` (or `/smoke-gateway`
  slash command).
