"""Rich terminal output for benchmark results."""
from __future__ import annotations
from typing import Optional
from rich.console import Console
from rich.table import Table
from rich.panel import Panel
from rich import box
from engine.metrics import ScenarioMetrics

console = Console()

METHODOLOGY_DISCLAIMER = """
[bold yellow]Benchmark Methodology Note[/bold yellow]
This is Nexus Gateway Benchmark v2. It supersedes Minimal Benchmark v1 (2026-05-19).

v1 was invalidated:
  1. Nexus had caching ENABLED (44.16% hit rate). Bifrost/LiteLLM had 0% hit rate.
     Nexus W-01 TTFT p95 of 4ms was a cache hit — not a model call.
  2. Load generator ran from a MacBook Pro, not within AWS — uncontrolled network jitter.
  3. No warmup phase documented or controlled.

v2 corrects all issues. Cache DISABLED on all gateways for comparison tests.
Nexus caching benchmarked separately (S-08) as a product feature.
"""


def print_disclaimer() -> None:
    console.print(Panel(METHODOLOGY_DISCLAIMER, title="v2 Methodology", border_style="yellow"))


def print_scenario_result(metrics: ScenarioMetrics, thresholds: dict | None = None) -> None:
    thresholds = thresholds or {}

    def _fmt(v: Optional[float], unit: str = "ms") -> str:
        if v is None:
            return "[dim]N/A[/dim]"
        return f"{v:.1f} {unit}"

    def _pct(v: Optional[float]) -> str:
        return f"{v:.2f}%" if v is not None else "[dim]N/A[/dim]"

    def _threshold_color(v: Optional[float], threshold: Optional[float], lower_is_better: bool = True) -> str:
        if v is None or threshold is None:
            return "white"
        ok = v <= threshold if lower_is_better else v >= threshold
        return "green" if ok else "red"

    table = Table(
        title=f"[bold]{metrics.scenario_id} — {metrics.gateway_name}[/bold] [{metrics.mode}]",
        box=box.ROUNDED, show_lines=True,
    )
    table.add_column("Metric", style="cyan", min_width=28)
    table.add_column("Value", justify="right", min_width=16)
    table.add_column("Threshold", justify="right", min_width=14, style="dim")
    table.add_column("Pass/Fail", justify="center", min_width=10)

    def _row(label: str, value: str, threshold_key: str | None = None, raw_val: Optional[float] = None):
        thresh_str = ""
        pf_str = ""
        if threshold_key and threshold_key in thresholds:
            t = thresholds[threshold_key]
            thresh_str = f"≤ {t}"
            if raw_val is not None:
                pf_str = "[green]✅[/green]" if raw_val <= t else "[red]❌[/red]"
        table.add_row(label, value, thresh_str, pf_str)

    _row("Total Requests",       str(metrics.total_requests))
    _row("Successful",           str(metrics.successful))
    _row("Failed",               str(metrics.failed))
    _row("HTTP 4xx",             str(metrics.http_4xx))
    _row("HTTP 5xx",             str(metrics.http_5xx))
    _row("Stream Broken",        str(metrics.stream_broken))
    _row("Connection Timeouts",  str(metrics.connection_timeouts))
    table.add_section()
    _row("TTFT avg",    _fmt(metrics.ttft_avg))
    _row("TTFT p50",    _fmt(metrics.ttft_p50))
    _row("TTFT p95",    _fmt(metrics.ttft_p95),  "ttft_p95_ms", metrics.ttft_p95)
    _row("TTFT p99",    _fmt(metrics.ttft_p99))
    _row("TTFT stddev", _fmt(metrics.ttft_stddev))
    table.add_section()
    _row("E2E avg",     _fmt(metrics.e2e_avg))
    _row("E2E p50",     _fmt(metrics.e2e_p50))
    _row("E2E p95",     _fmt(metrics.e2e_p95))
    _row("E2E p99",     _fmt(metrics.e2e_p99))
    table.add_section()
    _row("Throughput (RPS)",     f"{metrics.rps:.3f}")
    _row("HTTP Failure Rate",    _pct(metrics.http_failure_rate), "http_failure_pct", metrics.http_failure_rate)
    _row("Stream Broken Rate",   _pct(metrics.stream_broken_rate), "stream_broken_pct", metrics.stream_broken_rate)
    table.add_section()
    _row("Cache Hit Rate",        _pct(metrics.cache_hit_rate))
    _row("Cache Hit TTFT p95",    _fmt(metrics.cache_hit_ttft_p95))
    _row("Cache Miss TTFT p95",   _fmt(metrics.cache_miss_ttft_p95))
    _row("TTFT Gain p95",         _fmt(metrics.ttft_gain_p95))

    console.print(table)


def print_comparison_table(results: list[ScenarioMetrics], scenario_id: str) -> None:
    """Print side-by-side comparison of same scenario across gateways."""
    table = Table(
        title=f"[bold]{scenario_id} — Gateway Comparison[/bold]",
        box=box.DOUBLE_EDGE, show_lines=True,
    )
    table.add_column("Metric", style="cyan", min_width=22)
    for m in results:
        table.add_column(m.gateway_name, justify="right", min_width=14)

    def _row(label: str, vals: list[str]) -> None:
        table.add_row(label, *vals)

    def _fmt_all(fn) -> list[str]:
        return [f"{fn(m):.1f}" if fn(m) is not None else "N/A" for m in results]

    _row("TTFT p50 (ms)",         _fmt_all(lambda m: m.ttft_p50))
    _row("TTFT p95 (ms)",         _fmt_all(lambda m: m.ttft_p95))
    _row("TTFT p99 (ms)",         _fmt_all(lambda m: m.ttft_p99))
    _row("E2E p95 (ms)",          _fmt_all(lambda m: m.e2e_p95))
    _row("Throughput (RPS)",      [f"{m.rps:.2f}" for m in results])
    _row("HTTP Failure %",        [f"{m.http_failure_rate:.2f}%" for m in results])
    _row("Stream Broken %",       [f"{m.stream_broken_rate:.2f}%" for m in results])
    _row("Total Requests",        [str(m.total_requests) for m in results])

    console.print(table)
