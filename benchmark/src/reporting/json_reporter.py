"""
JSON result reporter.

Writes a structured JSON artifact per run. The envelope includes environment
metadata, config summary, and per-concurrency-level metrics so that results
from different gateway runs can be compared by loading two JSON files.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Optional

from src.config import BenchmarkConfig, EnvironmentInfo
from src.metrics.aggregator import MetricsSummary
from src.validation.checker import ConsistencyResult


def write_json_report(
    summaries: list[MetricsSummary],
    config: BenchmarkConfig,
    env: EnvironmentInfo,
    output_path: Path,
    consistency_results: Optional[list[ConsistencyResult]] = None,
) -> None:
    report: dict = {
        "environment": env.to_dict(),
        "config": {
            "gateway_name": config.gateway_name,
            "base_url": config.base_url,
            "endpoint": config.endpoint,
            "profile": config.profile.value,
            "model": config.payload.model,
            "total_requests_per_level": config.total_requests,
            "warmup_requests": config.warmup_requests,
            "timeout_seconds": config.timeout_seconds,
            "retry_enabled": config.retry.enabled,
            "retry_max_attempts": config.retry.max_attempts,
            "caching_enabled": config.caching.enabled,
            "caching_note": config.caching.note,
            # Fingerprint lets you verify two runs used equivalent configs.
            "config_fingerprint": config.fingerprint(),
        },
        "results": [s.to_dict() for s in summaries],
    }

    if consistency_results is not None:
        report["consistency"] = [r.to_dict() for r in consistency_results]

    output_path.parent.mkdir(parents=True, exist_ok=True)
    with open(output_path, "w") as f:
        json.dump(report, f, indent=2)
