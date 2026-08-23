"""AP-3 cases 26-30 — structured output (response_format) via the official SDK.

Assertions are on the SHAPE of the returned object — which keys exist and what
JSON types they hold — never on the values. The schema is the contract under
test; what the model chose to put in it is not.
"""

from __future__ import annotations

import json

import pytest

from .conftest import pick_model

pytestmark = pytest.mark.sdk_compat

PERSON_SCHEMA = {
    "type": "object",
    "properties": {"name": {"type": "string"}, "age": {"type": "integer"}},
    "required": ["name", "age"],
    "additionalProperties": False,
}

STRICT_FORMAT = {
    "type": "json_schema",
    "json_schema": {"name": "person", "strict": True, "schema": PERSON_SCHEMA},
}

PROMPT = "Invent a person and return them as JSON with keys name and age."


def test_json_schema_strict_returns_conforming_object(openai_client, catalog) -> None:
    model = pick_model(catalog, feature="json_mode", family="gpt-4o")
    resp = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": PROMPT}],
        response_format=STRICT_FORMAT,
        max_tokens=128,
    )
    obj = json.loads(resp.choices[0].message.content)
    assert set(obj) == {"name", "age"}, (
        f"strict schema must yield exactly the declared keys; got {sorted(obj)}"
    )
    assert isinstance(obj["name"], str), f"name is {type(obj['name'])}, want str"
    assert isinstance(obj["age"], int) and not isinstance(obj["age"], bool), (
        f"age is {type(obj['age'])}, want int"
    )


def test_json_object_mode_returns_parseable_object(openai_client, catalog) -> None:
    model = pick_model(catalog, feature="json_mode", family="gpt-5.4")
    resp = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": PROMPT}],
        response_format={"type": "json_object"},
        max_tokens=128,
    )
    obj = json.loads(resp.choices[0].message.content)
    assert isinstance(obj, dict), f"json_object mode returned {type(obj)}"


def test_json_object_mode_cross_format_falls_back_to_prompt(openai_client, catalog) -> None:
    """Anthropic has no JSON mode, so the codec appends a system instruction.

    The point of this case is the contrast with the strict-schema sibling
    (test_divergence.py case 17), which is a hard 400: a prompt hint is an
    acceptable degradation for "give me JSON", and is NOT acceptable for "conform
    to this schema". This asserts the permissive half still succeeds.
    """
    model = pick_model(catalog, family="claude-")
    resp = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": PROMPT}],
        response_format={"type": "json_object"},
        max_tokens=256,
    )
    obj = json.loads(resp.choices[0].message.content)
    assert isinstance(obj, dict), f"cross-format json_object returned {type(obj)}"


def test_json_schema_missing_additional_properties_is_rejected(openai_client, catalog) -> None:
    """A strict schema without `additionalProperties: false` must 400.

    The assertion is that the gateway forwards the schema VERBATIM rather than
    silently repairing it — a repaired schema would be a different output
    contract than the caller wrote. The 400 itself originates upstream, so this
    case is upstream-dependent by design.
    """
    import openai

    model = pick_model(catalog, feature="json_mode", family="gpt-4o")
    loose_schema = {k: v for k, v in PERSON_SCHEMA.items() if k != "additionalProperties"}
    with pytest.raises(openai.BadRequestError) as excinfo:
        openai_client.chat.completions.create(
            model=model,
            messages=[{"role": "user", "content": PROMPT}],
            response_format={
                "type": "json_schema",
                "json_schema": {"name": "person", "strict": True, "schema": loose_schema},
            },
            max_tokens=128,
        )
    assert excinfo.value.status_code == 400
    assert "additionalProperties" in str(excinfo.value), (
        f"the rejection must name the offending schema key, proving the schema "
        f"reached upstream unmodified; got {excinfo.value}"
    )


def test_json_schema_strict_over_stream(openai_client, catalog) -> None:
    """Streamed deltas under a strict schema must join into a conforming object."""
    model = pick_model(catalog, feature="json_mode", family="gpt-4o")
    stream = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": PROMPT}],
        response_format=STRICT_FORMAT,
        max_tokens=128,
        stream=True,
    )
    obj = json.loads(
        "".join(
            chunk.choices[0].delta.content
            for chunk in stream
            if chunk.choices and chunk.choices[0].delta.content
        )
    )
    assert set(obj) == {"name", "age"}, f"streamed strict output keys = {sorted(obj)}"
