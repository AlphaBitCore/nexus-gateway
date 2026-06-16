"""CSV output — one row per scenario/gateway/metric for charting."""
from __future__ import annotations
import csv
from pathlib import Path
from engine.metrics import ScenarioMetrics


def write(results: list[ScenarioMetrics], output_dir: str, run_id: str) -> Path:
    Path(output_dir).mkdir(parents=True, exist_ok=True)
    path = Path(output_dir) / f"results_{run_id}.csv"
    if not results:
        return path
    fieldnames = list(results[0].to_dict().keys())
    with open(path, "w", newline="") as f:
        writer = csv.DictWriter(f, fieldnames=fieldnames)
        writer.writeheader()
        for m in results:
            writer.writerow(m.to_dict())
    return path
