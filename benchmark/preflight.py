#!/usr/bin/env python3
"""Config parity checker.

Reads all gateway configs and prints a side-by-side table of the fields that
must match for a *fair* comparison — model, stream, cache_mode,
request_timeout, max_tokens — and WARNs when any of them diverge.

The model field is compared with provider prefixes normalised away, so
Bifrost's ``openai/gpt-4o-mini`` is treated as equal to ``gpt-4o-mini``.

Usage:
    python benchmark/preflight.py
"""
from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from harness import config as cfgmod   # noqa: E402

# (field label, attribute, normaliser)
_FIELDS = [
    ("model", lambda c: c.normalized_model),
    ("stream", lambda c: c.stream),
    ("cache_mode", lambda c: c.cache_mode),
    ("request_timeout", lambda c: c.request_timeout),
    ("max_tokens", lambda c: c.max_tokens),
]


def main(argv: list[str] | None = None) -> int:
    gateways = cfgmod.all_gateways()
    if not gateways:
        print("No gateway configs found under benchmark/configs/.")
        return 1

    configs = {}
    errors = []
    for gw in gateways:
        try:
            configs[gw] = cfgmod.load_config(gw, require_key=False)
        except Exception as e:
            errors.append(f"{gw}: {e}")

    if errors:
        print("Config load errors:")
        for e in errors:
            print(f"  ✖ {e}")
        if not configs:
            return 1

    names = list(configs.keys())

    # ---- table ----
    col_w = max(14, max(len(n) for n in names) + 2)
    label_w = max(len(label) for label, _ in _FIELDS) + 2
    header = "Field".ljust(label_w) + "".join(n.ljust(col_w) for n in names)
    print("\nConfig parity")
    print("=" * len(header))
    print(header)
    print("-" * len(header))

    warnings: list[str] = []
    for label, getter in _FIELDS:
        cells = []
        values = []
        for n in names:
            v = getter(configs[n])
            values.append(v)
            cells.append(str(v).ljust(col_w))
        print(label.ljust(label_w) + "".join(cells))
        if len(set(values)) > 1:
            pairs = ", ".join(f"{n}={getter(configs[n])}" for n in names)
            warnings.append(f"{label} differs across gateways → {pairs}")

    print("-" * len(header))

    # Also surface the raw (un-normalised) model ids and key presence for context.
    print("\nDetails:")
    for n in names:
        c = configs[n]
        key = "set" if c.api_key else f"MISSING ({c.api_key_env})"
        print(f"  {n:8s} raw_model={c.model!r}  base_url={c.base_url}  api_key={key}")

    # ---- verdict ----
    print()
    if warnings:
        print("⚠  PARITY WARNINGS (these will bias a fair comparison):")
        for w in warnings:
            print(f"   - {w}")
        print("\n   If a difference is intentional (e.g. cache_mode for a "
              "cache-feature run), you can ignore the relevant warning.")
        return 2
    print("✔  All compared fields match across gateways — safe for a fair run.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
