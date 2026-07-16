"""
Benchmark stability helpers born from the July-14 LiteLLM anomaly:
  - run-order (LiteLLM LAST so a cold connection pool / upstream warmup doesn't
    contaminate its numbers),
  - a warmup record,
  - best-effort resource telemetry (docker CPU/mem/restarts) — null, never 0,
    when unreadable,
  - anomaly classification so a struggling gateway is labeled honestly instead
    of being reported as merely "slow".
No fabricated values anywhere.
"""
from __future__ import annotations

import shutil
import subprocess
import threading
import time
from typing import Optional

# Default fair ordering: Nexus first, LiteLLM LAST (cold-pool guard).
DEFAULT_RUN_ORDER = ["nexus-hooks-on", "nexus-hooks-off", "bifrost", "agentgateway", "litellm"]
# Map a run-order token to the CLI gateway name (nexus-hooks-* both → "nexus").
_GW_ALIASES = {"nexus-hooks-on": "nexus", "nexus-hooks-off": "nexus"}


def order_gateways(gateways: list[str], run_order: Optional[list[str]] = None) -> list[str]:
    """Return `gateways` sorted by `run_order` rank; unranked names keep their
    relative order and go last-but-before-litellm; litellm is forced LAST
    regardless (its cold-start is the whole reason this exists)."""
    order = run_order or DEFAULT_RUN_ORDER
    rank = {}
    for i, tok in enumerate(order):
        rank[_GW_ALIASES.get(tok, tok)] = i
    big = len(order) + 1

    def key(g: str):
        if g == "litellm":
            return (10_000, g)  # always last
        return (rank.get(g, big), g)

    return sorted(gateways, key=key)


def warmup_record(warmup_seconds: int, completed: bool = True) -> dict:
    return {"duration_s": warmup_seconds, "completed": completed, "excluded_from_metrics": True}


def classify_anomaly(metric, ratio_threshold: float = 4.0) -> None:
    """Set metric.anomaly_status / infrastructure_status IN PLACE.

    A gateway is 'anomalous' only when BOTH a tail blowup (ttft p95/p50 >
    threshold) AND real errors/timeouts are present — never label a clean run
    anomalous, and never call a gateway 'slow' when the evidence points at the
    upstream or the rig. Leaves both None when there's no signal.
    """
    ratio = metric.anomaly_ratio()
    had_errors = (metric.failed > 0 or metric.connection_timeouts > 0
                  or metric.stream_timeouts > 0 or metric.http_5xx > 0)
    if ratio is not None and ratio > ratio_threshold and had_errors:
        metric.anomaly_status = "anomalous"
        # We cannot, from client-side metrics alone, distinguish cold start /
        # upstream timeout / memory pressure — so the rig status is 'unknown'
        # unless resource telemetry proved otherwise (set elsewhere).
        if metric.infrastructure_status is None:
            metric.infrastructure_status = "unknown"


class ResourceSampler:
    """Best-effort background sampler of a docker container's CPU%/mem during a
    run. Use as a context manager; `.observations()` returns the peak values
    with source_status. If docker or the container is unavailable, EVERY numeric
    field is None and source_status='unavailable' — never a fabricated 0.

    Container name comes from the caller (deployment-specific); no guessing.
    """
    def __init__(self, container: Optional[str], interval_s: float = 2.0):
        self.container = container
        self.interval_s = interval_s
        self._stop = threading.Event()
        self._thread: Optional[threading.Thread] = None
        self._cpu_peak: Optional[float] = None
        self._mem_peak_mb: Optional[float] = None
        self._samples = 0
        self._available = bool(container) and shutil.which("docker") is not None

    def __enter__(self):
        if self._available:
            self._thread = threading.Thread(target=self._loop, daemon=True)
            self._thread.start()
        return self

    def __exit__(self, *exc):
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=self.interval_s + 2)

    def _loop(self):
        while not self._stop.is_set():
            self._sample_once()
            self._stop.wait(self.interval_s)

    def _sample_once(self):
        try:
            out = subprocess.run(
                ["docker", "stats", "--no-stream", "--format", "{{.CPUPerc}}|{{.MemUsage}}", self.container],
                capture_output=True, text=True, timeout=8)
            if out.returncode != 0 or not out.stdout.strip():
                return
            cpu_s, mem_s = out.stdout.strip().split("|", 1)
            cpu = float(cpu_s.strip().rstrip("%"))
            mem_mb = _parse_mem_mb(mem_s.split("/", 1)[0].strip())
            self._samples += 1
            if cpu is not None and (self._cpu_peak is None or cpu > self._cpu_peak):
                self._cpu_peak = cpu
            if mem_mb is not None and (self._mem_peak_mb is None or mem_mb > self._mem_peak_mb):
                self._mem_peak_mb = mem_mb
        except Exception:
            return  # best-effort: a failed sample never fabricates data

    def _restart_count(self) -> Optional[int]:
        if not self._available:
            return None
        try:
            out = subprocess.run(
                ["docker", "inspect", "--format", "{{.RestartCount}}", self.container],
                capture_output=True, text=True, timeout=8)
            if out.returncode == 0 and out.stdout.strip().isdigit():
                return int(out.stdout.strip())
        except Exception:
            pass
        return None

    def observations(self, metric=None) -> dict:
        """Combine sampled host stats with upstream error counts from `metric`
        (if given). source_status: available (got samples) / partial (docker
        present but no samples) / unavailable (no container/docker)."""
        if not self._available:
            status = "unavailable"
        elif self._samples > 0:
            status = "available"
        else:
            status = "partial"
        obs = {
            "cpu_peak_pct": round(self._cpu_peak, 1) if self._cpu_peak is not None else None,
            "memory_peak_mb": round(self._mem_peak_mb, 1) if self._mem_peak_mb is not None else None,
            "container_restart_count": self._restart_count(),
            "samples": self._samples,
            "source_status": status,
        }
        if metric is not None:
            obs["upstream_5xx_count"] = metric.http_5xx
            obs["timeout_count"] = metric.connection_timeouts + metric.stream_timeouts
            # 4xx as a proxy for upstream 429s (the harness doesn't split them out)
            obs["upstream_4xx_count"] = metric.http_4xx
        return obs


def _parse_mem_mb(s: str) -> Optional[float]:
    """Parse a docker-stats mem string → MB. docker stats emits binary units
    (KiB/MiB/GiB); we also tolerate the decimal spellings just in case. None on
    an unrecognized string (never a fabricated 0)."""
    s = s.strip()
    units = (("GiB", 1024.0), ("MiB", 1.0), ("KiB", 1.0 / 1024.0),
             ("GB", 1000.0), ("MB", 1.0), ("kB", 1.0 / 1000.0), ("B", 1.0 / (1024.0 * 1024.0)))
    for unit, to_mb in units:
        if s.endswith(unit):
            try:
                return float(s[: -len(unit)].strip()) * to_mb
            except ValueError:
                return None
    return None
