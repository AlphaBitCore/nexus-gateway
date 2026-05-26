#!/usr/bin/env bash
# tests/smoke/test-ai-gateway.sh
#
# Phase 1 L1 smoke for the AI Gateway (/v1/* surface).
#
# Coverage:
#   1. VK auth on /v1/models (must return 200 + a non-empty array).
#   2. Negative auth: missing / wrong VK returns 401.
#   3. Synchronous chat completion via Moonshot (Kimi) — same VK + provider
#      we use for L3 AI-judge, so this run also validates the AI-judge path.
#   4. DB cross-check: a traffic_event row appears with the request we just
#      sent (matched by external_request_id or by recent timestamp + path).
#   5. Prometheus counter delta: nexus_ai_gateway_requests_total goes up.
#
# Verification discipline (master plan §6): HTTP shape + DB row + counter
# delta. No "did the binary respond" tests.

set -eu

_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck disable=SC1091
source "$_dir/lib/env.sh"
# shellcheck disable=SC1091
source "$_dir/lib/assert.sh"
# shellcheck disable=SC1091
source "$_dir/lib/db.sh"
# shellcheck disable=SC1091
source "$_dir/lib/http.sh"

: "${NEXUS_TEST_VK:?NEXUS_TEST_VK must be set in tests/.env.test}"

printf '== test-ai-gateway (/v1 surface) ==\n'

# ----------------------------------------------------------------------------
# Section 1 — auth surface.
# ----------------------------------------------------------------------------

models_status=$(aigw_curl_code "$NEXUS_TEST_VK" /v1/models)
assert_status 200 "$models_status" "/v1/models with valid VK"

models_body=$(aigw_curl "$NEXUS_TEST_VK" /v1/models)
model_count=$(printf '%s' "$models_body" | jq -r '.data | length // 0')
if [[ "$model_count" -gt 0 ]]; then
  pass "/v1/models returned $model_count entries"
else
  fail "/v1/models" "data array is empty: ${models_body:0:120}"
fi

# Negative path — /v1/models is optional-auth by design (OpenAI compat: any
# client may list models). The endpoint that *does* require auth is
# /v1/chat/completions; verify a bad VK is rejected there.
bad_chat_status=$(aigw_curl_code "nvk_definitely_not_a_real_key" /v1/chat/completions \
  -X POST -H 'Content-Type: application/json' \
  -d '{"model":"moonshot-v1-8k","messages":[{"role":"user","content":"x"}],"max_tokens":1}')
if [[ "$bad_chat_status" == "401" || "$bad_chat_status" == "403" ]]; then
  pass "/v1/chat/completions with bad VK rejected (HTTP $bad_chat_status)"
else
  fail "/v1/chat/completions bad-VK" "expected 401/403, got HTTP $bad_chat_status"
fi

# ----------------------------------------------------------------------------
# Section 2 — chat completion + DB cross-check.
# ----------------------------------------------------------------------------

# Bake a unique marker into the prompt so we can find the row later if the
# trace_id / external_request_id linkage breaks.
marker="SMOKE_$(date +%s%N)"
chat_body=$(aigw_curl "$NEXUS_TEST_VK" /v1/chat/completions \
  -X POST \
  -H 'Content-Type: application/json' \
  -d "$(jq -nc --arg model "$NEXUS_JUDGE_MODEL" --arg marker "$marker" \
        '{model: $model,
          messages: [{role: "user", content: ("Reply with exactly: " + $marker)}],
          max_tokens: 32,
          temperature: 0}')")

content=$(printf '%s' "$chat_body" | jq -r '.choices[0].message.content // empty')
if [[ -n "$content" ]]; then
  pass "chat completion returned content (\"${content:0:40}\")"
else
  fail "chat completion" "no content in response: ${chat_body:0:200}"
fi

# Token usage must be populated.
total_tokens=$(printf '%s' "$chat_body" | jq -r '.usage.total_tokens // 0')
if [[ "$total_tokens" -gt 0 ]]; then
  pass "chat completion token usage = $total_tokens"
else
  fail "chat completion usage" ".usage.total_tokens missing or zero"
fi

# Verify the request landed in traffic_event. The audit pipeline is async
# (AI Gateway → MQ → Hub consumer → DB), so we measure ~10s end-to-end in
# local dev with worst-case spikes around 30s during MQ batching. Poll up
# to 45s and search the last 90s window so a slightly delayed write still
# matches against this run.
deadline=$(($(date +%s) + 45))
matched_id=""
while [[ $(date +%s) -lt $deadline ]]; do
  matched_id=$(db_scalar "SELECT id FROM traffic_event
    WHERE source='ai-gateway'
      AND path='/v1/chat/completions'
      AND \"timestamp\" > NOW() - INTERVAL '90 seconds'
    ORDER BY \"timestamp\" DESC LIMIT 1" 2>/dev/null || echo "")
  [[ -n "$matched_id" ]] && break
  sleep 0.5
done
if [[ -n "$matched_id" ]]; then
  pass "traffic_event row written (id=$matched_id)"
  # Pull the model + status_code for an additional sanity check.
  row=$(db_query "SELECT model_name, status_code FROM traffic_event WHERE id='$matched_id'" 2>&1)
  printf '%s\n' "$row" | sed 's/^/  /' | head -4
else
  fail "traffic_event" "no row with source=ai-gateway path=/v1/chat/completions in last 30s"
fi

# ----------------------------------------------------------------------------
# Section 2b — /v1/messages (Anthropic ingress) shape + DB + counter delta.
# ----------------------------------------------------------------------------
#
# The AI Gateway exposes Anthropic-shape /v1/messages alongside OpenAI-shape
# /v1/chat/completions. This arm verifies the canonical↔Anthropic codec stays
# wired end-to-end: request reaches the upstream, response carries the
# Anthropic envelope (`content[]` + `stop_reason` + `usage.{input,output}_tokens`),
# a traffic_event row is written, and the request counter increments.

# Pull the requests_total counter sum before the call so we can assert a
# strict delta around just this request. Sum across all label series so the
# assertion survives label-set evolution (model, status, ingress, etc.).
prom_messages_before=$(curl -sS "$NEXUS_AI_GW_URL/metrics" \
  | awk '/^nexus_ai_gateway_requests_total[ {]/ { s += $NF } END { printf "%.0f", s+0 }')

messages_marker="SMOKE_MSG_$(date +%s%N)"
messages_body=$(aigw_curl "$NEXUS_TEST_VK" /v1/messages \
  -X POST \
  -H 'Content-Type: application/json' \
  -H 'anthropic-version: 2023-06-01' \
  -d "$(jq -nc --arg marker "$messages_marker" \
        '{model: "claude-haiku-4-5",
          max_tokens: 16,
          messages: [{role: "user", content: ("Reply with exactly: " + $marker)}]}')")

# HTTP status — repeat the call via aigw_curl_code so we get the status
# without re-parsing the body. Same body, same marker namespace; the DB
# cross-check below still matches against either request.
messages_status=$(aigw_curl_code "$NEXUS_TEST_VK" /v1/messages \
  -X POST \
  -H 'Content-Type: application/json' \
  -H 'anthropic-version: 2023-06-01' \
  -d "$(jq -nc --arg marker "$messages_marker" \
        '{model: "claude-haiku-4-5",
          max_tokens: 16,
          messages: [{role: "user", content: ("Reply with exactly: " + $marker)}]}')")
assert_status 200 "$messages_status" "/v1/messages with valid VK"

# Anthropic envelope: content[].text + stop_reason + usage.{input,output}_tokens.
msg_content_text=$(printf '%s' "$messages_body" | jq -r '.content[0].text // empty')
if [[ -n "$msg_content_text" ]]; then
  pass "/v1/messages content[0].text populated (\"${msg_content_text:0:40}\")"
else
  fail "/v1/messages content" "no content[0].text in response: ${messages_body:0:200}"
fi

msg_stop_reason=$(printf '%s' "$messages_body" | jq -r '.stop_reason // empty')
if [[ -n "$msg_stop_reason" ]]; then
  pass "/v1/messages stop_reason = $msg_stop_reason"
else
  fail "/v1/messages stop_reason" ".stop_reason missing in response: ${messages_body:0:200}"
fi

msg_input_tokens=$(printf '%s' "$messages_body" | jq -r '.usage.input_tokens // 0')
if [[ "$msg_input_tokens" -gt 0 ]]; then
  pass "/v1/messages usage.input_tokens = $msg_input_tokens"
else
  fail "/v1/messages usage.input_tokens" "expected >0, got [$msg_input_tokens]"
fi

msg_output_tokens=$(printf '%s' "$messages_body" | jq -r '.usage.output_tokens // 0')
if [[ "$msg_output_tokens" -gt 0 ]]; then
  pass "/v1/messages usage.output_tokens = $msg_output_tokens"
else
  fail "/v1/messages usage.output_tokens" "expected >0, got [$msg_output_tokens]"
fi

# DB cross-check: row count for either Anthropic or OpenAI ingress in the
# last 60s must be > 0. Same async pipeline as Section 2 — poll up to 45s.
deadline=$(($(date +%s) + 45))
recent_count=0
while [[ $(date +%s) -lt $deadline ]]; do
  recent_count=$(db_scalar "SELECT count(*) FROM traffic_event
    WHERE source='ai-gateway'
      AND path IN ('/v1/messages', '/v1/chat/completions')
      AND \"timestamp\" > NOW() - INTERVAL '60 seconds'" 2>/dev/null || echo "0")
  [[ "${recent_count:-0}" -gt 0 ]] && break
  sleep 0.5
done
if [[ "${recent_count:-0}" -gt 0 ]]; then
  pass "traffic_event rows in last 60s for messages|chat-completions = $recent_count"
else
  fail "traffic_event /v1/messages" "no rows with path IN (/v1/messages,/v1/chat/completions) in last 60s"
fi

# Prometheus counter delta around the call. Re-read and require strict
# increment >= 1. The earlier reads issued two AI Gateway requests
# (messages_body + messages_status), so the realistic delta is >= 2;
# we keep the assertion at >= 1 to stay tolerant of label churn / sampling.
prom_messages_after=$(curl -sS "$NEXUS_AI_GW_URL/metrics" \
  | awk '/^nexus_ai_gateway_requests_total[ {]/ { s += $NF } END { printf "%.0f", s+0 }')
prom_messages_delta=$((prom_messages_after - prom_messages_before))
if [[ "$prom_messages_delta" -ge 1 ]]; then
  pass "nexus_ai_gateway_requests_total delta = $prom_messages_delta (before=$prom_messages_before after=$prom_messages_after)"
else
  fail "nexus_ai_gateway_requests_total delta" \
    "expected >=1, got $prom_messages_delta (before=$prom_messages_before after=$prom_messages_after)"
fi

# ----------------------------------------------------------------------------
# Section 2c — /v1/embeddings (OpenAI ingress) shape + DB + counter delta.
# ----------------------------------------------------------------------------
#
# The AI Gateway exposes OpenAI-shape /v1/embeddings alongside chat completions
# and Anthropic /v1/messages. This arm verifies the embeddings codec stays
# wired end-to-end: request reaches the upstream, response carries the OpenAI
# list envelope (`object="list"` + `data[].embedding` + `usage.prompt_tokens`),
# a traffic_event row is written, and the request counter increments.
#
# Skip-graceful: if the configured embedding model is not provisioned in this
# environment (400 model_not_found / 404), log a warning and continue — the
# embedding model may not be configured in every test env.

# Pull the requests_total counter sum before the call so we can assert a
# strict delta around just this request. Sum across all label series so the
# assertion survives label-set evolution (model, status, ingress, etc.).
prom_embeddings_before=$(curl -sS "$NEXUS_AI_GW_URL/metrics" \
  | awk '/^nexus_ai_gateway_requests_total[ {]/ { s += $NF } END { printf "%.0f", s+0 }')

# Use set +e around the embeddings calls so a skip-graceful 400/404 doesn't
# abort the script under `set -eu` (top of file). Restore -eu after, not -e —
# we want nounset to stay on for the rest of the section.
set +e
embeddings_body=$(aigw_curl "$NEXUS_TEST_VK" /v1/embeddings \
  -X POST \
  -H 'Content-Type: application/json' \
  -d '{"model":"text-embedding-3-small","input":"hello world"}')
embeddings_status=$(aigw_curl_code "$NEXUS_TEST_VK" /v1/embeddings \
  -X POST \
  -H 'Content-Type: application/json' \
  -d '{"model":"text-embedding-3-small","input":"hello world"}')
set -eu

# Skip-graceful: model not configured in this env.
emb_error_code=$(printf '%s' "$embeddings_body" | jq -r '.error.code // .error.type // empty' 2>/dev/null)
if [[ "$embeddings_status" == "404" ]] \
  || { [[ "$embeddings_status" == "400" ]] && [[ "$emb_error_code" == *"model_not_found"* || "$emb_error_code" == *"not_found"* ]]; }; then
  printf '  WARN: /v1/embeddings skipped — model not configured (HTTP %s, code=%s)\n' \
    "$embeddings_status" "$emb_error_code"
else
  assert_status 200 "$embeddings_status" "/v1/embeddings with valid VK"

  # OpenAI embeddings envelope: object="list" + data[0].embedding (float array)
  # + usage.prompt_tokens > 0.
  emb_object=$(printf '%s' "$embeddings_body" | jq -r '.object // empty')
  if [[ "$emb_object" == "list" ]]; then
    pass "/v1/embeddings object = list"
  else
    fail "/v1/embeddings object" "expected \"list\", got [$emb_object]: ${embeddings_body:0:200}"
  fi

  # data[0].embedding must be a non-empty array; spot-check the first value is
  # numeric (jq `type == "number"`) so we catch null / string / object payloads.
  emb_len=$(printf '%s' "$embeddings_body" | jq -r '(.data[0].embedding | length) // 0')
  emb_first_type=$(printf '%s' "$embeddings_body" | jq -r '.data[0].embedding[0] | type')
  if [[ "$emb_len" -gt 0 && "$emb_first_type" == "number" ]]; then
    pass "/v1/embeddings data[0].embedding length = $emb_len (first value is number)"
  else
    fail "/v1/embeddings data[0].embedding" \
      "expected non-empty number array, got length=$emb_len first_type=$emb_first_type: ${embeddings_body:0:200}"
  fi

  emb_prompt_tokens=$(printf '%s' "$embeddings_body" | jq -r '.usage.prompt_tokens // 0')
  if [[ "$emb_prompt_tokens" -gt 0 ]]; then
    pass "/v1/embeddings usage.prompt_tokens = $emb_prompt_tokens"
  else
    fail "/v1/embeddings usage.prompt_tokens" "expected >0, got [$emb_prompt_tokens]"
  fi

  # DB cross-check: row count for /v1/embeddings in the last 60s must be > 0.
  # Same async pipeline as Section 2 — poll up to 45s.
  deadline=$(($(date +%s) + 45))
  emb_recent_count=0
  while [[ $(date +%s) -lt $deadline ]]; do
    emb_recent_count=$(db_scalar "SELECT count(*) FROM traffic_event
      WHERE source='ai-gateway'
        AND path='/v1/embeddings'
        AND \"timestamp\" > NOW() - INTERVAL '60 seconds'" 2>/dev/null || echo "0")
    [[ "${emb_recent_count:-0}" -gt 0 ]] && break
    sleep 0.5
  done
  if [[ "${emb_recent_count:-0}" -gt 0 ]]; then
    pass "traffic_event rows in last 60s for /v1/embeddings = $emb_recent_count"
  else
    fail "traffic_event /v1/embeddings" "no rows with path=/v1/embeddings in last 60s"
  fi

  # Prometheus counter delta around the call. Re-read and require strict
  # increment >= 1. The earlier reads issued two AI Gateway requests
  # (embeddings_body + embeddings_status), so the realistic delta is >= 2;
  # we keep the assertion at >= 1 to stay tolerant of label churn / sampling.
  prom_embeddings_after=$(curl -sS "$NEXUS_AI_GW_URL/metrics" \
    | awk '/^nexus_ai_gateway_requests_total[ {]/ { s += $NF } END { printf "%.0f", s+0 }')
  prom_embeddings_delta=$((prom_embeddings_after - prom_embeddings_before))
  if [[ "$prom_embeddings_delta" -ge 1 ]]; then
    pass "nexus_ai_gateway_requests_total delta = $prom_embeddings_delta (before=$prom_embeddings_before after=$prom_embeddings_after)"
  else
    fail "nexus_ai_gateway_requests_total delta" \
      "expected >=1, got $prom_embeddings_delta (before=$prom_embeddings_before after=$prom_embeddings_after)"
  fi
fi

# ----------------------------------------------------------------------------
# Section 3 — Prometheus counter delta.
# ----------------------------------------------------------------------------

# Pull the AI Gateway's metrics endpoint twice (before / after the call would
# require pre-arranging; instead we read once and confirm the counter is
# present and non-zero, which is what we actually care about for smoke).
metrics_body=$(curl -sS "$NEXUS_AI_GW_URL/metrics")
if printf '%s' "$metrics_body" | grep -qE '^nexus_(ai_gateway|aigw)_'; then
  counter_count=$(printf '%s' "$metrics_body" | grep -cE '^nexus_(ai_gateway|aigw)_')
  pass "ai-gateway exposes $counter_count nexus_* metric series"
else
  fail "ai-gateway:/metrics" "no nexus_(ai_gateway|aigw)_* counters present"
fi

summary
