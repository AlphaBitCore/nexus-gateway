"""
Rich terminal reporter.

Prints a formatted table to stdout after each benchmark pass.
Colors: green = good, red = bad, yellow = caution.
"""

from __future__ import annotations

from rich import box
from rich.console import Console
from rich.table import Table

from src.metrics.aggregator import MetricsSummary
from src.validation.checker import ConsistencyResult

console = Console()


def print_summary_table(summaries: list[MetricsSummary], gateway_name: str) -> None:
    table = Table(
        title=f"[bold cyan]Benchmark Results — {gateway_name}[/bold cyan]",
        box=box.ROUNDED,
        show_lines=True,
    )

    table.add_column("Concurrency", style="cyan", justify="right")
    table.add_column("Total", justify="right")
    table.add_column("Success", style="green", justify="right")
    table.add_column("Failed", style="red", justify="right")
    table.add_column("Timeout", style="yellow", justify="right")
    table.add_column("Avg ms", justify="right")
    table.add_column("p50 ms", justify="right")
    table.add_column("p95 ms", justify="right")
    table.add_column("p99 ms", justify="right")
    table.add_column("RPS", style="magenta", justify="right")
    table.add_column("Wall s", justify="right")

    for s in summaries:
        fail_style = "red" if s.failed > 0 else "green"
        table.add_row(
            str(s.concurrency),
            str(s.total_requests),
            str(s.successful),
            f"[{fail_style}]{s.failed}[/{fail_style}]",
            str(s.timed_out),
            f"{s.avg_latency_ms:.1f}",
            f"{s.p50_ms:.1f}",
            f"{s.p95_ms:.1f}",
            f"{s.p99_ms:.1f}",
            f"{s.rps:.2f}",
            f"{s.wall_time_seconds:.1f}",
        )

    console.print(table)


def print_consistency_table(
    results: list[ConsistencyResult], gateway_name: str
) -> None:
    table = Table(
        title=f"[bold cyan]Consistency Check — {gateway_name}[/bold cyan]",
        box=box.ROUNDED,
        show_lines=True,
    )
    table.add_column("Prompt (truncated)", style="white")
    table.add_column("Attempts", justify="right")
    table.add_column("Successes", style="green", justify="right")
    table.add_column("Failures", style="red", justify="right")
    table.add_column("Consistent?", justify="center")

    for r in results:
        label = r.prompt[:55] + "..." if len(r.prompt) > 55 else r.prompt
        consistent_str = (
            "[green]YES[/green]" if r.is_consistent else "[red]NO[/red]"
        )
        table.add_row(
            label,
            str(r.attempts),
            str(r.successes),
            str(r.failures),
            consistent_str,
        )

    console.print(table)
