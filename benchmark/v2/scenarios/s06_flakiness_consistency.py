"""S-06: Consistency / Flakiness Test."""
from __future__ import annotations
import asyncio
from engine.metrics import ScenarioMetrics
from engine.runner import run_scenario
from engine.models import GatewayFullConfig
from gateway_adapters.base import BaseGatewayAdapter

SCENARIO_ID = "S-06"
REPEAT_COUNT = 100
TEST_PROMPT = "Explain what an API gateway does in exactly two sentences."

async def run(config: GatewayFullConfig, adapter: BaseGatewayAdapter, mode: str = "cache-disabled") -> ScenarioMetrics:
    prompts = [TEST_PROMPT] * REPEAT_COUNT
    metrics = ScenarioMetrics(gateway_name=config.gateway.name, scenario_id=SCENARIO_ID, mode=mode)
    # Single VU, sequential — isolates flakiness from load
    await run_scenario(
        config=config, adapter=adapter, prompts=prompts,
        virtual_users=1, duration_seconds=999999,  # exits when prompts exhausted
        metrics=metrics, warmup_seconds=0,
    )
    return metrics
