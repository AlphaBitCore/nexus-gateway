"""AP-3 — OpenAI SDK compatibility fixtures.

This suite pins the contract that an UNMODIFIED `openai` Python SDK can target
the Nexus AI Gateway with only `base_url` and `api_key` changed. Every fixture
here exists to keep that claim honest: the client is constructed the way a real
caller would construct it, with no Nexus-specific headers, no custom transport
behaviour beyond a proxy bypass, and no request rewriting.

Deliberately self-contained. It does NOT use the `nexus_env` fixture from
tests/e2e-python/conftest.py: that fixture reads a `tests/.env.test` which does
not exist in the repo, so it `pytest.skip`s unconditionally and takes the whole
`protocol/` + `ai_judge/` layer with it. Repairing it is tracked separately; this
suite must not inherit a fixture whose failure mode is a silent green.

House rule, inherited from protocol/: assert response SHAPE and protocol
semantics, never model content. Real LLMs are non-deterministic and content
assertions flake — a test that asserts the model said "PONG" is a test that will
be deleted in six months, taking its real coverage with it.
"""

from __future__ import annotations

import os
import pathlib
import sys
from typing import Any, Iterator

import httpx
import pytest

_TESTS_ROOT = pathlib.Path(__file__).resolve().parent.parent.parent
sys.path.insert(0, str(_TESTS_ROOT / "lib"))

import loadenv  # noqa: E402  (path insert above is load-bearing)


@pytest.fixture(scope="session")
def sdk_env() -> dict[str, str]:
    """Resolved NEXUS_* environment for the SDK-compat suite.

    The target is passed explicitly because loadenv.load() refuses to default to
    "local" on a non-TTY run (so CI cannot silently point at the wrong stack).
    This suite is local-by-design, and target=local still enforces that every
    NEXUS_*_URL is loopback, so an explicit default is safe here.

    A missing NEXUS_TEST_VK FAILS rather than skips. That is the whole lesson of
    the inert protocol/ layer: a suite that skips when misconfigured reports
    green forever and nobody notices it stopped testing anything.
    """
    target = os.environ.get("NEXUS_TEST_TARGET", "local")
    loadenv.load(target)
    missing = [
        key
        for key in ("NEXUS_TEST_VK", "NEXUS_AI_GW_URL")
        if not os.environ.get(key) or os.environ[key].startswith("nvk_REPLACE_ME")
    ]
    if missing:
        raise RuntimeError(
            f"sdk_compat: missing or placeholder {missing}. "
            f"Copy tests/.env.{target}.example to tests/.env.{target} and set a real "
            f"virtual key (mint one with: cd tests/scenarios && GOWORK=off go run "
            f"-tags mintvk ../scripts/mint-test-vk.go). This is a hard failure, not a "
            f"skip — a skipped compatibility suite is indistinguishable from a passing one."
        )
    return dict(os.environ)


@pytest.fixture(scope="session")
def base_url(sdk_env: dict[str, str]) -> str:
    """The exact base_url a caller would hand the SDK."""
    return sdk_env["NEXUS_AI_GW_URL"].rstrip("/") + "/v1"


@pytest.fixture()
def openai_client(sdk_env: dict[str, str], base_url: str):
    """`openai.OpenAI` pointed at the gateway — the two-line change under test.

    trust_env=False is mandatory, not tidiness: a workstation HTTP_PROXY on
    127.0.0.1 silently rewrites localhost calls into opaque 502s that look like
    gateway failures. protocol/conftest.py carries the same note, and the AI-judge
    client learned it the same way.

    Constructed per-test rather than per-session: some SDKs hold connections past
    fixture teardown, and a shared client makes one test's stream cancellation
    another test's flake.
    """
    from openai import OpenAI

    http = httpx.Client(timeout=60.0, trust_env=False)
    client = OpenAI(
        base_url=base_url,
        api_key=sdk_env["NEXUS_TEST_VK"],
        http_client=http,
    )
    yield client
    http.close()


@pytest.fixture()
def raw_client(base_url: str, sdk_env: dict[str, str]) -> Iterator[httpx.Client]:
    """Bare httpx client for requests the SDK refuses to construct.

    A few required cases are malformed on purpose — a body with no `model`, a
    call to an unmounted path. The SDK's typed methods cannot express those, and
    reaching into its private request builder would be testing the SDK rather
    than the gateway.
    """
    with httpx.Client(
        base_url=base_url,
        timeout=60.0,
        trust_env=False,
        headers={"Authorization": "Bearer " + sdk_env["NEXUS_TEST_VK"]},
    ) as client:
        yield client


@pytest.fixture(scope="session")
def catalog(base_url: str, sdk_env: dict[str, str]) -> dict[str, dict[str, Any]]:
    """`GET /v1/models` once for the whole suite, keyed by model id.

    Session-scoped on purpose: capability gating is consulted by most tests, and
    re-fetching per test turned into 3 redundant round-trips in the module this
    replaces (protocol/test_responses_compat.py::_list_models).

    Entries carry the gateway's Nexus extension fields alongside the OpenAI ones
    — `features`, `type`, `inputModalities`/`outputModalities`,
    `maxContextTokens`, `maxOutputTokens` — which is what makes capability-based
    skipping possible without hardcoding model ids that rot on every reseed.
    """
    with httpx.Client(timeout=30.0, trust_env=False) as client:
        resp = client.get(
            base_url + "/models",
            headers={"Authorization": "Bearer " + sdk_env["NEXUS_TEST_VK"]},
        )
    if resp.status_code != 200:
        raise RuntimeError(
            f"sdk_compat: GET {base_url}/models returned {resp.status_code}: "
            f"{resp.text[:400]}. The suite cannot select models without the catalog."
        )
    rows = resp.json().get("data") or []
    if not rows:
        raise RuntimeError(
            "sdk_compat: the catalog is empty for this virtual key. Enable models "
            "for the key's organization, or mint a key without an AllowedModels "
            "restriction."
        )
    return {entry["id"]: entry for entry in rows if entry.get("id")}


def model_kind(entry: dict[str, Any]) -> str:
    """Classify a catalog entry as "chat" | "embedding" | "other".

    Precedence mirrors tests/scripts/smoke-gateway.py's classify_model_modality:
    the explicit `type` field wins, then output modalities, then the id prefix.
    Copied rather than imported — that module is ~285 KB with import-time side
    effects (cost-policy load, global recorder), and importing it into a pytest
    session to reuse three lines would be a bad trade.
    """
    declared = (entry.get("type") or "").lower()
    if declared in ("chat", "embedding"):
        return declared
    outs = [m.lower() for m in (entry.get("outputModalities") or [])]
    if "embedding" in outs:
        return "embedding"
    if "text" in outs:
        return "chat"
    if "embed" in entry["id"].lower():
        return "embedding"
    return "other"


def skip_if_provider_unreachable(exc, model_id: str) -> None:
    """Skip when the gateway could not reach ANY upstream for model_id.

    A negative test that asserts "this must be a 4xx, not a 5xx" cannot reach a
    verdict against a provider that is down or holding a bad credential: the 502
    PROVIDER_UNAVAILABLE it gets back is indistinguishable from the failure it is
    trying to rule out. Reporting that as a compatibility regression is worse than
    reporting nothing, so it skips and names the model instead.

    Deliberately narrow — only 502 + PROVIDER_UNAVAILABLE. Any other 5xx, and any
    502 carrying a different code, still fails the caller's assertion.
    """
    if getattr(exc, "status_code", None) != 502:
        return
    body = getattr(exc, "body", None)
    code = ""
    if isinstance(body, dict):
        inner = body.get("error", body) if "error" in body else body
        code = (inner or {}).get("code", "") if isinstance(inner, dict) else ""
    if code == "PROVIDER_UNAVAILABLE":
        pytest.skip(
            f"no reachable upstream for {model_id} (502 PROVIDER_UNAVAILABLE) — this "
            f"model's provider is down or its credential is invalid, so the 4xx-vs-5xx "
            f"assertion cannot be evaluated. Fix the provider credential to exercise it."
        )


def pick_model(
    catalog: dict[str, dict[str, Any]],
    *,
    feature: str | None = None,
    family: str | None = None,
    lacks: str | None = None,
    needs_field: str | None = None,
    kind: str = "chat",
) -> str:
    """Return the id of the first catalog model matching the constraints, or skip.

    Capability-gated rather than id-pinned: the dev catalog is reseeded and
    reprovisioned regularly, so a hardcoded id fails as "model not found" long
    after the capability it stood for is still available under another name.

    feature     — must appear in the entry's `features` (e.g. "vision", "tools").
    family      — id prefix match (e.g. "gpt-4o", "claude-", "gpt-5").
    lacks       — must NOT advertise this feature (for negative cases).
    needs_field — the entry must carry this non-null field (e.g. "maxOutputTokens").
    kind        — "chat" | "embedding" | "any".

    Skips rather than fails: a capability the local providers do not expose is a
    provisioning gap, not a compatibility regression. The skip message names the
    unmet constraint so a run with unexpected skips is diagnosable.
    """
    for model_id in sorted(catalog):
        entry = catalog[model_id]
        if kind != "any" and model_kind(entry) != kind:
            continue
        features = [f.lower() for f in (entry.get("features") or [])]
        if feature and feature.lower() not in features:
            continue
        if lacks and lacks.lower() in features:
            continue
        if family and not model_id.startswith(family):
            continue
        if needs_field and entry.get(needs_field) in (None, 0, ""):
            continue
        return model_id
    constraints = ", ".join(
        f"{k}={v}"
        for k, v in (
            ("kind", kind),
            ("feature", feature),
            ("family", family),
            ("lacks", lacks),
            ("needs_field", needs_field),
        )
        if v
    )
    pytest.skip(f"no catalog model satisfies {constraints}; available: {sorted(catalog)}")
