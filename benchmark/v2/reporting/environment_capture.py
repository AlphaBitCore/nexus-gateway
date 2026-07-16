"""Capture host environment metadata for reproducibility."""
from __future__ import annotations
import hashlib, json, os, platform, subprocess, uuid
from datetime import datetime, timezone
from pathlib import Path

try:
    import psutil
    HAS_PSUTIL = True
except ImportError:
    HAS_PSUTIL = False


def capture(gateway_name: str, config_path: str | None = None) -> dict:
    info: dict = {
        "run_id": str(uuid.uuid4()),
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "gateway_name": gateway_name,
        "test_machine": {
            "hostname": platform.node(),
            "os": platform.platform(),
            "cpu": platform.processor() or "unknown",
            "cpu_count": os.cpu_count(),
            "python_version": platform.python_version(),
        },
        "git_commit": _git_commit(),
        "config_fingerprint": _fingerprint(config_path) if config_path else "N/A",
        # Methodology facts the validator gates on (recorded so a result proves
        # its own validity). BENCH_UNIQUE_PROMPTS=1 is required — without it,
        # Nexus's broker coalesces identical prompts and the load isn't
        # comparable. The v2 harness runs gateways one at a time (sequential).
        "bench_unique_prompts": os.getenv("BENCH_UNIQUE_PROMPTS", "") == "1",
        "run_mode": "sequential",
    }
    if HAS_PSUTIL:
        mem = psutil.virtual_memory()
        info["test_machine"]["ram_gb"] = round(mem.total / 1e9, 1)
    return info


def _git_commit() -> str:
    try:
        return subprocess.check_output(
            ["git", "rev-parse", "HEAD"], stderr=subprocess.DEVNULL, text=True
        ).strip()[:12]
    except Exception:
        return "unknown"


def _fingerprint(path: str) -> str:
    try:
        return hashlib.sha256(Path(path).read_bytes()).hexdigest()[:12]
    except Exception:
        return "unknown"
