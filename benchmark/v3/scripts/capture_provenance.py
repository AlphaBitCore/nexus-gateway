#!/usr/bin/env python3
"""benchmark/v3 — capture run provenance (CLI).

Assembles the reproducibility record for a run (gateway pin from versions.lock,
Nexus commit, CPU model, allocation, disk spec, load config, upstream, running
services) and writes it as JSON. The pure builder lives in
shared/provenance/provenance.py; this CLI wires the real readers + inputs.

    python scripts/capture_provenance.py --gateway nexus \
        --lock config/versions.lock --story-config config/story-a.yaml \
        --out story_a/results/provenance-<run_id>.json

Exit codes: 0 ok · 2 usage/config error.
"""
from __future__ import annotations

import argparse
import json
import sys
from datetime import datetime, timezone
from pathlib import Path

V3_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(V3_ROOT))

from shared.provenance.provenance import build_provenance, default_sources  # noqa: E402


def _load_json(path: Path) -> dict:
    return json.loads(path.read_text(encoding="utf-8"))


def _load_yaml(path: Path) -> dict:
    import yaml

    return yaml.safe_load(path.read_text(encoding="utf-8")) or {}


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="Capture v3 run provenance")
    ap.add_argument("--gateway", required=True, help="gateway name (must exist in the lock)")
    ap.add_argument("--lock", default=str(V3_ROOT / "config" / "versions.lock"))
    ap.add_argument("--story-config", default=str(V3_ROOT / "config" / "story-a.yaml"))
    ap.add_argument("--nexus-commit", default=None)
    ap.add_argument("--services", default="", help="comma-separated running service names")
    ap.add_argument("--out", default=None)
    a = ap.parse_args(argv)

    try:
        lock = _load_json(Path(a.lock))
        story = _load_yaml(Path(a.story_config))
    except (OSError, json.JSONDecodeError) as e:
        print(f"ERROR: {e}", file=sys.stderr)
        return 2
    except ModuleNotFoundError:
        print("ERROR: PyYAML required — `pip install pyyaml`", file=sys.stderr)
        return 2

    entry = (lock.get("entries") or {}).get(a.gateway)
    if not entry:
        print(f"ERROR: gateway '{a.gateway}' not in lock {a.lock}", file=sys.stderr)
        return 2

    run_context = {
        "captured_at": datetime.now(timezone.utc).isoformat(),
        "gateway": {
            "name": a.gateway,
            "version": entry.get("resolved_version"),
            "image_digest": entry.get("image_digest"),
            "commit": entry.get("commit"),
        },
        "nexus_commit": a.nexus_commit if a.gateway == "nexus" else None,
        "launch_type": story.get("launch_type", "fargate"),
        "cpu_vcpus": story.get("cpu_vcpus"),
        "memory_mb": story.get("memory_mb"),
        "instance_type": story.get("instance_type"),
        "disk": story.get("disk", {}),
        "loadtest_version": story.get("loadtest_version", "unknown"),
        "offered_load": story.get("offered_load", {}),
        "upstream": story.get("upstream", {}),
        "services_running": [s for s in a.services.split(",") if s.strip()],
    }

    prov = build_provenance(default_sources(), run_context)
    text = json.dumps(prov, indent=2)
    if a.out:
        Path(a.out).write_text(text + "\n", encoding="utf-8")
        print(json.dumps({"out": a.out, "gateway": a.gateway}, indent=2))
    else:
        print(text)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
