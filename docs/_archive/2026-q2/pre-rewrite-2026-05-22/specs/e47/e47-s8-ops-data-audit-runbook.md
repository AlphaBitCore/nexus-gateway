# E47 S8 — Admin API matchConditions guard + ops audit runbook

**Epic:** E47
**Requirements:** [e47-routing-canonical-payload.md](../../../../docs/developers/specs/e47/e47-routing-canonical-payload.md) — Must M6, M7
**OpenAPI:** none (extends the existing 400-error envelope on POST/PATCH `/api/admin/routing-rules`)
**Status:** Approved (user-approved plan 2026-05-13)
**Date:** 2026-05-13
**Builds on:** [e47-s5-smart-routing-negative-case.md](e47-s5-smart-routing-negative-case.md)

---

## Architecture summary

E47-S2 fixed the smart-routing missing-user-messages bug at the gateway data path. S8 is the **operator-side guard** that prevents the misconfiguration that surfaced the bug in production from being recreated. The root operator error was setting `RoutingRule.matchConditions = {}` on a `strategyType=smart` rule, which made the smart strategy fire on every request — including ones where smart routing made no sense.

S8 adds a server-side validation step in the control-plane admin API that rejects (HTTP 400) any RoutingRule with `strategyType == "smart"` whose `matchConditions` would broaden the smart strategy beyond `"auto"`:

- `matchConditions` is null / empty / `{}` — rejects with "must include `requestedModelLiterals`".
- `matchConditions.requestedModelLiterals` missing — same.
- `matchConditions.requestedModelLiterals` empty array — same.
- `matchConditions.requestedModelLiterals` contains a literal other than `"auto"` — rejects with a specific "literal X is not safe" message.

The error message points at the runbook `docs/operators/ops/runbooks/r-routing-rule-matchconditions-audit.md`, which documents the SQL query for finding dangerous existing rules and the recommended remediation. If an operator has a legitimate reason to bypass the guard (e.g. an internal experiment that smart-routes a specific model alias) they can SQL-update the row directly — the runbook calls this out as a deliberate escape hatch.

### Why not a force flag in the API

A `?force=true` query parameter on the admin endpoint was considered and rejected. Force flags become habitual: a UI that hits the validation error twice will start sending `force=true` reflexively, defeating the purpose. SQL bypass is a higher-friction escape valve that requires explicit operator intent and leaves an audit trail in the operator's shell history.

### Server-side only — no UI confirm modal in S8

The admin UI's confirmation modal (CLAUDE.md "ask the user when confirmation is needed") is a nice-to-have that depends on the i18n key catalog being current and on the Vitest fixture being green. S8 ships the backend guard first; the UI modal is tracked as a follow-up (could be E47-S9 if needed but is intentionally not blocking the bug-fix release).

### Update semantics

The guard fires on both `POST /api/admin/routing-rules` (CreateRoutingRule) and `PATCH /api/admin/routing-rules/:id` (UpdateRoutingRule). For Update the check applies when the request body supplies both `strategyType` and `matchConditions` together — the common UI-driven case where the full rule is resubmitted. The edge case where an operator updates only `matchConditions` on an existing smart rule is not blocked at the API; the runbook covers retroactive audit + cleanup for that path.

---

## Story

### S8 — Admin API matchConditions guard + ops audit runbook

**User story:** As a Nexus platform operator, when I create or update a smart-strategy RoutingRule, I want the admin API to reject configurations that would broaden the smart strategy to non-`"auto"` traffic — so I cannot re-introduce the production foot-gun that smart-routed every request through a router LLM that had no user-content visibility.

**Tasks:**

- **T8.1** — `packages/control-plane/internal/handler/admin_routing.go`:
  Add a new helper `validateSmartRuleMatchConditions(strategyType string, raw json.RawMessage) (string, bool)`:
  - Returns `("", true)` when `strategyType != "smart"` — no-op for other strategies.
  - When `strategyType == "smart"`: parses the JSONB and rejects (returns false) if `matchConditions` is empty / nil / `{}`, OR if `matchConditions.requestedModelLiterals` is missing / empty / contains a literal other than `"auto"`.
  - Rejection messages point at the runbook by relative path so the operator-facing error is self-documenting.

- **T8.2** — Wire the helper into both routing-rule write paths:
  - `CreateRoutingRule`: after the existing `validateMatchConditions` call (the legacy-organizations check), call `validateSmartRuleMatchConditions(body.StrategyType, body.MatchConditions)` and return HTTP 400 on rejection.
  - `UpdateRoutingRule`: after the existing legacy-key check, when both `body.StrategyType != nil` and `body.MatchConditions != nil`, run the same guard against the post-update state.

- **T8.3** — `packages/control-plane/internal/handler/admin_routing_validate_test.go`:
  Add table-driven cases for:
  - `strategyType=single` with arbitrary matchConditions → pass.
  - `strategyType=smart` + empty body / `null` / `{}` → reject.
  - `strategyType=smart` + matchConditions without `requestedModelLiterals` → reject.
  - `strategyType=smart` + `requestedModelLiterals: []` → reject.
  - `strategyType=smart` + `requestedModelLiterals: ["auto"]` → pass.
  - `strategyType=smart` + `requestedModelLiterals: ["auto", "claude-opus"]` → reject (mentions the offending literal).

- **T8.4** — `docs/operators/ops/runbooks/r-routing-rule-matchconditions-audit.md`:
  Audit runbook covering:
  - Background: link to the filed bug report and the E47 fix.
  - SQL query to find dangerous existing rules in production.
  - Recommended remediation SQL.
  - Sign-off checklist for the operator running the audit.
  - Escape hatch: when an operator legitimately needs a non-auto smart rule, the SQL-update bypass with a "you accept these implications" note.

**Acceptance:**

- POST `/api/admin/routing-rules` with `{ strategyType: "smart", matchConditions: {} }` returns HTTP 400.
- POST `/api/admin/routing-rules` with `{ strategyType: "smart", matchConditions: { "requestedModelLiterals": ["auto"] } }` succeeds.
- PATCH `/api/admin/routing-rules/:id` with `{ strategyType: "smart", matchConditions: { "requestedModelLiterals": ["auto", "claude-opus"] } }` returns HTTP 400 with a message referencing `claude-opus`.
- Non-smart strategies (single, fallback, loadbalance, conditional, ab_split, policy) pass through the new validator unaffected.
- The runbook file exists, lists the SQL audit + remediation, and links from the validator's error messages.

**Validation script:**

```bash
go build ./packages/control-plane/...
go test -race -count=1 ./packages/control-plane/internal/handler/... -run TestValidate

# Manual integration check (after deployment):
cp_curl -X POST /api/admin/routing-rules -d '{
  "name": "smart-bad",
  "strategyType": "smart",
  "matchConditions": {},
  "config": {"routerProviderId": "p", "routerModelId": "m"}
}'   # expect 400 with runbook reference

cp_curl -X POST /api/admin/routing-rules -d '{
  "name": "smart-good",
  "strategyType": "smart",
  "matchConditions": {"requestedModelLiterals": ["auto"]},
  "config": {"routerProviderId": "p", "routerModelId": "m"}
}'   # expect 200/201
```
