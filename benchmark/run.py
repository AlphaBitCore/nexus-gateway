#!/usr/bin/env python3
"""Benchmark harness entry point.

Compare OpenAI-compatible gateways (Nexus, Bifrost, LiteLLM) on TTFT, latency,
error rates, stream integrity, and cache behaviour.

Examples:
    python benchmark/run.py --gateway nexus --scenario smoke
    python benchmark/run.py --gateway all   --scenario w01 --duration 180
    python benchmark/run.py --gateway nexus --scenario w03b
    python benchmark/run.py --gateway litellm --scenario concurrency-sweep
    python benchmark/run.py --gateway all --scenario w01 --dry-run
"""
from __future__ import annotations

import argparse
import asyncio
import os
import sys
from datetime import datetime, timezone
from pathlib import Path

# Make the sibling ``harness`` package importable when invoked as a script
# from any working directory (e.g. ``python benchmark/run.py``).
sys.path.insert(0, str(Path(__file__).resolve().parent))

from harness import config as cfgmod          # noqa: E402
from harness import datasets                   # noqa: E402
from harness import metrics                     # noqa: E402
from harness import reporting                   # noqa: E402
from harness.meta import machine_info           # noqa: E402
from harness.scenarios import (                 # noqa: E402
    SCENARIOS, resolve_scenario, run_scenario, Scenario,
)

GATEWAY_CHOICES = ["nexus", "bifrost", "litellm", "all"]


def _now_iso() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _gateways_for(arg: str) -> list[str]:
    return ["nexus", "bifrost", "litellm"] if arg == "all" else [arg]


def _build_metadata(cfg: cfgmod.GatewayConfig, scenario: Scenario,
                    vus: int, warmup: int, timestamp: str) -> dict:
    return {
        "gateway_name": cfg.gateway_name,
        "version": cfg.version,
        "base_url": cfg.base_url,
        "model": cfg.model,
        "provider": cfg.provider,
        "timestamp": timestamp,
        "scenario": scenario.name,
        "scenario_description": scenario.description,
        "cache_mode": scenario.cache_mode,
        "vus": vus,
        "duration_requested_s": scenario.duration,
        "num_requests": scenario.num_requests,
        "dataset_id": scenario.dataset,
        "warmup": warmup,
        "stream": scenario.stream,
        "max_tokens": scenario.max_tokens,
        "machine": machine_info(),
        "config": cfg.to_public_dict(),
    }


def _out_dir(base: Path, gateway: str, scenario: str) -> Path:
    return base / gateway / scenario


# --------------------------------------------------------------------------- #
# Dry run
# --------------------------------------------------------------------------- #
def do_dry_run(gateways: list[str], scenario_name: str, vus, duration, warmup):
    print(f"DRY RUN — scenario '{scenario_name}' (no requests will be made)\n")
    scenario = resolve_scenario(scenario_name, vus=vus, duration=duration)
    print(f"Scenario: {scenario.name} — {scenario.description}")
    print(f"  stream={scenario.stream} cache_mode={scenario.cache_mode} "
          f"vus={scenario.vus} duration={scenario.duration} "
          f"num_requests={scenario.num_requests} warmup={warmup}")
    if scenario.sweep_levels:
        print(f"  sweep levels: {scenario.sweep_levels}")
    print(f"  dataset: {datasets.dataset_summary(scenario.dataset)}\n")

    for gw in gateways:
        try:
            cfg = cfgmod.load_config(gw, require_key=False)
        except Exception as e:
            print(f"  [{gw}] CONFIG ERROR: {e}")
            continue
        if scenario.nexus_only and gw != "nexus":
            print(f"  [{gw}] would be SKIPPED (scenario {scenario.name} is Nexus-only)")
            continue
        print(f"  [{gw}] -> {cfg.chat_completions_url}  model={cfg.model} "
              f"key={'set' if cfg.api_key else 'MISSING(' + cfg.api_key_env + ')'}")
    print("\nDry run complete.")
    return 0


# --------------------------------------------------------------------------- #
# Real runs
# --------------------------------------------------------------------------- #
def run_single(gw: str, scenario: Scenario, base_out: Path, warmup: int,
               timestamp: str) -> dict | None:
    """Run one duration/count scenario against one gateway; write outputs."""
    cfg = cfgmod.load_config(gw, require_key=False)
    vus = scenario.vus
    metadata = _build_metadata(cfg, scenario, vus, warmup, timestamp)

    print(f"\n▶ {gw} · {scenario.name} · {vus} VUs "
          f"({'count=' + str(scenario.num_requests) if scenario.num_requests else 'duration=' + str(scenario.duration) + 's'})")
    if not cfg.api_key:
        print(f"  ⚠ {cfg.api_key_env} not set — requests will likely 401/fail "
              f"(still exercising the HTTP path).")

    records, elapsed = asyncio.run(run_scenario(cfg, scenario, vus=vus, warmup=warmup))
    aggregate = metrics.aggregate(records, elapsed)

    cache_breakdown = None
    if scenario.name == "w03b":
        cache_breakdown = metrics.aggregate_by_cache_class(records)

    out_dir = _out_dir(base_out, gw, scenario.name)
    paths = reporting.write_run(out_dir, metadata, aggregate, records, cache_breakdown)
    reporting.print_summary_table(metadata, aggregate)
    if cache_breakdown:
        print("  cache breakdown:")
        for cls, d in cache_breakdown.items():
            print(f"    {cls:7s} hit%={d['cache_hit_rate_pct']} "
                  f"ttft_hit_p50={d['ttft_hit_p50']} ttft_miss_p50={d['ttft_miss_p50']}")
    print(f"  outputs: {paths['json']}  |  {paths['csv']}  |  {paths['md']}")
    return {"aggregate": aggregate, "records": records, "metadata": metadata}


def run_sweep(gw: str, scenario: Scenario, base_out: Path, warmup: int,
              timestamp: str) -> None:
    cfg = cfgmod.load_config(gw, require_key=False)
    levels = scenario.sweep_levels or [1, 5, 10, 20, 50, 100]
    print(f"\n▶ {gw} · concurrency-sweep · levels={levels} "
          f"(duration={scenario.duration}s each)")
    if not cfg.api_key:
        print(f"  ⚠ {cfg.api_key_env} not set — requests will likely 401/fail.")

    level_results = []
    for vus in levels:
        per = Scenario(**{f.name: getattr(scenario, f.name)
                          for f in scenario.__dataclass_fields__.values()})
        per.vus = vus
        per.sweep_levels = None
        records, elapsed = asyncio.run(run_scenario(cfg, per, vus=vus, warmup=warmup))
        agg = metrics.aggregate(records, elapsed)
        level_results.append({"vus": vus, "aggregate": agg})
        print(f"  level {vus:>3}: total={agg['total_requests']} rps={agg['rps']} "
              f"ttft_p95={agg['ttft_p95']} err%={agg['request_error_rate_pct']}")

    metadata = _build_metadata(cfg, scenario, vus=0, warmup=warmup, timestamp=timestamp)
    out_dir = _out_dir(base_out, gw, scenario.name)
    paths = reporting.write_sweep(out_dir, metadata, level_results)
    reporting.print_sweep_table(metadata, level_results)
    print(f"  outputs: {paths['json']}  |  {paths['csv']}  |  {paths['md']}")


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description="Gateway benchmark harness")
    p.add_argument("--gateway", required=True, choices=GATEWAY_CHOICES)
    p.add_argument("--scenario", required=True, choices=list(SCENARIOS.keys()))
    p.add_argument("--vus", type=int, default=None,
                   help="Override the scenario's virtual-user count.")
    p.add_argument("--duration", type=int, default=None,
                   help="Override the scenario's duration (seconds).")
    p.add_argument("--output-dir", type=str, default=None,
                   help="Base output directory (default: benchmark/results/<scenario>-<ts>).")
    p.add_argument("--warmup", type=int, default=3,
                   help="Warmup requests excluded from metrics (default 3).")
    p.add_argument("--dry-run", action="store_true",
                   help="Print config + dataset info without making requests.")
    args = p.parse_args(argv)

    timestamp = _now_iso()
    gateways = _gateways_for(args.gateway)

    if args.dry_run:
        return do_dry_run(gateways, args.scenario, args.vus, args.duration, args.warmup)

    base_out = Path(args.output_dir) if args.output_dir else (
        Path(__file__).resolve().parent / "results"
        / f"{args.scenario}-{timestamp.replace(':', '').replace('-', '')}"
    )

    scenario = resolve_scenario(args.scenario, vus=args.vus, duration=args.duration)
    is_smoke = scenario.name == "smoke"
    smoke_results: dict[str, bool] = {}

    for gw in gateways:
        if scenario.nexus_only and gw != "nexus":
            print(f"\n▶ {gw}: SKIPPED — scenario '{scenario.name}' is Nexus-only.")
            continue
        try:
            if scenario.sweep_levels:
                run_sweep(gw, scenario, base_out, args.warmup, timestamp)
            else:
                res = run_single(gw, scenario, base_out, args.warmup, timestamp)
                if is_smoke and res is not None:
                    agg = res["aggregate"]
                    passed = (
                        agg["total_requests"] > 0
                        and agg["successful_requests"] == agg["total_requests"]
                        and agg["stream_interruption_rate_pct"] == 0.0
                    )
                    smoke_results[gw] = passed
        except FileNotFoundError as e:
            print(f"\n▶ {gw}: CONFIG ERROR — {e}")
            if is_smoke:
                smoke_results[gw] = False

    if is_smoke:
        print("\n=== SMOKE RESULTS ===")
        for gw in gateways:
            if scenario.nexus_only and gw != "nexus":
                continue
            status = smoke_results.get(gw)
            label = "PASS" if status else "FAIL"
            print(f"  {gw:8s} {label}")
        if not all(smoke_results.get(g, False) for g in smoke_results):
            return 1

    print(f"\nAll outputs under: {base_out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
