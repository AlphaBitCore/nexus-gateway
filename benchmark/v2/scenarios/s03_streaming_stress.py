"""S-03: Streaming Stress — Cache Disabled."""
from __future__ import annotations
import asyncio
from engine.metrics import ScenarioMetrics
from engine.runner import run_scenario
from engine.models import GatewayFullConfig
from gateway_adapters.base import BaseGatewayAdapter
from scenarios.base_scenario import load_prompts

SCENARIO_ID = "S-03"
THRESHOLDS = {"stream_broken_pct": 1.0, "http_failure_pct": 1.0}

async def run(config: GatewayFullConfig, adapter: BaseGatewayAdapter, mode: str = "cache-disabled") -> ScenarioMetrics:
    prompts = load_prompts("streaming_v2.json")
    metrics = ScenarioMetrics(gateway_name=config.gateway.name, scenario_id=SCENARIO_ID, mode=mode)
    vus = min(30, config.benchmark.virtual_users + 10)
    await run_scenario(
        config=config, adapter=adapter, prompts=prompts,
        virtual_users=vus,
        duration_seconds=config.benchmark.test_duration_seconds,
        metrics=metrics,
        warmup_seconds=30,
    )
    return metrics
