"""
Phase 5 — Anthropic Python SDK compatibility against our /v1/messages.

The Nexus AI Gateway exposes a native Anthropic surface so apps already
written against `anthropic` can repoint at us with only a base_url change.
These tests pin that contract by driving the unmodified SDK and checking
protocol shape (never content).

Coverage:
- /v1/messages (sync)   — content[0].text + usage.input_tokens populated
- /v1/messages (stream) — RawContentBlockDeltaEvent chunks arrive +
                          MessageStopEvent terminates the stream

Runs against `claude-haiku-4-5-20251001`, the cheapest Claude model the
dev DB seeds.
"""

from __future__ import annotations

import pytest

from .conftest import ANTHROPIC_TEST_MODEL

pytestmark = pytest.mark.protocol


def test_messages_sync_shape(anthropic_client):
    resp = anthropic_client.messages.create(
        model=ANTHROPIC_TEST_MODEL,
        max_tokens=16,
        messages=[{"role": "user", "content": "Reply with exactly: PONG"}],
    )
    assert resp.id, "missing message id"
    assert resp.type == "message", f"unexpected type={resp.type!r}"
    assert resp.role == "assistant", f"unexpected role={resp.role!r}"
    assert resp.content, "no content blocks returned"
    # First block must be text and non-empty. We do not assert the words.
    first = resp.content[0]
    assert first.type == "text", f"first block type={first.type!r}"
    assert isinstance(first.text, str) and first.text, (
        f"content[0].text empty or non-string: {first!r}"
    )
    # Usage must be populated for downstream cost analytics.
    assert resp.usage is not None
    assert resp.usage.input_tokens > 0, f"input_tokens not populated: {resp.usage!r}"
    assert resp.usage.output_tokens >= 0, f"output_tokens not populated: {resp.usage!r}"
    assert resp.stop_reason in {"end_turn", "max_tokens", "stop_sequence", "tool_use"}, (
        f"unexpected stop_reason: {resp.stop_reason!r}"
    )


# Known compatibility gap: the Nexus AI Gateway's /v1/messages SSE response
# emits only `data: {...}` lines, while the upstream Anthropic API emits
# matched pairs of `event: <name>\ndata: {...}\n\n`. The anthropic-python
# SDK's MessageStream dispatches on the `event:` line; without it, the SDK
# silently produces zero events and `get_final_message()` asserts. Curl
# parses the stream just fine — only SDK clients break.
#
# Fix sketch (gateway side, packages/ai-gateway/internal/providers/spec_anthropic):
#   when streaming through, prepend `event: <ev.type>\n` before each
#   `data:` chunk in the SSE forwarder. Then `for event in stream:` will
#   produce typed events and this xfail can drop.
#
# Until then this test runs and reports the gap so it stays visible.
@pytest.mark.xfail(
    reason=(
        "Gateway /v1/messages SSE omits 'event:' lines, breaking "
        "anthropic-python's typed event dispatch. Curl works; SDK doesn't. "
        "Tracked separately."
    ),
    strict=True,
)
def test_messages_stream_shape(anthropic_client):
    """Anthropic streaming uses typed events (MessageStart, ContentBlockDelta,
    MessageStop). We confirm the SDK's iterator yields a MessageStart, at
    least one delta with text, and a MessageStop, exactly as it would
    against api.anthropic.com directly."""
    saw_message_start = False
    saw_text_delta = False
    saw_message_stop = False
    captured_input_tokens = 0

    with anthropic_client.messages.stream(
        model=ANTHROPIC_TEST_MODEL,
        max_tokens=32,
        messages=[{"role": "user", "content": "Count 1 to 3, separated by spaces."}],
    ) as stream:
        for event in stream:
            etype = getattr(event, "type", None)
            if etype == "message_start":
                saw_message_start = True
                # message_start carries the initial Message envelope
                # including usage.input_tokens; capture it now since we
                # don't rely on the SDK's post-stream accumulator (which
                # only fires when consuming via text_stream / similar
                # high-level wrappers).
                msg = getattr(event, "message", None)
                if msg and getattr(msg, "usage", None):
                    captured_input_tokens = int(msg.usage.input_tokens or 0)
            elif etype == "content_block_delta":
                delta = getattr(event, "delta", None)
                if delta and getattr(delta, "type", None) == "text_delta":
                    if getattr(delta, "text", ""):
                        saw_text_delta = True
            elif etype == "message_stop":
                saw_message_stop = True

    assert saw_message_start, "no message_start event seen"
    assert saw_text_delta, "no content_block_delta with text seen"
    assert saw_message_stop, "no message_stop event seen"
    assert captured_input_tokens > 0, (
        f"message_start did not include usage.input_tokens "
        f"(got {captured_input_tokens})"
    )
