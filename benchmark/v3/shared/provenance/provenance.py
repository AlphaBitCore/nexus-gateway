"""benchmark/v3 — provenance capture.

Assembles the reproducibility record for a run: exact gateway pin (from the
resolved lock), Nexus source commit, CPU model, allocation, disk spec, load
config, upstream, and the exact set of services running on the box.

`build_provenance` takes an injected `sources` mapping of readers so it is fully
testable offline (no /proc, no env, no subprocess in tests). The CLI
(scripts/capture_provenance.py) supplies real readers. Stdlib-only.
"""
from __future__ import annotations

import platform
import re
from typing import Any, Callable

PROVENANCE_SCHEMA = "provenance/v1"


def default_sources() -> dict[str, Callable[[], Any]]:
    """Real readers used by the CLI. Each is a zero-arg callable so tests can
    replace any of them with an in-memory fake."""
    import os

    def cpu_model() -> str:
        # Linux: parse /proc/cpuinfo "model name". Fallback: platform.processor().
        try:
            with open("/proc/cpuinfo", encoding="utf-8") as f:
                for line in f:
                    if line.lower().startswith("model name"):
                        return line.split(":", 1)[1].strip()
        except OSError:
            pass
        return platform.processor() or platform.machine() or "unknown"

    return {
        "cpu_model": cpu_model,
        "env": lambda: dict(os.environ),
    }


def _get(sources: dict[str, Callable[[], Any]], key: str, default: Any = None) -> Any:
    fn = sources.get(key)
    return fn() if fn else default


def build_provenance(sources: dict[str, Callable[[], Any]], run_context: dict[str, Any]) -> dict[str, Any]:
    """Build a provenance dict conforming to provenance.schema.json.

    `run_context` carries the run-time inputs the caller already knows (gateway
    pin from the lock, offered load, upstream, disk spec, service inventory,
    loadtest version, allocation, region/az, captured_at). `sources` provides the
    machine-read bits (cpu_model, env). Values that are genuinely unknown are set
    to null — never guessed (matches the v2 "null, not 0" honesty rule).
    """
    env = _get(sources, "env", {}) or {}
    cpu_model = _get(sources, "cpu_model", "unknown")

    gw = run_context.get("gateway", {}) or {}
    offered = run_context.get("offered_load", {}) or {}
    upstream = run_context.get("upstream", {}) or {}
    disk = run_context.get("disk", {}) or {}

    def env_first(*keys: str) -> str | None:
        for k in keys:
            if env.get(k):
                return env[k]
        return None

    prov = {
        "schema": PROVENANCE_SCHEMA,
        "captured_at": run_context.get("captured_at"),
        "gateway": {
            "name": gw.get("name"),
            "version": gw.get("version"),
            "image_digest": gw.get("image_digest"),
            "commit": gw.get("commit"),
        },
        "nexus_commit": run_context.get("nexus_commit"),
        "cpu_model": cpu_model,
        "cpu_vcpus": run_context.get("cpu_vcpus"),
        "memory_mb": run_context.get("memory_mb"),
        "launch_type": run_context.get("launch_type"),
        "instance_type": run_context.get("instance_type"),
        "fargate_platform_version": env_first("ECS_FARGATE_PLATFORM_VERSION")
        or run_context.get("fargate_platform_version"),
        "task_definition_revision": env_first("ECS_TASK_DEFINITION_REVISION")
        or run_context.get("task_definition_revision"),
        "disk": {
            "type": disk.get("type"),
            "iops": disk.get("iops"),
            "throughput_mibps": disk.get("throughput_mibps"),
        },
        "loadtest_version": run_context.get("loadtest_version"),
        "offered_load": {
            "model": offered.get("model"),
            "target_rps": offered.get("target_rps"),
            "warmup_seconds": offered.get("warmup_seconds"),
            "duration_seconds": offered.get("duration_seconds"),
        },
        "upstream": {
            "name": upstream.get("name"),
            "expected_response_id": upstream.get("expected_response_id"),
        },
        "services_running": list(run_context.get("services_running", []) or []),
        "region": run_context.get("region") or env_first("AWS_REGION", "AWS_DEFAULT_REGION"),
        "az": run_context.get("az"),
    }
    return prov


_GIT_SHA_RE = re.compile(r"^[0-9a-f]{7,40}$")


def looks_like_commit(value: str | None) -> bool:
    """True if the string is a plausible git SHA (used to sanity-check nexus_commit)."""
    return bool(value and _GIT_SHA_RE.match(value))
