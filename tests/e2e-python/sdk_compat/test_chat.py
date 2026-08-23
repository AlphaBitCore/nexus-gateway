"""AP-3 cases 1-10 — core chat + catalog surface via the official OpenAI SDK.

The baseline claim: point `openai.OpenAI` at the gateway, change nothing else,
and the ordinary calls behave. Shape and protocol semantics only — never what the
model actually said.
"""

from __future__ import annotations

import pytest

from .conftest import pick_model

pytestmark = pytest.mark.sdk_compat


def test_sdk_version_is_supported_and_recorded(record_property) -> None:
    """Provenance guard: every claim this suite makes is about ONE SDK version.

    tests/e2e-python has no committed uv.lock, so `openai>=1.55.0` resolves fresh
    on each sync — today that is a 2.x. Recording the resolved version into the
    junit output is what lets the compatibility matrix name a version instead of
    a range, and the floor assertion stops a stale pin from quietly invalidating
    every other case here.
    """
    import openai

    record_property("openai_sdk_version", openai.__version__)
    major_minor = tuple(int(part) for part in openai.__version__.split(".")[:2])
    assert major_minor >= (1, 55), (
        f"openai=={openai.__version__} predates the declared floor 1.55.0; the "
        f"compatibility claims in this suite were not written against it"
    )


def test_models_list_returns_openai_envelope(openai_client) -> None:
    """`client.models.list()` must deserialize into the SDK's page type."""
    items = openai_client.models.list().data
    assert items, "no models returned — the gateway exposed none for this key"
    for entry in items:
        assert entry.id, f"catalog row missing id: {entry}"
        assert entry.object == "model", f"row {entry.id} has object={entry.object}, want 'model'"


def test_models_list_carries_capability_fields(catalog) -> None:
    """Nexus ships `features` + `outputModalities` alongside the OpenAI fields.

    These are what let a caller pick a model without a second round-trip, and
    what this suite's own capability gating reads. If they vanish, every
    capability-gated case below silently degrades to a skip.
    """
    assert catalog, "empty catalog"
    for model_id, entry in catalog.items():
        features = entry.get("features")
        if features is not None:
            assert isinstance(features, list), (
                f"{model_id}: features present but not a list: {features}"
            )
        assert entry.get("outputModalities"), f"{model_id}: outputModalities empty"
    assert any(
        "function_calling" in (entry.get("features") or []) for entry in catalog.values()
    ), "no model advertises function_calling — the tool cases cannot be meaningful"


def test_model_detail_matches_list_entry(openai_client, catalog) -> None:
    """`client.models.retrieve(id)` returns the same id in the OpenAI shape."""
    model_id = sorted(catalog)[0]
    entry = openai_client.models.retrieve(model_id)
    assert entry.id == model_id, f"retrieve({model_id}) returned id={entry.id}"
    assert entry.object == "model"


def test_chat_completion_non_streaming(openai_client, catalog) -> None:
    model = pick_model(catalog, family="gpt-4o")
    resp = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "Reply with one short word."}],
        max_tokens=16,
        temperature=0,
    )
    assert resp.id, "missing response id"
    assert resp.object == "chat.completion", f"object={resp.object}"
    assert resp.choices, "no choices returned"
    msg = resp.choices[0].message
    assert isinstance(msg.content, str) and msg.content, f"empty content: {msg.content}"
    assert msg.role == "assistant"
    assert resp.usage is not None, "usage missing"
    assert resp.usage.prompt_tokens > 0, (
        f"prompt_tokens not populated: {resp.usage.prompt_tokens}"
    )
    assert resp.usage.completion_tokens > 0, (
        f"completion_tokens not populated: {resp.usage.completion_tokens}"
    )
    assert resp.usage.total_tokens >= (
        resp.usage.prompt_tokens + resp.usage.completion_tokens
    )


def test_chat_completion_streaming(openai_client, catalog) -> None:
    model = pick_model(catalog, family="gpt-4o")
    stream = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "Count from 1 to 5, space separated."}],
        max_tokens=48,
        temperature=0,
        stream=True,
    )
    chunk_count = 0
    chunks_with_text = 0
    finish_reason = None
    for chunk in stream:
        chunk_count += 1
        assert chunk.object == "chat.completion.chunk", f"object={chunk.object}"
        if not chunk.choices:
            continue
        if chunk.choices[0].delta.content:
            chunks_with_text += 1
        if chunk.choices[0].finish_reason:
            finish_reason = chunk.choices[0].finish_reason

    assert chunk_count > 0, "stream produced zero chunks"
    assert chunks_with_text > 0, "no chunk carried delta.content"
    assert finish_reason in {"stop", "length", "tool_calls", "content_filter"}, (
        f"terminal finish_reason={finish_reason}"
    )


def test_chat_multi_turn_history_replays(openai_client, catalog) -> None:
    """A prior assistant turn fed back must be accepted, not rejected.

    Cross-format matters here: the Anthropic codec requires strict user/assistant
    alternation, so a replayed history is where a turn-mapping bug shows up as a
    400 rather than a wrong answer.
    """
    model = pick_model(catalog, family="claude-")
    resp = openai_client.chat.completions.create(
        model=model,
        messages=[
            {"role": "user", "content": "My favourite colour is blue. Acknowledge briefly."},
            {"role": "assistant", "content": "Understood."},
            {"role": "user", "content": "Reply with one short word."},
        ],
        max_tokens=24,
    )
    assert resp.choices[0].message.role == "assistant"
    assert isinstance(resp.choices[0].message.content, str)


def test_chat_system_message_accepted_cross_format(openai_client, catalog) -> None:
    """`role: system` must be hoisted, not rejected.

    Anthropic's wire has no system message — it carries a top-level `system`
    field — so this pins that the codec hoists rather than passing an illegal
    role through.
    """
    model = pick_model(catalog, family="claude-")
    resp = openai_client.chat.completions.create(
        model=model,
        messages=[
            {"role": "system", "content": "You are terse."},
            {"role": "user", "content": "Reply with one short word."},
        ],
        max_tokens=24,
    )
    assert resp.choices[0].message.content


def test_max_tokens_truncation_reports_length(openai_client, catalog) -> None:
    """A generation cut short by max_tokens must say so via finish_reason."""
    model = pick_model(catalog, family="gpt-4o")
    resp = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "Write 400 words about the sea."}],
        max_tokens=8,
    )
    assert resp.choices[0].finish_reason == "length", (
        f"finish_reason={resp.choices[0].finish_reason}, want 'length'"
    )
    assert resp.usage.completion_tokens <= 8, (
        f"completion_tokens={resp.usage.completion_tokens} exceeds the max_tokens=8 ceiling"
    )


def test_n_two_returns_two_indexed_choices(openai_client, catalog) -> None:
    """`n=2` yields two choices with correct `index` values."""
    model = pick_model(catalog, family="gpt-4o")
    resp = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "Reply with one short word."}],
        max_tokens=16,
        n=2,
    )
    assert len(resp.choices) == 2, f"n=2 returned {len(resp.choices)} choices"
    assert [choice.index for choice in resp.choices] == [0, 1], (
        f"choice indices = {[choice.index for choice in resp.choices]}, want [0, 1]"
    )


def test_stream_usage_chunk_when_opted_in(openai_client, catalog) -> None:
    """`stream_options.include_usage` yields a usage chunk with empty choices.

    OpenAI emits usage in its own terminal frame carrying `choices: []`. A caller
    iterating deltas must not trip over it, and a caller summing cost must find it.
    """
    model = pick_model(catalog, family="gpt-4o")
    stream = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "Reply with one short word."}],
        max_tokens=16,
        stream=True,
        stream_options={"include_usage": True},
    )
    usage_chunks = [chunk for chunk in stream if chunk.usage]
    assert len(usage_chunks) >= 1, "include_usage=True produced no usage-bearing chunk"
    final = usage_chunks[-1]
    assert final.usage.prompt_tokens > 0, f"usage chunk has no prompt_tokens: {final.usage}"
    assert final.choices == [], (
        f"the usage frame must carry choices: [], got {final.choices}"
    )
