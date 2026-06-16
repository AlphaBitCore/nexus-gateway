"""Generate publishable markdown comparison report."""
from __future__ import annotations
from pathlib import Path
from engine.metrics import ScenarioMetrics

METHODOLOGY_NOTE = """
## Benchmark Methodology Note

This is Nexus Gateway Benchmark v2. It supersedes Minimal Benchmark v1 (2026-05-19).

v1 was invalidated for the following reasons:
1. Nexus had semantic caching enabled (44.16% cache hit rate).
   Bifrost and LiteLLM had caching disabled (0% hit rate).
   Nexus W-01 TTFT p95 of 4ms is a cache hit, not a model call.
   This makes all throughput and latency comparisons in v1 invalid.
2. The test client ran from a MacBook Pro, not from within AWS.
   Network jitter from a local machine to an AWS gateway is uncontrolled.
3. No warmup phase was documented or controlled.

v2 corrects all of these issues. All comparison tests run with
caching explicitly DISABLED across all gateways. Nexus caching
is benchmarked separately as a product feature (S-08), not a competitive comparison.
"""


def _fmt(v, suffix="ms") -> str:
    if v is None:
        return "—"
    return f"{v:.1f} {suffix}"


def _pct(v) -> str:
    return f"{v:.2f}%" if v is not None else "—"


def generate(
    results_by_scenario: dict[str, list[ScenarioMetrics]],
    env_info: dict,
    output_dir: str,
    run_id: str,
) -> Path:
    lines: list[str] = []

    lines.append("# Nexus Gateway Benchmark v2 — Results Report\n")
    lines.append(METHODOLOGY_NOTE)
    lines.append(f"\n**Run ID**: `{run_id}`  ")
    lines.append(f"**Timestamp**: {env_info.get('timestamp', 'unknown')}  ")
    lines.append(f"**Host**: {env_info.get('test_machine', {}).get('hostname', 'unknown')}  ")
    lines.append(f"**Git commit**: `{env_info.get('git_commit', 'unknown')}`\n")
    lines.append("---\n")

    for scenario_id, metrics_list in results_by_scenario.items():
        if not metrics_list:
            continue
        mode = metrics_list[0].mode
        lines.append(f"\n## {scenario_id} — {_mode_label(mode)}\n")
        lines.append(f"**Mode**: {mode}  ")
        if mode == "cache-disabled":
            lines.append(
                "**Environment**: Same instance · Same Python script · "
                "Same model (gpt-4o) · Caching: DISABLED on all gateways · "
                "Only variable: Gateway URL\n"
            )
        else:
            lines.append("**Note**: Nexus cache feature test — NOT a comparative benchmark.\n")

        lines.append("\n| Metric | " + " | ".join(m.gateway_name for m in metrics_list) + " |")
        lines.append("|---|" + "---|" * len(metrics_list))

        rows = [
            ("TTFT p50 (ms)",        [_fmt(m.ttft_p50) for m in metrics_list]),
            ("TTFT p95 (ms)",        [_fmt(m.ttft_p95) for m in metrics_list]),
            ("TTFT p99 (ms)",        [_fmt(m.ttft_p99) for m in metrics_list]),
            ("E2E p95 (ms)",         [_fmt(m.e2e_p95) for m in metrics_list]),
            ("Throughput (RPS)",     [f"{m.rps:.2f}" for m in metrics_list]),
            ("HTTP Failure Rate",    [_pct(m.http_failure_rate) for m in metrics_list]),
            ("Stream Broken Rate",   [_pct(m.stream_broken_rate) for m in metrics_list]),
            ("Total Requests",       [str(m.total_requests) for m in metrics_list]),
            ("Cache Hit Rate",       [_pct(m.cache_hit_rate) for m in metrics_list]),
        ]
        for label, vals in rows:
            lines.append(f"| {label} | " + " | ".join(vals) + " |")

        # Threshold pass/fail
        lines.append("\n**Threshold results:**\n")
        for m in metrics_list:
            ttft_ok = (m.ttft_p95 or 0) <= 1500
            fail_ok = m.http_failure_rate <= 1.0
            broken_ok = m.stream_broken_rate <= 0.5
            lines.append(
                f"- **{m.gateway_name}**: "
                f"TTFT p95 {'✅' if ttft_ok else '❌'} "
                f"HTTP failure {'✅' if fail_ok else '❌'} "
                f"Stream broken {'✅' if broken_ok else '❌'}"
            )
        lines.append("")

    Path(output_dir).mkdir(parents=True, exist_ok=True)
    path = Path(output_dir) / f"report_{run_id}.md"
    path.write_text("\n".join(lines))
    return path


def _mode_label(mode: str) -> str:
    return {
        "cache-disabled": "Cache Disabled — Fair Comparison",
        "cache-enabled": "Cache Enabled — Nexus Feature Test",
    }.get(mode, mode)
