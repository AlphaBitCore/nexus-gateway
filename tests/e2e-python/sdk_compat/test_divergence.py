"""AP-3 cases 11-17 — pins for the gateway's DELIBERATE divergences.

Every case here asserts behaviour that differs from stock OpenAI on purpose. They
are not bug reports; they are the tripwires that make a silent change to a known
divergence fail loudly, and they are the evidence behind the divergence rows of
the compatibility matrix in
docs/developers/architecture/services/ai-gateway/ingress-api.md.

If one of these starts failing, the question is not "how do I make the test
pass" — it is "did someone change a documented contract on purpose?"
"""

from __future__ import annotations

import pytest

from .conftest import pick_model

pytestmark = pytest.mark.sdk_compat

COERCED_HEADER = "x-nexus-coerced"


def test_stream_usage_injected_without_opt_in(openai_client, catalog) -> None:
    """DIVERGENCE: usage arrives on a stream the caller never opted into.

    The adapter force-sets `stream_options.include_usage` upstream
    (ensureStreamUsage, specs/openai/codec/rewrite_native.go) so the gateway can
    always meter cost, and nothing strips it on the way back out. Stock OpenAI
    would send no usage at all here.

    Both SDKs tolerate it — the extra frame surfaces as a chunk with empty
    choices — but a consumer asserting OpenAI's exact frame sequence will see one
    more frame than upstream would send.
    """
    model = pick_model(catalog, family="gpt-4o")
    stream = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "Reply with one short word."}],
        max_tokens=16,
        stream=True,
    )
    assert any(chunk.usage for chunk in stream), (
        "expected the gateway's injected usage chunk even without include_usage; "
        "if this now fails, the injection was removed — update the matrix"
    )


def test_max_tokens_clamped_to_model_ceiling_and_disclosed(openai_client, catalog) -> None:
    """DIVERGENCE: max_tokens above the model ceiling is lowered, not rejected.

    Stock OpenAI 400s. The gateway clamps to the model's maxOutputTokens and
    discloses it in X-Nexus-Coerced rather than failing the call.

    Asserted on a gpt-4o-family model specifically: gpt-5* also *renames*
    max_tokens to max_completion_tokens, and the two labels would be
    indistinguishable if this ran there. Case 15 covers the rename.
    """
    model = pick_model(catalog, family="gpt-4o", needs_field="maxOutputTokens")
    ceiling = catalog[model]["maxOutputTokens"]
    raw = openai_client.chat.completions.with_raw_response.create(
        model=model,
        messages=[{"role": "user", "content": "Reply with one short word."}],
        max_tokens=50000,
    )
    coerced = raw.headers.get(COERCED_HEADER, "")
    assert "_model_max" in coerced, (
        f"expected a '_model_max' clamp note in {COERCED_HEADER}; got {coerced!r}"
    )
    resp = raw.parse()
    assert resp.usage.completion_tokens <= ceiling, (
        f"completion_tokens={resp.usage.completion_tokens} exceeds the {ceiling} ceiling"
    )


def test_temperature_stripped_on_gpt5_and_disclosed(openai_client, catalog) -> None:
    """DIVERGENCE: gpt-5 rejects sampling params upstream, so the gateway strips them.

    Sending temperature to gpt-5.5 upstream is a 400. The gateway drops the field
    and reports the drop rather than surfacing the vendor error.
    """
    model = pick_model(catalog, family="gpt-5.5")
    raw = openai_client.chat.completions.with_raw_response.create(
        model=model,
        messages=[{"role": "user", "content": "Reply with one short word."}],
        max_tokens=16,
        temperature=0,
    )
    assert raw.status_code == 200
    coerced = raw.headers.get(COERCED_HEADER, "")
    assert "temperature" in coerced, f"expected a temperature strip note; got {coerced!r}"


def test_temperature_preserved_on_gpt54_carveout(openai_client, catalog) -> None:
    """DIVERGENCE-OF-A-DIVERGENCE: gpt-5.4 accepts temperature, so it is NOT stripped.

    A probed per-model carve-out in specs/openai/rewrites/rewrites.go. Pinned
    because the cheap fix for case 13 — strip on every `gpt-5*` prefix — would
    silently start degrading gpt-5.4 requests, and nothing else would notice.
    """
    model = pick_model(catalog, family="gpt-5.4")
    raw = openai_client.chat.completions.with_raw_response.create(
        model=model,
        messages=[{"role": "user", "content": "Reply with one short word."}],
        max_tokens=16,
        temperature=0,
    )
    assert raw.status_code == 200
    coerced = raw.headers.get(COERCED_HEADER, "")
    assert "temperature" not in coerced, (
        f"gpt-5.4 is a probed carve-out and must keep temperature; {coerced!r}"
    )


def test_max_tokens_renamed_on_gpt5_and_disclosed(openai_client, catalog) -> None:
    """DIVERGENCE: gpt-5 takes max_completion_tokens, so max_tokens is renamed.

    Deliberately well under the ceiling so the note can only be the rename, never
    the clamp from case 12.
    """
    model = pick_model(catalog, family="gpt-5")
    raw = openai_client.chat.completions.with_raw_response.create(
        model=model,
        messages=[{"role": "user", "content": "Reply with one short word."}],
        max_tokens=16,
    )
    assert raw.status_code == 200
    coerced = raw.headers.get(COERCED_HEADER, "")
    assert "max_completion_tokens" in coerced, (
        f"expected the max_tokens→max_completion_tokens rename note; got {coerced!r}"
    )


def test_cross_format_silently_drops_openai_only_params(openai_client, catalog) -> None:
    """DIVERGENCE: OpenAI-only knobs are DROPPED, not rejected, on a non-OpenAI target.

    `n`, `seed`, `logprobs`, `user`, `service_tier` have no Anthropic equivalent.
    The gateway drops them and serves the request; a caller relying on `n=2` gets
    one choice and no error.

    The load-bearing assertion is `len(choices) == 1` next to a 200: it proves the
    drop is real. A test that only asserted 200 would pass just as happily if the
    fields were being honoured.
    """
    model = pick_model(catalog, family="claude-")
    resp = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "Reply with one short word."}],
        max_tokens=16,
        n=2,
        seed=42,
        user="ap3-compat-suite",
        extra_body={"service_tier": "default"},
    )
    assert len(resp.choices) == 1, (
        f"cross-format n=2 should be dropped to a single choice; got {len(resp.choices)}"
    )


def test_anthropic_target_rejects_json_schema_strict(openai_client, catalog) -> None:
    """DIVERGENCE: structured output with a strict schema is a hard 400 on Anthropic.

    Unlike the params above, this one is NOT silently dropped — Anthropic has no
    schema-enforced mode, and degrading a caller's output contract to a prompt
    hint would be the silent-wrong-answer failure the codec exists to prevent.
    Case 28 covers the json_object sibling, which does fall back to a prompt.

    Asserts status + message, not `code`: this error is raised by the codec with
    the canonical lowercase `invalid_request`, not the gateway's UPPER_SNAKE.
    """
    import openai

    model = pick_model(catalog, family="claude-")
    with pytest.raises(openai.BadRequestError) as excinfo:
        openai_client.chat.completions.create(
            model=model,
            messages=[{"role": "user", "content": "Give me an object."}],
            max_tokens=64,
            response_format={
                "type": "json_schema",
                "json_schema": {
                    "name": "reply",
                    "strict": True,
                    "schema": {
                        "type": "object",
                        "properties": {"word": {"type": "string"}},
                        "required": ["word"],
                        "additionalProperties": False,
                    },
                },
            },
        )
    assert excinfo.value.status_code == 400
    assert "response_format" in str(excinfo.value), (
        f"the 400 must name the unsupported field; got {excinfo.value}"
    )
