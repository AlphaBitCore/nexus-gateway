#!/usr/bin/env python3
"""benchmark/v3 — resolve gateway channels to exact pins and write versions.lock.

Reads config/gateways.yaml (the gateway set + release channel per gateway),
resolves each to an exact version + image digest (+ commit for Nexus), and writes
config/versions.lock. Prints the names whose pin changed vs the prior lock so a
time series can be annotated at version boundaries.

Two resolver strategies (both do real work — neither is a placeholder):
  --resolver static  (default) : read the pinned `version`/`image_digest`/`commit`
      already recorded in gateways.yaml. Offline, deterministic — the right choice
      when an operator has pre-pinned the set.
  --resolver docker            : query the registry for the digest of
      `<image>:<channel>` via `docker buildx imagetools inspect`. Needs docker +
      network; used when resolving a moving channel to today's digest.

    python scripts/resolve_versions.py
    python scripts/resolve_versions.py --resolver docker --out config/versions.lock

Exit codes: 0 ok · 2 config/usage error · 3 a gateway could not be pinned.
"""
from __future__ import annotations

import argparse
import json
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

V3_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(V3_ROOT))

from shared.version_lock.version_lock import resolve_lock  # noqa: E402


def static_resolver(spec: dict) -> dict:
    """Pins come straight from gateways.yaml — the operator pre-resolved them."""
    return {
        "resolved_version": spec.get("version"),
        "image_digest": spec.get("image_digest"),
        "commit": spec.get("commit"),
    }


def docker_resolver(spec: dict) -> dict:
    """Resolve `<image>:<channel>` to its immutable digest via docker imagetools."""
    image = spec.get("source") or spec.get("image")
    channel = spec.get("channel")
    if not image or not channel:
        raise ValueError(f"gateway '{spec.get('name')}' needs source+channel for docker resolver")
    ref = f"{image}:{channel}"
    out = subprocess.run(
        ["docker", "buildx", "imagetools", "inspect", ref, "--format", "{{.Manifest.Digest}}"],
        capture_output=True, text=True, check=True,
    )
    digest = out.stdout.strip()
    return {"resolved_version": channel, "image_digest": digest, "commit": spec.get("commit")}


RESOLVERS = {"static": static_resolver, "docker": docker_resolver}


def _load_gateways(path: Path) -> list[dict]:
    import yaml

    data = yaml.safe_load(path.read_text(encoding="utf-8")) or {}
    gws = data.get("gateways")
    if not isinstance(gws, list) or not gws:
        raise ValueError(f"{path} has no non-empty 'gateways' list")
    return gws


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="Resolve gateway versions → versions.lock")
    ap.add_argument("--gateways", default=str(V3_ROOT / "config" / "gateways.yaml"))
    ap.add_argument("--out", default=str(V3_ROOT / "config" / "versions.lock"))
    ap.add_argument("--resolver", choices=sorted(RESOLVERS), default="static")
    a = ap.parse_args(argv)

    try:
        gateways = _load_gateways(Path(a.gateways))
    except (OSError, ValueError) as e:
        print(f"ERROR: {e}", file=sys.stderr)
        return 2
    except ModuleNotFoundError:
        print("ERROR: PyYAML required — `pip install pyyaml`", file=sys.stderr)
        return 2

    out_path = Path(a.out)
    prior = None
    if out_path.exists():
        try:
            prior = json.loads(out_path.read_text(encoding="utf-8"))
        except json.JSONDecodeError:
            prior = None

    now = datetime.now(timezone.utc).isoformat()
    try:
        lock = resolve_lock(gateways, RESOLVERS[a.resolver], resolved_at=now, prior_lock=prior)
    except ValueError as e:
        print(f"ERROR: {e}", file=sys.stderr)
        return 3

    out_path.write_text(json.dumps(lock, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"out": str(out_path), "changed": lock["changed"], "count": len(lock["entries"])}, indent=2))
    if lock["changed"]:
        print(f"version pins changed: {', '.join(lock['changed'])}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
