"""AP-3 cases 36-40 — reasoning tokens via the official OpenAI SDK.

`usage.completion_tokens_details.reasoning_tokens` is what a caller bills and
budgets against, so both directions matter: it must be reported where it exists,
and it must NOT be invented where it does not.

Scope note: the `claude-*` models advertise a `thinking` capability, but opting
in requires the Nexus-only `nexus.ext.anthropic.thinking` body field — which
violates AP-3's "only base_url and api_key changed". gpt-5.x satisfies the
acceptance criterion; a claude thinking case would not be SDK parity.
"""

from __future__ import annotations

import pytest

from .conftest import pick_model

pytestmark = pytest.mark.sdk_compat

REASONING_PROMPT = (
    "A shop sells pens at 3 for 7 dollars and books at 2 for 11 dollars. "
    "What is the cost of 9 pens and 6 books? Reply with just the number."
)


def _reasoning_tokens(usage) -> int | None:
    return getattr(getattr(usage, "completion_tokens_details", None), "reasoning_tokens", None)


def test_reasoning_tokens_reported_on_gpt5(openai_client, catalog) -> None:
    model = pick_model(catalog, family="gpt-5.5")
    resp = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": REASONING_PROMPT}],
        max_completion_tokens=2048,
    )
    tokens = _reasoning_tokens(resp.usage)
    assert tokens is not None, (
        f"completion_tokens_details.reasoning_tokens absent: {resp.usage}"
    )
    assert tokens > 0, f"reasoning model reported {tokens} reasoning tokens"
    assert tokens <= resp.usage.completion_tokens, (
        f"reasoning_tokens ({tokens}) cannot exceed completion_tokens "
        f"({resp.usage.completion_tokens}) — they are a subset, not an addition"
    )


def test_reasoning_tokens_not_fabricated_on_non_reasoning_model(openai_client, catalog) -> None:
    """A non-reasoning model must report zero or nothing — never a made-up count.

    The gateway normalizes usage across providers, and a normalizer that defaults
    a missing field to a plausible number would overbill every caller silently.
    """
    model = pick_model(catalog, family="gpt-4o")
    resp = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "Reply with one short word."}],
        max_tokens=16,
    )
    tokens = _reasoning_tokens(resp.usage)
    assert tokens in (None, 0), (
        f"non-reasoning model reported {tokens} reasoning tokens — the gateway must "
        f"not invent the field"
    )


def test_reasoning_effort_accepted_on_gpt5(openai_client, catalog) -> None:
    model = pick_model(catalog, family="gpt-5.5")
    resp = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": REASONING_PROMPT}],
        max_completion_tokens=2048,
        reasoning_effort="low",
    )
    tokens = _reasoning_tokens(resp.usage)
    assert tokens is not None and tokens > 0, (
        f"reasoning_effort=low should still spend reasoning tokens; got {tokens}"
    )


def test_reasoning_effort_dropped_cross_format(openai_client, catalog) -> None:
    """DIVERGENCE: `reasoning_effort` has no Anthropic equivalent and is dropped.

    Must be a 200, not a 400 — an unknown-to-the-target knob is dropped rather
    than rejected. Recorded in the compatibility matrix.
    """
    model = pick_model(catalog, family="claude-")
    resp = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "Reply with one short word."}],
        max_tokens=32,
        reasoning_effort="low",
    )
    assert resp.choices[0].message.content, (
        "reasoning_effort must be dropped silently on a cross-format target, not rejected"
    )


def test_reasoning_tokens_present_in_stream_usage_chunk(openai_client, catalog) -> None:
    """Streaming must report reasoning tokens too, or streamed calls under-bill."""
    model = pick_model(catalog, family="gpt-5.5")
    stream = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": REASONING_PROMPT}],
        max_completion_tokens=2048,
        stream=True,
        stream_options={"include_usage": True},
    )
    usage_chunks = [chunk for chunk in stream if chunk.usage]
    assert usage_chunks, "no usage chunk on a streamed reasoning call"
    tokens = _reasoning_tokens(usage_chunks[-1].usage)
    assert tokens is not None and tokens > 0, (
        f"streamed usage chunk lost reasoning_tokens: {usage_chunks[-1].usage}"
    )
