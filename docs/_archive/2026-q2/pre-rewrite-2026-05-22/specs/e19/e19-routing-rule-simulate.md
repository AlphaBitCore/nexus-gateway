# E19 — Routing Rule Simulation

**Status:** Draft — 2026-04-21
**Epic:** 19
**Depends on:** AI Gateway routing engine (`packages/ai-gateway/internal/router`), Control Plane admin API/BFF

## 1. Business Goal

Enterprise operators need a way to preview how the live routing engine would route a request **before** enabling a rule or debugging a production incident. The UI for this feature already exists on the Routing Rule Detail page ("Routing preview (simulate)" card) and the IAM action `admin:SimulateRoutingRule` is registered, but the endpoint is unimplemented — calls to `POST /api/admin/routing-rules/simulate` return 404.

Closing this gap gives operators:

- A deterministic way to answer *"Which rule will match this model request, and which provider+model will win?"* without sending real traffic.
- A configuration-validation tool analogous to the IAM Simulator, used **before** rolling out new routing rules or matchConditions changes.
- Runbook support: when a routing change causes a production surprise, operators can reproduce the exact decision path and share the JSON trace in the incident channel.

The product documentation already declares the feature in `docs/users/product/architecture-deep-dive-zh.md` §20.10.

## 2. Scope

### In scope

- A new internal endpoint on **AI Gateway**: `POST /internal/routing-simulate` that drives the real routing engine (`router.Resolver.Resolve`) and returns a decision trace.
- A new admin endpoint on **Control Plane**: `POST /api/admin/routing-rules/simulate` that authorizes via IAM (`admin:SimulateRoutingRule`) and forwards to the AI Gateway internal endpoint, mirroring the existing `POST /api/admin/providers/:id/test` pattern.
- A typed response shape covering pipeline stages, matched rule, winning rule's strategy trace, primary targets, recovery targets, and narrowing summary.
- Control Plane UI updates: replace `Record<string, unknown>` response with the typed shape; fix the existing button-vs-input vertical misalignment on the simulate card.
- Go unit tests for the simulate handler (no-match, auto modelId, policy narrowing, fallback targets) and a baseline frontend type check.

### Out of scope

- A prettier UI for rendering the decision trace. The current JSON `<pre>` view is kept as MVP; a structured table/tree view is a later UX iteration.
- Simulating with a specific virtual key or organization context. The MVP accepts `modelId`, `endpointType`, and optional `messages` (for smart routing). VK / org context can be added when the matchConditions UI surfaces these inputs.
- Running the simulation against non-current routing rules (e.g. dry-run a rule edit before saving). MVP evaluates the live DB state only.
- Streaming simulation (SSE response preview). Out of scope — simulate returns the plan only, not traffic.

## 3. User Roles & Personas

| Role | Need met by this epic |
|---|---|
| **Platform Admin** | Before enabling a new routing rule, verify it matches the intended models/providers and wins over lower-priority rules. |
| **Provider Ops** | Diagnose "why did model X route to provider Y instead of Z" incidents by replaying the exact model+endpoint combination. |
| **Compliance Officer** | Validate that stage-0 narrowing (allow/deny lists) excludes models and providers correctly before a compliance policy goes live. |
| **Support Engineer** | Reproduce customer-reported routing behavior deterministically; attach the JSON trace to tickets. |

## 4. Functional Requirements

### F1 — Simulate endpoint on Control Plane (MUST)

`POST /api/admin/routing-rules/simulate` MUST exist, require `admin:SimulateRoutingRule` IAM action, accept a JSON body containing `modelId` (required, string, may be `"auto"`), `endpointType` (required, `"chat" | "embeddings" | "models"`), and optional `messages` (array of `{role, content}` — used for smart routing when `modelId == "auto"`). The endpoint MUST return a JSON response matching the shape described in F3 with HTTP 200 on success.

### F2 — AI Gateway internal simulate endpoint (MUST)

`POST /internal/routing-simulate` MUST exist on AI Gateway, reachable service-to-service only (no VK auth, no user JWT — same trust boundary as `/internal/provider-test`). The handler MUST drive the same `Resolver.Resolve` path used by live `/v1/*` traffic, without performing HTTP fan-out to upstream providers and without recording traffic events. The returned decision MUST reflect the current cached routing rules.

### F3 — Response shape (MUST)

The response MUST include, as top-level JSON fields:

- `request`: echo of the normalized simulate input (`modelId`, `endpointType`).
- `originalModelId`: the client-requested model id (useful when substitution occurred).
- `substituted` (boolean): true when the winning target model differs from the requested model.
- `ruleId` / `ruleName` (strings, optional): the stage-1 rule that won. Absent when no rule matched.
- `stages` (array): per-pipeline-stage decision log `{stage, decision, durationMs}`.
- `trace` (array): per-strategy-evaluation entries from the winning rule `{ruleId, ruleName, strategyType, decision, durationMs}`.
- `targets` (array): primary resolved targets `{providerId, providerName, modelId, modelName, providerModelId, source}`.
- `recoveryTargets` (array): fallback/recovery targets (same shape).
- `narrowingSummary` (object, optional): applied stage-0 narrowing `{allowModelIds, denyModelIds, allowProviderIds, denyProviderIds}`. Absent when no stage-0 rules applied.
- `warnings` (array of strings, optional): non-fatal diagnostics, e.g. *"requested modelId not in catalog"*, *"smart routing requested but messages empty"*.

### F4 — Empty-result correctness (MUST)

When no stage-1 rule matches the request, the response MUST still return HTTP 200 with `targets: []`, `ruleId` absent, and a `warnings` entry indicating no rule matched. 404 is reserved for the endpoint itself being absent (which MUST NOT happen after this epic).

### F5 — IAM enforcement (MUST)

The Control Plane handler MUST be protected by the `admin:SimulateRoutingRule` action; unauthorized callers receive 403. The action is already registered in `internal/iam/managed.go` and exposed in the IAM Simulator UI.

### F6 — UI alignment fix (MUST)

The "Run simulation" button on `/config/routing/:id` MUST render vertically aligned with the ModelID input, not with the help text below it. Current `align="end"` on the Stack drops the button to the help-text baseline — this MUST be replaced by an input-row-level alignment.

### F7 — Typed frontend response (MUST)

The `routingApi.simulate` return type in `packages/control-plane-ui/src/api/services/routing.ts` MUST be replaced from `Record<string, unknown>` with a typed interface matching F3. The existing `<pre>` JSON render is retained; structured rendering is a follow-up.

## 5. Non-Functional Requirements

- **Latency**: p95 end-to-end (UI → CP → AG → response) < 500 ms. No upstream HTTP is performed; the cost is DB-cached rule lookup and pure in-memory strategy evaluation.
- **Observability**: AG handler MUST emit a structured log line per simulate call with `modelId`, `endpointType`, winning `ruleId`, target count — mirroring the existing `/internal/provider-test` log pattern.
- **Security**: The endpoint MUST NOT echo back virtual keys, credentials, or header values containing secrets. The request body is permitted to contain short `messages` payloads; the handler MUST truncate `messages` content beyond 4 KB in logs.
- **Isolation**: The simulate path MUST NOT write to `traffic_event`, MUST NOT emit MQ events, MUST NOT increment quota counters, and MUST NOT mutate health tracker state.

## 6. Constraints & Assumptions

- **Engine reuse**: The existing `router.Resolver.Resolve` signature already returns a `RoutingPlan` with pipeline trace and strategy trace. No engine refactor is required; the handler wraps `Resolve` and projects `RoutingPlan` into the response DTO.
- **VK/org context**: MVP passes `rctx.VirtualKey = nil` and uses no organization context. Rules with `matchConditions.virtualKeys` or `.organizations` specified will therefore not match in simulation unless those fields are empty on the rule. A follow-up may accept `virtualKeySlug` / `organizationId` in the simulate body.
- **Health ranker bypass**: Simulate MUST NOT consult live health/latency state. Target ordering in the response reflects the pre-health-ranker plan (`plan.Targets` / `plan.RecoveryTargets` directly).
- **English-only**: Response strings (`stages[*].decision`, `trace[*].decision`, `warnings[*]`) are English. The AG engine already emits English decisions; no i18n on the backend.

## 7. Glossary

| Term | Meaning |
|---|---|
| **Simulate** | A read-only, side-effect-free replay of the routing engine for a hypothetical request. |
| **Decision trace** | The ordered record of pipeline-stage and strategy evaluations, each with a human-readable decision string. |
| **Narrowing** | Stage-0 policy rules that restrict the allowed model/provider set before stage-1 strategies pick a target. |
| **Substitution** | When the routing engine's winning target uses a model different from the one the client requested (e.g. smart routing picks `gpt-4o-mini` for `auto`). |

## 8. Priority (MoSCoW)

| Requirement | Priority |
|---|---|
| F1 — CP simulate endpoint | MUST |
| F2 — AG internal simulate endpoint | MUST |
| F3 — Typed response shape | MUST |
| F4 — Empty-result correctness | MUST |
| F5 — IAM enforcement | MUST |
| F6 — UI alignment fix | MUST |
| F7 — Typed frontend response | MUST |
| Structured UI rendering (table/tree) for the trace | SHOULD (follow-up) |
| Simulate with virtualKey/org context | COULD (follow-up) |
| Simulate a draft rule before save | WON'T (out of scope) |

## 9. Architecture Impact

No architecture impact. This epic adds one internal HTTP endpoint on AI Gateway and one admin endpoint on Control Plane, reusing the existing `Resolver` and the existing CP-to-AG service trust boundary (same pattern as `/internal/provider-test`). No new service, no new data store, no new queue, no new cache. `docs/users/product/architecture.md` does not require an update.
