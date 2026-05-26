"""tests/lib/loadenv.py — target-aware env-file loader for Python test scripts.

Mirrors the semantics of tests/lib/loadenv.sh exactly so the Python smoke
script and the bash scenarios harness pull from the same .env files with
the same precedence + safety rules. Zero third-party dependencies — the
hand-rolled parser is ~30 lines and avoids forcing `pip install` on the
test-runner workflow.

Public API:
    target = load(target=None, *, allow_default_local=True) -> str
        Loads tests/.env.<target>.example then tests/.env.<target> into
        os.environ, returns the resolved target ("local"|"dev"|"prod").

    repo_tests_root() -> pathlib.Path
        Find the tests/ directory anchored at the repo root (walks up from
        cwd looking for the .env.local.example marker).

Semantics (must match loadenv.sh):
  - Selection: explicit `target` arg > NEXUS_TEST_TARGET env > "local" (TTY).
    Non-TTY runs with no target set raise RuntimeError so CI fails fast.
  - Non-overload: variables already in os.environ are NOT overwritten by
    file values. `MY_VAR=x python3 smoke.py` still wins.
  - Safety: target=local requires every NEXUS_*_URL to be loopback;
    target=prod requires NEXUS_CP_URL NOT to be loopback. Either mismatch
    raises RuntimeError.
"""
from __future__ import annotations

import os
import sys
from pathlib import Path
from typing import Optional

_VALID_TARGETS = ("local", "dev", "prod")

_URL_VARS = (
    "NEXUS_HUB_URL", "NEXUS_CP_URL", "NEXUS_AI_GW_URL",
    "NEXUS_PROXY_URL", "NEXUS_UI_URL",
)


def repo_tests_root() -> Path:
    """Walk up from cwd looking for tests/.env.local.example (the canonical
    layout marker). Raise FileNotFoundError if not found inside any ancestor."""
    cwd = Path.cwd().resolve()
    for d in (cwd, *cwd.parents):
        marker = d / "tests" / ".env.local.example"
        if marker.exists():
            return d / "tests"
        # Also accept being run from inside tests/ itself.
        marker2 = d / ".env.local.example"
        if marker2.exists():
            return d
    raise FileNotFoundError(
        "tests/.env.local.example not found in any ancestor of cwd; "
        "are you running inside the repo?"
    )


def _parse_env_file(path: Path) -> dict[str, str]:
    """Tiny dotenv parser — KEY=VALUE per line, # comments, optional
    'export ' prefix, optional surrounding ' or " on the value."""
    out: dict[str, str] = {}
    if not path.exists():
        return out
    with path.open("r", encoding="utf-8") as f:
        for raw in f:
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            if line.startswith("export "):
                line = line[len("export "):]
            if "=" not in line:
                continue
            key, _, value = line.partition("=")
            key = key.strip()
            value = value.strip()
            if len(value) >= 2 and value[0] == value[-1] and value[0] in ("'", '"'):
                value = value[1:-1]
            out[key] = value
    return out


def _resolve_target(target: Optional[str], *, allow_default_local: bool) -> str:
    if target:
        chosen = target
    elif os.environ.get("NEXUS_TEST_TARGET"):
        chosen = os.environ["NEXUS_TEST_TARGET"]
    elif allow_default_local and sys.stdout.isatty():
        chosen = "local"
    else:
        raise RuntimeError(
            "loadenv.py: refusing to default to 'local' for non-TTY run. "
            "Set NEXUS_TEST_TARGET=local|dev|prod explicitly, or pass "
            "target=... to load()."
        )
    if chosen not in _VALID_TARGETS:
        raise RuntimeError(
            f"loadenv.py: unknown target {chosen!r} (allowed: {'|'.join(_VALID_TARGETS)})"
        )
    return chosen


def _enforce_safety(target: str) -> None:
    if target == "local":
        for var in _URL_VARS:
            val = os.environ.get(var, "")
            if val and "localhost" not in val and "127.0.0.1" not in val:
                raise RuntimeError(
                    f"loadenv.py: target=local but {var}={val!r} does not "
                    f"reference localhost. Fix .env.local or set "
                    f"NEXUS_TEST_TARGET to the correct target."
                )
    elif target == "prod":
        cp = os.environ.get("NEXUS_CP_URL", "")
        if not cp or "localhost" in cp or "127.0.0.1" in cp:
            raise RuntimeError(
                f"loadenv.py: target=prod but NEXUS_CP_URL={cp!r} is loopback. "
                f"Fix tests/.env.prod to point at the real production hostname."
            )


def load(target: Optional[str] = None, *, allow_default_local: bool = True) -> str:
    """Load tests/.env.<target>.example then tests/.env.<target> into
    os.environ, honouring non-overload semantics + safety guards. Returns
    the resolved target string."""
    chosen = _resolve_target(target, allow_default_local=allow_default_local)
    tests_root = repo_tests_root()
    example = tests_root / f".env.{chosen}.example"
    user = tests_root / f".env.{chosen}"
    if not example.exists() and not user.exists():
        raise FileNotFoundError(
            f"loadenv.py: neither {example} nor {user} exists. "
            f"Copy .env.{chosen}.example to .env.{chosen} and fill in values."
        )

    # Snapshot pre-existing NEXUS_* / NEXUS_TEST_TARGET keys so we don't
    # overwrite them with file values (godotenv-style non-overload).
    preexisting = {k for k in os.environ if k.startswith("NEXUS_")}

    # Apply target so it's visible to anything looking at os.environ.
    os.environ["NEXUS_TEST_TARGET"] = chosen
    preexisting.add("NEXUS_TEST_TARGET")

    # Example provides defaults; user file overrides (but neither overrides
    # a value that was already in the process env).
    for path in (example, user):
        parsed = _parse_env_file(path)
        for k, v in parsed.items():
            if k in preexisting:
                continue
            os.environ[k] = v

    os.environ["NEXUS_TEST_ROOT"] = str(tests_root)
    os.makedirs(os.environ.get("NEXUS_TEST_LOG_DIR", "/tmp/nexus-test"), exist_ok=True)
    _enforce_safety(chosen)
    return chosen
