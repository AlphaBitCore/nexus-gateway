"""S-09: Compliance / PII Enforcement — Nexus Only."""
from __future__ import annotations
import asyncio
import json
from pathlib import Path
from engine.metrics import ScenarioMetrics, RequestRecord
from engine.runner import _execute_request
from engine.models import GatewayFullConfig
from gateway_adapters.base import BaseGatewayAdapter
import httpx

SCENARIO_ID = "S-09"
DATASETS = Path(__file__).parent.parent / "datasets"

async def run(config: GatewayFullConfig, adapter: BaseGatewayAdapter, mode: str = "cache-disabled") -> dict:
    data = json.loads((DATASETS / "compliance_pii_v2.json").read_text())
    clean_prompts = data["clean_prompts"]
    pii_prompts = data["pii_prompts"]

    clean_metrics = ScenarioMetrics(gateway_name=config.gateway.name, scenario_id=f"{SCENARIO_ID}-clean", mode=mode)
    pii_metrics   = ScenarioMetrics(gateway_name=config.gateway.name, scenario_id=f"{SCENARIO_ID}-pii",   mode=mode)

    limits = httpx.Limits(max_connections=5, max_keepalive_connections=3)
    async with httpx.AsyncClient(timeout=httpx.Timeout(config.request.timeout_seconds), limits=limits) as client:
        for i, p in enumerate(clean_prompts):
            r = await _execute_request(client, adapter, p, i, is_warmup=False)
            clean_metrics.add(r)
        for i, p in enumerate(pii_prompts):
            r = await _execute_request(client, adapter, p, i + len(clean_prompts), is_warmup=False)
            # PII prompts BLOCKED = http_4xx (403/422) = "success" for compliance
            pii_metrics.add(r)

    pii_blocked = pii_metrics.http_4xx
    pii_total   = pii_metrics.total_requests
    block_rate  = (pii_blocked / pii_total * 100) if pii_total else 0
    clean_blocked = clean_metrics.http_4xx
    false_positive_rate = (clean_blocked / len(clean_prompts) * 100) if clean_prompts else 0

    return {
        "clean_metrics": clean_metrics.to_dict(),
        "pii_metrics": pii_metrics.to_dict(),
        "compliance_summary": {
            "pii_prompts_tested": pii_total,
            "pii_blocked": pii_blocked,
            "block_rate_pct": round(block_rate, 2),
            "clean_prompts_tested": len(clean_prompts),
            "false_positives": clean_blocked,
            "false_positive_rate_pct": round(false_positive_rate, 2),
            "compliance_overhead_ms": round(
                (clean_metrics.ttft_avg or 0) - (pii_metrics.ttft_avg or 0), 2
            ),
        },
    }
