"""
CSV result reporter.

Writes one row per concurrency level. Useful for pasting into spreadsheets
or plotting with pandas/matplotlib for cross-gateway comparison charts.
"""

from __future__ import annotations

import csv
from pathlib import Path

from src.metrics.aggregator import MetricsSummary


def write_csv_report(summaries: list[MetricsSummary], output_path: Path) -> None:
    if not summaries:
        return

    output_path.parent.mkdir(parents=True, exist_ok=True)
    fieldnames = list(summaries[0].to_dict().keys())

    with open(output_path, "w", newline="") as f:
        writer = csv.DictWriter(f, fieldnames=fieldnames)
        writer.writeheader()
        for s in summaries:
            writer.writerow(s.to_dict())
