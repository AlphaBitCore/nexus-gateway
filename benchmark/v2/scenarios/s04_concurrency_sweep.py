"""S-04: Concurrency Sweep — Cache Disabled."""
from __future__ import annotations
import asyncio
import csv
import time
from pathlib import Path
from engine.metrics import ScenarioMetrics
from engine.runner import run_scenario
from engine.models import GatewayFullConfig
from gateway_adapters.base import BaseGatewayAdapter
from scenarios.base_scenario import load_prompts

SCENARIO_ID = "S-04"
VU_LEVELS = [1, 5, 10, 20, 50, 100]
LEVEL_DURATION = 120  # 2 minutes per level

async def run(config: GatewayFullConfig, adapter: BaseGatewayAdapter,
              mode: str = "cache-disabled", output_dir: str = "./results") -> list[ScenarioMetrics]:
    prompts = load_prompts("short_chat_v2.json")
    all_metrics: list[ScenarioMetrics] = []
    for vu in VU_LEVELS:
        m = ScenarioMetrics(gateway_name=config.gateway.name, scenario_id=f"{SCENARIO_ID}-VU{vu}", mode=mode)
        await run_scenario(
            config=config, adapter=adapter, prompts=prompts,
            virtual_users=vu, duration_seconds=LEVEL_DURATION,
            metrics=m, warmup_seconds=15,
        )
        all_metrics.append(m)
        print(f"  VU={vu}: TTFT p95={m.ttft_p95} ms, RPS={m.rps:.2f}, Errors={m.http_failure_rate:.1f}%")

    # Write CSV for charting
    Path(output_dir).mkdir(parents=True, exist_ok=True)
    csv_path = Path(output_dir) / f"s04_concurrency_sweep_{config.gateway.name}.csv"
    with open(csv_path, "w", newline="") as f:
        writer = csv.DictWriter(f, fieldnames=["vu", "ttft_p95_ms", "rps", "error_rate_pct", "stream_broken_pct"])
        writer.writeheader()
        for i, m in enumerate(all_metrics):
            writer.writerow({
                "vu": VU_LEVELS[i],
                "ttft_p95_ms": round(m.ttft_p95 or 0, 2),
                "rps": round(m.rps, 3),
                "error_rate_pct": round(m.http_failure_rate, 2),
                "stream_broken_pct": round(m.stream_broken_rate, 2),
            })
    print(f"  Concurrency sweep CSV: {csv_path}")
    return all_metrics
