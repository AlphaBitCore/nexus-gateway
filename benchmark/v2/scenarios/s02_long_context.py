"""S-02: Long Context — Cache Disabled."""
from __future__ import annotations
import asyncio
import os
from urllib.parse import urlparse
from engine.metrics import ScenarioMetrics
from engine.runner import run_scenario
from engine.models import GatewayFullConfig
from gateway_adapters.base import BaseGatewayAdapter
from scenarios.base_scenario import load_prompts, DATASETS

SCENARIO_ID = "S-02"
THRESHOLDS = {"ttft_p95_ms": 3000, "http_failure_pct": 2.0}
DATASET_FILE = "long_context_v2.json"
MIN_PROMPT_TOKENS = 10_000  # a real long-context prompt; guards under-padded data


def estimate_tokens(text: str) -> int:
    """Cheap preflight token estimate (word-split × 1.3). A real tokenizer is
    not needed to tell a ~16k-token prompt from a ~41-token one-liner."""
    return int(len(text.split()) * 1.3)


def validate_dataset_tokens(prompts: list[str], dataset_path) -> None:
    """Hard-stop the run if any prompt is too short to be a long-context test.
    Root cause of the unpadded-dataset bug (Bug 5): a ~41-token one-liner was
    silently used for a full 300s S-02 run, measuring nothing meaningful. Raises
    SystemExit (not a warning) so a bad dataset can never reach the load phase."""
    if not prompts:
        print(f"[S-02] PREFLIGHT FAILED: dataset has no prompts")
        print(f"Dataset file: {dataset_path}")
        raise SystemExit(1)
    estimates = []
    for i, p in enumerate(prompts):
        est = estimate_tokens(p)
        if est < MIN_PROMPT_TOKENS:
            print(f"[S-02] PREFLIGHT FAILED: dataset prompt {i} has ~{est} estimated tokens "
                  f"(expected >= {MIN_PROMPT_TOKENS:,})")
            print(f"Dataset file: {dataset_path}")
            print("Fix: run benchmark/v2/scripts/pad_long_context_dataset.py to regenerate the padded dataset,")
            print("     or fetch the padded version from the repo.")
            raise SystemExit(1)
        estimates.append(est)
    print(f"  [S-02] dataset preflight: {len(prompts)} prompts, min ~{min(estimates):,} tokens ✓")


def mock_colocation_note(config: GatewayFullConfig) -> None:
    """Print a methodology note if the mock/upstream provider is co-located with
    the Nexus gateway (loopback RTT advantage over LiteLLM/Bifrost).

    The benchmark config only exposes the GATEWAY url (config.gateway.base_url),
    NOT the gateway's upstream — the harness cannot see where the gateway forwards.
    So co-location is detected from an explicit MOCK_PROVIDER_URL the operator sets
    to the mock's address. Only emitted on the Nexus run; silent otherwise."""
    upstream = os.getenv("MOCK_PROVIDER_URL", "").strip()
    if not upstream:
        return
    nexus_host = urlparse(os.getenv("NEXUS_BASE_URL", "")).hostname or ""
    gw_host = urlparse(config.gateway.base_url).hostname or ""
    # Only relevant when THIS run is the Nexus gateway.
    if not (nexus_host and gw_host == nexus_host):
        return
    up_host = urlparse(upstream).hostname or ""
    if up_host in ("localhost", "127.0.0.1", "::1") or (up_host and up_host == nexus_host):
        print(f"  [S-02] METHODOLOGY NOTE: mock provider appears to be co-located with the Nexus gateway")
        print(f"         (upstream: {upstream}). Nexus will have lower upstream RTT than")
        print(f"         LiteLLM/Bifrost. Nexus hooks-OFF vs LiteLLM comparison is partially affected.")
        print(f"         For neutral comparison: move mock provider to a separate instance.")


async def run(config: GatewayFullConfig, adapter: BaseGatewayAdapter, mode: str = "cache-disabled") -> ScenarioMetrics:
    prompts = load_prompts(DATASET_FILE)
    # Dataset preflight (Task 2) — hard-stop on an under-padded dataset before any
    # request is sent. Methodology note (Task 3) — flag a co-located mock upstream.
    validate_dataset_tokens(prompts, DATASETS / DATASET_FILE)
    mock_colocation_note(config)
    metrics = ScenarioMetrics(gateway_name=config.gateway.name, scenario_id=SCENARIO_ID, mode=mode)
    # S-02 historically halves VUs because each 16k-token request is far heavier
    # upstream than short chat (one 16k request ≈ many short ones on TPM). This
    # USED to be silent — BENCH_VUS=3 quietly ran at 1 VU. Now it's logged, and
    # BENCH_S02_NO_HALVE=1 disables the halving for operators who want the exact
    # BENCH_VUS they set. To get 3 effective VUs with halving on, set BENCH_VUS=6.
    configured = config.benchmark.virtual_users
    if os.getenv("BENCH_S02_NO_HALVE", "").lower() in ("1", "true", "yes"):
        vus = max(1, configured)
        print(f"  [S-02] BENCH_S02_NO_HALVE set — running {vus} VU(s) (no halving)")
    else:
        vus = max(1, configured // 2)
        print(f"  [S-02] long-context VU halving: configured={configured} → effective={vus} VU(s) "
              f"(set BENCH_S02_NO_HALVE=1 to disable, or BENCH_VUS={configured*2 if configured else 6} for {configured or 3} effective)")
    await run_scenario(
        config=config, adapter=adapter, prompts=prompts,
        virtual_users=vus,
        duration_seconds=config.benchmark.test_duration_seconds,
        metrics=metrics,
        warmup_seconds=60,
    )
    return metrics
