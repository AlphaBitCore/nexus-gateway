"""S-05: Soak / Stability Test — Cache Disabled (30 min)."""
from __future__ import annotations
import asyncio
import time
from engine.metrics import ScenarioMetrics
from engine.runner import run_scenario
from engine.models import GatewayFullConfig
from gateway_adapters.base import BaseGatewayAdapter
from scenarios.base_scenario import load_prompts
from rich.console import Console

SCENARIO_ID = "S-05"
SAMPLE_INTERVAL = 60   # seconds
SOAK_DURATION = 1800   # 30 minutes
DEGRADATION_THRESHOLD = 0.20  # 20% increase in p95 triggers warning

console = Console()

async def run(config: GatewayFullConfig, adapter: BaseGatewayAdapter, mode: str = "cache-disabled") -> ScenarioMetrics:
    prompts = load_prompts("short_chat_v2.json")
    metrics = ScenarioMetrics(gateway_name=config.gateway.name, scenario_id=SCENARIO_ID, mode=mode)
    snapshots: list[dict] = []

    # Run in 60s windows, collect p95 per window
    baseline_p95: float | None = None
    elapsed = 0
    while elapsed < SOAK_DURATION:
        window = ScenarioMetrics(gateway_name=config.gateway.name, scenario_id=SCENARIO_ID, mode=mode)
        await run_scenario(
            config=config, adapter=adapter, prompts=prompts,
            virtual_users=config.benchmark.virtual_users,
            duration_seconds=SAMPLE_INTERVAL,
            metrics=window, warmup_seconds=0,
        )
        # Merge window into overall
        metrics._ttft_samples.extend(window._ttft_samples)
        metrics._e2e_samples.extend(window._e2e_samples)
        metrics.total_requests += window.total_requests
        metrics.successful += window.successful
        metrics.failed += window.failed
        metrics.stream_broken += window.stream_broken

        p95 = window.ttft_p95
        elapsed += SAMPLE_INTERVAL
        snapshots.append({"elapsed_s": elapsed, "ttft_p95_ms": p95})

        if baseline_p95 is None and p95 is not None:
            baseline_p95 = p95

        if baseline_p95 and p95 and p95 > baseline_p95 * (1 + DEGRADATION_THRESHOLD):
            console.print(f"[yellow]⚠ SOAK WARNING at {elapsed}s: p95 {p95:.0f}ms > baseline {baseline_p95:.0f}ms (+{DEGRADATION_THRESHOLD*100:.0f}%)[/yellow]")

        console.print(f"  [{elapsed}s] TTFT p95={p95} ms, RPS={window.rps:.2f}")

    metrics.wall_time_seconds = SOAK_DURATION
    return metrics
