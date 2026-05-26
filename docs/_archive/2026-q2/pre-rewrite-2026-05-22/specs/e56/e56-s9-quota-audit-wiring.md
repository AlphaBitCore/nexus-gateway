# E56-S9 — Quota / audit / routing-simulate / smoke wiring

**Epic:** E56 OpenAI Responses-API ingress
**Type:** Ancillary wiring (no new behavior, plumbing only)
**Owner:** nexus
**Depends on:** S1-S8.

## User story

> As an operator, I want Responses-API traffic to appear in dashboards,
> quota counters, and routing-rule simulation under the same surfaces as
> chat-completions, separated only by an `endpointType="responses"`
> facet — so I don't need to learn a new admin UI to manage it.

## Tasks

### T9.1 — Quota counter bucketing

**File:** `packages/ai-gateway/internal/pipeline/quota/` (per-org counter keys)

Confirm the existing sliding-window key includes `endpointType` as a discriminator. If it currently keys on `(orgID, providerID, modelID)` only, extend to `(orgID, providerID, modelID, endpointType)` so a configurable rate limit on `chat/completions` doesn't accidentally cap `responses` or vice versa.

If the keying is already endpoint-aware, no change. Verify by reading `packages/ai-gateway/internal/pipeline/quota/counter.go` (or equivalent).

### T9.2 — Audit pipeline

**File:** `packages/shared/audit/` (audit envelope schema)

`traffic_event.endpoint_type` is already a free `text` column (verified prior to S1). No schema change needed; new value `"responses"` writes through. Add a unit test in `audit_test.go` pinning the round-trip with `endpoint_type="responses"`.

### T9.3 — Routing-rule simulate endpoint

Done in S1-T1.5. T9.3 here is a sanity re-check: a simulate request with `endpointType:"responses"` returns the same shape the live proxy would route to.

### T9.4 — /smoke-gateway skill extension

**File:** `.claude/skills/smoke-gateway/SKILL.md`

Add a Responses-API test arm to the smoke matrix. For each model in the catalog that ships with `RequestShapes` including `responses-api` (today: OpenAI gpt-5.x, gpt-4o, o-series; ignore others), the smoke runs:

1. Non-stream text request via `/v1/responses` with `input:"…"`, expect 200 + Responses-shape body.
2. SSE stream request, expect Responses event sequence including `response.completed`.
3. Function-call request, expect `output:[{type:"function_call",...}]`.

Cross-check `traffic_event` row count delta + Prometheus counter delta (`ai_gateway_request_total{endpoint_type="responses"}`).

If the routing rule resolves a non-Responses target, the test arm asserts the cross-format path returns 200 with response shape converted — but does NOT exercise stateful fields / built-in tools (those are S6's 400 contract, verified by S10's integration test, not by /smoke-gateway against live providers).

### T9.5 — Prometheus metric label cardinality

Validate that adding `endpoint_type="responses"` to existing `ai_gateway_request_total` / `ai_gateway_request_duration_seconds` etc. doesn't blow up label cardinality: total endpoint_type values stays at 5 (`chat/completions`, `responses`, `embeddings`, `models`, `completions`). Within Prom best-practice bounds.

### T9.6 — Tests

**File:** `packages/ai-gateway/internal/handler/routing_simulate_endpoint_test.go` (extend)

- Add case: `endpointType:"responses"` resolves correctly.

**File:** `packages/ai-gateway/internal/pipeline/quota/counter_test.go` (extend)

- Add case: `responses` and `chat/completions` traffic for the same (org, provider, model) use independent counters.

## Acceptance criteria

- AC-9.1: `traffic_event` writes succeed with `endpoint_type="responses"` (covered by S10 integration test).
- AC-9.2: Quota counter for `responses` is independent from `chat/completions`.
- AC-9.3: routing-rule simulate handles `endpointType="responses"` cleanly.
- AC-9.4: `/smoke-gateway --vk <vk>` shows a green Responses-API arm in its Markdown report.
- AC-9.5: Prometheus metric cardinality count for `endpoint_type` label is ≤ 5 in prod after deploy.

## Verification

```
go test ./packages/ai-gateway/internal/pipeline/quota/ -race -count=1
go test ./packages/ai-gateway/internal/handler/ -run TestRoutingSimulate -race -count=1
/smoke-gateway --vk $LOCAL_TEST_VK
```

## Risks

- **R-9.1:** If quota keying is NOT already endpoint-aware (i.e. `(org,provider,model)` only), a customer running both `/v1/chat/completions` and `/v1/responses` on the same model sees their cap halve. T9.1's verification step is critical. If the keying isn't endpoint-aware today, this becomes a non-trivial change — flag it during implementation and ask before extending.
