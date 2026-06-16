"""
Async SSE-capable request execution engine.

Uses httpx + httpx_sse for streaming. Records TTFT (first chunk received),
E2E (stream complete), and stream-broken (connection dropped before [DONE]).
Concurrency controlled via asyncio.Semaphore.
"""
from __future__ import annotations

import asyncio
import json
import logging
import time
from typing import AsyncIterator, Callable, Optional

import httpx
from httpx_sse import aconnect_sse

from engine.metrics import RequestRecord, ScenarioMetrics
from engine.models import GatewayFullConfig

logger = logging.getLogger(__name__)

ProgressCallback = Optional[Callable[[int, int], None]]

# Fair-comparison knob: when BENCH_UNIQUE_PROMPTS is set, every request gets a
# distinct prompt (base prompt + per-request nonce). This defeats the Nexus
# in-flight streaming dedupe broker and any response cache (different cache key
# per request), so each gateway makes a real upstream call per request and the
# latency numbers are comparable. Default OFF — cache scenarios (e.g. S-08)
# depend on repeated prompts to exercise cache hits.
import os as _os
import secrets as _secrets

_UNIQUE_PROMPTS = _os.getenv("BENCH_UNIQUE_PROMPTS", "").lower() in ("1", "true", "yes")
# Letter-heavy nonce. Earlier digit-only format (timestamp-pid-req_id) triggered
# the PII scanner's phone-number regex (\b\d{3}[-.\s]?\d{4}\b) on every request,
# producing blanket 403 from the pii-scanner hook (block-hard). token_urlsafe
# is base64url (mixed alphanum + -_), so no all-digit run long enough to look
# like a phone / SSN / CC.
_RUN_NONCE = _secrets.token_urlsafe(8)


async def run_scenario(
    config: GatewayFullConfig,
    adapter,               # BaseGatewayAdapter
    prompts: list[str],
    virtual_users: int,
    duration_seconds: int,
    metrics: ScenarioMetrics,
    warmup_seconds: int = 30,
    progress_cb: ProgressCallback = None,
    is_warmup: bool = False,
) -> None:
    """
    Drive concurrent VUs for `duration_seconds`. Records into `metrics`.
    Warmup phase runs first with is_warmup=True (excluded from metrics).
    """
    semaphore = asyncio.Semaphore(virtual_users)
    stop_event = asyncio.Event()
    counter = {"n": 0}

    limits = httpx.Limits(
        max_connections=virtual_users + 20,
        max_keepalive_connections=virtual_users,
    )

    async with httpx.AsyncClient(
        timeout=httpx.Timeout(config.request.timeout_seconds),
        limits=limits,
        follow_redirects=True,
    ) as client:
        # ── Warmup ──────────────────────────────────────────────────────
        if warmup_seconds > 0 and not is_warmup:
            warmup_metrics = ScenarioMetrics(
                gateway_name=metrics.gateway_name,
                scenario_id=metrics.scenario_id,
                mode=metrics.mode,
            )
            await _run_for_duration(
                client, adapter, prompts, virtual_users, warmup_seconds,
                warmup_metrics, semaphore, stop_event, counter,
                is_warmup=True, progress_cb=None,
            )
            logger.info("Warmup complete (%ds). Starting timed run.", warmup_seconds)
            # Reset semaphore and counter
            semaphore = asyncio.Semaphore(virtual_users)
            stop_event.clear()
            counter["n"] = 0

        # ── Timed run ────────────────────────────────────────────────────
        wall_start = time.perf_counter()
        await _run_for_duration(
            client, adapter, prompts, virtual_users, duration_seconds,
            metrics, semaphore, stop_event, counter,
            is_warmup=is_warmup, progress_cb=progress_cb,
        )
        metrics.wall_time_seconds = time.perf_counter() - wall_start


async def _run_for_duration(
    client: httpx.AsyncClient,
    adapter,
    prompts: list[str],
    virtual_users: int,
    duration_seconds: int,
    metrics: ScenarioMetrics,
    semaphore: asyncio.Semaphore,
    stop_event: asyncio.Event,
    counter: dict,
    is_warmup: bool,
    progress_cb: ProgressCallback,
) -> None:
    deadline = time.perf_counter() + duration_seconds

    async def worker(vu_id: int) -> None:
        prompt_idx = vu_id
        while time.perf_counter() < deadline and not stop_event.is_set():
            prompt = prompts[prompt_idx % len(prompts)]
            req_id = counter["n"]
            counter["n"] += 1
            if _UNIQUE_PROMPTS:
                # req_id is hex to avoid long decimal runs the PII scanner's
                # phone regex would match. See _RUN_NONCE comment above.
                prompt = (f"{prompt}\n\n[bench nonce {_RUN_NONCE}-{req_id:x}]")
            async with semaphore:
                record = await _execute_request(client, adapter, prompt, req_id, is_warmup)
            metrics.add(record)
            if progress_cb and not is_warmup:
                progress_cb(counter["n"], -1)
            prompt_idx += virtual_users

    tasks = [asyncio.create_task(worker(i)) for i in range(virtual_users)]
    await asyncio.gather(*tasks, return_exceptions=True)


async def _execute_request(
    client: httpx.AsyncClient,
    adapter,
    prompt: str,
    req_id: int,
    is_warmup: bool,
) -> RequestRecord:
    url, headers, body = adapter.build_request(prompt)
    stream_mode: bool = body.get("stream", False)

    ttft_ms: Optional[float] = None
    e2e_ms: float = 0.0
    status_code: Optional[int] = None
    success = False
    stream_broken = False
    http_4xx = False
    http_5xx = False
    conn_timeout = False
    stream_timeout = False
    json_error = False
    error_msg: Optional[str] = None
    cache_hit: Optional[bool] = None
    tokens_generated: Optional[int] = None

    wall_start = time.perf_counter()

    try:
        if stream_mode:
            # ── SSE streaming path ──────────────────────────────────────
            async with aconnect_sse(client, "POST", url, headers=headers, json=body) as sse:
                status_code = sse.response.status_code
                if status_code >= 500:
                    http_5xx = True
                elif status_code >= 400:
                    http_4xx = True
                    error_msg = f"HTTP {status_code}"
                else:
                    chunk_count = 0
                    done_seen = False
                    async for event in sse.aiter_sse():
                        if event.data == "[DONE]":
                            done_seen = True
                            break
                        if not event.data:
                            continue
                        try:
                            chunk = json.loads(event.data)
                        except json.JSONDecodeError:
                            json_error = True
                            continue

                        # First content chunk → TTFT
                        if ttft_ms is None:
                            choices = chunk.get("choices") or []
                            delta = ""
                            if choices:
                                delta = (choices[0].get("delta") or {}).get("content", "")
                            if delta:
                                ttft_ms = (time.perf_counter() - wall_start) * 1000.0

                        # Cache header. Nexus emits "X-Nexus-Cache: HIT|MISS|..."
                        # while LiteLLM/Bifrost use the OpenAI-style "x-cache-status"
                        # (also used by older Nexus builds). Check both so cache-hit
                        # telemetry works across gateways.
                        cache_header = (
                            sse.response.headers.get("X-Nexus-Cache")
                            or sse.response.headers.get("x-cache-status", "")
                        )
                        if cache_header.upper() in ("HIT", "SEMANTIC_HIT"):
                            cache_hit = True
                        elif cache_header:
                            cache_hit = False

                        chunk_count += 1
                        # OpenAI/Nexus stream chunks carry "usage": null on every
                        # delta; guard against None (key present, value null).
                        usage = chunk.get("usage")
                        if usage:
                            tokens_generated = usage.get("completion_tokens")

                    stream_broken = not done_seen
                    success = not stream_broken and not http_4xx and not http_5xx
        else:
            # ── Non-streaming path ──────────────────────────────────────
            resp = await client.post(url, headers=headers, json=body)
            status_code = resp.status_code
            ttft_ms = (time.perf_counter() - wall_start) * 1000.0
            if status_code >= 500:
                http_5xx = True
            elif status_code >= 400:
                http_4xx = True
                error_msg = f"HTTP {status_code}"
            else:
                try:
                    data = resp.json()
                    # Same dual-header read as the streaming path above.
                    cache_header = (
                        resp.headers.get("X-Nexus-Cache")
                        or resp.headers.get("x-cache-status", "")
                    )
                    if cache_header.upper() in ("HIT", "SEMANTIC_HIT"):
                        cache_hit = True
                    elif cache_header:
                        cache_hit = False
                    tokens_generated = (data.get("usage", {}) or {}).get("completion_tokens")
                    success = True
                except json.JSONDecodeError:
                    json_error = True

    except httpx.ConnectTimeout:
        conn_timeout = True
        error_msg = "ConnectTimeout"
    except httpx.ReadTimeout:
        stream_timeout = True
        error_msg = "ReadTimeout"
    except httpx.TimeoutException as exc:
        conn_timeout = True
        error_msg = f"TimeoutException: {exc}"
    except httpx.RequestError as exc:
        error_msg = f"{type(exc).__name__}: {exc}"
        logger.debug("Request error on req %d: %s", req_id, exc)
    except Exception as exc:
        error_msg = f"{type(exc).__name__}: {exc}"
        logger.warning("Unexpected error on req %d: %s", req_id, exc)

    e2e_ms = (time.perf_counter() - wall_start) * 1000.0

    return RequestRecord(
        request_id=req_id,
        prompt_index=req_id,
        ttft_ms=ttft_ms,
        e2e_ms=e2e_ms,
        status_code=status_code,
        success=success,
        stream_broken=stream_broken,
        http_4xx=http_4xx,
        http_5xx=http_5xx,
        connection_timeout=conn_timeout,
        stream_timeout=stream_timeout,
        json_parse_error=json_error,
        error_msg=error_msg,
        cache_hit=cache_hit,
        tokens_generated=tokens_generated,
        is_warmup=is_warmup,
    )
