"""
Shared pytest fixtures for tests/e2e-python.

Loads tests/.env.test exactly once, exposes the same NEXUS_* names that the
shell helpers use, and provides:

- nexus_env: dict of every NEXUS_* env var resolved with sensible defaults.
- nexus_judge: a Judge instance bound to the configured Nexus VK.
- nexus_db: a psycopg connection for cross-checking traffic_event rows.

Tests that need raw HTTP access to the AI Gateway should construct their own
openai.OpenAI() (or httpx.Client) — keeping the auth path explicit in the
test makes regressions easier to triage.
"""

from __future__ import annotations

import os
import pathlib
from typing import Iterator

import psycopg
import pytest

from ai_judge.judge import Judge

_TESTS_ROOT = pathlib.Path(__file__).resolve().parent.parent
_ENV_FILE = _TESTS_ROOT / ".env.test"
_ENV_FILE_EXAMPLE = _TESTS_ROOT / ".env.test.example"


def _load_env_file(path: pathlib.Path) -> dict[str, str]:
    """Parse a KEY=value .env file (no quoting tricks, no shell expansion).

    Blank lines and lines starting with # are ignored. Surrounding double
    quotes are stripped so values copied verbatim from shell-style configs
    still parse cleanly.
    """
    if not path.exists():
        return {}
    out: dict[str, str] = {}
    for raw in path.read_text().splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if "=" not in line:
            continue
        key, _, value = line.partition("=")
        key = key.strip()
        value = value.strip()
        if value.startswith('"') and value.endswith('"'):
            value = value[1:-1]
        out[key] = value
    return out


@pytest.fixture(scope="session")
def nexus_env() -> dict[str, str]:
    """Resolved NEXUS_* environment.

    Real env vars take precedence over .env.test, which takes precedence over
    .env.test.example. Tests should never read os.environ for NEXUS_* keys
    directly — go through this fixture so the source-of-truth is one place.
    """
    base = _load_env_file(_ENV_FILE_EXAMPLE)
    base.update(_load_env_file(_ENV_FILE))
    for key in list(base.keys()):
        if key in os.environ:
            base[key] = os.environ[key]
    # Hard requirements — fail loudly if missing.
    required = ["NEXUS_TEST_VK", "NEXUS_AI_GW_URL", "NEXUS_JUDGE_MODEL"]
    missing = [k for k in required if not base.get(k) or base[k].startswith("nvk_REPLACE_ME")]
    if missing:
        pytest.skip(
            f"Skipping AI-judge tests — missing or placeholder values for: {missing}. "
            "Edit tests/.env.test."
        )
    base.setdefault("NEXUS_JUDGE_BASE_URL", base["NEXUS_AI_GW_URL"] + "/v1")
    base.setdefault("NEXUS_PG_CONTAINER", "nexus-postgres")
    base.setdefault("NEXUS_PG_DB", "nexus_gateway")
    base.setdefault("NEXUS_PG_USER", "postgres")
    base.setdefault("NEXUS_PG_PASSWORD", "postgres")
    base.setdefault("NEXUS_PG_HOST", "localhost")
    base.setdefault("NEXUS_PG_PORT", "55532")
    return base


@pytest.fixture(scope="session")
def nexus_judge(nexus_env: dict[str, str]) -> Judge:
    return Judge(
        base_url=nexus_env["NEXUS_JUDGE_BASE_URL"],
        api_key=nexus_env["NEXUS_TEST_VK"],
        model=nexus_env["NEXUS_JUDGE_MODEL"],
    )


@pytest.fixture()
def nexus_db(nexus_env: dict[str, str]) -> Iterator[psycopg.Connection]:
    """Open a psycopg connection to the dev Postgres on the host port.

    The dev compose maps Postgres to 55532 on the host; the existing shell
    helpers shell out to `docker exec`, but for Python we go direct to keep
    fixtures fast and cancellable.
    """
    dsn = (
        f"host={nexus_env['NEXUS_PG_HOST']} port={nexus_env['NEXUS_PG_PORT']} "
        f"user={nexus_env['NEXUS_PG_USER']} password={nexus_env['NEXUS_PG_PASSWORD']} "
        f"dbname={nexus_env['NEXUS_PG_DB']}"
    )
    conn = psycopg.connect(dsn)
    try:
        yield conn
    finally:
        conn.close()
