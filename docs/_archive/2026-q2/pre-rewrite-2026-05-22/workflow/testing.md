# Nexus Gateway — End-to-End Test Program

## 1. Goal

Provide automated regression coverage for the ~75 business flows inventoried in
this document so that "did my change break something" is answered by one
command, not by manual click-through.

Source inventory: see Section 11 ("Business Flow Catalog") for the full list of
flows by domain. The catalog is grouped into 14 business domains spanning
Setup, Tenancy/IAM, Catalog/Vault, Routing, Hooks, Streaming, Quota, Traffic
& Audit, Analytics, Alerting, Fleet, Infra Introspection, Compliance Proxy,
and Tools.

## 2. Non-goals (this iteration)

- CI integration (handled separately once green locally)
- Code coverage targets
- Load / soak / performance testing
- Pixel-level visual regression baselines
- External provider availability (we test through real upstreams but do not
  page on upstream outages)

## 3. Layered architecture

| Layer | Tooling | Verifies | Share of cases |
|-------|---------|----------|----------------|
| L1 black-box smoke | Bash + curl + psql, run via `.claude/skills/test-*` skills | HTTP shape, status codes, DB rows, Prometheus counters | ~40% |
| L1 in-process integration | Go `_test.go` in `tests/integration-go/` | Routing dispatch, hook pipeline, body capture, streaming, quota | ~30% |
| L2 protocol compliance | Python + `openai` / `anthropic` SDK in `tests/e2e-python/protocol/` | Drop-in compatibility with provider SDKs | ~10% |
| L3 AI-judge | Python + Kimi 128k via Nexus VK in `tests/e2e-python/ai-judge/` | Semantic correctness (PII masking, output checks, classifier recall) | ~15% |
| L4 UI E2E | Playwright in `tests/e2e-ui/` | Page render, navigation, cache isolation, i18n keys | ~5% |

Rationale for the split:

- **Determinism first.** ~70% of flows are deterministic given config — assert
  exact values in code, never with an LLM. LLM judging is slow, fuzzy, and
  has its own failure mode.
- **Real upstreams where needed.** L2 dials real providers because the bug
  class we care about (adapter shape regressions) only shows up against real
  responses.
- **Dogfood the gateway.** L3 drives Kimi 128k *through our own AI Gateway*
  using a Nexus VK. The test exercises the gateway path on every assertion,
  giving us a free smoke test of the AI Gateway every time AI-judge runs.

## 4. AI-judge configuration (locked)

| Field | Value |
|-------|-------|
| Judge model | `moonshot-v1-128k` |
| Endpoint | `http://localhost:3050/v1/chat/completions` (Nexus AI Gateway) |
| Auth | `Authorization: Bearer ${NEXUS_TEST_VK}` |
| Default temperature | `0` (deterministic judging) |
| Wire format | OpenAI-compatible chat completion |

The VK is sourced from the operator's environment (`NEXUS_TEST_VK`) — never
hardcoded in code. The `research-all-models` VK is the suggested default for
local dev.

Failure semantics: if the Nexus AI Gateway is unreachable, AI-judge tests
**fail loudly** (exit non-zero, do not silently skip). The whole point of
dogfooding is that AI-judge red ≈ AI Gateway red.

## 5. Directory layout

Actual layout under `tests/` (run `find tests -maxdepth 3 -type f` to refresh
when files are added). Tests live in a top-level `tests/` directory, *not*
inside individual `packages/<service>/`, because most flows cross multiple
services.

```
tests/
├── README.md                           # how to run, prerequisites
├── run-all.sh                          # top-level aggregator (preflight + L1..L4)
├── lib/                                # shared shell + python helpers
│   ├── assert.sh                       # pass/fail reporting + summary
│   ├── auth.sh                         # OAuth+PKCE login + bearer-token reuse
│   ├── db.sh                           # docker exec psql wrapper
│   ├── env.sh                          # NEXUS_TEST_TARGET resolution + fail-closed guards
│   ├── http.sh                         # curl wrapper with timing
│   ├── loadenv.py                      # python loader for tests/.env.<target>
│   ├── loadenv.sh                      # bash loader for the same files
│   └── preflight.sh                    # block-until-ready for the 4 local services
├── smoke/                              # L1 black-box (skill-driven)
│   ├── run-all.sh                      # runs every test-* smoke
│   ├── test-ai-gateway.sh
│   ├── test-control-plane.sh
│   └── test-hub.sh
├── integration-go/                     # L1 Go (in-process HTTP-level)
│   ├── helpers/                        # aigw.go / client.go / db.go / env.go / io.go
│   ├── hooks_test.go
│   └── setup_test.go
├── scenarios/                          # scenario-driven Go tests (every admin
│   │                                   # API ≥1 scenario; /v1/* ≥3 each)
│   ├── 00-catalog.md                   # scenario inventory + status
│   ├── COVERAGE.md                     # coverage matrix
│   ├── READINESS.md                    # readiness gating
│   ├── setup_test.go                   # shared test setup
│   ├── helpers/                        # admin / builtin_rules / cleanup /
│   │                                   # metrics / preflight / safety
│   └── *_test.go                       # ~50 scenario files (alerts, audit,
│                                       # cache_*, routing, quota, scim, …)
├── e2e-python/                         # L2 protocol + L3 AI-judge
│   ├── README.md
│   ├── conftest.py                     # shared fixtures
│   ├── protocol/                       # L2 SDK compat
│   │   ├── test_anthropic_compat.py
│   │   ├── test_embeddings_compat.py
│   │   ├── test_openai_compat.py
│   │   └── test_responses_compat.py
│   └── ai_judge/                       # L3 judge driver + cases
│       ├── judge.py                    # Kimi 128k judge wrapper
│       ├── test_judge_smoke.py
│       └── test_pii_detection.py
├── e2e-ui/                             # L4 Playwright
│   ├── playwright.config.ts
│   ├── global-setup.ts
│   └── specs/                          # audit-drawer / cache-* / cost-stamping /
│                                       # fleet-overview / hook-test / login /
│                                       # metrics-explorer / routing-crud /
│                                       # traffic-monitor
├── agent/                              # agent-specific suites (off the main
│   │                                   # L1..L4 ladder; run manually on macOS)
│   └── gap_closure/                    # E74 pf gap-closure tests
├── manual/                             # adapter synthetic chats (cursor, geminiweb)
└── scripts/                            # cross-cutting harness scripts
    ├── coverage-gap.py
    ├── i18n_gap_check.py
    ├── mint-test-vk.go
    ├── smoke-e61.py
    └── smoke-gateway.py                # full-surface AI Gateway smoke
```

Per `docs/developers/workflow/local-dev-debugging.md` "Test / skill env files",
the per-target `tests/.env.<target>` files (`local` / `dev` / `prod`) live
alongside this tree but are gitignored — copy from `.env.example` after
checkout.

## 6. Verification discipline (binding)

Every test must verify reality through at least one of these channels:

1. **HTTP response assertion** — status code + body shape + header
2. **Database cross-check** — `docker exec psql` query confirming the row /
   column / count
3. **Prometheus counter** — scrape `/metrics` before + after, assert delta
4. **AI-judge** — send the artifact (request body, response, audit row) to
   Kimi 128k and check the structured verdict

Forbidden: assertions that check only "did the tool return without error"
or "did the binary produce *some* output". Tests that pass without verifying
real state are worse than no test, because they hide regressions.

## 7. Service prerequisites

Before any test run:

| Service | Port | Health check |
|---------|------|--------------|
| Nexus Hub | 3060 | `GET /health` 200 |
| Control Plane | 3001 | `GET /healthz` 200 (or `cp_curl_full /api/admin/ready` for deep readiness) |
| AI Gateway | 3050 | `GET /v1/models` 200 with VK |
| Compliance Proxy | 3040 | `GET /` 200 |
| Control Plane UI | 3000 | (only required for L4) |
| Postgres | 55532 | `docker exec nexus-postgres pg_isready` |
| Redis | 6437 | `docker exec ... redis-cli ping` |

The runner is allowed to autonomously restart workspace Go services per
`CLAUDE.md` ("Service lifecycle"). It is NOT allowed to touch Postgres,
Redis, or any container the test did not start.

## 8. Phases & order of execution

| Phase | Scope | Output |
|-------|-------|--------|
| 0 | Foundation: directory layout, shared helpers, runner skeleton, this doc | `tests/lib/*`, `tests/.env.local.example`, `tests/run-all.sh` |
| 1 | L1 smoke scripts for Control Plane and Hub (alongside existing test-ai-gateway / test-compliance-proxy) | `tests/smoke/test-control-plane.sh`, `tests/smoke/test-hub.sh` |
| 2 | L1 Go integration tests for routing / hooks / streaming / quota / auth | `tests/integration-go/*_test.go` |
| 3 | L4 Playwright minimal critical-path specs | `tests/e2e-ui/specs/*.spec.ts` |
| 4 | L3 AI-judge using Kimi 128k via Nexus VK | `tests/e2e-python/ai_judge/*.py` |
| 5 | L2 protocol compatibility via openai / anthropic Python SDKs | `tests/e2e-python/protocol/*.py` |
| 6 | `/test-all` aggregator skill + unified markdown report | `.claude/skills/test-all/SKILL.md` |

Phase 0 unblocks everything else. Phases 1–5 are independent and could run
in parallel, but the live execution does them sequentially to keep commits
reviewable. Phase 6 depends on 1–5 producing stable artifacts.

## 9. Reporting contract

Every run produces a single markdown report at
`/tmp/nexus-test/test-all-<UTC-timestamp>.md` (path emitted by
`.claude/skills/test-all/SKILL.md`) with this shape:

```
# Nexus Gateway — Test Run YYYY-MM-DDTHH:MM:SSZ

## Summary
- L1 smoke:        12/12 ✅
- L1 integration:  18/20 ❌
- L2 protocol:     8/8 ✅
- L3 AI-judge:     skipped (--quick mode)
- L4 UI E2E:       6/7 ❌
- Duration:        4m 12s

## Failures
1. tests/integration-go/streaming_test.go::TestSSEBufferFullBlock
   - Expected HTTP 451 on PII reject, got 200
   - Audit row 0x...: request_hook_decision=approve (expected reject)
   - Likely cause: ...

## Per-phase detail
...
```

The report is the single artifact a reviewer needs. Logs live under
`/tmp/nexus-test/<ts>/{phase}/...` and are referenced from the report.

## 10. Open questions / future work

- Multi-tenant test isolation: currently we share the dev DB with manual
  testing. If/when this collides, switch tests to a dedicated schema (e.g.
  `nexus_test`) plus a fast seed step.
- AI-judge cost: each judging call is one Kimi 128k completion. Phase 4
  starts with 4 cases × ~10 invocations = ~40 calls per full run. Acceptable
  for now; revisit if it grows past ~200/run.
- CI parallelism: when this graduates to CI, Phase 2 / 3 / 4 can shard.

## 11. Business flow catalog

(Full catalog from session inventory — kept here as the canonical source for
what the test program must cover. Marked items show which Phase covers them.)

### A. Setup & auth
- A1 Setup wizard initial bootstrap — Phase 3 (UI)
- A2 Admin login + session — Phase 1 (smoke), Phase 2 (Go), Phase 3 (UI)
- A3 Logout + session invalidation — Phase 1
- A4 Personal account / change password — Phase 1

### B. Tenancy & IAM
- B1 Org / Project CRUD — Phase 1
- B2 IAM user + role/group bind — Phase 1
- B3 IAM Policy editor (JSON document) — Phase 1
- B4 IAM Simulator — Phase 1
- B5 Role-based page access — Phase 2 (Go), Phase 3 (UI)

### C. Catalog & vault
- C1 Provider create + adapter selection — Phase 1
- C2 Provider model list pull — Phase 1
- C3 Credential CRUD (AES-256-GCM) — Phase 1
- C4 Virtual Key issue + scope — Phase 1
- C5 Personal VK self-service — Phase 1

### D. Routing
- D1 Routing rule CRUD (6 strategies) — Phase 1
- D2 Match conditions editor — Phase 2
- D3 Routing simulator dry-run — Phase 1
- D4 No-match passthrough / fallback — Phase 2
- D5 Fallback chain on HTTP status — Phase 2

### E. Hooks
- E1 Hook list + CRUD — Phase 1
- E2 Rule pack CRUD — Phase 1
- E3 Hook test (admin proxy) — Phase 1
- E4 Keyword reject end-to-end — Phase 2 (deterministic) + Phase 4 (semantic)
- E5 PII detector — Phase 2 + Phase 4
- E6 AI Guard `/v1/ai-guard/classify` — Phase 4
- E7 Compliance exemption grant — Phase 1

### F. Streaming & body capture
- F1 SSE pass-through — Phase 2
- F2 buffer_full_block + reject — Phase 2
- F3 chunked_async — Phase 2
- F4 Body inline (<256 KiB) — Phase 2
- F5 Body spillstore (≥256 KiB) — Phase 2
- F6 Compliance Proxy SSE — Phase 2
- F7 Agent SSE response — Phase 1 (smoke; Agent runs on demand)

### G. Quota
- G1 Quota policy CRUD — Phase 1
- G2 Quota override CRUD — Phase 1
- G3 Quota enforcement (429) — Phase 2

### H. Traffic & audit
- H1 Live traffic page — Phase 3
- H2 Audit drawer drilldown — Phase 3
- H3 Audit log search/filter — Phase 1
- H4 Live traffic advanced filters — Phase 3
- H5 DSAR export — Phase 1
- H6 SIEM forwarding — Phase 1

### I. Analytics
- I1 Dashboard overview — Phase 3
- I2 Cost / latency analytics — Phase 1
- I3 Quota usage analytics — Phase 1
- I4 Compliance dashboard — Phase 3
- I5 Metrics explorer — Phase 1
- I6 UTC time-zone consistency — Phase 1

### J. Alerting & kill switch
- J1 Alert rule CRUD — Phase 1
- J2 Alert channel CRUD (webhook) — Phase 1
- J3 Hub-side evaluator triggers alert — Phase 2
- J4 Kill switch hard-stop — Phase 2

### K. Fleet
- K1 Agent enrollment + mTLS — Phase 1
- K2 Device group CRUD — Phase 1
- K3 Agent config sync (ETag) — Phase 1
- K4 Force sync — Phase 1
- K5 Thing override — Phase 1
- K6 Agent exemption — Phase 1
- K7 Agent event search — Phase 1
- K8 Fleet user / device detail — Phase 3
- K9 Crash reports / recent errors — Phase 1
- K10 Diag mode — Phase 1

### L. Infra introspection
- L1 Nodes list + detail — Phase 3
- L2 Runtime config tab — Phase 1
- L3 Runtime logs tab — Phase 3
- L4 Runtime metrics tab — Phase 1
- L5 Runtime state tab — Phase 1
- L6 Scheduled jobs — Phase 1
- L7 Hub self-registration — Phase 1

### M. Compliance proxy scope
- M1 Interception domain CRUD — Phase 1
- M2 Pinning exemption — Phase 1
- M3 Compliance proxy status / reject config — Phase 1
- M4 Discovery — Phase 1

### N. Tools
- N1 AI Gateway simulator — Phase 3
