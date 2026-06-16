"""S-01: Short Chat — Cache Disabled (Fair Comparison)."""
from __future__ import annotations
import asyncio
from engine.metrics import ScenarioMetrics
from engine.runner import run_scenario
from engine.models import GatewayFullConfig
from gateway_adapters.base import BaseGatewayAdapter
from scenarios.base_scenario import load_prompts

SCENARIO_ID = "S-01"
THRESHOLDS = {"ttft_p95_ms": 1500, "http_failure_pct": 1.0, "stream_broken_pct": 0.5}

async def run(config: GatewayFullConfig, adapter: BaseGatewayAdapter, mode: str = "cache-disabled") -> ScenarioMetrics:
    prompts = load_prompts("short_chat_v2.json")
    metrics = ScenarioMetrics(gateway_name=config.gateway.name, scenario_id=SCENARIO_ID, mode=mode)
    await run_scenario(
        config=config, adapter=adapter, prompts=prompts,
        virtual_users=config.benchmark.virtual_users,
        duration_seconds=config.benchmark.test_duration_seconds,
        metrics=metrics,
        warmup_seconds=config.benchmark.warmup_duration_seconds,
    )
    return metrics
