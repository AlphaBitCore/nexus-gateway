"""
Warmup phase.

Sends a configurable number of requests before recording metrics.
Results are discarded. The goal is to prime connection pools, JIT caches,
and any gateway-side startup behaviour so the first measured request is not
a cold-start outlier.
"""

from __future__ import annotations

import asyncio
import logging

import httpx

from src.adapters.base import GatewayAdapter
from src.config import BenchmarkConfig

logger = logging.getLogger(__name__)


async def run_warmup(
    config: BenchmarkConfig,
    adapter: GatewayAdapter,
    prompts: list[str],
) -> None:
    n = config.warmup_requests
    if n <= 0:
        return

    logger.info("Warmup: sending %d requests (results discarded)...", n)
    async with httpx.AsyncClient(timeout=config.timeout_seconds) as client:
        tasks = [
            _warmup_one(client, adapter, prompts[i % len(prompts)], config)
            for i in range(n)
        ]
        outcomes = await asyncio.gather(*tasks, return_exceptions=True)

    successes = sum(1 for o in outcomes if o is True)
    logger.info("Warmup complete: %d/%d succeeded", successes, n)
    if successes == 0:
        logger.warning(
            "All warmup requests failed — gateway may be unreachable or misconfigured"
        )


async def _warmup_one(
    client: httpx.AsyncClient,
    adapter: GatewayAdapter,
    prompt: str,
    config: BenchmarkConfig,
) -> bool:
    try:
        kwargs = adapter.build_request(prompt, config.payload)
        response = await client.request(**kwargs)
        return response.status_code == 200
    except Exception as exc:
        logger.debug("Warmup request failed: %s", exc)
        return False
