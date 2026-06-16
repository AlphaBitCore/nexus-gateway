"""S-11: Provider Failover Behavior — Nexus Only.
Simulates provider outage by temporarily routing to an unreachable URL.
Measures time-to-detect, fallback latency, and error propagation.
"""
from __future__ import annotations
import asyncio
import time
from engine.metrics import ScenarioMetrics
from engine.runner import run_scenario
from engine.models import GatewayFullConfig
from gateway_adapters.nexus import NexusAdapter
from scenarios.base_scenario import load_prompts
from rich.console import Console

SCENARIO_ID = "S-11"
console = Console()

async def run(config: GatewayFullConfig, adapter: BaseGatewayAdapter, mode: str = "cache-disabled") -> dict:
    prompts = load_prompts("short_chat_v2.json")

    # Phase 1: Baseline — normal operation
    baseline = ScenarioMetrics(gateway_name=config.gateway.name, scenario_id=f"{SCENARIO_ID}-baseline", mode=mode)
    await run_scenario(config=config, adapter=adapter, prompts=prompts,
                       virtual_users=5, duration_seconds=60, metrics=baseline, warmup_seconds=10)
    console.print(f"  Baseline TTFT p95: {baseline.ttft_p95} ms, RPS: {baseline.rps:.2f}")

    # Phase 2: Failover — point adapter to unreachable endpoint
    dead_adapter = NexusAdapter(
        base_url="http://127.0.0.1:19999",  # intentionally unreachable
        api_key=config.gateway.api_key,
        model="gpt-4o", stream=config.request.stream,
    )
    failover = ScenarioMetrics(gateway_name=f"{config.gateway.name}-failover", scenario_id=f"{SCENARIO_ID}-failover", mode=mode)
    start = time.perf_counter()
    await run_scenario(config=config, adapter=dead_adapter, prompts=prompts,
                       virtual_users=5, duration_seconds=30, metrics=failover, warmup_seconds=0)
    time_to_fail = time.perf_counter() - start
    console.print(f"  Failover error rate: {failover.http_failure_rate:.1f}%, detection time: {time_to_fail:.1f}s")

    return {
        "baseline": baseline.to_dict(),
        "failover": failover.to_dict(),
        "time_to_detect_seconds": round(time_to_fail, 2),
    }
