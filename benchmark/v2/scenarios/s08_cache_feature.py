"""S-08: Cache Feature Test — Nexus Only.
NOT a head-to-head comparison. Tests Nexus semantic caching as a feature.
"""
from __future__ import annotations
import asyncio
from engine.metrics import ScenarioMetrics
from engine.runner import run_scenario
from engine.models import GatewayFullConfig
from gateway_adapters.base import BaseGatewayAdapter
from scenarios.base_scenario import load_prompts

SCENARIO_ID = "S-08"

async def run_exact_match(config: GatewayFullConfig, adapter: BaseGatewayAdapter) -> ScenarioMetrics:
    prompts = load_prompts("cache_exact_v2.json")
    metrics = ScenarioMetrics(gateway_name=config.gateway.name, scenario_id=f"{SCENARIO_ID}-exact", mode="cache-enabled")
    await run_scenario(config=config, adapter=adapter, prompts=prompts,
                       virtual_users=10, duration_seconds=120, metrics=metrics, warmup_seconds=10)
    return metrics

async def run_prefix_match(config: GatewayFullConfig, adapter: BaseGatewayAdapter) -> ScenarioMetrics:
    prompts = load_prompts("cache_prefix_v2.json")
    metrics = ScenarioMetrics(gateway_name=config.gateway.name, scenario_id=f"{SCENARIO_ID}-prefix", mode="cache-enabled")
    await run_scenario(config=config, adapter=adapter, prompts=prompts,
                       virtual_users=10, duration_seconds=120, metrics=metrics, warmup_seconds=10)
    return metrics

async def run_mixed_traffic(config: GatewayFullConfig, adapter: BaseGatewayAdapter) -> ScenarioMetrics:
    """40% repeated prompts, 60% novel — simulates real-world cache benefit."""
    exact = load_prompts("cache_exact_v2.json")
    novel = load_prompts("short_chat_v2.json")
    import random
    mixed: list[str] = []
    for _ in range(50):
        if random.random() < 0.4:
            mixed.append(random.choice(exact))
        else:
            mixed.append(random.choice(novel))
    metrics = ScenarioMetrics(gateway_name=config.gateway.name, scenario_id=f"{SCENARIO_ID}-mixed", mode="cache-enabled")
    await run_scenario(config=config, adapter=adapter, prompts=mixed,
                       virtual_users=10, duration_seconds=120, metrics=metrics, warmup_seconds=10)
    return metrics

async def run(config: GatewayFullConfig, adapter: BaseGatewayAdapter, mode: str = "cache-enabled") -> list[ScenarioMetrics]:
    results = []
    results.append(await run_exact_match(config, adapter))
    results.append(await run_prefix_match(config, adapter))
    results.append(await run_mixed_traffic(config, adapter))
    return results
