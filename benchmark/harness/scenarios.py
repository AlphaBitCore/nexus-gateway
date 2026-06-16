"""Scenario definitions, prompt sourcing, and the async load runner.

Load model: closed-loop virtual users. ``vus`` worker coroutines each issue
requests back-to-back (one in flight per VU), which both simulates N concurrent
users and naturally bounds concurrency to N. Workers stop on a wall-clock
deadline (duration scenarios) or once a shared request budget is exhausted
(fixed-count scenarios like smoke).
"""
from __future__ import annotations

import asyncio
import time
import uuid
from dataclasses import dataclass, field
from typing import Optional

import httpx

from . import datasets
from .client import send_request
from .config import GatewayConfig
from .metrics import RequestRecord


@dataclass
class Scenario:
    name: str
    stream: bool
    dataset: str
    cache_mode: str                      # "enabled" | "disabled"
    description: str
    vus: int = 1
    duration: Optional[int] = None       # seconds (duration-based)
    num_requests: Optional[int] = None   # total across all VUs (count-based)
    max_tokens: Optional[int] = None     # None -> use gateway config value
    nexus_only: bool = False
    sweep_levels: Optional[list[int]] = None


SCENARIOS: dict[str, Scenario] = {
    "smoke": Scenario(
        name="smoke", stream=True, dataset="short_chat.jsonl",
        cache_mode="disabled", vus=1, num_requests=5, max_tokens=64,
        description="1 user, 5 requests, verify auth + schema + output files.",
    ),
    "w01": Scenario(
        name="w01", stream=True, dataset="short_chat.jsonl",
        cache_mode="disabled", vus=20, duration=180,
        description="Short Chat: 20 VUs, 3 min, streaming, cache disabled.",
    ),
    "w02": Scenario(
        name="w02", stream=True, dataset="long_context.jsonl",
        cache_mode="disabled", vus=10, duration=180, max_tokens=256,
        description="Long Context: 10 VUs, 3 min, ~16k-token prompts, cache disabled.",
    ),
    "w03a": Scenario(
        name="w03a", stream=True, dataset="short_chat.jsonl",
        cache_mode="disabled", vus=20, duration=180,
        description="Cache-disabled fair comparison (w01 config, cache off for all).",
    ),
    "w03b": Scenario(
        name="w03b", stream=True, dataset="short_chat.jsonl",
        cache_mode="enabled", vus=10, duration=120, nexus_only=True,
        description="Nexus cache feature: exact/prefix/mixed traffic, hit rates + TTFT.",
    ),
    "w04": Scenario(
        name="w04", stream=True, dataset="streaming_stress.jsonl",
        cache_mode="disabled", vus=30, duration=180, max_tokens=1024,
        description="Streaming stress: 30 VUs, 3 min, long-form prompts, cache disabled.",
    ),
    "concurrency-sweep": Scenario(
        name="concurrency-sweep", stream=True, dataset="short_chat.jsonl",
        cache_mode="disabled", duration=30, sweep_levels=[1, 5, 10, 20, 50, 100],
        description="w01 workload swept across VU levels [1,5,10,20,50,100].",
    ),
}

# Fixed prompts reused by the w03b cache scenario so repeats are byte-identical.
_W03B_EXACT_POOL = [
    "Explain what an API gateway does and list its main responsibilities.",
    "Describe how server-sent events deliver streaming responses.",
    "What is the difference between latency and throughput?",
]
_W03B_PREFIX = (
    "You are a senior systems engineer. Read the following standing context "
    "carefully before answering. Context: an OpenAI-compatible gateway proxies "
    "chat completions, streams tokens via SSE, records TTFT and end-to-end "
    "latency, and can optionally cache responses to cut cost and latency. "
    "Given that fixed context, answer this specific question: "
)
_W03B_PREFIX_QUESTIONS = [
    "how would you measure cache hit rate fairly?",
    "what headers might indicate a cache hit?",
    "why can prefix caching help long prompts?",
    "what confounders bias a proxy comparison?",
]


def resolve_scenario(name: str, *, vus: Optional[int],
                     duration: Optional[int]) -> Scenario:
    """Return the scenario with CLI overrides applied (vus/duration)."""
    if name not in SCENARIOS:
        raise ValueError(
            f"Unknown scenario '{name}'. Choices: {', '.join(SCENARIOS)}"
        )
    base = SCENARIOS[name]
    # Shallow copy so overrides don't mutate the registry.
    s = Scenario(**{f.name: getattr(base, f.name) for f in base.__dataclass_fields__.values()})
    if vus is not None:
        s.vus = vus
    if duration is not None and s.num_requests is None:
        s.duration = duration
    return s


class PromptSource:
    """Builds (prompt_id, messages, cache_class) tuples for a scenario."""

    def __init__(self, scenario: Scenario, gateway: str):
        self.scenario = scenario
        self.gateway = gateway
        self.rows = datasets.load_jsonl(scenario.dataset)
        self._is_long = "target_tokens" in self.rows[0]

    def _nonce(self, vu: int, iteration: int) -> str:
        # Unique per request; guarantees no accidental cache reuse.
        return f"[req {self.gateway} vu={vu} iter={iteration} id={uuid.uuid4().hex[:12]}]"

    def build(self, vu: int, iteration: int) -> tuple[str, list[dict], str]:
        if self.scenario.name == "w03b":
            return self._build_cache(vu, iteration)

        idx = (vu * 1_000_003 + iteration) % len(self.rows)
        row = self.rows[idx]
        prompt_id = row.get("prompt_id", f"row-{idx}")

        if self._is_long:
            content = datasets.render_long_context(row, vu, iteration)
        else:
            content = datasets.render_short(row)

        if self.scenario.cache_mode == "disabled":
            content = f"{self._nonce(vu, iteration)}\n{content}"
        return prompt_id, [{"role": "user", "content": content}], ""

    def _build_cache(self, vu: int, iteration: int) -> tuple[str, list[dict], str]:
        """w03b: alternate exact / prefix / mixed traffic by iteration."""
        bucket = iteration % 3
        if bucket == 0:  # exact repeats — identical bytes => cacheable
            p = _W03B_EXACT_POOL[iteration % len(_W03B_EXACT_POOL)]
            return f"exact-{iteration % len(_W03B_EXACT_POOL)}", \
                   [{"role": "user", "content": p}], "exact"
        if bucket == 1:  # shared long prefix, small varying suffix
            q = _W03B_PREFIX_QUESTIONS[iteration % len(_W03B_PREFIX_QUESTIONS)]
            return f"prefix-{iteration % len(_W03B_PREFIX_QUESTIONS)}", \
                   [{"role": "user", "content": _W03B_PREFIX + q}], "prefix"
        # mixed: half repeat a hot prompt, half unique
        if iteration % 2 == 0:
            p = _W03B_EXACT_POOL[0]
            return "mixed-repeat", [{"role": "user", "content": p}], "mixed"
        content = f"{self._nonce(vu, iteration)} Summarize the benefits of caching."
        return "mixed-unique", [{"role": "user", "content": content}], "mixed"


async def run_scenario(
    cfg: GatewayConfig,
    scenario: Scenario,
    *,
    vus: int,
    warmup: int,
) -> tuple[list[RequestRecord], float]:
    """Run one scenario instance against one gateway. Returns (records, elapsed_s)."""
    source = PromptSource(scenario, cfg.gateway_name)
    max_tokens = scenario.max_tokens or cfg.max_tokens
    limits = httpx.Limits(max_connections=max(vus * 2, 10),
                          max_keepalive_connections=max(vus, 10))
    timeout = httpx.Timeout(cfg.request_timeout, connect=10.0)

    records: list[RequestRecord] = []

    async with httpx.AsyncClient(limits=limits, timeout=timeout) as client:
        # ---- warmup (excluded from metrics) ----
        for w in range(max(0, warmup)):
            pid, messages, cclass = source.build(vu=-1, iteration=w)
            await send_request(
                client, cfg, messages=messages, stream=scenario.stream,
                max_tokens=max_tokens, gateway=cfg.gateway_name,
                scenario=scenario.name, vu=-1, iteration=w, prompt_id=pid,
                cache_class=cclass,
            )

        # ---- measured load ----
        budget = {"remaining": scenario.num_requests}  # None => duration mode
        deadline = (time.perf_counter() + scenario.duration) if scenario.duration else None
        start = time.perf_counter()

        async def worker(vu_id: int):
            it = 0
            while True:
                # Claim work atomically (no await between check and decrement).
                if budget["remaining"] is not None:
                    if budget["remaining"] <= 0:
                        break
                    budget["remaining"] -= 1
                elif deadline is not None and time.perf_counter() >= deadline:
                    break

                pid, messages, cclass = source.build(vu_id, it)
                rec = await send_request(
                    client, cfg, messages=messages, stream=scenario.stream,
                    max_tokens=max_tokens, gateway=cfg.gateway_name,
                    scenario=scenario.name, vu=vu_id, iteration=it,
                    prompt_id=pid, cache_class=cclass,
                )
                records.append(rec)
                it += 1

        await asyncio.gather(*(worker(i) for i in range(vus)))
        elapsed = time.perf_counter() - start

    return records, elapsed
