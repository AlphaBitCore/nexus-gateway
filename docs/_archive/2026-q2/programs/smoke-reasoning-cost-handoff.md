# Smoke v2: Reasoning + Cost Checks — Handoff

**Status (2026-05-16T18:20Z)**: prerequisite gateway work done + pushed. Other session is shipping reasoning-content normalizer changes across spec_{anthropic,gemini,openai}/stream.go + shared/normalize. This doc captures the smoke-side additions needed AFTER that work lands.

## What's already done (2026-05-16 session)

- Smoke refactored: `IngressSpec` + generic `_run_ingress_model_suite` (`tests/scripts/smoke-gateway.py`).
- `GWClient`: 8 endpoint methods collapsed into `_post_sync` + `_post_sse` + 4 module-level SSE parsers.
- Per-ingress nonce (user content + system tail) isolates gateway L1 cache + provider prefix cache.
- Envelope-shape assertions FAIL on wrong-ingress JSON (catches reshape bugs).
- Cache cross-ingress matrix in the report (catches "A-hit B-miss" asymmetry per user binding).
- 4 ingresses (chat / responses / messages / gemini) tested end-to-end; full 29-model run on 2026-05-16T06:08Z passed 218/28/0 → asymmetry caught → root-caused to `OpenAIChatCompletionToGenerateContentResponse` dropping `cached_tokens` → fixed in commit `19c63a8be`.
- Cross-format /v1beta SSE 400 ("stream_options requires stream enabled") fixed in `de61fc3b8`.
- All on origin/main.

## What's NOT yet checked (this handoff)

1. **Reasoning token counts** — every ingress has its own field name
2. **Reasoning content (chain-of-thought text)** — every ingress has its own envelope
3. **Per-request cost** — gateway tracks `EstimatedCostUsd` but doesn't expose it on response headers; smoke needs to read it from `/v1/usage` delta or via a new header

## Field-name reference (verified in Go code 2026-05-16)

### Reasoning tokens (usage subfield)

| Ingress | Field path | Go source |
|---|---|---|
| P3 chat-completions | `usage.completion_tokens_details.reasoning_tokens` | `shared/normalize/openai_chat.go:398` |
| P3R responses | `usage.output_tokens_details.reasoning_tokens` | `shared/normalize/openai_chat.go:400` |
| P3A messages (Anthropic) | _not exposed as separate count_ — counted into `output_tokens` | Anthropic doesn't break out reasoning tokens in API |
| P3G gemini | `usageMetadata.thoughtsTokenCount` | `shared/normalize/gemini_generate.go:346` |

### Reasoning content (text body)

| Ingress | Field path | Notes |
|---|---|---|
| P3 chat-completions | `choices[0].message.reasoning_content` | DeepSeek-R1/V4, Kimi K2 streaming style — present alongside `content` |
| P3R responses | `output[]` items with `type=="reasoning"` containing `summary[].text` or similar | OpenAI Responses-API reasoning block format |
| P3A messages | `content[]` items with `type=="thinking"`, field name `thinking` (string) | Anthropic extended-thinking; verified in `shared/normalize/anthropic_messages.go:194` |
| P3G gemini | `candidates[0].content.parts[]` items with `thought: true` + `text` | Gemini 2.5+ extended-thinking; verified in `shared/normalize/gemini_generate.go:93` |

### Per-request cost

Gateway stamps `rec.EstimatedCostUsd` (`packages/ai-gateway/internal/handler/proxy.go:2030`) but does NOT currently surface it via response header. Three options for smoke:

- **A** (preferred): add `x-nexus-aigw-cost-usd` response header in `setResponseHeaders` (`proxy.go:1820`). One-line gateway change. Smoke reads header per call → exact per-request cost.
- **B**: smoke calls `/v1/usage` before + after each model arm; delta = sum of per-call costs. No gateway change. 2 extra HTTP calls per model.
- **C**: read `traffic_event.gateway_cost_usd` directly from DB. Only works for local-mode smoke; prod runs skip DB cross-check.

Recommend A: smallest gateway change, cleanest UX, doesn't depend on DB.

## Concrete smoke additions

Extend `IngressSpec` (in `tests/scripts/smoke-gateway.py`):

```python
@dataclass
class IngressSpec:
    # ...existing fields...
    extract_reasoning_text: Callable[[dict], str]      # data → reasoning chain-of-thought, "" if absent
    extract_reasoning_tokens: Callable[[dict], int]    # data → reasoning_tokens count, 0 if absent
```

Add 4 per-ingress extractors near the spec definitions:

```python
def _chat_extract_reasoning_text(data: dict) -> str:
    choices = data.get("choices") or []
    if not choices: return ""
    return choices[0].get("message", {}).get("reasoning_content") or ""

def _chat_extract_reasoning_tokens(data: dict) -> int:
    u = data.get("usage") or {}
    return int((u.get("completion_tokens_details") or {}).get("reasoning_tokens", 0) or 0)

def _responses_extract_reasoning_text(data: dict) -> str:
    parts = []
    for item in data.get("output") or []:
        if isinstance(item, dict) and item.get("type") == "reasoning":
            # Reasoning block structure per OpenAI Responses-API
            for s in item.get("summary") or []:
                if isinstance(s, dict) and isinstance(s.get("text"), str):
                    parts.append(s["text"])
            # Some variants stash text in content[]
            for c in item.get("content") or []:
                if isinstance(c, dict) and isinstance(c.get("text"), str):
                    parts.append(c["text"])
    return "".join(parts)

def _responses_extract_reasoning_tokens(data: dict) -> int:
    u = data.get("usage") or {}
    return int((u.get("output_tokens_details") or {}).get("reasoning_tokens", 0) or 0)

def _messages_extract_reasoning_text(data: dict) -> str:
    parts = []
    for blk in data.get("content") or []:
        if isinstance(blk, dict) and blk.get("type") == "thinking":
            t = blk.get("thinking")
            if isinstance(t, str):
                parts.append(t)
    return "".join(parts)

def _messages_extract_reasoning_tokens(data: dict) -> int:
    # Anthropic doesn't expose reasoning tokens as a separate count.
    # Returning 0 is correct — smoke should not flag this as missing.
    return 0

def _gemini_extract_reasoning_text(data: dict) -> str:
    parts = []
    for cand in data.get("candidates") or []:
        content = cand.get("content") or {}
        for p in content.get("parts") or []:
            if isinstance(p, dict) and p.get("thought") is True:
                t = p.get("text")
                if isinstance(t, str):
                    parts.append(t)
    return "".join(parts)

def _gemini_extract_reasoning_tokens(data: dict) -> int:
    u = data.get("usageMetadata") or {}
    return int(u.get("thoughtsTokenCount", 0) or 0)
```

Wire the 4 specs:

```python
INGRESS_CHAT = IngressSpec(
    # ...,
    extract_reasoning_text=_chat_extract_reasoning_text,
    extract_reasoning_tokens=_chat_extract_reasoning_tokens,
)
# ...same for INGRESS_RESPONSES, INGRESS_MESSAGES, INGRESS_GEMINI
```

In `_run_ingress_model_suite`, after the non-stream PASS branch:

```python
if text:
    # ...existing usage / text log...
    # NEW: reasoning checks (only on reasoning-capable models)
    if is_reasoning(model) or uses_thinking_tokens(model):
        rt = spec.extract_reasoning_text(data)
        rtoks = spec.extract_reasoning_tokens(data)
        # Anthropic doesn't expose reasoning_tokens; tolerate 0 for messages ingress
        anthropic_native = spec.nonce_id == "msg" and spec.is_native(model)
        if not anthropic_native and rtoks == 0:
            log_warn(f"  [{model}] reasoning model but reasoning_tokens=0 ({spec.label})")
            rec(phase_tag, f"{model}/reasoning-tokens").warning("reasoning model returned 0 reasoning_tokens")
        # Reasoning content is best-effort — some models (e.g. o1) hide it
        # behind summary blocks that aren't always populated. Log info only.
        log_info(f"  [{model}] reasoning_tokens={rtoks} reasoning_text_len={len(rt)}")
```

## Cost check (after gateway exposes `x-nexus-aigw-cost-usd`)

In `_post_sync`, capture the response header alongside the body:

```python
out = {"status": r.status, "data": data, "elapsed": elapsed, "stream": False}
cost_hdr = r.getheader("x-nexus-aigw-cost-usd", "")
if cost_hdr:
    try:
        out["cost_usd"] = float(cost_hdr)
    except ValueError:
        pass
```

In the generic suite's non-stream PASS branch:

```python
cost = r.get("cost_usd", 0.0)
if cost == 0.0 and not is_free_tier_model(model):
    log_warn(f"  [{model}] cost not reported on response header")
    rec(phase_tag, f"{model}/cost").warning("cost header missing or 0")
```

For SSE (`_post_sse`), the header lands the same way since headers come before the SSE body — no extra work.

## Cross-ingress cost matrix (extending the report)

In `render_report`, mirror the cache-matrix section but tabulate cost-per-call (or total cost per model across all 4 ingresses). Surfaces ingress-specific pricing leaks (e.g. one ingress accidentally getting charged twice).

## Test order: which to add first

1. **Reasoning tokens** — pure response-shape extraction, no gateway change needed. Can ship today.
2. **Reasoning content (extract + length report)** — same, no gateway change.
3. **Cost via /v1/usage delta** — works today, but slow (2 extra HTTP calls per model). Stopgap.
4. **Cost via response header** — needs gateway PR to add `x-nexus-aigw-cost-usd` in `setResponseHeaders`. Once shipped, switch smoke from delta to header.

## Things to verify in the gateway BEFORE running new smoke

1. Other session's reasoning-content normalizer changes deploy cleanly — read their commits when they push.
2. Run the existing 3-model probe (`--all-ingress`, 3 models, stream + non-stream + cache) to confirm no regression from their changes.
3. Then add the smoke extensions above.
4. Then full 29-model `--all-ingress` re-run.

## Related memory entries

- `feedback_cache_mandatory_all_ingress` — every ingress must run cache test
- `project_e56_responses_api_done_prod` — Responses-API ingress shipped
- `project_parallel_worktree_sessions` — commit with `-- <pathspec>` ALWAYS

## Open questions for the next session

- For P3A (messages ingress) reasoning: should `reasoning_text` extraction also cover the streaming `thinking_delta` events? (Currently only non-stream is in the spec.) — see `shared/normalize/anthropic_messages.go:426` for the streaming path.
- For P3R (responses): structured outputs + reasoning effort + built-in tools matrix lives in the separate `/test-openai-responses` skill. Decide whether the new reasoning checks duplicate that or live only here.
- For cost: per-call accuracy vs per-model-arm aggregate — which is more useful for the user's debugging? (Per-call requires the gateway header.)
