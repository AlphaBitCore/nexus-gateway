# E55 — TLS-Bump Trinity: shared/tlsbump for compliance-proxy AND agent

**Status:** in design (2026-05-15)
**Owner:** platform
**Triggered by:** macOS Agent NETransparentProxy bridge ingress shipped without
re-using compliance-proxy's proven MITM core; ad-hoc re-implementation in
`packages/agent/core/network/proxy/proxy.go` (~965 LOC) duplicates and lags the
reference implementation in `packages/compliance-proxy/internal/proxy/`
(~4400 LOC across 24 files).

## Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| F-1 | A single Go package `packages/shared/transport/tlsbump/` provides the TLS interception, ALPN dispatch, HTTP/1.1+HTTP/2 forward handling, request/response hook orchestration, SSE / streaming-compliance pipeline, payload capture, cert pinning detection, opaque passthrough fallback, and reject-response generation. | Must |
| F-2 | `compliance-proxy` consumes `shared/tlsbump` via `tlsbump.HandleConnection(ctx, conn, dst, deps)`. Its `internal/proxy/listener.go` (CONNECT ingress) is the only proxy-layer file that remains in the cp package. The 23 other files in `cp/internal/proxy/*` are deleted. | Must |
| F-3 | The macOS Agent's NETransparentProxy bridge ingress consumes the same `shared/tlsbump.HandleConnection` after parsing the `BRIDGE host:port flowID\n` header from the Swift NE side. `packages/agent/core/network/proxy/proxy.go` (~965 LOC of duplicate logic) is deleted and replaced with `bridge.go` (the ingress wrapper). | Must |
| F-4 | Agent consumes `system_metadata['streaming_compliance.config']` and `system_metadata['payload_capture.config']` from Hub (Cat B Thing config keys) and uses them — including per-host overrides on `interception_domain` rows — to drive runtime behavior. The three streaming modes (`passthrough`, `chunked_async`, `buffer_full_block`) all behave identically to compliance-proxy. | Must |
| F-5 | Audit emission: cp continues writing to its existing `audit.Writer` → Hub MQ pipeline. Agent writes through its existing local-SQLite `audit.Queue` → Hub HTTP upload pipeline. The `tlsbump` package depends only on a small `EmitFunc` callback or a `shared/audit.Writer` interface — never on a concrete cp or agent type. | Must |
| F-6 | Agent's MITMRelay supports both HTTP/1.1 and HTTP/2 (no forced ALPN downgrade). Cursor / Claude CLI / Anthropic SDK / OpenAI SDK clients connect over their preferred ALPN protocol without breakage. | Must |
| F-7 | Detailed structured logs at every decision branch in tlsbump and agent's bridge ingress: SNI peek source, domain match outcome, inspect-vs-passthrough decision, bump TLS handshake outcome, request parse, inspector return, upstream relay outcome, response status, streaming-mode pick, hook-pipeline outcomes, body capture decision. Goal: any prod incident is diagnosable from `agent.log` + `compliance-proxy.log` alone. | Must |
| F-8 | Existing compliance-proxy unit tests (in `internal/proxy/*_test.go`) move to `shared/tlsbump/*_test.go` and continue to pass under `go test -race -count=1`. | Must |
| F-9 | Agent integration scenarios that previously failed (non-streaming `curl https://api.anthropic.com/v1/messages` hang, Claude CLI block, method/path empty in `traffic_event`) all pass after the trinity refactor; regressions diagnosed from logs. | Must |
| F-10 | Trinity rollout adds NO defer/mock/follow-up production code. Tests may use mocks per normal practice. | Must |

## Non-Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| NF-1 | TLS handshake p99 latency on the bumped path stays within ±10% of the pre-refactor compliance-proxy baseline (measured via `metrics.TLSHandshakeMs`). | Must |
| NF-2 | No additional allocations on the steady-state per-request hot path vs the pre-refactor cp baseline (`go test -bench` parity within ±5%). | Should |
| NF-3 | The `shared/tlsbump` package's exported API surface stays additive-only after this E55 lands (per `shared` API stability rule in CLAUDE.md). | Must |
| NF-4 | All English-only repo artifact rule applies (CLAUDE.md). | Must |
| NF-5 | No new third-party Go dependency added to `shared/` outside the vetted set in CLAUDE.md (the `golang.org/x/net/http2` + `andybalholm/brotli` + `klauspost/compress/zstd` + `tidwall/gjson|sjson` set used by cp's proxy package is already vetted). | Must |
| NF-6 | macOS NE fail-open invariant from CLAUDE.md preserved: agent bridge ingress still falls open to native networking on any tlsbump error (the bridge ingress wrapper, not tlsbump itself, is responsible for catching and degrading). | Must (safety-critical) |

## User Roles & Personas

| Role | Interaction |
|------|-------------|
| Platform engineer | Maintains `shared/tlsbump`. Adds new streaming modes / capture knobs / pinning rules in one place; both ingresses pick them up automatically. |
| Compliance-proxy operator | No behavior change. Same admin UI knobs (`Streaming Compliance` / `Payload Capture` / `Interception Domains`) drive the same pipeline. |
| Agent end user | Sees method/path/request body/response body in admin UI Traffic detail for inspect flows. No more silent drops. |
| Admin | Toggles `streaming_compliance.mode = buffer_full_block` and the agent honors it (block streaming on hook reject) — same as cp does today. |

## Constraints & Assumptions

- Compliance-proxy is in production. The refactor must not regress its behavior. Verification gate: existing cp tests + manual smoke against `https://api.openai.com/v1/chat/completions` through cp.
- Agent has no installed user base outside dev / pilot. Per CLAUDE.md "no backward compatibility, no defer" — duplicate `agent/internal/proxy/proxy.go` is deleted in the same PR series, not kept as a parallel path.
- Hub schema for `streaming_compliance.config` and `payload_capture.config` already exists (cp + ai-gateway already consume them). No DB migration needed for agent to start consuming.
- `interception_domain.streamingMode / streamingChunkBytes / streamingHookTimeoutMs / streamingMaxBufferBytes / streamingFailBehavior / captureRequestBody / captureResponseBody / rawBodySpillEnabled` columns exist on the prod schema (E33). No DB migration.

## Glossary

| Term | Meaning |
|------|---------|
| TLS bump | TLS interception by terminating client TLS with a dynamically minted leaf cert, then re-establishing TLS to upstream. Two L7 visibility points per flow. |
| ALPN dispatch | After TLS handshake completes, switch to HTTP/1.1 or HTTP/2 based on the negotiated `alpn` protocol value. |
| Streaming compliance mode | One of `passthrough` / `chunked_async` / `buffer_full_block` — controls how SSE/streaming responses are captured and which hooks may block. |
| Per-host override | `interception_domain` row's `streaming_*` and `capture*` columns; merged with global `streaming_compliance.config` / `payload_capture.config` defaults via `streampolicy.Resolve`. |
| Bridge ingress | Agent's Swift NE → daemon Go bridge: NE writes `BRIDGE host:port flowID\n` then opaque TLS bytes. |
| Inspect domain | A host listed on `interception_domain` with `action = inspect` (vs `passthrough`); only inspect domains get TLS-bumped + hook-evaluated. |
| Trinity | The compliance pipeline (hooks + policy resolution + audit) is identical across ai-gateway / compliance-proxy / agent. After E55 this trinity now extends to the TLS-bump core for compliance-proxy + agent (ai-gateway stays protocol-aware via provider adapters). |

## MoSCoW Summary

- **Must:** F-1 through F-10, NF-1, NF-3, NF-4, NF-5, NF-6.
- **Should:** NF-2.
- **Could:** Generalize the audit emitter (`cp/internal/compliance/emitter.go`, ~528 LOC) into `shared/compliance/audit_emitter.go` so agent re-uses the cp event-shape mapping. Deferred only if scope risk is too high; preferred to land in this E55.
- **Won't:** ai-gateway migration to tlsbump (it is protocol-aware via provider adapters, not TLS-bump). Hub-side schema changes (none needed). Removing `golang.org/x/net/http2` dependency (vetted; required for h2 support).
