"""Async request execution against an OpenAI-compatible gateway.

Handles both streaming (SSE) and non-streaming requests, measuring:

* TTFT  — time from request send to first ``data:`` chunk (streaming) or to the
          full response (non-streaming).
* E2E   — time from request send to response fully consumed.
* stream_broken — stream started but ended before ``data: [DONE]``.
* cache_hit / cache_detected — parsed from response headers if present.
"""
from __future__ import annotations

import json
import time
from typing import Optional

import httpx

from .config import GatewayConfig
from .metrics import RequestRecord

# Headers various gateways use to advertise a cache result. Keys are matched
# case-insensitively (httpx.Headers is case-insensitive).
_CACHE_HEADERS = (
    "x-nexus-cache",
    "x-nexus-cache-status",
    "x-cache",
    "x-cache-status",
    "cache-status",
    "x-cache-hit",
    "x-litellm-cache-hit",
    "x-llm-cache",
    "x-bifrost-cache",
)


def _parse_cache_header(headers: httpx.Headers) -> tuple[bool, bool]:
    """Return (cache_detected, cache_hit) from response headers."""
    for name in _CACHE_HEADERS:
        if name in headers:
            val = headers[name].strip().lower()
            if not val:
                continue
            if "hit" in val or val in ("true", "1", "yes"):
                return True, True
            if "miss" in val or val in ("false", "0", "no"):
                return True, False
            # Header present but unrecognised value: count as observed, not hit.
            return True, False
    return False, False


def build_payload(cfg: GatewayConfig, messages: list[dict], *,
                  stream: bool, max_tokens: int) -> dict:
    payload = {
        "model": cfg.model,
        "messages": messages,
        "max_tokens": max_tokens,
        "stream": stream,
    }
    if stream:
        # Ask for usage in the final streamed chunk where supported (OpenAI,
        # LiteLLM). Gateways that ignore it simply omit usage.
        payload["stream_options"] = {"include_usage": True}
    return payload


async def send_request(
    client: httpx.AsyncClient,
    cfg: GatewayConfig,
    *,
    messages: list[dict],
    stream: bool,
    max_tokens: int,
    gateway: str,
    scenario: str,
    vu: int,
    iteration: int,
    prompt_id: str,
    cache_class: str = "",
    extra_headers: Optional[dict] = None,
) -> RequestRecord:
    rec = RequestRecord(
        gateway=gateway, scenario=scenario, vu=vu, iteration=iteration,
        prompt_id=prompt_id, cache_class=cache_class,
    )
    headers = {"Content-Type": "application/json"}
    if cfg.api_key:
        headers["Authorization"] = f"Bearer {cfg.api_key}"
    if extra_headers:
        headers.update(extra_headers)

    payload = build_payload(cfg, messages, stream=stream, max_tokens=max_tokens)
    url = cfg.chat_completions_url

    t0 = time.perf_counter()
    try:
        if stream:
            await _do_stream(client, url, headers, payload, rec, t0)
        else:
            await _do_unary(client, url, headers, payload, rec, t0)
    except (httpx.ConnectError, httpx.ConnectTimeout) as e:
        rec.request_error = True
        rec.error_message = f"connect_error: {e!r}"
    except httpx.ReadTimeout as e:
        rec.request_error = True
        rec.error_message = f"read_timeout: {e!r}"
    except httpx.HTTPError as e:
        rec.request_error = True
        rec.error_message = f"http_error: {e!r}"
    except Exception as e:  # never let one bad request crash the run
        rec.request_error = True
        rec.error_message = f"unexpected: {e!r}"
    return rec


async def _do_stream(client, url, headers, payload, rec: RequestRecord, t0: float):
    got_done = False
    got_any_data = False
    content_chunks = 0
    usage_tokens: Optional[int] = None

    async with client.stream("POST", url, headers=headers, json=payload) as resp:
        rec.status_code = resp.status_code
        det, hit = _parse_cache_header(resp.headers)
        rec.cache_detected, rec.cache_hit = det, hit

        if resp.status_code != 200:
            # Drain body for the error message; not a stream break.
            body = (await resp.aread()).decode("utf-8", "replace")
            rec.error_message = body[:300]
            rec.e2e = time.perf_counter() - t0
            return

        async for raw in resp.aiter_lines():
            line = raw.rstrip("\r")
            if not line.startswith("data:"):
                continue
            data = line[len("data:"):].strip()
            if data == "[DONE]":
                got_done = True
                break
            if not got_any_data:
                rec.ttft = time.perf_counter() - t0
                got_any_data = True
            # Parse for content/usage; tolerate non-JSON keepalive lines.
            try:
                obj = json.loads(data)
            except json.JSONDecodeError:
                continue
            if obj.get("usage"):
                usage_tokens = obj["usage"].get("completion_tokens", usage_tokens)
            for choice in obj.get("choices", []):
                delta = choice.get("delta") or {}
                if delta.get("content"):
                    content_chunks += 1

    rec.e2e = time.perf_counter() - t0
    rec.completion_tokens = usage_tokens if usage_tokens is not None else (
        content_chunks or None
    )
    # Stream started (we got data) but never saw the clean [DONE] marker.
    if got_any_data and not got_done:
        rec.stream_broken = True
    # Got a 200 with zero data and no DONE → also a broken/empty stream.
    if not got_any_data and not got_done:
        rec.stream_broken = True


async def _do_unary(client, url, headers, payload, rec: RequestRecord, t0: float):
    resp = await client.post(url, headers=headers, json=payload)
    rec.status_code = resp.status_code
    det, hit = _parse_cache_header(resp.headers)
    rec.cache_detected, rec.cache_hit = det, hit
    elapsed = time.perf_counter() - t0
    # Non-streaming: first token and full response arrive together.
    rec.ttft = elapsed
    rec.e2e = elapsed
    if resp.status_code != 200:
        rec.error_message = resp.text[:300]
        return
    try:
        obj = resp.json()
        usage = obj.get("usage") or {}
        rec.completion_tokens = usage.get("completion_tokens")
    except Exception:
        # 200 but unparseable body: treat as a request error for schema purposes.
        rec.request_error = True
        rec.error_message = "non-JSON 200 response"
