"""AP-3 cases 18-25 — tool / function calling via the official OpenAI SDK.

Tool calling is the densest translation surface in the gateway: OpenAI's flat
`tool_calls[]` array has to round-trip against Anthropic's `tool_use` content
blocks, and the streaming form has to reassemble argument fragments. Both
directions are covered, because the cross-format path is where the shape work
actually happens.
"""

from __future__ import annotations

import json

import pytest

from .conftest import pick_model

pytestmark = pytest.mark.sdk_compat

WEATHER_TOOL = {
    "type": "function",
    "function": {
        "name": "get_weather",
        "description": "Get the current weather for a city.",
        "parameters": {
            "type": "object",
            "properties": {"city": {"type": "string", "description": "City name"}},
            "required": ["city"],
            "additionalProperties": False,
        },
    },
}


def _tool_model(catalog, family):
    return pick_model(catalog, feature="function_calling", family=family)


def test_single_tool_call_shape(openai_client, catalog) -> None:
    model = _tool_model(catalog, family="gpt-4o")
    resp = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "What is the weather in Paris?"}],
        tools=[WEATHER_TOOL],
        max_tokens=128,
    )
    choice = resp.choices[0]
    assert choice.finish_reason == "tool_calls", f"finish_reason={choice.finish_reason}"
    calls = choice.message.tool_calls
    assert calls, "no tool_calls on a prompt that requires the tool"
    call = calls[0]
    assert call.id, "tool call missing id — the id is required to reply with a result"
    assert call.type == "function"
    assert call.function.name == "get_weather", f"called {call.function.name}"
    args = json.loads(call.function.arguments)
    assert isinstance(args, dict), f"arguments must decode to an object: {args}"
    assert "city" in args, f"required parameter absent from arguments: {args}"


def test_tool_result_roundtrip_completes(openai_client, catalog) -> None:
    """Replaying a tool result must produce a final assistant turn.

    This is the half that breaks silently: emitting tool_calls is easy, accepting
    the `role: "tool"` reply keyed by the echoed tool_call_id is where an id
    mapping bug surfaces.
    """
    model = _tool_model(catalog, family="gpt-4o")
    messages = [{"role": "user", "content": "What is the weather in Paris?"}]
    first = openai_client.chat.completions.create(
        model=model,
        messages=messages,
        tools=[WEATHER_TOOL],
        max_tokens=128,
    )
    call = first.choices[0].message.tool_calls[0]
    messages.append(first.choices[0].message.model_dump(exclude_none=True))
    messages.append(
        {
            "role": "tool",
            "tool_call_id": call.id,
            "content": json.dumps({"temp_c": 18, "sky": "clear"}),
        }
    )
    second = openai_client.chat.completions.create(
        model=model,
        messages=messages,
        tools=[WEATHER_TOOL],
        max_tokens=128,
    )
    assert second.choices[0].finish_reason == "stop", (
        f"after a tool result the model should answer; "
        f"finish_reason={second.choices[0].finish_reason}"
    )
    assert second.choices[0].message.content, "no final answer after the tool result"


def test_parallel_tool_calls_openai_family(openai_client, catalog) -> None:
    model = _tool_model(catalog, family="gpt-4o")
    resp = openai_client.chat.completions.create(
        model=model,
        messages=[
            {"role": "user", "content": "Weather in Paris and in Tokyo? Call the tool for each."}
        ],
        tools=[WEATHER_TOOL],
        parallel_tool_calls=True,
        max_tokens=256,
    )
    calls = resp.choices[0].message.tool_calls
    assert calls and len(calls) >= 2, f"expected parallel calls, got {calls}"
    ids = [call.id for call in calls]
    assert len(set(ids)) == len(ids), f"tool call ids must be unique: {ids}"


def test_parallel_tool_calls_cross_format(openai_client, catalog) -> None:
    """Anthropic returns several `tool_use` blocks; they must arrive as indexed calls."""
    model = _tool_model(catalog, family="claude-")
    resp = openai_client.chat.completions.create(
        model=model,
        messages=[
            {"role": "user", "content": "Weather in Paris and in Tokyo? Call the tool for each."}
        ],
        tools=[WEATHER_TOOL],
        parallel_tool_calls=True,
        max_tokens=512,
    )
    calls = resp.choices[0].message.tool_calls
    assert calls and len(calls) >= 2, (
        f"expected multiple tool_use blocks mapped to calls, got {calls}"
    )
    assert len({call.id for call in calls}) == len(calls), (
        "cross-format tool call ids collided"
    )


def test_parallel_tool_calls_false_yields_single_call(openai_client, catalog) -> None:
    """`parallel_tool_calls: false` maps to Anthropic's disable_parallel_tool_use.

    Only meaningful when `tools` is present — the codec attaches it to
    tool_choice, so a bug here shows up as the flag being dropped and two calls
    coming back.
    """
    model = _tool_model(catalog, family="claude-")
    resp = openai_client.chat.completions.create(
        model=model,
        messages=[
            {"role": "user", "content": "Weather in Paris and in Tokyo? Call the tool for each."}
        ],
        tools=[WEATHER_TOOL],
        parallel_tool_calls=False,
        max_tokens=512,
    )
    calls = resp.choices[0].message.tool_calls
    assert len(calls) == 1, (
        f"parallel_tool_calls=False must yield exactly one call, got {len(calls)}"
    )


def test_tool_choice_required_forces_a_call(openai_client, catalog) -> None:
    """`tool_choice: "required"` must force a call even on a chatty prompt."""
    model = _tool_model(catalog, family="gpt-4o")
    resp = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "Hello, how are you today?"}],
        tools=[WEATHER_TOOL],
        tool_choice="required",
        max_tokens=128,
    )
    assert resp.choices[0].finish_reason == "tool_calls", (
        f"tool_choice=required did not force a call; "
        f"finish_reason={resp.choices[0].finish_reason}"
    )


def test_tool_choice_none_suppresses_calls(openai_client, catalog) -> None:
    """`tool_choice: "none"` with tools present must yield prose, not a call."""
    model = _tool_model(catalog, family="gpt-4o")
    resp = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "What is the weather in Paris?"}],
        tools=[WEATHER_TOOL],
        tool_choice="none",
        max_tokens=128,
    )
    msg = resp.choices[0].message
    assert not msg.tool_calls, f"tool_choice=none still produced calls: {msg.tool_calls}"
    assert isinstance(msg.content, str) and msg.content, "expected a prose answer instead"


def test_streamed_tool_call_arguments_reassemble(openai_client, catalog) -> None:
    """Streamed argument fragments must concatenate into parseable JSON.

    OpenAI splits `function.arguments` across chunks keyed by `index`. Reassembly
    is the contract every agent framework depends on, and a chunk dropped or
    misordered produces JSON that fails to parse — which is what this asserts.
    """
    model = _tool_model(catalog, family="gpt-4o")
    stream = openai_client.chat.completions.create(
        model=model,
        messages=[{"role": "user", "content": "What is the weather in Paris?"}],
        tools=[WEATHER_TOOL],
        max_tokens=128,
        stream=True,
    )
    fragments: dict[int, str] = {}
    names: dict[int, str] = {}
    finish_reason = None
    for chunk in stream:
        if not chunk.choices:
            continue
        choice = chunk.choices[0]
        if choice.finish_reason:
            finish_reason = choice.finish_reason
        for delta_call in choice.delta.tool_calls or []:
            idx = delta_call.index
            assert idx is not None, "streamed tool call delta must carry an index"
            if delta_call.function is None:
                continue
            if delta_call.function.name:
                names[idx] = delta_call.function.name
            if delta_call.function.arguments:
                fragments[idx] = fragments.get(idx, "") + delta_call.function.arguments

    assert finish_reason == "tool_calls", f"finish_reason={finish_reason}"
    assert fragments, "no streamed tool-call argument fragments"
    for idx, raw in fragments.items():
        args = json.loads(raw)
        assert isinstance(args, dict), f"call {idx} arguments decoded to {type(args)}"
    assert "get_weather" in names.values(), f"streamed call names = {names}"
