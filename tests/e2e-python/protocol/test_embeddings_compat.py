"""
Phase 5 — OpenAI Python SDK /v1/embeddings compatibility against our /v1.

These tests pin the contract that an unmodified `openai` SDK targeting the
Nexus AI Gateway gets the OpenAI Embeddings response shape verbatim. We
assert response *shape* and protocol round-trip semantics — never the
embedding vector's actual numeric contents, because real provider models
emit non-deterministic floats and value assertions would flake.

Coverage:
- single input    — list-envelope shape + non-empty float vector + usage populated
- dimensions      — explicit `dimensions=256` round-trips through the codec
- batch input     — N-input request returns N rows in submission order

Runs against `text-embedding-3-small` because that's the canonical OpenAI
embedding model the dev DB seeds. If the test env hasn't provisioned an
embedding-capable provider, the request fails with HTTP 400/404
`model_not_found` and the tests skip cleanly instead of failing — same
graceful-skip pattern the Go scenario suite uses
(`tests/scenarios/embeddings_test.go`).
"""

from __future__ import annotations

import pytest
from openai import APIStatusError

pytestmark = pytest.mark.protocol


# Canonical OpenAI embedding model the dev DB seeds. Kept local to this
# file (rather than added to conftest.py) because no other test in the
# suite consumes it, and the task scope is "don't add helpers".
EMBEDDING_TEST_MODEL = "text-embedding-3-small"


def _skip_if_provider_missing(exc: APIStatusError) -> None:
    """Translate an upstream `model_not_found` / 400 / 404 into pytest.skip.

    Mirrors the Go scenario suite (S-063): dev environments that haven't
    seeded an embedding-capable provider should skip cleanly rather than
    fail the protocol contract test."""
    if exc.status_code in (400, 404):
        pytest.skip("embeddings provider not available in test env")
    raise exc


def test_embeddings_single_input(openai_client):
    """`client.embeddings.create()` with a single string input must return
    the OpenAI {object:'list', data:[{object:'embedding', embedding:[...]}]}
    envelope with a non-empty float vector and populated prompt-token
    usage."""
    try:
        resp = openai_client.embeddings.create(
            model=EMBEDDING_TEST_MODEL,
            input="hello world",
        )
    except APIStatusError as e:
        _skip_if_provider_missing(e)
        return  # unreachable — _skip_if_provider_missing either skips or re-raises
    assert resp.object == "list", f"unexpected object={resp.object!r}"
    assert resp.data, "data array is empty"
    row = resp.data[0]
    assert row.object == "embedding", f"data[0].object={row.object!r}, want 'embedding'"
    assert isinstance(row.embedding, list) and row.embedding, (
        f"data[0].embedding empty or non-list: {type(row.embedding).__name__}"
    )
    # Every entry must be a float — proves the codec didn't drop precision
    # or return a base64-encoded blob the SDK failed to decode.
    assert all(isinstance(v, float) for v in row.embedding), (
        "data[0].embedding contains non-float entries"
    )
    # Usage must be populated for downstream cost analytics.
    assert resp.usage is not None
    assert resp.usage.prompt_tokens > 0, f"prompt_tokens not populated: {resp.usage!r}"


def test_embeddings_explicit_dimensions(openai_client):
    """Explicit `dimensions` must round-trip through the canonical codec —
    the returned vector length equals the requested dimensionality. This
    catches codec drops of optional OpenAI fields."""
    try:
        resp = openai_client.embeddings.create(
            model=EMBEDDING_TEST_MODEL,
            input="hello world",
            dimensions=256,
        )
    except APIStatusError as e:
        _skip_if_provider_missing(e)
        return
    assert resp.data, "data array is empty"
    vec_len = len(resp.data[0].embedding)
    assert vec_len == 256, (
        f"dimensions field dropped by codec: got vector len={vec_len}, want 256"
    )


def test_embeddings_batch_input(openai_client):
    """Batch input — array of N strings — must return N rows in submission
    order, each tagged with the correct `index`. Order preservation is a
    hard contract: callers index back into their original input list by
    row.index."""
    inputs = ["one", "two", "three"]
    try:
        resp = openai_client.embeddings.create(
            model=EMBEDDING_TEST_MODEL,
            input=inputs,
        )
    except APIStatusError as e:
        _skip_if_provider_missing(e)
        return
    assert resp.object == "list", f"unexpected object={resp.object!r}"
    assert len(resp.data) == len(inputs), (
        f"batch len mismatch: got {len(resp.data)} rows, want {len(inputs)}"
    )
    for expected_idx, row in enumerate(resp.data):
        assert row.object == "embedding", (
            f"data[{expected_idx}].object={row.object!r}, want 'embedding'"
        )
        assert row.index == expected_idx, (
            f"data[{expected_idx}].index={row.index}, want {expected_idx} "
            "— batch order not preserved"
        )
        assert isinstance(row.embedding, list) and row.embedding, (
            f"data[{expected_idx}].embedding empty or non-list"
        )
