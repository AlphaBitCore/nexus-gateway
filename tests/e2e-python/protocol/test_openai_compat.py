"""
Phase 5 — OpenAI Python SDK compatibility against our /v1.

These tests pin the contract that an unmodified `openai` SDK can target
the Nexus AI Gateway as a drop-in OpenAI replacement. We assert response
*shape* and protocol semantics — never the model's actual content,
because real LLMs are non-deterministic and content assertions flake.

Coverage:
- /v1/models        — list returns at least one entry with the OpenAI shape
- /v1/chat (sync)   — choices[0].message.content + usage fields populated
- /v1/chat (stream) — chunks have delta.content, terminal chunk has
                      finish_reason

Runs against `moonshot-v1-8k` because it's the cheapest model the dev DB
exposes that speaks the OpenAI wire format end-to-end.
"""

from __future__ import annotations

import pytest

from .conftest import OPENAI_TEST_MODEL

pytestmark = pytest.mark.protocol


def test_models_list_returns_openai_shape(openai_client):
    """`client.models.list()` must return the OpenAI {object:list,data:[...]}
    envelope with each entry carrying id + object='model'."""
    page = openai_client.models.list()
    items = list(page.data)
    assert items, "no models returned — gateway has no enabled models for this VK"
    first = items[0]
    assert getattr(first, "id", None), f"model entry missing id: {first}"
    # The OpenAI envelope guarantees object='model' on each row.
    assert first.object == "model", f"model.object should be 'model', got {first.object!r}"


def test_chat_completion_sync_shape(openai_client):
    resp = openai_client.chat.completions.create(
        model=OPENAI_TEST_MODEL,
        messages=[{"role": "user", "content": "Reply with exactly: PONG"}],
        max_tokens=8,
        temperature=0,
    )
    assert resp.id, "missing response id"
    assert resp.object == "chat.completion", f"unexpected object={resp.object!r}"
    assert resp.choices, "no choices returned"
    msg = resp.choices[0].message
    # Shape only: content must exist and be a string. We do not assert what
    # the model actually said.
    assert isinstance(msg.content, str) and msg.content, (
        f"choices[0].message.content empty or non-string: {msg!r}"
    )
    assert msg.role == "assistant"
    # Usage must be populated for downstream cost analytics.
    assert resp.usage is not None
    assert resp.usage.prompt_tokens > 0, f"prompt_tokens not populated: {resp.usage!r}"
    assert resp.usage.total_tokens > 0


def test_chat_completion_stream_shape(openai_client):
    """Streaming compatibility: collect deltas, assert at least one chunk
    carries text and the final chunk carries finish_reason. We do not
    assert chunk count or content — only the protocol shape."""
    stream = openai_client.chat.completions.create(
        model=OPENAI_TEST_MODEL,
        messages=[{"role": "user", "content": "Count 1 to 3, separated by spaces."}],
        max_tokens=32,
        temperature=0,
        stream=True,
    )
    chunks_with_text = 0
    finish_reason = None
    chunk_count = 0
    for chunk in stream:
        chunk_count += 1
        # OpenAI stream contract: object='chat.completion.chunk' on every chunk.
        assert chunk.object == "chat.completion.chunk", (
            f"unexpected stream object={chunk.object!r}"
        )
        if chunk.choices:
            choice = chunk.choices[0]
            if choice.delta and choice.delta.content:
                chunks_with_text += 1
            if choice.finish_reason:
                finish_reason = choice.finish_reason
    assert chunk_count > 0, "stream produced zero chunks"
    assert chunks_with_text > 0, "no chunk carried delta.content text"
    assert finish_reason in {"stop", "length", "content_filter", "tool_calls"}, (
        f"missing or unexpected finish_reason on terminal chunk: {finish_reason!r}"
    )
