"""AP-3 cases 54-57 — embeddings via the official OpenAI SDK.

PROVISIONING NOTE — this module FAILS rather than skips when no embedding model
is available, and that is deliberate.

The acceptance criterion requires embeddings to pass on both SDKs. A skip here
would leave that criterion unverified while looking identical to "passed" in a
summary line — the exact failure mode that let tests/e2e-python/protocol/ sit
inert for its whole life. Every other module in this suite skips on a missing
capability, because a capability the local providers do not expose is a
provisioning gap rather than a compatibility regression; embeddings is the one
capability the ticket names explicitly, so it does not get that latitude.

Fix by enabling an embedding model (e.g. text-embedding-3-small) for the test
key's provider, not by relaxing the assertions.

The `dimensions` and `encoding_format=base64` cases are the exception to the
fail-hard stance: the gateway capability-gates both (a model must DECLARE the
dimension in `supported_dimensions`, and must declare `base64` among
`supported_encoding_formats`, or routing rejects the request — see
internal/routing/capability/filter.go). Requiring them of a model that never
advertised them would assert a provider's feature set, not the gateway's
translation, so those two select a declaring model and skip when none exists.
"""

from __future__ import annotations

import base64

import pytest

from .conftest import model_kind

pytestmark = pytest.mark.sdk_compat


def _embedding_spec(entry) -> dict:
    """The `embeddings` block of a catalog entry's capabilityJson, or {}."""
    capability = entry.get("capabilityJson") or {}
    return capability.get("embeddings") or {}


def _embedding_model(catalog) -> str:
    """The embedding model that supports the most of what this module tests.

    Prefers one declaring `supported_encoding_formats` and more than one supported
    dimension, so all four cases below exercise the SAME model and their results
    are comparable. Falling back to "first embedding model by id" would split the
    module across two providers — and pick by alphabetical accident: in a catalog
    holding both, `embed-english-v3.0` sorts before `text-embedding-3-small` while
    supporting neither an alternate dimension nor base64.

    Fails hard, unlike the rest of the suite's capability gating: embeddings is the
    one capability the acceptance criterion names explicitly, so a missing one is
    reported rather than skipped past.
    """
    candidates = [m for m in sorted(catalog) if model_kind(catalog[m]) == "embedding"]
    if not candidates:
        raise AssertionError(
            "no embedding-modality model in the catalog, so the embeddings acceptance "
            "criterion cannot be verified. Enable one (e.g. text-embedding-3-small) for "
            f"the test key's provider. Catalog: {sorted(catalog)}"
        )

    def richness(model_id: str) -> tuple[int, int]:
        spec = _embedding_spec(catalog[model_id])
        return (
            len(spec.get("supported_encoding_formats") or []),
            len(spec.get("supported_dimensions") or []),
        )

    return max(candidates, key=richness)


def _alternate_dimension(catalog, model_id: str) -> int:
    """A declared dimension for model_id that differs from its default.

    Skips when the model's only supported dimension IS its default (Cohere's v3
    family), because then `dimensions` cannot be shown to shorten anything. Reads
    the model chosen by _embedding_model rather than searching independently, so
    every case in this module exercises one model.
    """
    spec = _embedding_spec(catalog[model_id])
    default = spec.get("default_dimension")
    alternates = [
        d for d in (spec.get("supported_dimensions") or []) if isinstance(d, int) and d != default
    ]
    if not alternates:
        pytest.skip(
            f"{model_id} declares no supported dimension other than its default "
            f"({default}), so `dimensions` cannot be shown to shorten the vector"
        )
    return min(alternates)


def _model_declaring_base64(catalog) -> str:
    for model_id in sorted(catalog):
        entry = catalog[model_id]
        if model_kind(entry) != "embedding":
            continue
        formats = _embedding_spec(entry).get("supported_encoding_formats") or []
        if "base64" in [f.lower() for f in formats]:
            return model_id
    pytest.skip("no embedding model declares base64 among supported_encoding_formats")


def test_embeddings_single_input_returns_float_vector(openai_client, catalog) -> None:
    model = _embedding_model(catalog)
    resp = openai_client.embeddings.create(model=model, input="the quick brown fox")
    assert resp.object == "list", f"object={resp.object}"
    assert len(resp.data) == 1, f"one input must yield one row, got {len(resp.data)}"
    row = resp.data[0]
    assert row.index == 0
    assert isinstance(row.embedding, list) and row.embedding, "empty embedding vector"
    assert all(isinstance(value, float) for value in row.embedding), (
        f"embedding must be floats; got {[type(v) for v in row.embedding[:4]]}"
    )
    assert resp.usage.prompt_tokens > 0, f"usage not populated: {resp.usage}"


def test_embeddings_explicit_dimensions_honoured(openai_client, catalog) -> None:
    """`dimensions` must actually shorten the vector, not be ignored."""
    model = _embedding_model(catalog)
    want = _alternate_dimension(catalog, model)
    resp = openai_client.embeddings.create(
        model=model, input="dimension probe", dimensions=want
    )
    got = len(resp.data[0].embedding)
    assert got == want, f"dimensions={want} produced a vector of length {got}"


def test_embeddings_batch_input_returns_indexed_rows(openai_client, catalog) -> None:
    """A batch must return one correctly-indexed row per input, in order."""
    model = _embedding_model(catalog)
    inputs = ["first text", "second text", "third text"]
    resp = openai_client.embeddings.create(model=model, input=inputs)
    assert len(resp.data) == len(inputs), (
        f"{len(inputs)} inputs yielded {len(resp.data)} rows"
    )
    assert [row.index for row in resp.data] == [0, 1, 2], (
        f"row indices = {[row.index for row in resp.data]}, want [0, 1, 2]"
    )
    lengths = {len(row.embedding) for row in resp.data}
    assert len(lengths) == 1, f"batch rows have inconsistent vector lengths: {lengths}"


def test_embeddings_base64_encoding_format_round_trips(openai_client, catalog) -> None:
    """An EXPLICIT `encoding_format="base64"` returns base64 of the right width.

    Which encoding the caller sees depends on who asked for base64:

      - Caller omitted the field — the SDK adds `encoding_format="base64"` itself
        and post-processes the reply back into a float list, so the caller sees
        number[] and never knows base64 was involved. That path is the one the
        gateway's re-encode exists for, and it is covered by the two cases above
        (which pass no encoding_format).
      - Caller passed it explicitly, as here — the SDK returns the payload
        untouched, exactly as api.openai.com does. A str is CORRECT.

    So this asserts the width instead: decode the base64 and check it holds the
    same number of float32s as the float encoding of the same input. That is what
    catches a mis-framed payload — the AP-3 bug shipped a quarter-width buffer,
    which the implicit path silently read as a quarter-length garbage vector.
    """
    model = _model_declaring_base64(catalog)
    encoded = openai_client.embeddings.create(
        model=model, input="base64 probe", encoding_format="base64"
    )
    payload = encoded.data[0].embedding
    assert isinstance(payload, str) and payload, (
        f"an explicit encoding_format=base64 should hand back the raw payload; got "
        f"{type(payload)}"
    )
    raw = base64.b64decode(payload)
    assert len(raw) % 4 == 0, (
        f"base64 payload is {len(raw)} bytes, not a multiple of 4 — not packed float32"
    )

    plain = openai_client.embeddings.create(
        model=model, input="base64 probe", encoding_format="float"
    )
    want = len(plain.data[0].embedding)
    assert len(raw) // 4 == want, (
        f"base64 payload holds {len(raw) // 4} float32s but encoding_format=float "
        f"returned {want} components for the same input"
    )
