"""
Async execution engine.

Drives the benchmark by firing `total_requests` requests at a given concurrency
level using asyncio + httpx. Each request goes through retry logic before being
recorded. Results flow directly into a MetricsAggregator.

Design note: a semaphore limits in-flight requests to `concurrency`. All tasks
are created up-front and handed to asyncio.gather so the event loop can schedule
them freely — this avoids the artificial serialization of a producer/consumer
queue while still bounding concurrency tightly.
"""

from __future__ import annotations

import asyncio
import logging
import time
from typing import Callable, Optional

import httpx

from src.adapters.base import GatewayAdapter
from src.config import BenchmarkConfig
from src.metrics.aggregator import MetricsAggregator, RequestResult

logger = logging.getLogger(__name__)

ProgressCallback = Optional[Callable[[int, int], None]]


async def run_benchmark(
    config: BenchmarkConfig,
    adapter: GatewayAdapter,
    prompts: list[str],
    concurrency: int,
    total_requests: int,
    aggregator: MetricsAggregator,
    progress_callback: ProgressCallback = None,
) -> float:
    """
    Fire `total_requests` requests at `concurrency` concurrency.
    Returns wall-clock elapsed seconds (used to compute RPS).
    """
    semaphore = asyncio.Semaphore(concurrency)

    async with httpx.AsyncClient(
        timeout=config.timeout_seconds,
        follow_redirects=True,
        # A shared connection pool across all requests is intentional: it
        # models real gateway clients that reuse keep-alive connections.
        limits=httpx.Limits(max_connections=concurrency + 10, max_keepalive_connections=concurrency),
    ) as client:
        tasks = [
            _send_with_retry(
                client=client,
                adapter=adapter,
                config=config,
                prompt=prompts[i % len(prompts)],
                prompt_index=i,
                semaphore=semaphore,
            )
            for i in range(total_requests)
        ]

        wall_start = time.perf_counter()
        results = await asyncio.gather(*tasks)
        wall_time = time.perf_counter() - wall_start

    for idx, result in enumerate(results, start=1):
        aggregator.add(result)
        if progress_callback:
            progress_callback(idx, total_requests)
        # Warn (not abort) when failure rate trends high; let the run finish.
        if idx % 25 == 0:
            rate = aggregator.failure_rate()
            if rate > config.failure_threshold_pct:
                logger.warning(
                    "Failure rate %.1f%% exceeds threshold %.1f%% after %d requests",
                    rate,
                    config.failure_threshold_pct,
                    idx,
                )

    return wall_time


async def _send_with_retry(
    client: httpx.AsyncClient,
    adapter: GatewayAdapter,
    config: BenchmarkConfig,
    prompt: str,
    prompt_index: int,
    semaphore: asyncio.Semaphore,
) -> RequestResult:
    retry_cfg = config.retry
    max_attempts = retry_cfg.max_attempts if retry_cfg.enabled else 1
    last_result: Optional[RequestResult] = None

    for attempt in range(max_attempts):
        async with semaphore:
            last_result = await _send_once(client, adapter, config, prompt, prompt_index)
        if last_result.success:
            return last_result
        if attempt < max_attempts - 1:
            await asyncio.sleep(retry_cfg.backoff_seconds * (attempt + 1))

    return last_result  # type: ignore[return-value]


async def _send_once(
    client: httpx.AsyncClient,
    adapter: GatewayAdapter,
    config: BenchmarkConfig,
    prompt: str,
    prompt_index: int,
) -> RequestResult:
    kwargs = adapter.build_request(prompt, config.payload)
    start = time.perf_counter()

    status_code: Optional[int] = None
    exception_msg: Optional[str] = None
    timed_out = False
    validation_ok = False
    validation_reason = "not_attempted"
    content_length = 0

    try:
        response = await client.request(**kwargs)
        latency_ms = (time.perf_counter() - start) * 1000.0
        status_code = response.status_code

        try:
            body = response.json()
        except Exception:
            body = {}

        validation_ok, validation_reason = adapter.validate_response(status_code, body)
        if validation_ok:
            content_length = len(adapter.extract_content(body))
        success = validation_ok

    except httpx.TimeoutException:
        latency_ms = (time.perf_counter() - start) * 1000.0
        timed_out = True
        success = False
        exception_msg = "TimeoutException"
        validation_reason = "timeout"

    except httpx.RequestError as exc:
        latency_ms = (time.perf_counter() - start) * 1000.0
        success = False
        exception_msg = type(exc).__name__
        validation_reason = "request_error"
        logger.debug("Request error on index %d: %s", prompt_index, exc)

    except Exception as exc:
        latency_ms = (time.perf_counter() - start) * 1000.0
        success = False
        exception_msg = f"{type(exc).__name__}: {exc}"
        validation_reason = "unexpected_error"
        logger.exception("Unexpected error on request %d", prompt_index)

    return RequestResult(
        prompt_index=prompt_index,
        latency_ms=latency_ms,
        status_code=status_code,
        success=success,
        timed_out=timed_out,
        exception=exception_msg,
        validation_ok=validation_ok,
        validation_reason=validation_reason,
        content_length=content_length,
    )
