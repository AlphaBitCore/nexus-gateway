"""
Consistency checker.

Sends the same prompt N times in sequence and records whether each attempt
succeeds. A gateway is "consistent" on a given prompt if every attempt returns
a valid, non-empty response. Inconsistency under identical inputs signals
flakiness: transient upstream errors, unstable routing, or rate-limit drift.

This is separate from the throughput benchmark — it deliberately avoids
concurrency so failures cannot be attributed to load.
"""

from __future__ import annotations

import asyncio
import logging
from dataclasses import dataclass, field

import httpx

from src.adapters.base import GatewayAdapter
from src.config import BenchmarkConfig

logger = logging.getLogger(__name__)


@dataclass
class ConsistencyResult:
    prompt: str
    attempts: int
    successes: int
    failures: int
    failure_reasons: list[str] = field(default_factory=list)

    @property
    def is_consistent(self) -> bool:
        return self.failures == 0

    def to_dict(self) -> dict:
        preview = self.prompt[:80] + "..." if len(self.prompt) > 80 else self.prompt
        return {
            "prompt": preview,
            "attempts": self.attempts,
            "successes": self.successes,
            "failures": self.failures,
            "failure_reasons": self.failure_reasons,
            "is_consistent": self.is_consistent,
        }


async def run_consistency_check(
    config: BenchmarkConfig,
    adapter: GatewayAdapter,
    prompts: list[str],
    repetitions: int = 5,
) -> list[ConsistencyResult]:
    """
    Check each prompt `repetitions` times sequentially.
    Returns one ConsistencyResult per prompt.
    """
    results: list[ConsistencyResult] = []
    async with httpx.AsyncClient(timeout=config.timeout_seconds) as client:
        for prompt in prompts:
            result = await _check_prompt(client, adapter, config, prompt, repetitions)
            results.append(result)
            if not result.is_consistent:
                logger.warning(
                    "Inconsistent responses for '%s...': %d/%d failed. Reasons: %s",
                    prompt[:40],
                    result.failures,
                    result.attempts,
                    list(set(result.failure_reasons)),
                )
    return results


async def _check_prompt(
    client: httpx.AsyncClient,
    adapter: GatewayAdapter,
    config: BenchmarkConfig,
    prompt: str,
    repetitions: int,
) -> ConsistencyResult:
    successes = 0
    failures = 0
    reasons: list[str] = []

    for _ in range(repetitions):
        try:
            kwargs = adapter.build_request(prompt, config.payload)
            response = await client.request(**kwargs)
            try:
                body = response.json()
            except Exception:
                body = {}
            valid, reason = adapter.validate_response(response.status_code, body)
            if valid:
                successes += 1
            else:
                failures += 1
                reasons.append(reason)
        except Exception as exc:
            failures += 1
            reasons.append(f"{type(exc).__name__}: {exc}")

        # Brief pause between sequential consistency probes to avoid
        # accidentally triggering per-second rate limits.
        await asyncio.sleep(0.1)

    return ConsistencyResult(
        prompt=prompt,
        attempts=repetitions,
        successes=successes,
        failures=failures,
        failure_reasons=reasons,
    )
