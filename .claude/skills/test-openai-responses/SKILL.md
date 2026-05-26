---
name: test-openai-responses
description: >
  End-to-end synthetic test for the OpenAI Responses-API ingress (E56).
  Sends 5 hand-rolled requests through the local AI Gateway on
  http://localhost:3050/v1/responses covering text non-stream, text SSE,
  function-call SSE, structured outputs (text.format json_schema), and
  reasoning-effort=high non-stream. For each arm, verifies HTTP response
  shape, cross-checks the traffic_event DB row's endpoint_type=
  "responses", and diffs Prometheus counters. Optional --cross-format
  arm hits a routing rule that resolves to non-OpenAI to verify the S6
  guard rejects previous_response_id / store / built-in tools with a
  Responses-shape 400. Trigger keywords: test openai responses,
  responses-api test, /v1/responses synthetic, /test-openai-responses,
  responses smoke, e56 smoke.
user-invocable: true
---

# Test OpenAI Responses API ingress

Synthetic end-to-end test for the `POST /v1/responses` ingress added in
E56. Mirrors the pattern of `/test-cursor-adapter` and
`/test-geminiweb-adapter`: hand-roll a request body, send it through
the live local AI Gateway, then cross-check the resulting traffic_event
row in Postgres.

## When to use

- After editing anything under `packages/ai-gateway/internal/providers/spec_openai/codec_responses*.go` or `stream_responses.go`.
- After modifying the Responses bridge wiring in `canonicalbridge/bridge.go` or `stream_encoders_responses.go`.
- After the gateway is rebuilt / restarted and you want to confirm `/v1/responses` is mounted and routable.
- As a smoke gate before merging any E56 follow-up PR.
- As the regression suite for the OpenAI Responses-API surface — the broader smoke (`/smoke-gateway`) only hits `--responses` opt-in arm.

## Prerequisites

Local stack must be healthy:

```bash
curl -fsS http://localhost:3050/healthz   # ai-gateway
curl -fsS http://localhost:3001/ready     # control-plane
docker ps --filter "name=postgres" -q     # postgres container
```

A virtual key with at least one routing rule pointing at an OpenAI
provider with a model in the Responses-supported list (gpt-5.x /
gpt-4o family / o-series). Either:

1. **Use the project's standing local test VK** (recorded in memory: `[Local test VK](project_local_test_vk.md)`) and re-use it.
2. **Mint a fresh VK** via prod-login + research-all-models — only if a fresh key is needed for the run.

The Responses-supported model the skill exercises by default is `gpt-5.2`; override with `--model`.

## How to run

```bash
# Default — uses tests/.env.local for credentials and the standing local VK.
.claude/skills/test-openai-responses/run.sh

# Or explicitly:
.claude/skills/test-openai-responses/run.sh \
  --vk nvk_... \
  --model gpt-5.2 \
  --gw-url http://localhost:3050
```

Output: a Markdown report at `/tmp/test-openai-responses-<UTC-timestamp>.md`.

## Test arms

| Arm | Body skeleton | Pass criteria |
|---|---|---|
| 1. text non-stream | `{"model":"gpt-5.2","input":"haiku about clouds","max_output_tokens":40}` | HTTP 200, response has `object:"response"`, `status:"completed"`, `output[].type:"message"`, `usage.input_tokens > 0`. |
| 2. text SSE | arm-1 body + `"stream":true` | HTTP 200, `Content-Type: text/event-stream`, at least one `event: response.output_text.delta`, final `event: response.completed` with `usage.input_tokens > 0`. |
| 3. function-call SSE | text body + `tools:[{type:"function","function":{name:"get_weather","parameters":{...}}}]` + `"input":"weather in Tokyo?"` | HTTP 200, SSE contains `event: response.function_call_arguments.delta`, terminal `response.completed`. |
| 4. structured outputs | `text.format:{type:"json_schema",json_schema:{name:"city_extract",schema:{type:"object",properties:{city:{type:"string"}}}}}` + `"input":"Meeting in Tokyo on March 5"` | HTTP 200, `output[0].content[0].text` parses as JSON matching schema. |
| 5. reasoning effort | `reasoning:{effort:"high"}` + `"input":"prove that sqrt(2) is irrational"` | HTTP 200, `output[].type:"reasoning"` item present (when model supports thinking) OR usage.output_tokens_details.reasoning_tokens > 0. |

After each arm, the skill queries:

```sql
SELECT id, endpoint_type, prompt_tokens, completion_tokens,
       reasoning_tokens, prompt_cache_tokens
FROM traffic_event
WHERE id = '<x-nexus-request-id from the response>';
```

and asserts `endpoint_type = 'responses'` plus non-NULL token columns where applicable.

Prometheus delta:

```bash
curl -s http://localhost:3050/metrics | grep -E '^ai_gateway_request_total\{.*endpoint_type="responses"'
```

must increase by exactly 5 (one per arm).

## Optional --cross-format arm

Runs the same arms against a routing rule resolving to a **non-OpenAI** provider (e.g. Anthropic). Verifies:

1. Arms 1, 2, 4, 5 succeed (cross-format canonical bridge round-trips).
2. Adding `"previous_response_id":"resp_abc"` to any arm returns HTTP 400 with `error.code = "feature_requires_native_responses_target"` and `error.param = "previous_response_id"` (S6 guard).
3. Adding `"tools":[{"type":"web_search"}]` returns HTTP 400 with `error.param = "tools[0].type"`.

## Failure recovery (auto-fix loop)

When an arm fails, the skill produces a diagnostic block in the report:
- HTTP status + body excerpt
- Recent `packages/ai-gateway/logs/ai-gateway.log` lines for the request_id
- The exact SQL the skill ran and the empty / mismatching row
- Top-3 candidate code locations for the bug (based on the failing assertion)

Suggested loop:
1. Read the diagnostic block + log excerpt
2. Edit the candidate file
3. Rebuild + restart ai-gateway (see CLAUDE.md "Service lifecycle")
4. Re-run `/test-openai-responses` until green.

## Output report format

The report is Markdown with the following sections:

```
# /test-openai-responses — <UTC timestamp>

## Summary
- Total arms: 5 (or 8 with --cross-format)
- Passed: N
- Failed: N

## Arm 1 — text non-stream
- Status: PASS / FAIL
- HTTP: 200
- request_id: ...
- traffic_event.endpoint_type: responses
- traffic_event.prompt_tokens: ...
- usage parity: ok / mismatch
- Diagnostics (when FAIL): ...

## Arm 2 — text SSE
...

## Conclusion: PASS | FAIL (N issue(s))
```

## Co-existence with /smoke-gateway --responses

- `/smoke-gateway --responses` exercises **every catalog model that prefix-matches the supported list** but only with 2 arms (non-stream + SSE happy path). It's the broad coverage.
- `/test-openai-responses` runs a **single configurable model** but covers **5 distinct request shapes** + optional cross-format guard verification. It's the depth coverage.

Use `/test-openai-responses` first when iterating on E56 code; use `/smoke-gateway --responses` to confirm no regression across the OpenAI catalog before commit.
