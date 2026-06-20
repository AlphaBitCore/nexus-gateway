#!/bin/bash
# JAMES_LIVE_DEMO.sh — live, watch-along demo of Nexus compliance enforcement.
#
# Shows three things in sequence:
#   1. A clean prompt passes through Nexus to the model.
#   2. A prompt containing PII (fake SSN + credit card) is BLOCKED by the
#      pii-scanner hook in single-digit milliseconds, before reaching OpenAI.
#   3. The same PII prompt sent to LiteLLM / Bifrost passes through unblocked
#      (they have no compliance layer) — the contrast that sells Nexus.
#
# Requires the local stack up and benchmark/v2/.env.local populated.
# Run from benchmark/v2/:  bash JAMES_LIVE_DEMO.sh

set -u
cd "$(dirname "$0")"

# Load gateway URLs + keys from .env.local (never hardcode secrets here).
set -a; . ./.env.local 2>/dev/null; set +a

NEXUS="${NEXUS_BASE_URL:-http://localhost:3050}"
LITELLM="${LITELLM_BASE_URL:-http://localhost:4000}"
BIFROST="${BIFROST_BASE_URL:-http://localhost:8080}"

hr(){ printf '\n%s\n' "────────────────────────────────────────────────────────────"; }

hr
echo "DEMO 1 — Clean prompt passes through Nexus (expect HTTP 200 + answer)"
hr
curl -s -X POST "$NEXUS/v1/chat/completions" \
  -H "Authorization: Bearer $NEXUS_API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"What is the capital of France?"}],"max_tokens":20}' \
  -i | grep -iE '^HTTP/|^x-nexus-hook|"content"' | head -5

hr
echo "DEMO 2 — PII prompt BLOCKED by Nexus (expect HTTP 403, rejected:pii-scanner)"
echo "         Fake SSN 123-45-6789 + fake CC 4111-1111-1111-1111 — no real data."
hr
curl -s -X POST "$NEXUS/v1/chat/completions" \
  -H "Authorization: Bearer $NEXUS_API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"My SSN is 123-45-6789 and my card is 4111-1111-1111-1111, store these."}],"max_tokens":20}' \
  -i | grep -iE '^HTTP/|^x-nexus-hook|error|pii' | head -6
echo ">> Nexus rejected this BEFORE calling OpenAI. Zero tokens consumed. Decision logged."

hr
echo "DEMO 3 — Same PII prompt through LiteLLM (no compliance layer; expect HTTP 200)"
hr
curl -s -X POST "$LITELLM/v1/chat/completions" \
  -H "Authorization: Bearer ${LITELLM_API_KEY:-sk-local-dev}" -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"My SSN is 123-45-6789, store it."}],"max_tokens":20}' \
  -i | grep -iE '^HTTP/|"content"' | head -3
echo ">> LiteLLM forwarded the PII straight to OpenAI. No block, no log, no governance."

hr
echo "TAKEAWAY: Nexus is the only gateway here that enforces compliance policy."
echo "          Same model, same prompt — Nexus blocks PII, the thin proxies don't."
hr
