"""Output writers (JSON / CSV / Markdown) and terminal summary.

All file writes are atomic: content is written to a sibling ``*.tmp`` file and
then ``os.replace``d into place, so a reader never sees a half-written file and
an interrupted run never corrupts an existing result.
"""
from __future__ import annotations

import csv
import io
import json
import os
from pathlib import Path
from typing import Optional

from .metrics import RequestRecord

# Metric -> (label, unit, is_ms) for human-facing tables.
_LATENCY_KEYS = [
    ("ttft_p50", "TTFT p50"), ("ttft_p95", "TTFT p95"), ("ttft_p99", "TTFT p99"),
    ("e2e_p50", "E2E p50"), ("e2e_p95", "E2E p95"), ("e2e_p99", "E2E p99"),
]
_RATE_KEYS = [
    ("http_failure_rate_pct", "HTTP failure %"),
    ("request_error_rate_pct", "Request error %"),
    ("stream_interruption_rate_pct", "Stream break %"),
    ("cache_hit_rate_pct", "Cache hit %"),
]


def _atomic_write_text(path: Path, text: str) -> None:
    tmp = path.with_name(path.name + ".tmp")
    with open(tmp, "w", encoding="utf-8") as f:
        f.write(text)
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)


def _fmt_secs(v: Optional[float]) -> str:
    return "—" if v is None else f"{v * 1000:.1f} ms"


def _fmt_pct(v: Optional[float]) -> str:
    return "—" if v is None else f"{v:.2f}%"


# --------------------------------------------------------------------------- #
# Single-run outputs
# --------------------------------------------------------------------------- #
def write_json(output_dir: Path, payload: dict) -> Path:
    path = output_dir / "results.json"
    _atomic_write_text(path, json.dumps(payload, indent=2, default=str))
    return path


def write_csv(output_dir: Path, records: list[RequestRecord]) -> Path:
    path = output_dir / "results.csv"
    buf = io.StringIO()
    fieldnames = list(RequestRecord.__dataclass_fields__.keys())
    writer = csv.DictWriter(buf, fieldnames=fieldnames)
    writer.writeheader()
    for r in records:
        writer.writerow(r.to_row())
    _atomic_write_text(path, buf.getvalue())
    return path


def write_summary_md(output_dir: Path, metadata: dict, aggregate: dict,
                     cache_breakdown: Optional[dict] = None) -> Path:
    path = output_dir / "summary.md"
    lines: list[str] = []
    lines.append(f"# Benchmark summary — {metadata['gateway_name']} / {metadata['scenario']}")
    lines.append("")
    lines.append(f"- **Gateway:** {metadata['gateway_name']} (v{metadata['version']}, {metadata['provider']})")
    lines.append(f"- **Model:** {metadata['model']}")
    lines.append(f"- **Base URL:** {metadata['base_url']}")
    lines.append(f"- **Scenario:** {metadata['scenario']} — cache_mode={metadata['cache_mode']}")
    lines.append(f"- **Concurrency (VUs):** {metadata['vus']}")
    lines.append(f"- **Duration:** {aggregate['duration_s']} s")
    lines.append(f"- **Timestamp:** {metadata['timestamp']}")
    lines.append(f"- **Dataset:** {metadata['dataset_id']}")
    lines.append(f"- **Host:** {metadata['machine']['hostname']} "
                 f"({metadata['machine']['cpu_count']} CPU, "
                 f"{metadata['machine']['ram_gb']} GB RAM)")
    lines.append("")
    lines.append("| Metric | Value |")
    lines.append("| --- | --- |")
    lines.append(f"| Total requests | {aggregate['total_requests']} |")
    lines.append(f"| Successful | {aggregate['successful_requests']} |")
    lines.append(f"| RPS | {aggregate['rps']} |")
    for key, label in _LATENCY_KEYS:
        lines.append(f"| {label} | {_fmt_secs(aggregate[key])} |")
    for key, label in _RATE_KEYS:
        lines.append(f"| {label} | {_fmt_pct(aggregate[key])} |")
    lines.append("")

    if cache_breakdown:
        lines.append("## Cache breakdown (w03b)")
        lines.append("")
        lines.append("| Class | Requests | Cache hit % | TTFT hit p50 | TTFT miss p50 |")
        lines.append("| --- | --- | --- | --- | --- |")
        for cls, d in cache_breakdown.items():
            lines.append(
                f"| {cls} | {d['requests']} | {_fmt_pct(d['cache_hit_rate_pct'])} "
                f"| {_fmt_secs(d['ttft_hit_p50'])} | {_fmt_secs(d['ttft_miss_p50'])} |"
            )
        lines.append("")

    _atomic_write_text(path, "\n".join(lines))
    return path


def write_run(output_dir: Path, metadata: dict, aggregate: dict,
              records: list[RequestRecord],
              cache_breakdown: Optional[dict] = None) -> dict[str, Path]:
    output_dir.mkdir(parents=True, exist_ok=True)
    payload = {
        "metadata": metadata,
        "aggregate": aggregate,
        "cache_breakdown": cache_breakdown,
        "records": [r.to_row() for r in records],
    }
    return {
        "json": write_json(output_dir, payload),
        "csv": write_csv(output_dir, records),
        "md": write_summary_md(output_dir, metadata, aggregate, cache_breakdown),
    }


def print_summary_table(metadata: dict, aggregate: dict) -> None:
    rows = [
        ("Total requests", str(aggregate["total_requests"])),
        ("Successful", str(aggregate["successful_requests"])),
        ("RPS", str(aggregate["rps"])),
    ]
    rows += [(label, _fmt_secs(aggregate[k])) for k, label in _LATENCY_KEYS]
    rows += [(label, _fmt_pct(aggregate[k])) for k, label in _RATE_KEYS]

    title = f" {metadata['gateway_name']} · {metadata['scenario']} · {metadata['vus']} VUs "
    w_label = max(len(r[0]) for r in rows)
    w_val = max(max(len(r[1]) for r in rows), len(title))
    bar = "+" + "-" * (w_label + 2) + "+" + "-" * (w_val + 2) + "+"
    print(bar)
    print(f"|{title.center(w_label + w_val + 5)}|")
    print(bar)
    for label, val in rows:
        print(f"| {label.ljust(w_label)} | {val.ljust(w_val)} |")
    print(bar)


# --------------------------------------------------------------------------- #
# Concurrency-sweep outputs (one CSV row per VU level)
# --------------------------------------------------------------------------- #
_SWEEP_FIELDS = [
    "gateway", "vus", "total_requests", "successful_requests", "rps",
    "ttft_p50", "ttft_p95", "ttft_p99", "e2e_p50", "e2e_p95", "e2e_p99",
    "http_failure_rate_pct", "request_error_rate_pct",
    "stream_interruption_rate_pct",
]


def write_sweep(output_dir: Path, metadata: dict,
                level_results: list[dict]) -> dict[str, Path]:
    """level_results: list of {'vus': int, 'aggregate': {...}}."""
    output_dir.mkdir(parents=True, exist_ok=True)

    buf = io.StringIO()
    writer = csv.DictWriter(buf, fieldnames=_SWEEP_FIELDS)
    writer.writeheader()
    for lvl in level_results:
        agg = lvl["aggregate"]
        writer.writerow({
            "gateway": metadata["gateway_name"],
            "vus": lvl["vus"],
            **{k: agg.get(k) for k in _SWEEP_FIELDS if k not in ("gateway", "vus")},
        })
    csv_path = output_dir / "results.csv"
    _atomic_write_text(csv_path, buf.getvalue())

    json_path = output_dir / "results.json"
    _atomic_write_text(json_path, json.dumps(
        {"metadata": metadata, "sweep": level_results}, indent=2, default=str))

    # Markdown
    lines = [f"# Concurrency sweep — {metadata['gateway_name']}", "",
             f"- Scenario base: w01 workload (short chat, streaming, cache disabled)",
             f"- Model: {metadata['model']}  |  Host: {metadata['machine']['hostname']}",
             "",
             "| VUs | Total | RPS | TTFT p50 | TTFT p95 | E2E p50 | E2E p95 | Err % |",
             "| --- | --- | --- | --- | --- | --- | --- | --- |"]
    for lvl in level_results:
        a = lvl["aggregate"]
        lines.append(
            f"| {lvl['vus']} | {a['total_requests']} | {a['rps']} "
            f"| {_fmt_secs(a['ttft_p50'])} | {_fmt_secs(a['ttft_p95'])} "
            f"| {_fmt_secs(a['e2e_p50'])} | {_fmt_secs(a['e2e_p95'])} "
            f"| {_fmt_pct(a['request_error_rate_pct'])} |")
    md_path = output_dir / "summary.md"
    _atomic_write_text(md_path, "\n".join(lines))

    return {"json": json_path, "csv": csv_path, "md": md_path}


def print_sweep_table(metadata: dict, level_results: list[dict]) -> None:
    header = f" Concurrency sweep · {metadata['gateway_name']} "
    print("=" * max(len(header), 60))
    print(header)
    print("=" * max(len(header), 60))
    print(f"{'VUs':>5} {'Total':>7} {'RPS':>8} {'TTFTp50':>9} {'TTFTp95':>9} {'E2Ep95':>9} {'Err%':>7}")
    for lvl in level_results:
        a = lvl["aggregate"]
        print(f"{lvl['vus']:>5} {a['total_requests']:>7} {str(a['rps']):>8} "
              f"{_fmt_secs(a['ttft_p50']):>9} {_fmt_secs(a['ttft_p95']):>9} "
              f"{_fmt_secs(a['e2e_p95']):>9} {_fmt_pct(a['request_error_rate_pct']):>7}")
