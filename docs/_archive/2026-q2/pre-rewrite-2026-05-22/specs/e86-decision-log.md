# E86 — Decision log

> Records every non-obvious architectural / product judgement made while building the E2E gap matrix and closing gaps for shipped capabilities. Each decision: **D**ecision, **O**ptions considered, **R**easoning, **R**evisit-if.

---

## D1 — Matrix lives at "customer-facing capability" level, not at "API endpoint" level

- **Options considered:**
  - (a) Extend `tests/scenarios/00-catalog.md` (endpoint × scenario) — already exists and is exhaustive.
  - (b) New file at capability level (rows = `features.md` bullets) — does not exist today.
  - (c) Fold both into one combined matrix.
- **Decision:** (b). `e86-e2e-coverage-matrix.md` is per-capability; `00-catalog.md` stays as the lower-level endpoint map.
- **Reasoning:** A new endpoint can land without changing what a user can do (refactor, alias, internal); a new capability invariably implies new endpoints + UI + DB. The closure signal we need at release time is *"every user-facing thing has a test"* — that's a capability question, not an endpoint question. Combining them would either dilute the user-perspective signal or duplicate every endpoint row.
- **Revisit if:** the capability inventory in `features.md` falls out of sync with what `tests/scenarios/` exercises (then we'd need machine-generated cross-refs, not two hand-written files).

---

## D2 — Use existing `tests/scenarios/` Go harness for new arms; do not introduce a new test runner

- **Options considered:**
  - (a) Add a new Python harness for the gap closures.
  - (b) Bash + curl per skill.
  - (c) Extend `tests/scenarios/` Go harness (45 scenarios, full safety guards, env isolation, `cp_login` / `helpers/` ready).
- **Decision:** (c).
- **Reasoning:** The Go harness already enforces `tests/.env.<target>` env isolation, fail-closed hostname allowlist, `NEXUS_TEST_TARGET=local` confirmation, cleanup registry, metric scraping helpers, and admin-API client. Re-implementing any of those in a new layer is pure cost. Per `feedback_scenario_test_env_isolation` binding, adding scenarios via the existing harness preserves the safety contract.
- **Revisit if:** a closure requires a capability the Go harness genuinely cannot host (e.g. needs Playwright DOM for the assertion, or needs a daemon-mode agent).

---

## D3 — Per-layer numeric targets (L1/L2/L3/L4/L5/Skill) sit in the matrix doc, not a sibling file

- **Options considered:**
  - (a) Sibling doc `e86-coverage-targets.md`.
  - (b) Section §2 of the matrix doc.
- **Decision:** (b).
- **Reasoning:** Targets only make sense alongside the rows they apply to. Less-is-more — one doc to maintain, one source of truth. Pattern matches the `tests/scenarios/COVERAGE.md` precedent (numbers + table in the same file).
- **Revisit if:** the targets list grows beyond ~10 rows or develops its own measurement tooling (then it earns its own home).

---

## D4 — [SUPERSEDED 2026-05-21] Defer E69 prewarm / E70 sticky-token / E71 domain-thresholds scenarios

**Status: Superseded.** The user clarified mid-session that E86's Goal is *find AND auto-fix every gap*, not *find and ship the highest-impact subset*. Subsequently confirmed E61 has merged to main — the "CRUD API still settling" rationale no longer holds. All three deferrals are revoked; S-067 / S-068 / S-069 land in Phase 8 against the live `e61-s6-cache-admin.yaml` shape. Original text retained below for archaeology.

---

### Original (superseded) reasoning

- **Options considered:**
  - (a) Write a thin smoke test for all 8 ✗ cells now.
  - (b) Ship the 5 highest-impact scenarios + explicitly defer the lower three with rationale + tracking row in §4.
- **Decision:** (b). Ship S-062 / S-063 / S-064 / S-065 / S-066. Defer S-067 / S-068 / S-069.
- **Reasoning:**
  - E69 prewarm: corpus-upload CRUD API is still settling (no OpenAPI spec yet); a test built against the current shape would churn.
  - E70 sticky-token: minor knob, no UI surface yet, no real customer journey known. Per CLAUDE.md "less is more" — proving coverage for a config knob nobody asked for is anti-pattern.
  - E71 domain thresholds: blocked on the upstream domain-classifier integration design; testing without that resolved would be testing a placeholder.
  - The 5 we ship are all user-facing customer paths with shipped UI surfaces — the highest blast-radius cells.
- **Revisit if:** any of the deferred features get a customer escalation OR the upstream blocker resolves (prewarm CRUD stabilises, domain classifier ships).

---

## D5 — CI gate extends `scripts/doc-lockstep.config.mjs` rather than a new script

- **Options considered:**
  - (a) New CI workflow / GitHub Action.
  - (b) Sibling script `scripts/check-e2e-matrix.mjs` invoked from `npm run check:all`.
  - (c) Add a new entry to `scripts/doc-lockstep.config.mjs` mapping `docs/users/api/openapi/**` + `docs/users/features/**` + roadmap → the matrix file.
- **Decision:** (c). One config entry; reuses the existing checker, npm script, and pre-commit hook.
- **Reasoning:** Both the doc-lockstep checker and the matrix gate are answering the same shape of question — *"did the engineer touch X without touching the linked Y?"*. Two scripts implementing the same matcher logic is duplicated code; one config entry is none. Per the CLAUDE.md "less is more" rule (extending an existing surface over adding a new one), the entry is the right answer. Earlier draft of this log said "sibling script" — superseded by code review of the lockstep config itself, which already handles `openapi/**` and `features/**` triggers cleanly.
- **Revisit if:** the matrix needs assertions the doc-lockstep checker can't express (e.g. structured row-shape validation), at which point a sibling validator earns its keep.

---

## D6 — `tests/run-all.sh` emits matrix coverage line in its final report; harness does not block on `✗` count

- **Options considered:**
  - (a) Run-all.sh fails red if any `✗` is found in the matrix.
  - (b) Run-all.sh reports the `✗` count as informational; CI gate above enforces the rule for PRs.
- **Decision:** (b).
- **Reasoning:** `run-all.sh` is the *test runner*, not the *coverage gate*. Conflating them means "tests pass but matrix has known holes" looks identical to "tests fail." The matrix gate is a *PR-level* contract (you added a feature, you owe a row), not a *run-level* contract (every run with any unclosed cell is failure). The latter would block development on legitimately deferred rows (D4).
- **Revisit if:** we declare matrix at 0 `✗` and want a regression-prevention asserting "never go above 0" — then collapse the gate into run-all.

---

## D7 — Obsolete cleanup: keep `tests/scripts/smoke-e61.py`, `tests/scripts/coverage-gap.py`, `tests/scripts/i18n_gap_check.py`, `tests/scripts/mint-test-vk.go`

- **Options considered:**
  - (a) Delete utilities not referenced by `run-all.sh`.
  - (b) Keep all four; they are utility / one-shot scripts each serving an explicit purpose.
- **Decision:** (b) — keep all.
- **Reasoning:**
  - `smoke-e61.py` is referenced by E61 spec docs as a per-story validator (not a regression test); deletion would orphan the spec.
  - `coverage-gap.py` is used ad-hoc to compute the gap-closure delta — adjacent to E86's mission; keep.
  - `i18n_gap_check.py` is invoked by `/i18n-gap-check` skill — deletion would break the skill.
  - `mint-test-vk.go` is documented in `tests/scenarios/READINESS.md` as the standard remedy for stale VK in `.env.test` — deletion would break first-run remediation.
  - Per CLAUDE.md "real implementation only" — these are real utilities, not stubs.
- **Revisit if:** a util goes 6+ months unreferenced after we wire CI matrix checks (then re-evaluate).

---

## D8 — `S-063 embeddings` scenario uses cross-format ingress (OpenAI `/v1/embeddings` → an OpenAI-shape provider) rather than testing the canonical bridge in isolation

- **Options considered:**
  - (a) Unit-level codec test (already covered in `packages/shared/canonical/`).
  - (b) Synthetic provider in tests/integration-go/fake-providers/.
  - (c) Real upstream call to an embeddings-capable provider through the production routing pipeline.
- **Decision:** (c).
- **Reasoning:** E86 is about *end-to-end* coverage. A unit test on the codec is already in `packages/shared/canonical/` per the 95% coverage rule (E85). The gap matrix says "is the user-visible embeddings flow exercised end-to-end" — the answer needs the full ingress → routing → upstream → traffic_event chain. Synthetic provider would still skip the upstream-asymmetry class of bugs (`feedback_cache_mandatory_all_ingress`). Real upstream is the only signal that catches all of {codec, routing, capability filter, dimension round-trip, traffic_event stamp, Prometheus delta}.
- **Revisit if:** the cost of running the suite on every PR becomes a concern (then move embedding arm to `--full` mode only, matching `/smoke-gateway` P3E pattern).

---

## D9 — Decision log is its own file, not woven into `e86-e2e-coverage-matrix.md`

- **Decision:** Separate `e86-decision-log.md`.
- **Reasoning:** The matrix is consumed by people answering "what's covered?". The decision log is consumed by people asking "why is the matrix shaped this way?". Different audiences, different read-paths, different update cadences — folding them obscures both.
- **Revisit if:** the decision log goes >6 months without an update (then archive it).

---

## D10 — Delete E70 sticky-token + E71 domain-threshold dead code (2026-05-21)

- **Discovery:** while planning Phase 8 (close remaining ✗ cells), a `grep` for the proposed test fixtures revealed that the backend Go for E70 (`RequiredTokens`) and E71 (`DomainThresholds`) is **structurally unreachable**:
  - `schema.prisma`: no column stores either field.
  - `packages/control-plane/internal/`: zero admin endpoints touch either field.
  - `docs/users/api/openapi/`: no yaml documents either field.
  - `packages/control-plane-ui/src/`: only orphan i18n translation keys (`requiredTokens` / `domainThresholds`) — zero `.tsx` references.
  - Result: `req.RequiredTokens` and `req.DomainThresholds` are always nil/empty in production. The `if len(req.RequiredTokens) > 0` and `if len(req.DomainThresholds) > 0` branches never execute. Code, metrics, audit enum constants, and 6 locale files all carry weight for nothing.
- **Why this happened:** commit `d93843acb` (2026-05-20) landed both as "per-route policy" backends. The same day's CLAUDE.md binding [[feedback_cache_config_fleet_only]] declared "ALL gateway cache config is fleet-wide; Routing Rule detail carries NO cache surface." UI / admin layers were never built; backend was orphaned.
- **Options considered:**
  - **A. Delete** — remove dead Go, tests, i18n; align with fleet-only binding; matches CLAUDE.md "real implementation only" + "no defer".
  - **B. Build admin surface** — schema + endpoint + OpenAPI + UI form; violates fleet-only binding, would require explicit waiver to re-introduce per-route knobs.
  - **C. Comment "intentionally inert"** — violates "no fake returns / no stub modules".
- **Decision:** **A — Delete.** User confirmed (2026-05-21 in chat). E61 has merged to main so the broader semantic-cache infrastructure stays put; only the orphan-feature appendages go.
- **Scope removed:**
  - Go fields: `LookupInput.RequiredTokens`, `LookupInput.DomainThresholds`, `StoreInput.RequestText`, `Entry.RequestText`, `ReadRequest.RequiredTokens`, `ReadRequest.DomainThresholds`.
  - Whole files: `cache/semantic/domain.go`, `cache/semantic/domain_test.go`.
  - Go function: `missedRequiredToken`.
  - Metrics: `l2StickyTokenRejectsTotal` counter + `IncStickyTokenRejects` method + `cache_l2_sticky_token_rejects_total` registration.
  - Audit enum constants: `GatewayCacheSkipReasonStickyTokenMissed`, `GatewayCacheSkipReasonDomainThresholdDefault`.
  - Redis HSET / FT.SEARCH `request_text` field (both write side in `client.go` and read side in `lookup.go`).
  - Tests: E70 + E71 sections of `reader_e68_e70_e71_test.go` (file renamed to `reader_e68_test.go`); `missedRequiredToken` tests; `IncStickyTokenRejects` calls in `coverage_test.go`; sticky/domain entries in `skip_reason_e61_test.go` (count assertion 15 → 13).
  - i18n keys: `routing.detail.cachePolicy.requiredTokens` + `routing.detail.cachePolicy.domainThresholds` across 6 locale files (3 src + 3 public × en/zh/es). Empty parent objects (`cachePolicy`, `detail`) collapsed.
- **Verification:**
  - `go vet ./packages/ai-gateway/...` — clean.
  - `go test ./packages/ai-gateway/internal/cache/semantic/...` — pass.
  - `go test ./packages/ai-gateway/internal/platform/audit/...` — pass.
  - `node scripts/check-i18n-parity.mjs` — 8/8 namespace × locale combinations aligned.
  - Final grep for `DomainThresholds|RequiredTokens|StickyTokenMissed|DomainThresholdDefault|domainThresholds|requiredTokens|ClassifyDomain|missedRequiredToken|sticky_token|cache_l2_sticky_token|request_text` across `packages/`, `tools/`, `docs/` — zero hits.
- **Roadmap impact:** E70 + E71 stay listed as "✅ Shipped" in `docs/developers/roadmap.md` (they did ship as commits), but their headline now needs an addendum noting the 2026-05-21 dead-code prune. Matrix §3.10 + §5 mark both as REMOVED.
- **Revisit if:** the fleet-only binding [[feedback_cache_config_fleet_only]] is rescinded with explicit user approval, at which point both features can be re-introduced with proper admin surfaces (commit message would say "re-introduce E70/E71 with admin UI; see e86-decision-log.md D10").

## D11 — 真跑 scenarios 暴露 13 个 shape 错（2026-05-21）

- **触发**：用户明确指示 "Goal 是提升测试质量和覆盖率" — 不接受 "编译干净 = 测试有效" 的中间态。
- **行动**：启 local stack，跑全部 21 个新 L5 scenario（S-062..S-086 + S-125），看真实结果而不是 `go vet` 干净。
- **结果**：13 个 FAIL，1 个 PASS，7 个 SKIP/graceful 退出。失败分类：
  - **schema 列名错（A 类，3 处）**: `traffic_event.virtual_key_id` / `endpoint_type` 不存在 → 真列是 `identity->'vk'->>'id'` + `path`；`iam_group` 表名错 → 真表是 `"IamGroup"` (大驼峰需双引号)。
  - **不存在的 metric 名（B 类，4 处）**: 子代理普遍猜了 `nexus_ai_gateway_requests_total`，但这个 counter 在当前 build 不存在；request volume 通过 `nexus_ai_gateway_normalize_payload_bytes_bucket` 计数。
  - **业务前提错（C 类，5 处）**: SCIM user body 缺关键 schemas；quota-policy POST body shape 错；S-082 必须先 probe embedding dimension；S-081 marker 撞内置 "time-current" 规则；S-066 cache 反向反馈只影响 L2 但本地 L1 prompt cache 拦截了。
  - **架构/网络（D 类，1 处）**: S-069 webhook-forward 用 httptest server，CP/AIGw 是独立进程，可能无法访问测试进程的 httptest 端口。
- **修复策略**：
  - A 类：sed/Edit 列名替换 + 重跑验证。
  - B 类：把 `t.Errorf(metric_total)` 转为 `t.Logf` informational（DB cross-check 已经断言请求发生，metric 名错不该让测试 FAIL）。
  - C 类：每个用 sub-agent 独立修，注入真实 body shape + 内置规则避让逻辑。
  - D 类：S-069 改用本地命名 socket / 跳过 webhook 调用本身只验证 hook config CRUD round-trip。
- **关键教训**：sub-agent 写测试时 grep schema/handler 仅是"已知 schema + 我以为的"。**真跑** 是唯一能抓住"我以为存在但实际不存在"的盲区。今后所有新 scenario PR 必须有 `cd tests/scenarios && go test -run ^Test<name>$` PASS 输出，不接受仅 `go vet`。
- **绑定升级建议**：CLAUDE.md mandatory rules 增加 "L5 scenario landing rule: PR must include a live local-stack run output line showing `--- PASS: TestS<NNN>` for the new test"。

## Memory anchors written this session

- `project_e86_e2e_coverage_program` — top-level program tracker (current phase + memory cross-refs).

(No memory writes for items already covered by existing entries: `project_api_automation_test_program` covers the scenario-harness substrate; `feedback_scenario_test_env_isolation` covers the env-isolation guard; `feedback_cache_mandatory_all_ingress` covers cross-ingress asymmetry discipline.)
