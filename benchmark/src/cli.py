"""
CLI entry point and top-level orchestration.

Wires together: config loading → prompt loading → warmup → consistency check
(optional) → benchmark loop → reporting. Each concern is delegated to its own
module; this file is the glue.
"""

from __future__ import annotations

import argparse
import asyncio
import logging
import sys
from datetime import datetime, timezone
from pathlib import Path

from rich.console import Console

from src.adapters.openai_compat import OpenAICompatAdapter
from src.config import BenchmarkConfig, EnvironmentInfo, TestProfile, load_config
from src.engine.runner import run_benchmark
from src.engine.warmup import run_warmup
from src.metrics.aggregator import MetricsAggregator
from src.reporting.csv_reporter import write_csv_report
from src.reporting.json_reporter import write_json_report
from src.reporting.terminal import print_consistency_table, print_summary_table
from src.validation.checker import run_consistency_check

console = Console()


def setup_logging(level: str) -> None:
    logging.basicConfig(
        level=getattr(logging, level.upper(), logging.INFO),
        format="%(asctime)s [%(levelname)-8s] %(name)s — %(message)s",
        datefmt="%Y-%m-%dT%H:%M:%S",
    )


def load_prompts(config: BenchmarkConfig) -> list[str]:
    """Load prompts from a JSONL file or fall back to the static prompt."""
    if not config.prompts_file:
        return [config.static_prompt]

    path = Path(config.prompts_file)
    if not path.exists():
        console.print(f"[red]Prompts file not found: {path}[/red]")
        sys.exit(1)

    import json

    prompts: list[str] = []
    for line in path.read_text().strip().splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
            if isinstance(obj, dict):
                prompts.append(obj.get("prompt") or obj.get("text") or str(obj))
            else:
                prompts.append(str(obj))
        except json.JSONDecodeError:
            prompts.append(line)

    if not prompts:
        console.print(f"[red]No prompts loaded from {path}[/red]")
        sys.exit(1)

    console.print(f"[dim]Loaded {len(prompts)} prompts from {path}[/dim]")
    return prompts


def make_output_dir(config: BenchmarkConfig) -> Path:
    ts = datetime.now(tz=timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    slug = config.gateway_name.lower().replace(" ", "_").replace("/", "_")
    out = Path(config.output_dir) / f"{slug}_{config.profile.value}_{ts}"
    out.mkdir(parents=True, exist_ok=True)
    return out


def _resolve_run_params(
    config: BenchmarkConfig,
) -> tuple[list[int], int]:
    """Return (concurrency_levels, total_requests) appropriate for the profile."""
    profile = config.profile

    if profile == TestProfile.SMOKE:
        return [1], min(10, config.total_requests)

    if profile == TestProfile.CONCURRENCY:
        return config.concurrency_levels, config.total_requests

    if profile == TestProfile.SOAK:
        return [config.concurrency_levels[0]], config.total_requests

    # latency, throughput, consistency
    return [config.concurrency_levels[0]], config.total_requests


async def _run(config: BenchmarkConfig, log_level: str) -> None:
    setup_logging(log_level)
    logger = logging.getLogger(__name__)

    ts = datetime.now(tz=timezone.utc).isoformat()
    env = EnvironmentInfo(gateway_name=config.gateway_name, timestamp=ts)
    prompts = load_prompts(config)
    adapter = OpenAICompatAdapter(config)
    output_dir = make_output_dir(config)

    # Header
    console.rule(
        f"[bold cyan]{config.gateway_name} — {config.profile.value.upper()}[/bold cyan]"
    )
    console.print(f"  Endpoint : [dim]{config.base_url}{config.endpoint}[/dim]")
    console.print(f"  Model    : [dim]{config.payload.model}[/dim]")
    console.print(f"  Output   : [dim]{output_dir}[/dim]")
    if config.caching.enabled:
        console.print(
            f"  [yellow]Caching ON[/yellow] — {config.caching.note or 'see config'}"
        )
    else:
        console.print("  Caching  : [dim]OFF[/dim]")
    console.print()

    # Warmup
    await run_warmup(config, adapter, prompts)

    # Consistency check for smoke and consistency profiles
    consistency_results = None
    if config.profile in (TestProfile.SMOKE, TestProfile.CONSISTENCY):
        console.print("[bold]Running consistency check...[/bold]")
        check_prompts = prompts[: min(3, len(prompts))]
        consistency_results = await run_consistency_check(
            config, adapter, check_prompts, repetitions=5
        )
        print_consistency_table(consistency_results, config.gateway_name)
        console.print()

    # Consistency-only: skip throughput measurement
    if config.profile == TestProfile.CONSISTENCY:
        _write_outputs([], config, env, output_dir, consistency_results)
        console.print(f"\n[green]Results written to: {output_dir}[/green]")
        return

    # Benchmark passes
    concurrency_levels, total = _resolve_run_params(config)
    all_summaries = []

    for concurrency in concurrency_levels:
        aggregator = MetricsAggregator(concurrency)
        console.print(
            f"[bold]Concurrency={concurrency}  Requests={total}...[/bold]"
        )

        report_every = max(1, total // 10)

        def progress(done: int, total_n: int) -> None:
            if done % report_every == 0 or done == total_n:
                console.print(f"  [dim]{done}/{total_n}[/dim]")

        wall_time = await run_benchmark(
            config=config,
            adapter=adapter,
            prompts=prompts,
            concurrency=concurrency,
            total_requests=total,
            aggregator=aggregator,
            progress_callback=progress,
        )
        summary = aggregator.summarize(wall_time)
        all_summaries.append(summary)

        logger.info(
            "concurrency=%d rps=%.2f p99=%.1fms success=%d/%d",
            concurrency,
            summary.rps,
            summary.p99_ms,
            summary.successful,
            summary.total_requests,
        )

    print_summary_table(all_summaries, config.gateway_name)
    _write_outputs(all_summaries, config, env, output_dir, consistency_results)
    console.print(f"\n[green]Results written to: {output_dir}[/green]")


def _write_outputs(
    summaries,
    config: BenchmarkConfig,
    env: EnvironmentInfo,
    output_dir: Path,
    consistency_results,
) -> None:
    write_json_report(summaries, config, env, output_dir / "results.json", consistency_results)
    if summaries:
        write_csv_report(summaries, output_dir / "results.csv")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="run_benchmark",
        description="Nexus Gateway Benchmark Harness — tests LLM gateways via chat completions",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
examples:
  python run_benchmark.py --config config/nexus_smoke.yaml
  python run_benchmark.py --config config/nexus_concurrency.yaml --log-level DEBUG
  python run_benchmark.py --config config/nexus_soak.yaml --output results/soak_run1

fair benchmarking note:
  - Run each gateway test on the same machine, one at a time.
  - Use identical model names, payload sizes, and caching settings.
  - Record environment info from the JSON output artifact for later comparison.
        """,
    )
    parser.add_argument(
        "--config", required=True, metavar="PATH",
        help="Path to YAML or JSON benchmark config file",
    )
    parser.add_argument(
        "--log-level", default="INFO",
        choices=["DEBUG", "INFO", "WARNING", "ERROR"],
        help="Logging verbosity (default: INFO)",
    )
    parser.add_argument(
        "--output", default=None, metavar="DIR",
        help="Override the output directory from config",
    )
    return parser


def main() -> None:
    parser = build_parser()
    args = parser.parse_args()

    try:
        config = load_config(args.config)
    except FileNotFoundError as exc:
        console.print(f"[red]Error: {exc}[/red]")
        sys.exit(1)
    except Exception as exc:
        console.print(f"[red]Config error: {exc}[/red]")
        sys.exit(1)

    if args.output:
        config.output_dir = args.output

    asyncio.run(_run(config, args.log_level))
