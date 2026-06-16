"""S-07: Gateway Overhead Isolation.
Uses gpt-4o-mini with max_tokens=1 to minimize model time.
Computes gateway-only overhead by subtracting direct API baseline.
"""
from __future__ import annotations
import asyncio
from engine.metrics import ScenarioMetrics
from engine.runner import run_scenario
from engine.models import GatewayFullConfig
from gateway_adapters.base import BaseGatewayAdapter
from scenarios.base_scenario import load_prompts

SCENARIO_ID = "S-07"
OVERHEAD_MAX_TOKENS = 1   # Minimize model inference time

async def run(config: GatewayFullConfig, adapter: BaseGatewayAdapter, mode: str = "cache-disabled") -> ScenarioMetrics:
    prompts = load_prompts("short_chat_v2.json")
    # Override max_tokens to 1 for overhead isolation
    adapter.max_tokens = OVERHEAD_MAX_TOKENS
    metrics = ScenarioMetrics(gateway_name=config.gateway.name, scenario_id=SCENARIO_ID, mode=mode)
    await run_scenario(
        config=config, adapter=adapter, prompts=prompts,
        virtual_users=5, duration_seconds=120,
        metrics=metrics, warmup_seconds=15,
    )
    return metrics
