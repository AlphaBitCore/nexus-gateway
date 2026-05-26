"""
Phase 5 — OpenAI Responses-API (/v1/responses) protocol shape compatibility.

The Nexus AI Gateway exposes the OpenAI Responses-API at POST /v1/responses
(E56). These tests pin the wire-protocol contract by driving raw HTTPX —
the Responses surface on the `openai` Python SDK has evolved across minor
versions, so going through raw HTTP keeps the shape assertions stable and
mirrors how `tests/scripts/smoke-gateway.py` exercises the same ingress.

Coverage:
- /v1/responses (sync)   — `object == "response"` + `output[]` populated.
- /v1/responses (stream) — `Content-Type: text/event-stream`, `data:`
                           framing, terminal `event: response.completed`.
- /v1/responses (cross-format guard, E56-S6) — `previous_response_id` on
                           a non-Responses-native routing target returns
                           HTTP 400 with the stateless-rejection envelope.

Skip-graceful: if the test VK's catalog has no Responses-API-native model
(prefix gpt-5/gpt-4o/gpt-4.1/o1/o3/o4 per smoke-gateway.py) the first two
tests skip; if no non-native model is present, test 3 skips.
"""

from __future__ import annotations

import httpx
import pytest

pytestmark = pytest.mark.protocol

# Models empirically known to natively serve /v1/responses (verified 200
# from real OpenAI). Kept in lockstep with RESPONSES_API_MODEL_PREFIXES in
# tests/scripts/smoke-gateway.py.
_RESPONSES_NATIVE_PREFIXES = ("gpt-5", "gpt-4o", "gpt-4.1", "o1", "o3", "o4")


def _list_models(base_url: str, api_key: str) -> list[str]:
    """Return the catalog model IDs exposed to the test VK.

    Uses raw HTTPX (not the OpenAI SDK) so this helper stays independent
    of the SDK version that ships with the test environment.
    """
    with httpx.Client(timeout=15.0, trust_env=False) as client:
        r = client.get(
            f"{base_url}/v1/models",
            headers={"Authorization": f"Bearer {api_key}"},
        )
    if r.status_code != 200:
        pytest.skip(f"/v1/models returned {r.status_code}: {r.text[:200]}")
    body = r.json()
    return [m.get("id", "") for m in body.get("data", []) if m.get("id")]


def _pick_native_responses_model(models: list[str]) -> str | None:
    for m in models:
        for prefix in _RESPONSES_NATIVE_PREFIXES:
            if m.startswith(prefix):
                return m
    return None


def _pick_non_native_responses_model(models: list[str]) -> str | None:
    """Pick a model that goes through the cross-format /v1/responses path.

    Used by the E56-S6 guard test: `previous_response_id` is rejected
    iff the routing target does NOT natively serve the Responses-API.
    """
    for m in models:
        if not any(m.startswith(p) for p in _RESPONSES_NATIVE_PREFIXES):
            return m
    return None


@pytest.fixture()
def responses_http(nexus_env):
    """Plain HTTPX client with proxy bypass + bearer auth pre-set.

    Same trust_env=False fix as the SDK fixtures — a workstation
    HTTP_PROXY would otherwise silently break localhost calls.
    """
    base_url = nexus_env["NEXUS_AI_GW_URL"]
    api_key = nexus_env["NEXUS_TEST_VK"]
    with httpx.Client(
        base_url=base_url,
        timeout=30.0,
        trust_env=False,
        headers={"Authorization": f"Bearer {api_key}"},
    ) as client:
        yield client, base_url, api_key


def test_responses_sync_shape(responses_http):
    """POST /v1/responses non-stream returns the Responses envelope:
    `object == "response"` with at least one `output[]` item."""
    client, base_url, api_key = responses_http
    models = _list_models(base_url, api_key)
    model = _pick_native_responses_model(models)
    if not model:
        pytest.skip("Responses-API model not available in test env")

    r = client.post(
        "/v1/responses",
        json={
            "model": model,
            "input": "Reply with exactly: PONG",
            "max_output_tokens": 16,
            "temperature": 0,
        },
    )
    assert r.status_code == 200, f"non-200: {r.status_code} {r.text[:300]}"
    body = r.json()
    assert body.get("object") == "response", (
        f"unexpected object={body.get('object')!r}; body={body!r}"
    )
    output = body.get("output")
    assert isinstance(output, list) and output, (
        f"output must be a non-empty array; got {output!r}"
    )


def test_responses_stream_shape(responses_http):
    """POST /v1/responses with stream=true must respond with
    `Content-Type: text/event-stream`, `data:` framing, and a terminal
    `event: response.completed` line."""
    client, base_url, api_key = responses_http
    models = _list_models(base_url, api_key)
    model = _pick_native_responses_model(models)
    if not model:
        pytest.skip("Responses-API model not available in test env")

    with client.stream(
        "POST",
        "/v1/responses",
        json={
            "model": model,
            "input": "Count 1 to 3, separated by spaces.",
            "max_output_tokens": 32,
            "temperature": 0,
            "stream": True,
        },
    ) as r:
        assert r.status_code == 200, f"non-200: {r.status_code}"
        ctype = r.headers.get("content-type", "")
        assert "text/event-stream" in ctype, (
            f"Content-Type must be text/event-stream; got {ctype!r}"
        )
        saw_data_frame = False
        saw_completed_event = False
        for raw in r.iter_lines():
            # iter_lines() yields strings already stripped of the trailing
            # \n; SSE framing keeps the leading `data: ` / `event: ` prefix.
            if raw.startswith("data: "):
                saw_data_frame = True
            if raw.startswith("event: ") and raw.split(":", 1)[1].strip() == "response.completed":
                saw_completed_event = True
        assert saw_data_frame, "stream produced no `data:` SSE frames"
        assert saw_completed_event, (
            "terminal `event: response.completed` not seen in stream"
        )


def test_responses_rejects_previous_response_id_on_cross_format(responses_http):
    """E56-S6 stateless guard: `previous_response_id` cannot be served on
    a routing target that does not natively support the Responses-API.
    The gateway must return HTTP 400 with the structured rejection
    envelope (error.param == "previous_response_id")."""
    client, base_url, api_key = responses_http
    models = _list_models(base_url, api_key)
    model = _pick_non_native_responses_model(models)
    if not model:
        pytest.skip(
            "No non-OpenAI-native model in catalog to exercise "
            "cross-format Responses guard"
        )

    r = client.post(
        "/v1/responses",
        json={
            "model": model,
            "input": "Should be rejected before reaching the upstream.",
            "max_output_tokens": 8,
            "temperature": 0,
            # The stateless guard fires on any non-empty previous_response_id
            # when the resolved target's adapter does not natively serve
            # the Responses-API (see cross_format.go
            # validateResponsesIngressForCrossFormat).
            "previous_response_id": "resp_does_not_exist_synthetic",
        },
    )
    assert r.status_code == 400, (
        f"expected 400 cross-format rejection; got {r.status_code} {r.text[:300]}"
    )
    body = r.json()
    err = body.get("error") or {}
    assert err.get("param") == "previous_response_id", (
        f"error.param should pinpoint previous_response_id; got {err!r}"
    )
    # Pin the structured rejection code so the contract surface stays
    # stable across refactors (matches writeResponsesFeatureRejection in
    # packages/ai-gateway/internal/ingress/proxy/cross_format.go).
    assert err.get("code") == "feature_requires_native_responses_target", (
        f"error.code should be feature_requires_native_responses_target; got {err!r}"
    )
