#!/usr/bin/env python3
"""
validate_benchmark.py — result-integrity gate for benchmark v2.

Reads result JSON ({run_id, environment, results:[...]}) and enforces the
methodology invariants the July-14 run taught us. Pure stdlib, no network.

BLOCK (exit 1) — the result is INVALID and must not be compared/published:
  - a Nexus hooks-off result whose gateway runtime was NOT proven off
    (governance.gateway_runtime_verified != true) — the June-16 DB-edit trap;
  - hooks-off with a nonzero runtime response-hook count;
  - hooks-on with a zero runtime hook count;
  - NEXUS_AUDIT_DISABLED present on a governed Nexus run;
  - BENCH_UNIQUE_PROMPTS != 1 (broker coalescing invalidates the load);
  - run_mode != sequential.

WARN (exit 0, but printed) — LiteLLM stability caveats:
  - LiteLLM without a warmup record / not run last / telemetry unavailable /
    ttft p95:p50 over threshold / nonzero timeout/5xx.

Usage: python scripts/validate_benchmark.py results/results_*.json [--p95-ratio 4.0]
"""
from __future__ import annotations

import argparse
import glob
import json
import sys

BLOCK, WARN, OK = "BLOCK", "WARN", "OK"


def check_result_file(doc: dict, ratio_threshold: float) -> list[tuple[str, str]]:
    issues: list[tuple[str, str]] = []
    env = doc.get("environment", {}) or {}
    results = doc.get("results", []) or []

    # ── methodology facts (whole-run) ────────────────────────────────────────
    if env.get("bench_unique_prompts") is not True:
        issues.append((BLOCK, "BENCH_UNIQUE_PROMPTS != 1 — Nexus broker coalescing makes the load non-comparable"))
    rm = env.get("run_mode")
    if rm is not None and rm != "sequential":
        issues.append((BLOCK, f"run_mode={rm!r} — the published methodology is sequential-only"))

    # ── per-result ────────────────────────────────────────────────────────────
    names = [r.get("gateway", "?") for r in results]
    for r in results:
        gw = r.get("gateway", "?")
        gov = r.get("governance")
        if gov is not None:  # a Nexus governed run
            mode = gov.get("requested_mode")
            if gov.get("audit_disabled_env_present") is True:
                issues.append((BLOCK, f"{gw}: NEXUS_AUDIT_DISABLED present on a governed run — audit records dropped, result tainted"))
            if mode == "hooks-off":
                if gov.get("gateway_runtime_verified") is not True:
                    issues.append((BLOCK, f"{gw}: hooks-off NOT proven at the gateway runtime "
                                          "(gateway_runtime_verified != true) — a DB edit does not propagate; result invalid"))
                rc = gov.get("runtime_response_hook_count")
                if isinstance(rc, int) and rc > 0:
                    issues.append((BLOCK, f"{gw}: hooks-off but runtime still shows {rc} response-stage hook(s) — not actually off"))
            elif mode == "hooks-on":
                hc = gov.get("runtime_hook_count")
                if isinstance(hc, int) and hc == 0:
                    issues.append((BLOCK, f"{gw}: hooks-on but runtime shows 0 hooks — governance not actually applied"))

        # ── LiteLLM stability warnings ──────────────────────────────────────
        if gw == "litellm":
            if r.get("warmup") is None:
                issues.append((WARN, "litellm: no warmup record — cold connection pool may skew its numbers"))
            if names and names[-1] != "litellm":
                issues.append((WARN, "litellm did not run last — cold-pool / upstream warmup may contaminate it"))
            ro = r.get("resource_observations") or {}
            if ro.get("source_status") in (None, "unavailable"):
                issues.append((WARN, "litellm: resource telemetry unavailable — can't distinguish gateway vs rig/upstream limit"))
            p50, p95 = r.get("ttft_p50_ms"), r.get("ttft_p95_ms")
            if p50 and p95 and p50 > 0 and (p95 / p50) > ratio_threshold:
                issues.append((WARN, f"litellm: ttft p95/p50 = {p95/p50:.1f}x (> {ratio_threshold}) — anomalous tail; "
                                     f"classified '{r.get('anomaly_status')}', infra '{r.get('infrastructure_status')}'"))
            to = (r.get("http_5xx", 0) or 0) + (r.get("connection_timeouts", 0) or 0) + (r.get("stream_timeouts", 0) or 0)
            if to > 0:
                issues.append((WARN, f"litellm: {to} timeout/5xx — investigate upstream vs gateway before quoting the number"))
    return issues


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("results", nargs="+", help="result JSON files (globs ok)")
    ap.add_argument("--p95-ratio", type=float, default=4.0, help="ttft p95/p50 anomaly threshold")
    a = ap.parse_args()
    files = [f for pat in a.results for f in glob.glob(pat)] or a.results
    blockers = 0
    for path in files:
        try:
            doc = json.load(open(path))
        except Exception as e:
            print(f"{BLOCK}  {path}: unreadable — {e}"); blockers += 1; continue
        issues = check_result_file(doc, a.p95_ratio)
        if not issues:
            print(f"{OK}     {path}"); continue
        for level, msg in issues:
            print(f"{level}  {path}: {msg}")
            if level == BLOCK:
                blockers += 1
    if blockers:
        print(f"\n{blockers} integrity blocker(s) — these results are INVALID for comparison/publishing.", file=sys.stderr)
        return 1
    print("\nAll results pass the integrity gate.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
