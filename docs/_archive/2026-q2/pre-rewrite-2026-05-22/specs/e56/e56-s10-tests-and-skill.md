# E56-S10 — Integration tests + new /test-openai-responses skill

**Epic:** E56 OpenAI Responses-API ingress
**Type:** Test harness + skill
**Owner:** nexus
**Depends on:** S1-S9.

## User story

> As a developer landing E56, I want a single command (`/test-openai-responses`)
> that hits a running AI Gateway, sends 5 hand-rolled Responses-API
> requests covering the main code paths, and reports DB / Prometheus
> evidence that everything stamped correctly — so I never ship a broken
> Responses path to prod.

> As an operator, I want `/test-all` to include the Responses-API smoke
> so every release regression-tests it without manual intervention.

## Tasks

### T10.1 — Handler integration tests

**File:** `packages/ai-gateway/internal/handler/integration_test.go` (extend)

New test cases under a `t.Run("ResponsesAPI", ...)` group, each spinning up the handler with a mock upstream:

| Case | Setup | Assertion |
|---|---|---|
| I1 | Routing target = mock OpenAI provider (RequestShapes ⊇ responses-api); body has `input:"hi"`, `model:"gpt-5.2"`. | Mock upstream receives `/v1/responses` POST with verbatim body. Response body returned to client is verbatim. `traffic_event.endpoint_type = "responses"`. |
| I2 | Routing target = mock Anthropic provider; body has `input:"hi"`, `model:"claude-sonnet-4-6"`, `instructions:"be brief"`. | Mock upstream receives `/v1/messages` with `system:"be brief"`, `messages:[{role:"user",content:"hi"}]`. Response synthesized as Anthropic /v1/messages shape; client receives Responses-shape body with `output:[{type:"message",...}]`. |
| I3 | Routing target = mock Anthropic; body has `previous_response_id:"resp_abc"`. | 400 with Responses-shape error envelope, `error.param == "previous_response_id"`. |
| I4 | Routing target = mock Anthropic; body has `tools:[{type:"web_search"}]`. | 400 with `error.param == "tools[0].type"`. |
| I5 | Routing target = mock OpenAI; body has `stream:true`. | Mock upstream emits a canned Responses SSE transcript. Client receives the verbatim transcript. Prometheus stream-completion counter increments. |
| I6 | Routing target = mock Anthropic; body has `stream:true`. | Mock upstream emits Anthropic SSE. Client receives Responses-shape SSE (`response.created` → … → `response.completed`). `sequence_number` monotonic, starts at 0. |
| I7 | Routing target = mock OpenAI; body has `stream:true`; mid-stream upstream emits a 500. | Client receives upstream events through the failure point, then a `response.failed` SSE event with the error code. |
| I8 | Cache HIT path: I1 run twice; second run hits cache. | Second run's response body is byte-stable with first run; usage block is Responses-shape; `traffic_event.cache_hit=true`. |
| I9 | Same-shape passthrough never invokes Responses codec — verified by spying on `spec_openai.DecodeResponsesRequest`/`EncodeResponsesResponse` call counts. | Count == 0. |
| I10 | Cross-format path always invokes codec — same spy on I2. | Count == 1 (request) + 1 (response). |

### T10.2 — Skill: /test-openai-responses

**File:** `.claude/skills/test-openai-responses/SKILL.md` (new)

Skill body (analogous to `test-cursor-adapter`, `test-geminiweb-adapter`, `test-compliance-proxy`):

```
End-to-end synthetic test for AI Gateway's /v1/responses ingress (E56).
Sends 5 hand-rolled Responses-API requests through the local AI Gateway
on http://localhost:3050, then verifies the resulting traffic_event row
shows endpoint_type="responses" and the response body shape matches.

Required env: LOCAL_TEST_VK (from tests/.env.test).

Test arms:
  1. text-non-stream:  input:"haiku about clouds"
  2. text-sse:         same + stream:true
  3. function-call-sse: tools:[{type:function,function:{name:get_weather,...}}], input:"weather in Tokyo", stream:true
  4. structured-outputs: text:{format:{type:json_schema,...}}, input:"...", non-stream
  5. reasoning-high: input:"...", reasoning:{effort:"high"}, non-stream

For each arm:
  - assert HTTP 200 (or for arm 5 on a non-reasoning model, accept upstream 400)
  - assert response body shape (object=="response", output[] exists, usage.input_tokens > 0)
  - cross-check DB: SELECT id,endpoint_type,prompt_tokens,completion_tokens,
    reasoning_tokens,prompt_cache_tokens FROM traffic_event WHERE id=?
  - cross-check Prom: ai_gateway_request_total{endpoint_type="responses"} delta

Output: Markdown report at /tmp/test-openai-responses-<UTC-timestamp>.md
```

### T10.3 — Wire into /test-all

**File:** `tests/run-all.sh`

Add a line in the L1 smoke + L2 protocol stages invoking `/test-openai-responses`. Fail-fast on non-zero exit; surface the Markdown report into the unified `/tmp/nexus-test/test-all-*.md`.

### T10.4 — Existing-traffic regression

**File:** `packages/ai-gateway/internal/handler/proxy_test.go` (extend)

After all E56 code lands, add a single regression test that pins a vanilla `/v1/chat/completions` request (text non-stream, OpenAI target, OpenAI ingress) still produces the exact same upstream POST body + the same response body it did before E56. Mock-driven. Pinned via golden file `testdata/regression/chat_completions_baseline_post_e56.json`.

## Acceptance criteria

- AC-10.1: All 10 integration test cases (T10.1) pass.
- AC-10.2: `/test-openai-responses` runs green against a fresh local AI Gateway start (`./scripts/dev-start.sh` + restart ai-gateway via the dev workflow).
- AC-10.3: `/test-all` includes the new arm and stays green.
- AC-10.4: T10.4 regression test passes — no chat-completions behavior change.

## Verification

```
go test ./packages/ai-gateway/internal/handler/ -run TestIntegrationResponsesAPI -race -count=1
/test-openai-responses
/test-all
```

## Risks

- **R-10.1:** Mock upstream subtleties — getting the cross-format Anthropic-SSE-to-Responses-SSE conversion test (I6) to faithfully reproduce production-like SSE timing is delicate. Use the existing `testhelpers_test.go::mockProvider` patterns plus a canned `[]byte` transcript; don't try to mock the real Anthropic API's network behavior.
- **R-10.2:** `/test-openai-responses` against a model that isn't in the local provider's credential pool will fail at upstream auth. The skill MUST verify the configured local test VK has at least one routing rule pointing at OpenAI with a model in the supported list (gpt-5.x / o-series); if not, skip arms 1-5 with a clear "no upstream credential" report rather than reporting test failure.
