#!/usr/bin/env python3
"""benchmark/v3 — mandatory run validator (CLI).

The single, non-optional gate in the v3 pipeline:
    run → capture provenance → validate_run.py → publish (valid) | quarantine (invalid)

It (1) schema-checks the run against shared/schemas/run.schema.json, (2) applies
the gateway-agnostic rules in shared/validation/validator.py, and (3) on invalid,
optionally moves the artifact into a quarantine dir WITH its reasons (raw file
retained, never deleted).

    python scripts/validate_run.py <run.json>
    python scripts/validate_run.py <run.json> --config config/validation.yaml \
        --quarantine story_a/results/quarantine

Exit codes: 0 valid · 1 invalid (or quarantined) · 2 malformed input / usage.
Machine verdict → stdout as JSON; human summary → stderr.
"""
from __future__ import annotations

import argparse
import json
import shutil
import sys
from pathlib import Path

V3_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(V3_ROOT))

from shared.validation.validator import (  # noqa: E402
    VALIDATOR_VERSION,
    classify,
)

SCHEMA_PATH = V3_ROOT / "shared" / "schemas" / "run.schema.json"


def _load_json(path: Path) -> object:
    with open(path, encoding="utf-8") as f:
        return json.load(f)


def _load_config(path: Path | None) -> dict | None:
    if not path:
        return None
    text = Path(path).read_text(encoding="utf-8")
    try:
        import yaml  # PyYAML

        return yaml.safe_load(text) or {}
    except ModuleNotFoundError:
        # config is YAML; without PyYAML we fall back to defaults rather than crash.
        print("WARN: PyYAML not installed — using default validation thresholds", file=sys.stderr)
        return None


def _schema_errors(run: object) -> list[str]:
    """Return schema violation messages (empty if valid). Requires jsonschema."""
    try:
        import jsonschema
    except ModuleNotFoundError:
        print(
            "ERROR: jsonschema is required — `pip install jsonschema` "
            "(see benchmark/v3/requirements.txt)",
            file=sys.stderr,
        )
        sys.exit(2)
    schema = _load_json(SCHEMA_PATH)
    validator = jsonschema.Draft7Validator(schema)
    return [
        f"schema: {'/'.join(str(p) for p in e.absolute_path) or '<root>'}: {e.message}"
        for e in sorted(validator.iter_errors(run), key=lambda e: list(e.absolute_path))
    ]


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="Validate a v3 benchmark run.")
    ap.add_argument("run", help="path to the run JSON")
    ap.add_argument("--config", help="validation.yaml with threshold overrides", default=None)
    ap.add_argument(
        "--quarantine",
        default=None,
        help="dir to move an INVALID run into (with a .reasons.json sidecar); raw file retained",
    )
    a = ap.parse_args(argv)

    run_path = Path(a.run)
    try:
        run = _load_json(run_path)
    except (OSError, json.JSONDecodeError) as e:
        print(f"ERROR: cannot read/parse {run_path}: {e}", file=sys.stderr)
        return 2
    if not isinstance(run, dict):
        print(f"ERROR: {run_path} is not a JSON object", file=sys.stderr)
        return 2

    config = _load_config(a.config)

    reasons = _schema_errors(run)
    # Only run the semantic rules once the shape is valid enough to trust the keys.
    if not reasons:
        _, rule_reasons = classify(run, config)
        reasons = rule_reasons

    status = "valid" if not reasons else "invalid"
    verdict = {
        "run_id": run.get("run_id"),
        "gateway": (run.get("gateway") or {}).get("name"),
        "story": run.get("story"),
        "status": status,
        "reasons": reasons,
        "validator_version": VALIDATOR_VERSION,
    }

    if status == "invalid" and a.quarantine:
        qdir = Path(a.quarantine)
        qdir.mkdir(parents=True, exist_ok=True)
        dest = qdir / run_path.name
        shutil.move(str(run_path), str(dest))  # retained, not deleted
        (qdir / f"{run_path.stem}.reasons.json").write_text(
            json.dumps({**verdict, "status": "quarantined"}, indent=2), encoding="utf-8"
        )
        verdict["status"] = "quarantined"
        verdict["quarantined_to"] = str(dest)

    print(json.dumps(verdict, indent=2))
    if status == "valid":
        print(f"VALID: {verdict['gateway']} / story {verdict['story']}", file=sys.stderr)
    else:
        print(f"INVALID ({len(reasons)} reason(s)):", file=sys.stderr)
        for r in reasons:
            print(f"  - {r}", file=sys.stderr)
    return 0 if status == "valid" else 1


if __name__ == "__main__":
    raise SystemExit(main())
