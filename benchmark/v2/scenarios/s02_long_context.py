"""S-02: Long Context — Cache Disabled."""
from __future__ import annotations
import asyncio
from engine.metrics import ScenarioMetrics
from engine.runner import run_scenario
from engine.models import GatewayFullConfig
from gateway_adapters.base import BaseGatewayAdapter
from scenarios.base_scenario import load_prompts

SCENARIO_ID = "S-02"
THRESHOLDS = {"ttft_p95_ms": 3000, "http_failure_pct": 2.0}

async def run(config: GatewayFullConfig, adapter: BaseGatewayAdapter, mode: str = "cache-disabled") -> ScenarioMetrics:
    prompts = load_prompts("long_context_v2.json")
    metrics = ScenarioMetrics(gateway_name=config.gateway.name, scenario_id=SCENARIO_ID, mode=mode)
    vus = max(1, config.benchmark.virtual_users // 2)
    await run_scenario(
        config=config, adapter=adapter, prompts=prompts,
        virtual_users=vus,
        duration_seconds=config.benchmark.test_duration_seconds,
        metrics=metrics,
        warmup_seconds=60,
    )
    return metrics
