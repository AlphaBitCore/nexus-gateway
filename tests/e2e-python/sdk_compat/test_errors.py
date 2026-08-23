"""AP-3 cases 41-53 — error scenarios via the official OpenAI SDK.

Covers the acceptance criterion's three named failures (invalid model, overly
long input, invalid tool schema) plus the auth and unsupported-endpoint paths.

Two rules apply throughout:

1. The SDK selects its exception class from the HTTP STATUS alone
   (openai-python `_client._make_status_error`), so the status assertions are
   what prove SDK compatibility. `type` and `param` are what a consumer reads
   after the catch.
2. `error.code` stays Nexus UPPER_SNAKE and is NOT translated to OpenAI's code
   strings. It is load-bearing for the Control Plane UI and the Hub alert
   aggregators. Case 51 pins that deliberately, so a future "make it more
   OpenAI-like" change has to break a test that explains why not.
"""

from __future__ import annotations

import re

import httpx
import openai as openai_pkg
import pytest

from .conftest import pick_model

pytestmark = pytest.mark.sdk_compat

BOGUS_MODEL = "nexus-definitely-not-a-real-model"

UNSUPPORTED_PATHS = ("/completions", "/moderations", "/images/edits", "/images/variations")


def _error_body(exc: openai_pkg.APIStatusError) -> dict:
    """The inner `error` object, however the SDK happened to unwrap it.

    openai-python's `_make_status_error` already unwraps `{"error": {...}}` to
    the inner object before constructing the exception, so `exc.body` is normally
    the error itself. Accept both shapes rather than depending on that detail.
    """
    body = exc.body
    if isinstance(body, dict):
        return body.get("error", body) if "error" in body else body
    return {}


def test_unknown_model_is_404(openai_client) -> None:
    with pytest.raises(openai_pkg.NotFoundError) as exc:
        openai_client.chat.completions.create(
            model=BOGUS_MODEL,
            messages=[{"role": "user", "content": "hi"}],
            max_tokens=8,
        )
    assert exc.value.status_code == 404
    body = _error_body(exc.value)
    assert body.get("code") == "ROUTING_NO_MATCH", f"error.code={body.get('code')}"
    assert body.get("type") == "not_found_error", f"error.type={body.get('type')}"
    assert body.get("param") == "model", (
        f"an unroutable model must name the offending field; error.param={body.get('param')}"
    )


def test_missing_model_field_is_400_naming_model(raw_client) -> None:
    """Sent raw: the SDK cannot construct a body without `model`."""
    resp = raw_client.post(
        "/chat/completions", json={"messages": [{"role": "user", "content": "hi"}]}
    )
    assert resp.status_code == 400, f"status={resp.status_code} body={resp.text[:300]}"
    error = resp.json().get("error") or {}
    assert error.get("code") == "MODEL_REQUIRED", f"error.code={error.get('code')}"
    assert error.get("type") == "invalid_request_error", f"error.type={error.get('type')}"
    assert error.get("param") == "model", f"error.param={error.get('param')}"


def test_invalid_virtual_key_is_401(base_url) -> None:
    with httpx.Client(timeout=30.0, trust_env=False) as http:
        client = openai_pkg.OpenAI(
            base_url=base_url,
            api_key="nvk_bogus_key_that_will_never_exist",
            http_client=http,
        )
        with pytest.raises(openai_pkg.AuthenticationError) as exc:
            client.chat.completions.create(
                model=BOGUS_MODEL,
                messages=[{"role": "user", "content": "hi"}],
                max_tokens=8,
            )
    assert exc.value.status_code == 401
    body = _error_body(exc.value)
    assert body.get("code") == "AUTH_INVALID_KEY", f"error.code={body.get('code')}"
    assert body.get("type") == "authentication_error", f"error.type={body.get('type')}"


def test_missing_authorization_header_is_401_json(base_url) -> None:
    with httpx.Client(timeout=30.0, trust_env=False) as http:
        resp = http.post(
            base_url + "/chat/completions",
            json={"model": BOGUS_MODEL, "messages": [{"role": "user", "content": "hi"}]},
        )
    assert resp.status_code == 401, f"status={resp.status_code}"
    error = resp.json().get("error") or {}
    assert error.get("type") == "authentication_error", f"error.type={error.get('type')}"
    assert error.get("message"), "a 401 must carry a message the SDK can surface"


@pytest.mark.slow
@pytest.mark.timeout(300)
def test_context_overflow_is_client_error_not_5xx(openai_client, catalog) -> None:
    """Overly long input must be a 4xx, never a 5xx.

    Marked slow: the smallest context window in the local catalog is large, so
    this uploads a multi-hundred-KB body. Deselect with -m 'not slow'.

    No pre-flight length guard exists in the gateway, so the rejection comes from
    the real upstream — which is the point: whatever the provider says must reach
    the caller as a client error rather than being reflected as a gateway fault.
    """
    model = pick_model(catalog, family="gpt-4o", needs_field="maxContextTokens")
    # "word " is ~1 token, so one filler word per context token overshoots the window.
    filler = "word " * catalog[model]["maxContextTokens"]
    with pytest.raises(openai_pkg.APIStatusError) as excinfo:
        openai_client.chat.completions.create(
            model=model,
            messages=[{"role": "user", "content": filler}],
            max_tokens=16,
        )
    status = excinfo.value.status_code
    assert status < 500, (
        f"context overflow surfaced as {status}; a caller's oversized input is not our fault"
    )
    assert str(excinfo.value), "the overflow error must carry a message"


def test_invalid_tool_parameters_schema_is_400(openai_client, catalog) -> None:
    model = pick_model(catalog, feature="function_calling", family="gpt-4o")
    with pytest.raises(openai_pkg.BadRequestError) as excinfo:
        openai_client.chat.completions.create(
            model=model,
            messages=[{"role": "user", "content": "hi"}],
            tools=[
                {
                    "type": "function",
                    "function": {
                        "name": "broken",
                        "parameters": {"type": "not-a-json-schema-type"},
                    },
                }
            ],
            max_tokens=16,
        )
    assert excinfo.value.status_code == 400
    assert str(excinfo.value), "an invalid tool schema must explain itself"


def test_tool_with_empty_function_name_is_400(openai_client, catalog) -> None:
    model = pick_model(catalog, feature="function_calling", family="gpt-4o")
    with pytest.raises(openai_pkg.BadRequestError) as excinfo:
        openai_client.chat.completions.create(
            model=model,
            messages=[{"role": "user", "content": "hi"}],
            tools=[
                {
                    "type": "function",
                    "function": {"name": "", "parameters": {"type": "object"}},
                }
            ],
            max_tokens=16,
        )
    assert excinfo.value.status_code == 400


@pytest.mark.parametrize("path", UNSUPPORTED_PATHS)
def test_unsupported_endpoints_return_json_envelope(raw_client, path) -> None:
    """Endpoints the gateway declines must answer in JSON, not plain text.

    Go's default ServeMux 404 body is `404 page not found` as text/plain, and the
    SDKs JSON-parse every error body — so before AP-3 these produced an
    APIStatusError carrying no message at all.
    """
    resp = raw_client.post(path, json={})
    assert resp.status_code == 404, f"{path}: status={resp.status_code}"
    content_type = resp.headers.get("content-type", "")
    assert content_type.startswith("application/json"), (
        f"{path}: content-type={content_type} — the SDK cannot parse this"
    )
    error = resp.json().get("error") or {}
    assert error.get("code") == "ENDPOINT_NOT_SUPPORTED", (
        f"{path}: error.code={error.get('code')}"
    )
    assert error.get("type") == "not_found_error", f"{path}: error.type={error.get('type')}"
    assert path in error.get("message", ""), (
        f"{path}: message must name the path; got {error.get('message')}"
    )


def test_unknown_model_detail_is_404(raw_client) -> None:
    resp = raw_client.get("/models/" + BOGUS_MODEL)
    assert resp.status_code == 404, f"status={resp.status_code} body={resp.text[:200]}"


def test_error_code_stays_nexus_upper_snake(openai_client, base_url) -> None:
    """DELIBERATE DIVERGENCE: error.code is a Nexus code, not an OpenAI code.

    OpenAI would return `model_not_found` / `invalid_api_key`. We return
    ROUTING_NO_MATCH / AUTH_INVALID_KEY, because those codes are matched by the
    Control Plane UI, the Hub alert aggregators, and the documented 429
    discriminator. OpenAI's `error.code` is a free-form string, so keeping ours
    costs SDK callers nothing.

    This test exists so that "make the codes more OpenAI-like" cannot land
    quietly — it has to break here first, and read this comment.
    """
    codes = []

    with pytest.raises(openai_pkg.NotFoundError) as exc:
        openai_client.chat.completions.create(
            model=BOGUS_MODEL,
            messages=[{"role": "user", "content": "hi"}],
            max_tokens=8,
        )
    codes.append(_error_body(exc.value).get("code"))

    with httpx.Client(timeout=30.0, trust_env=False) as http:
        client = openai_pkg.OpenAI(
            base_url=base_url,
            api_key="nvk_bogus_key_that_will_never_exist",
            http_client=http,
        )
        with pytest.raises(openai_pkg.AuthenticationError) as auth_exc:
            client.chat.completions.create(
                model=BOGUS_MODEL,
                messages=[{"role": "user", "content": "hi"}],
                max_tokens=8,
            )
    codes.append(_error_body(auth_exc.value).get("code"))

    for code in codes:
        assert isinstance(code, str) and re.fullmatch(r"[A-Z][A-Z0-9_]+", code), (
            f"error.code={code} is not a Nexus UPPER_SNAKE code — see the docstring "
            f"before changing this"
        )


def test_negative_max_tokens_is_client_error(openai_client, catalog) -> None:
    model = pick_model(catalog, family="gpt-4o")
    with pytest.raises(openai_pkg.APIStatusError) as excinfo:
        openai_client.chat.completions.create(
            model=model,
            messages=[{"role": "user", "content": "hi"}],
            max_tokens=-1,
        )
    assert excinfo.value.status_code < 500, (
        f"a negative max_tokens is a caller error; got {excinfo.value.status_code}"
    )


def test_model_outside_key_allowlist_is_403(base_url, sdk_env, catalog) -> None:
    """A key scoped away from a model must get 403 MODEL_NOT_ALLOWED, not 404.

    Needs a second, allowlist-restricted key; skipped when one is not provisioned.
    """
    restricted = sdk_env.get("NEXUS_TEST_VK_RESTRICTED")
    if not restricted:
        pytest.skip("NEXUS_TEST_VK_RESTRICTED not set — cannot exercise the allowlist path")

    with httpx.Client(timeout=30.0, trust_env=False) as http:
        client = openai_pkg.OpenAI(base_url=base_url, api_key=restricted, http_client=http)
        with pytest.raises(openai_pkg.PermissionDeniedError) as excinfo:
            client.chat.completions.create(
                model=sorted(catalog)[0],
                messages=[{"role": "user", "content": "hi"}],
                max_tokens=8,
            )
    assert excinfo.value.status_code == 403
    body = _error_body(excinfo.value)
    assert body.get("code") == "MODEL_NOT_ALLOWED", f"error.code={body.get('code')}"
    assert body.get("type") == "permission_error", f"error.type={body.get('type')}"
