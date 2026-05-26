# Quota + kill-switch + AI-Guard refactor handoff

Three architectural cleanups initiated in this program. **PR-A landed**; **PR-B + PR-C remain to do** in fresh sessions to avoid context bloat.

## Goal

Eliminate three pieces of accumulated architectural debt the doc-write audit pass surfaced:

1. **Q1 (PR-A — landed)** — VK budget enforcement had two parallel surfaces (per-VK `budgetLimitUsd` column hard cap + the hierarchical QuotaPolicy/QuotaOverride system). Two mental models for the same concept; legacy surface is strictly less capable than the new one.
2. **Q2 (PR-B — TODO)** — kill-switch wire field `enabled` has **inverted semantics** between compliance-proxy (`enabled=true` ⇒ kill engaged ⇒ blocking off) and agent (`enabled=true` ⇒ bump allowed ⇒ blocking on). Agent runs an `IsKillSwitchEngaged()` inversion wrapper. Footgun for maintainers; violates the user idiom of "kill switch = thing it kills is off."
3. **Q3 (PR-C — TODO)** — `ReasonAIGuardSuggestedVsPolicy` constant + UI chip + i18n bundles all anticipate the AI-Guard reconcile-with-admin-policy event, but **no Go producer exists**. AI-Guard hook returns raw LLM Decision without reconciling against `OnMatchConfig.InflightAction`. Half-built feature.

## Status

| PR | Scope | Status |
|---|---|---|
| PR-A | Q1 VK budgetLimitUsd cleanup | **Landed** on `feature/docs-backfill`. See commit log. |
| PR-B | Q2 kill-switch rename `enabled` → `engaged` | TODO — start fresh session |
| PR-C | Q3 AI-Guard reconcile producer | TODO — start fresh session |

## PR-B — kill-switch wire rename

### Context

Wire shape: shadow key `killswitch` carries `{ enabled: bool }`. Receivers:

- **compliance-proxy** (`packages/compliance-proxy/...`) — `enabled=true` ⇒ engage kill switch ⇒ fall through TLS-bump, do passthrough relay.
- **agent** (`packages/agent/...`) — `enabled=true` ⇒ bump allowed (normal operation). Bridge `IsKillSwitchEngaged()` inverts to make the same wire shape produce opposite local semantics.

CP-UI writes `enabled=true` and the bridge handles the inversion so admins see consistent UX, but the code is a maintenance trap.

### Plan

1. **Rename wire field** `enabled` → `engaged` in shadow schema.
2. **Both receivers read `engaged` directly** — no inversion wrapper.
3. **Default `false`** (fail-safe baseline: kill not engaged ⇒ normal).
4. **Delete `agent.bridge.IsKillSwitchEngaged()` wrapper** — single read everywhere.
5. **CP-UI label** changes from "Enable/Disable Kill Switch" to "Engage/Disengage Kill Switch" (avoid the "enable kill = enable blocking? or enable kill = engage kill?" ambiguity).
6. **Shadow data migration** — flip existing prod rows: agent-type rows with `enabled=true` → `engaged=false`; agent-type rows with `enabled=false` → `engaged=true`; CP rows pass through unchanged (`enabled` value = `engaged` value).

### File touch list (estimate)

- `packages/shared/schemas/configkey/` — schema rename + validation.
- `packages/compliance-proxy/...` — kill-switch reader callers (likely `internal/proxy/forward/` and config loader).
- `packages/agent/...` — bridge `IsKillSwitchEngaged()` definition + every caller.
- `packages/control-plane/internal/governance/killswitch/handler/handler.go` — admin write path.
- `packages/control-plane-ui/src/pages/.../KillSwitchPage.tsx` (or wherever) — toggle label.
- `packages/control-plane-ui/src/i18n/locales/{en,es,zh}/pages.json` + `public/locales/...` — string keys.
- Shadow data migration in `tools/db-migrate/migrations/` if shadow lives in Postgres, otherwise an ops one-shot script in `tools/scripts/`.

### Doc updates

- **B04 `kill-switch-architecture.md`** — current draft describes the inverted `enabled` semantics; rewrite §5 fail-open behaviour to describe `engaged` semantic; describe both services reading the same field identically.

### Verification

- `go vet` + tests across compliance-proxy + agent + CP.
- AIGW + agent smoke (`tests/scripts/...`).
- Manual: prod-shadow simulate write `{engaged:true}` → both services enter passthrough.

## PR-C — AI-Guard reconcile producer

### Context

Constant + UI chip + i18n bundles for `AIGUARD_SUGGESTED_VS_POLICY` are wired (`packages/shared/policy/decision/types.go`, `packages/control-plane-ui/src/pages/traffic/audit-drawer/trafficAuditDrawer.tsx`, `packages/control-plane-ui/src/i18n/locales/{en,es,zh}/pages.json`). But the producer that would stamp it on `rec.HookReasonCode` does not exist. `packages/shared/policy/hooks/core/onmatch.go` doc comment says "aiguard once re-platformed" — confirms aiguard is awaiting OnMatch integration.

Current aiguard hook (`packages/ai-gateway/internal/policy/aiguard/classify.go`) returns raw LLM `Decision string`; no reconcile against admin `OnMatchConfig.InflightAction`.

### Plan

1. **AI-Guard hook reads `OnMatchConfig`** via `core.ParseOnMatch` at config time.
2. **At Execute time**, after LLM judge produces `suggested Decision`, compute `strictest(suggested, onMatchInflight)` using `core.StrictestStorageAction` or an equivalent inflight helper.
3. **If reconciled decision ≠ suggested decision** → set `HookResult.ReasonCode = ReasonAIGuardSuggestedVsPolicy` + set `Reason` to a string carrying both values (e.g. `AI-Guard suggested redact; policy ceiling: block-hard`).
4. **Tests** — unit test the reconcile + e2e verify the chip renders in CP-UI traffic-audit drawer.

### File touch list (estimate)

- `packages/ai-gateway/internal/policy/aiguard/classify.go` — Execute reconcile.
- `packages/ai-gateway/internal/policy/aiguard/config_cache.go` (or `inproc.go`) — OnMatchConfig parsing into hook config.
- Test file(s) under `packages/ai-gateway/internal/policy/aiguard/`.
- Possibly a CP-side i18n test confirming the chip + locale strings already work.

### Doc updates

- **B06 `pii-redaction-policy-architecture.md`** — current draft says producer exists; rewrite §5 (Decision precedence) to describe the wired reconcile path + cite the actual stamp site.

### Verification

- `go vet` + tests.
- E2E: craft a hook config with `inflightAction=block-hard`, configure aiguard to suggest `redact`, send traffic, verify `traffic_event.request_hook_reason_code = AIGUARD_SUGGESTED_VS_POLICY` + UI chip renders.

## Cross-cutting reminders (binding)

- Both PRs use `/doc-write` skill for the affected doc + `/doc-review` skill for the audit gate. Per-claim verdicts; CLEAN before commit.
- No archaeology, no dates, no Epic/SDD/bug refs, no line numbers in doc body, `## References` section at end.
- One doc per commit; show user a summary in user's preferred language and wait for approval before commit.
- Sub-agent dispatch must inline the supreme-constitution rules — sub-agents have zero session memory.
- AIGW smoke after any change under `packages/ai-gateway/**` (CLAUDE.md binding) — partial scope acceptable per scoped-retest memory.

## Decision log behind these changes

User Q&A surfaced the three issues during a doc-write pass on B03/B04/B06. All three follow the "delete instead of add / perfect architecture no compromise" principle:

- Q1: dual surfaces collapse to one; per-VK column dropped without compatibility shim.
- Q2: wire semantic inversion is a footgun → rename + collapse inversion wrapper.
- Q3: half-built feature finished, not deferred — wired producer + reconcile.

Per user explicit direction: "全部自动执行" (full autonomous), "遇到问题使用 brainstorm" (independently brainstorm), "完美架构无妥协" (perfect architecture no compromise).
