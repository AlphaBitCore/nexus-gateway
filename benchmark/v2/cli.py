#!/usr/bin/env python3
"""
Nexus Gateway Benchmark v2 — CLI entry point.

Usage examples:
  python cli.py run --scenario s01 --gateway nexus --mode cache-disabled
  python cli.py run-suite --mode cache-disabled --output ./results/run-001/
  python cli.py run --scenario s08 --gateway nexus --mode cache-enabled
  python cli.py run --scenario s09 --gateway nexus
  python cli.py validate-config --mode cache-disabled
  python cli.py report --results-dir ./results/run-001/ --format markdown
"""
from __future__ import annotations

import asyncio
import importlib
import sys
from pathlib import Path
from typing import Optional

import typer
from rich.console import Console

# Add v2 root to path so relative imports work when run as a script
sys.path.insert(0, str(Path(__file__).parent))

from engine.models import GatewayFullConfig
from gateway_adapters.nexus import NexusAdapter
from gateway_adapters.litellm import LiteLLMAdapter
from gateway_adapters.bifrost import BifrostAdapter
from engine.metrics import ScenarioMetrics
from reporting import environment_capture, json_report, csv_report, markdown_report, terminal

app = typer.Typer(help="Nexus Gateway Benchmark v2", add_completion=False)
console = Console()

CONFIG_DIR = Path(__file__).parent / "config"
GATEWAY_MAP = {
    "nexus":   ("nexus.yaml",   NexusAdapter),
    "litellm": ("litellm.yaml", LiteLLMAdapter),
    "bifrost": ("bifrost.yaml", BifrostAdapter),
}

SCENARIO_MAP = {
    "s01": "scenarios.s01_short_chat",
    "s02": "scenarios.s02_long_context",
    "s03": "scenarios.s03_streaming_stress",
    "s04": "scenarios.s04_concurrency_sweep",
    "s05": "scenarios.s05_soak_test",
    "s06": "scenarios.s06_flakiness_consistency",
    "s07": "scenarios.s07_overhead_isolation",
    "s08": "scenarios.s08_cache_feature",
    "s09": "scenarios.s09_compliance_pii",
    "s10": "scenarios.s10_config_parity",
    "s11": "scenarios.s11_provider_failover",
}


# Stable credential id for the seeded "openai-prod" credential in local dev.
# Override via NEXUS_OPENAI_CREDENTIAL_ID env on AWS where the id differs.
_NEXUS_OPENAI_CRED_ID = "abff2f77-5506-4d73-99a3-6b60ed756bac"


def _reset_nexus_circuit(config: GatewayFullConfig, retries: int = 3, probe: bool = True) -> bool:
    """
    Reset the Nexus openai-prod credential circuit breaker. Returns True if the
    post-reset probe call succeeds within `retries` attempts. Falls back to
    silent no-op when admin URL/key aren't configured (CI / restricted envs).
    """
    import os, time as _time, httpx
    admin_url = config.admin.admin_base_url or os.getenv("NEXUS_ADMIN_URL", "")
    admin_key = config.admin.admin_api_key or os.getenv("NEXUS_ADMIN_API_KEY", "")
    cred_id = os.getenv("NEXUS_OPENAI_CREDENTIAL_ID", _NEXUS_OPENAI_CRED_ID)
    if not admin_url:
        return False  # nothing to do — silent no-op
    base = config.gateway.base_url
    vk = config.gateway.api_key or ""

    for attempt in range(1, retries + 1):
        try:
            r = httpx.post(
                f"{admin_url}/api/admin/credentials/{cred_id}/circuit-reset",
                headers={"Authorization": f"Bearer {admin_key}"} if admin_key else {},
                timeout=10.0,
            )
            if r.status_code not in (200, 201, 204):
                # 401/403 means admin auth misconfig — fall through and probe anyway
                # 404 means cred id wrong — fatal-ish, but probe will reveal
                console.print(f"[yellow]circuit-reset attempt {attempt} → HTTP {r.status_code}[/yellow]")
        except Exception as e:
            console.print(f"[yellow]circuit-reset attempt {attempt} error: {e}[/yellow]")

        if not probe:
            return True

        _time.sleep(2.0)
        try:
            pr = httpx.post(
                f"{base}/v1/chat/completions",
                headers={"Authorization": f"Bearer {vk}", "Content-Type": "application/json"},
                json={"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "preflight"}], "max_tokens": 3, "stream": False},
                timeout=20.0,
            )
            if pr.status_code == 200:
                return True
            console.print(f"[yellow]probe after reset attempt {attempt}: HTTP {pr.status_code}[/yellow]")
        except Exception as e:
            console.print(f"[yellow]probe after reset attempt {attempt} error: {e}[/yellow]")

    return False


def _load_gateway(name: str, model: str = "gpt-4o-mini") -> tuple[GatewayFullConfig, object]:
    if name not in GATEWAY_MAP:
        console.print(f"[red]Unknown gateway: {name}. Choose: {list(GATEWAY_MAP)}[/red]")
        raise typer.Exit(1)
    yaml_file, adapter_cls = GATEWAY_MAP[name]
    config = GatewayFullConfig.from_yaml(CONFIG_DIR / yaml_file)
    adapter = adapter_cls.from_config(config, model=model)
    return config, adapter


@app.command("run")
def run_single(
    scenario: str = typer.Option(..., help="Scenario ID: s01-s11"),
    gateway: str = typer.Option(..., help="Gateway: nexus | litellm | bifrost"),
    mode: str = typer.Option("cache-disabled", help="cache-disabled | cache-enabled"),
    model: str = typer.Option("gpt-4o-mini", help="Model name"),
    output: str = typer.Option("./results", help="Output directory"),
) -> None:
    """Run a single scenario against one gateway."""
    terminal.print_disclaimer()
    config, adapter = _load_gateway(gateway, model)

    if mode == "cache-enabled":
        config.features.caching_enabled = True
    else:
        config.features.caching_enabled = False

    # Env overrides for ad-hoc fair-comparison runs (low concurrency, short
    # duration) without editing the per-gateway YAML. Unset => YAML defaults.
    import os
    if os.getenv("BENCH_VUS"):
        config.benchmark.virtual_users = int(os.environ["BENCH_VUS"])
    if os.getenv("BENCH_DURATION"):
        config.benchmark.test_duration_seconds = int(os.environ["BENCH_DURATION"])
    if os.getenv("BENCH_WARMUP"):
        config.benchmark.warmup_duration_seconds = int(os.environ["BENCH_WARMUP"])

    if scenario not in SCENARIO_MAP:
        console.print(f"[red]Unknown scenario: {scenario}[/red]")
        raise typer.Exit(1)

    mod = importlib.import_module(SCENARIO_MAP[scenario])

    # Auto-reset Nexus credential circuit breaker before each nexus run so a
    # prior failure (drained OpenAI quota, PII-rejected nonce) doesn't cause
    # the next run to fast-fail. Best-effort — failure here is logged not fatal.
    if gateway == "nexus":
        try:
            _reset_nexus_circuit(config, retries=3, probe=True)
        except Exception as e:
            console.print(f"[yellow]circuit reset best-effort failed: {e} (continuing)[/yellow]")

    console.print(f"\n[bold cyan]Running {scenario.upper()} on {gateway} [{mode}]...[/bold cyan]\n")

    env = environment_capture.capture(gateway, str(CONFIG_DIR / GATEWAY_MAP[gateway][0]))
    run_id = env["run_id"][:8]

    result = asyncio.run(mod.run(config, adapter, mode=mode))

    if isinstance(result, list):
        results = result if isinstance(result[0], ScenarioMetrics) else []
    elif isinstance(result, ScenarioMetrics):
        results = [result]
    else:
        console.print(result)
        return

    for m in results:
        terminal.print_scenario_result(m, thresholds=getattr(mod, "THRESHOLDS", {}))

    json_report.write(results, env, output, run_id)
    csv_report.write(results, output, run_id)
    console.print(f"\n[green]Results written to {output}/results_{run_id}.{{json,csv}}[/green]")


@app.command("run-suite")
def run_suite(
    mode: str = typer.Option("cache-disabled", help="cache-disabled | cache-enabled"),
    gateways: str = typer.Option("nexus,litellm,bifrost", help="Comma-separated gateway list"),
    model: str = typer.Option("gpt-4o-mini", help="Model name"),
    output: str = typer.Option("./results", help="Output directory"),
    scenarios: str = typer.Option("s01,s02,s03,s04,s06", help="Comma-separated scenario list"),
) -> None:
    """Run full comparison suite across all gateways."""
    terminal.print_disclaimer()

    gateway_list = [g.strip() for g in gateways.split(",")]
    scenario_list = [s.strip() for s in scenarios.split(",")]

    # Pre-flight config parity check
    configs = [_load_gateway(g, model)[0] for g in gateway_list]
    from scenarios.s10_config_parity import run as validate_parity
    validate_parity(configs, required_caching=(mode == "cache-enabled"))

    all_results: list[ScenarioMetrics] = []
    results_by_scenario: dict[str, list[ScenarioMetrics]] = {}

    import uuid
    run_id = str(uuid.uuid4())[:8]
    env = environment_capture.capture("suite")

    for scenario_id in scenario_list:
        if scenario_id not in SCENARIO_MAP:
            console.print(f"[yellow]Skipping unknown scenario: {scenario_id}[/yellow]")
            continue
        mod = importlib.import_module(SCENARIO_MAP[scenario_id])
        scenario_results: list[ScenarioMetrics] = []

        for gw_name in gateway_list:
            config, adapter = _load_gateway(gw_name, model)
            config.features.caching_enabled = (mode == "cache-enabled")
            console.print(f"\n[bold cyan]{scenario_id.upper()} → {gw_name}[/bold cyan]")
            result = asyncio.run(mod.run(config, adapter, mode=mode))
            if isinstance(result, ScenarioMetrics):
                scenario_results.append(result)
                all_results.append(result)
            elif isinstance(result, list):
                for r in result:
                    if isinstance(r, ScenarioMetrics):
                        scenario_results.append(r)
                        all_results.append(r)

        results_by_scenario[scenario_id.upper()] = scenario_results
        if len(scenario_results) > 1:
            terminal.print_comparison_table(scenario_results, scenario_id.upper())

    # Write all reports
    json_report.write(all_results, env, output, run_id)
    csv_report.write(all_results, output, run_id)
    md_path = markdown_report.generate(results_by_scenario, env, output, run_id)

    console.print(f"\n[bold green]Suite complete. Reports in {output}/[/bold green]")
    console.print(f"  JSON:     results_{run_id}.json")
    console.print(f"  CSV:      results_{run_id}.csv")
    console.print(f"  Markdown: {md_path.name}")


@app.command("validate-config")
def validate_config_cmd(
    mode: str = typer.Option("cache-disabled"),
    gateways: str = typer.Option("nexus,litellm,bifrost"),
) -> None:
    """Run pre-flight config parity check without starting a benchmark."""
    gateway_list = [g.strip() for g in gateways.split(",")]
    configs = [_load_gateway(g)[0] for g in gateway_list]
    from scenarios.s10_config_parity import run as validate_parity
    validate_parity(configs, required_caching=(mode == "cache-enabled"), halt_on_failure=False)


@app.command("report")
def report_cmd(
    results_dir: str = typer.Option(..., help="Directory containing results JSON files"),
    format: str = typer.Option("markdown", help="markdown | terminal"),
) -> None:
    """Generate comparison report from existing results."""
    import json, glob
    json_files = glob.glob(f"{results_dir}/results_*.json")
    if not json_files:
        console.print(f"[red]No results JSON found in {results_dir}[/red]")
        raise typer.Exit(1)

    all_metrics: list[ScenarioMetrics] = []
    env_info: dict = {}
    for jf in json_files:
        data = json.loads(Path(jf).read_text())
        env_info = data.get("environment", {})
        for r in data.get("results", []):
            m = ScenarioMetrics(gateway_name=r["gateway"], scenario_id=r["scenario"], mode=r["mode"])
            # Hydrate key fields
            m.total_requests = r.get("total_requests", 0)
            m.successful = r.get("successful", 0)
            m.failed = r.get("failed", 0)
            m.http_4xx = r.get("http_4xx", 0)
            m.http_5xx = r.get("http_5xx", 0)
            m.stream_broken = r.get("stream_broken", 0)
            m.wall_time_seconds = r.get("wall_time_seconds", 0)
            all_metrics.append(m)

    if format == "terminal":
        for m in all_metrics:
            terminal.print_scenario_result(m)
    else:
        import uuid
        by_scenario: dict[str, list[ScenarioMetrics]] = {}
        for m in all_metrics:
            by_scenario.setdefault(m.scenario_id, []).append(m)
        path = markdown_report.generate(by_scenario, env_info, results_dir, str(uuid.uuid4())[:8])
        console.print(f"[green]Report written: {path}[/green]")


# ───────────────────────────────────────────────────────────────────────────
# Scenario metadata — used by `validate-all` to surface which scenarios are
# implemented, which gateways they target, which dataset they consume, and
# the rough wall-clock cost at default knobs. Keep in sync with the actual
# scenario modules; touched whenever a new scenario is added.
# ───────────────────────────────────────────────────────────────────────────
SCENARIO_META = {
    "s01": {"name": "Short Chat",            "gateways": ["nexus", "litellm", "bifrost"], "dataset": "short_chat_v2.json",    "default_vus": 20, "default_duration_s": 300, "warmup_s": 30, "scope": "head-to-head", "mode": "cache-disabled"},
    "s02": {"name": "Long Context",          "gateways": ["nexus", "litellm", "bifrost"], "dataset": "long_context_v2.json",  "default_vus": 10, "default_duration_s": 300, "warmup_s": 60, "scope": "head-to-head", "mode": "cache-disabled"},
    "s03": {"name": "Streaming Stress",      "gateways": ["nexus", "litellm", "bifrost"], "dataset": "streaming_v2.json",     "default_vus": 30, "default_duration_s": 300, "warmup_s": 30, "scope": "head-to-head", "mode": "cache-disabled"},
    "s04": {"name": "Concurrency Sweep",     "gateways": ["nexus", "litellm", "bifrost"], "dataset": "short_chat_v2.json",    "default_vus": "1→100", "default_duration_s": 720, "warmup_s": 15, "scope": "head-to-head", "mode": "cache-disabled"},
    "s05": {"name": "Soak / Stability",      "gateways": ["nexus", "litellm", "bifrost"], "dataset": "short_chat_v2.json",    "default_vus": 20, "default_duration_s": 1800, "warmup_s": 0, "scope": "head-to-head", "mode": "cache-disabled"},
    "s06": {"name": "Flakiness / Consistency", "gateways": ["nexus", "litellm", "bifrost"], "dataset": "(inline) 100x repeat", "default_vus": 1, "default_duration_s": "~100 reqs", "warmup_s": 0, "scope": "head-to-head", "mode": "cache-disabled"},
    "s07": {"name": "Overhead Isolation",    "gateways": ["nexus", "litellm", "bifrost"], "dataset": "short_chat_v2.json (max_tokens=1)", "default_vus": 5, "default_duration_s": 120, "warmup_s": 15, "scope": "head-to-head", "mode": "cache-disabled"},
    "s08": {"name": "Cache Feature",         "gateways": ["nexus"],                       "dataset": "cache_exact_v2.json + cache_prefix_v2.json + mixed", "default_vus": 10, "default_duration_s": "120×3 sub-tests", "warmup_s": 10, "scope": "nexus-feature", "mode": "cache-enabled"},
    "s09": {"name": "Compliance / PII",      "gateways": ["nexus"],                       "dataset": "compliance_pii_v2.json", "default_vus": 1, "default_duration_s": "~N prompts", "warmup_s": 0, "scope": "nexus-feature", "mode": "any"},
    "s10": {"name": "Config Parity",         "gateways": ["nexus", "litellm", "bifrost"], "dataset": "(none — config-only check)", "default_vus": 0, "default_duration_s": "<1", "warmup_s": 0, "scope": "preflight", "mode": "any"},
    "s11": {"name": "Provider Failover",     "gateways": ["nexus"],                       "dataset": "short_chat_v2.json",    "default_vus": 5, "default_duration_s": 90, "warmup_s": 10, "scope": "nexus-feature", "mode": "cache-disabled"},
}


@app.command("validate-all")
def validate_all_cmd(
    dry_run: bool = typer.Option(True, "--dry-run/--live", help="--dry-run prints the catalog; --live additionally imports each module to verify it loads"),
    output: str = typer.Option("./SCENARIO_STATUS.md", help="Write the catalog to this Markdown file"),
) -> None:
    """List every scenario, its gateway/dataset/duration metadata, and implementation status."""
    from rich.table import Table
    table = Table(title="Benchmark scenario catalog", show_lines=False, header_style="bold cyan")
    table.add_column("ID")
    table.add_column("Name")
    table.add_column("Scope")
    table.add_column("Gateways")
    table.add_column("Dataset")
    table.add_column("Default VU × Dur")
    table.add_column("Mode")
    table.add_column("Status")

    md_lines = [
        "# Scenario Catalog — Pre-AWS Status",
        "",
        f"Auto-generated by `python cli.py validate-all`.",
        "",
        "| ID | Name | Scope | Gateways | Dataset | Default VU × Dur | Mode | Status |",
        "|---|---|---|---|---|---|---|---|",
    ]

    import importlib as _importlib
    for sid, meta in SCENARIO_META.items():
        status = "✅ implemented"
        if dry_run is False:
            try:
                mod = _importlib.import_module(SCENARIO_MAP[sid])
                if not hasattr(mod, "run"):
                    status = "⚠️ no run() entrypoint"
            except Exception as e:
                status = f"❌ import error: {type(e).__name__}"
        gws = ", ".join(meta["gateways"])
        vu_dur = f"{meta['default_vus']} VU × {meta['default_duration_s']}s"
        table.add_row(sid.upper(), meta["name"], meta["scope"], gws, str(meta["dataset"])[:40], vu_dur, meta["mode"], status)
        md_lines.append(f"| {sid.upper()} | {meta['name']} | {meta['scope']} | {gws} | `{meta['dataset']}` | {vu_dur} | {meta['mode']} | {status} |")

    console.print(table)
    Path(output).write_text("\n".join(md_lines) + "\n")
    console.print(f"\n[green]Catalog written to {output}[/green]")


@app.command("preflight")
def preflight_cmd(
    gateways: str = typer.Option("nexus,litellm,bifrost", help="Comma-separated gateway list to probe"),
    skip_quota: bool = typer.Option(False, "--skip-quota", help="Skip the OpenAI quota check (saves one paid call)"),
) -> None:
    """Pass/fail preflight before any scenario. Verifies OpenAI quota, gateway health, VK shape, and nexus circuit state."""
    import os, json, time as _time, httpx
    rows = []  # (check, status, detail)

    # 1) OPENAI_API_KEY funded?
    if skip_quota:
        rows.append(("OpenAI quota", "SKIP", "—"))
    else:
        key = os.getenv("OPENAI_API_KEY")
        if not key or "REPLACE" in key:
            rows.append(("OpenAI quota", "FAIL", "OPENAI_API_KEY missing or placeholder"))
        else:
            try:
                r = httpx.post(
                    "https://api.openai.com/v1/chat/completions",
                    headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"},
                    json={"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "ok"}], "max_tokens": 3},
                    timeout=15.0,
                )
                if r.status_code == 200:
                    rows.append(("OpenAI quota", "PASS", "200 from api.openai.com"))
                elif r.status_code == 429 and "insufficient_quota" in r.text:
                    rows.append(("OpenAI quota", "FAIL", "insufficient_quota — top up billing"))
                else:
                    rows.append(("OpenAI quota", "FAIL", f"HTTP {r.status_code}: {r.text[:80]}"))
            except Exception as e:
                rows.append(("OpenAI quota", "FAIL", f"{type(e).__name__}: {e}"))

    # 2) Per-gateway: health + key shape
    for gw in gateways.split(","):
        gw = gw.strip()
        if gw not in GATEWAY_MAP:
            rows.append((f"{gw}: known", "FAIL", "unknown gateway")); continue
        try:
            cfg, _ = _load_gateway(gw)
        except Exception as e:
            rows.append((f"{gw}: config load", "FAIL", f"{type(e).__name__}: {e}")); continue

        # Health — retry with backoff. A container just after `docker restart`
        # sits in health: starting (~8s for Bifrost) and a single immediate probe
        # gives a false UNHEALTHY (observed on AWS). Retry up to ~15s before failing.
        base = cfg.gateway.base_url
        hdrs = {"Authorization": f"Bearer {cfg.gateway.api_key}"} if cfg.gateway.api_key else {}
        healthy = False
        last_detail = "no response"
        for attempt in range(5):  # ~0+3+3+3+3 = up to ~12s of retries
            try:
                for path in ("/health", "/healthz", "/v1/models"):
                    r = httpx.get(base + path, timeout=5.0, headers=hdrs, verify=False)
                    if r.status_code < 500:
                        rows.append((f"{gw}: reachable ({path})", "PASS",
                                     f"HTTP {r.status_code}" + (f" (attempt {attempt+1})" if attempt else "")))
                        healthy = True
                        break
                if healthy:
                    break
                last_detail = "no health endpoint < 500"
            except Exception as e:
                last_detail = f"{type(e).__name__}: {e}"
            if attempt < 4:
                _time.sleep(3.0)
        if not healthy:
            rows.append((f"{gw}: reachable", "FAIL", f"{last_detail} (after 5 attempts / ~12s)"))

        # Nexus VK shape
        if gw == "nexus":
            vk = cfg.gateway.api_key or ""
            if vk.startswith("nvk_") and len(vk) > 20:
                rows.append(("nexus: VK shape", "PASS", f"{vk[:12]}…"))
            else:
                rows.append(("nexus: VK shape", "FAIL", "NEXUS_API_KEY missing or wrong prefix"))

            # Nexus circuit reset (defensive — if admin URL + key present)
            admin_url = cfg.admin.admin_base_url
            admin_key = os.getenv("NEXUS_ADMIN_API_KEY", "")
            if admin_url and "nvk_" not in admin_key:
                # Best-effort reset for the openai-prod credential (id stable in seed)
                cred_id = "abff2f77-5506-4d73-99a3-6b60ed756bac"
                try:
                    rr = httpx.post(f"{admin_url}/api/admin/credentials/{cred_id}/circuit-reset",
                                    headers={"Authorization": f"Bearer {admin_key}"}, timeout=10.0)
                    if rr.status_code == 200:
                        rows.append(("nexus: circuit reset", "PASS", "reset OK"))
                    else:
                        rows.append(("nexus: circuit reset", "WARN", f"HTTP {rr.status_code} — proceed with caution"))
                except Exception as e:
                    rows.append(("nexus: circuit reset", "WARN", f"could not reach admin: {e}"))

            # Probe one request through nexus
            try:
                pr = httpx.post(
                    f"{base}/v1/chat/completions",
                    headers={"Authorization": f"Bearer {cfg.gateway.api_key}", "Content-Type": "application/json"},
                    json={"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "preflight ok?"}], "max_tokens": 5, "stream": False},
                    timeout=25.0,
                )
                if pr.status_code == 200:
                    rows.append(("nexus: probe call", "PASS", "200 with completion"))
                else:
                    rows.append(("nexus: probe call", "FAIL", f"HTTP {pr.status_code}: {pr.text[:80]}"))
            except Exception as e:
                rows.append(("nexus: probe call", "FAIL", f"{type(e).__name__}: {e}"))

    # Render
    from rich.table import Table
    t = Table(title="Preflight", show_lines=False, header_style="bold cyan")
    t.add_column("Check"); t.add_column("Status"); t.add_column("Detail")
    any_fail = False
    for check, status, detail in rows:
        color = {"PASS": "green", "FAIL": "red", "WARN": "yellow", "SKIP": "dim"}.get(status, "white")
        t.add_row(check, f"[{color}]{status}[/{color}]", detail)
        if status == "FAIL":
            any_fail = True
    console.print(t)
    if any_fail:
        console.print("\n[red]preflight: one or more checks FAILED — abort before running benchmarks[/red]")
        raise typer.Exit(2)
    console.print("\n[green]preflight: OK[/green]")


if __name__ == "__main__":
    app()
