"""Capture real upstream REQUEST bodies — the ones upstream answered 200 for.

The other two corpora capture what upstream SENT. This one captures what
upstream ACCEPTED, which is the evidence the request-direction gates need and
the only kind that cannot be invented: a request shape is real when a provider
answered it, not when it looks right.

Every case is a SECOND-turn request, because that is the shape with something
to lose. Turn 1 asks a question with a tool and real media attached; turn 2
replays turn 1's answer — copied VERBATIM from what the provider returned, so
the fixture carries the provider's own grammar for tool calls, item ids and
thinking blocks rather than a reconstruction of it — and adds the tool result.
Only a 200 on turn 2 makes it a fixture.

Media comes from media.py, which generates a valid PNG and a valid PDF. The
hand-written fixtures this corpus replaces used an unfetchable URL and a
truncated PDF; a capture with those would have measured a 400.

Credentials stay out of the files; only bodies land.
"""

import json
import os
import pathlib
import sys
import urllib.request

from media import PDF_B64, PDF_DATA_URL, PNG_B64, PNG_DATA_URL

KEYS = json.load(open(os.path.expanduser("~/.nexus/provider-keys.json")))
OUT = pathlib.Path(__file__).parent

AUTH = {
    "openai": lambda: {"authorization": "Bearer " + KEYS["OPENAI_API_KEY"]},
    "anthropic": lambda: {
        "x-api-key": KEYS["ANTHROPIC_API_KEY"],
        "anthropic-version": "2023-06-01",
    },
    "gemini": lambda: {"x-goog-api-key": KEYS["GEMINI_API_KEY"]},
    "cohere": lambda: {"authorization": "Bearer " + KEYS["COHERE_API_KEY"]},
}

RESPONSES = "https://api.openai.com/v1/responses"
OPENAI_CHAT = "https://api.openai.com/v1/chat/completions"
ANTHROPIC_MESSAGES = "https://api.anthropic.com/v1/messages"
GEMINI_GEN = (
    "https://generativelanguage.googleapis.com/v1beta/models/"
    "gemini-2.5-flash:generateContent"
)
COHERE_CHAT = "https://api.cohere.com/v2/chat"

ASK = "What is the weather in Paris? Use the tool. The image and file are context."
FOLLOW_UP = "And what about tomorrow?"
TOOL_RESULT = json.dumps({"city": "Paris", "temp_c": 18})

# The same tool throughout, in each wire's own spelling — so a cross-format
# test comparing two fixtures compares the conversion, not two different tools.
TOOL_PARAMS = {
    "type": "object",
    "properties": {"city": {"type": "string"}},
    "required": ["city"],
}
TOOL_OPENAI = [
    {
        "type": "function",
        "function": {
            "name": "get_weather",
            "description": "get weather for a city",
            "parameters": TOOL_PARAMS,
        },
    }
]
TOOL_ANTHROPIC = [
    {"name": "get_weather", "description": "get weather for a city", "input_schema": TOOL_PARAMS}
]
TOOL_GEMINI = [
    {
        "functionDeclarations": [
            {
                "name": "get_weather",
                "description": "get weather for a city",
                "parameters": TOOL_PARAMS,
            }
        ]
    }
]
WEATHER_TOOL_RESPONSES = {
    "type": "function",
    "name": "get_weather",
    "description": "get weather",
    "parameters": {
        "type": "object",
        "properties": {"city": {"type": "string"}},
        "required": ["city"],
        "additionalProperties": False,
    },
}


def post(url, kind, body):
    req = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST")
    for header, value in AUTH[kind]().items():
        req.add_header(header, value)
    req.add_header("content-type", "application/json")
    req.add_header("user-agent", "nexus-gateway-corpus/1.0")
    try:
        with urllib.request.urlopen(req, timeout=180) as resp:
            return json.loads(resp.read()), None
    except Exception as err:  # noqa: BLE001
        detail = err.read()[:400].decode(errors="replace") if hasattr(err, "read") else str(err)
        return None, f"{err} {detail}"


def write_case(name, url, kind, model, turn1, first, turn2, second, channels):
    (OUT / f"{name}.turn1.request.json").write_text(
        json.dumps(turn1, indent=2, ensure_ascii=False) + "\n"
    )
    (OUT / f"{name}.turn1.response.json").write_text(
        json.dumps(first, indent=2, ensure_ascii=False) + "\n"
    )
    (OUT / f"{name}.request.json").write_text(
        json.dumps(turn2, indent=2, ensure_ascii=False) + "\n"
    )
    (OUT / f"{name}.response.json").write_text(
        json.dumps(second, indent=2, ensure_ascii=False) + "\n"
    )
    print(f"  {name}: turn2 accepted  [{channels}]")
    return {
        "url": url.split("?")[0],
        "provider": kind,
        "model": model,
        "channels": channels,
        "note": "turn2 replays turn1's own answer verbatim; both turns answered 200",
    }


# ---------------------------------------------------------------- Responses


def responses_tool_turn(name, model, extra=None, prompt="weather in Paris? use the tool"):
    turn1 = {
        "model": model,
        "input": [
            {
                "type": "message",
                "role": "user",
                "content": [{"type": "input_text", "text": prompt}],
            }
        ],
        "tools": [WEATHER_TOOL_RESPONSES],
        "max_output_tokens": 600,
        "store": False,
    }
    if extra:
        turn1.update(extra)

    first, err = post(RESPONSES, "openai", turn1)
    if err:
        print(f"  {name}: turn1 FAILED {err}")
        return None

    calls = [i for i in first.get("output", []) if i.get("type") == "function_call"]
    if not calls:
        kinds = [i.get("type") for i in first.get("output", [])]
        print(f"  {name}: turn1 returned no function_call (got {kinds}) — nothing to capture")
        return None

    turn2 = dict(turn1)
    turn2["input"] = (
        list(turn1["input"])
        + list(first.get("output", []))
        + [
            {
                "type": "function_call_output",
                "call_id": c["call_id"],
                "output": json.dumps(
                    {"city": json.loads(c["arguments"]).get("city", "Paris"), "temp_c": 18}
                ),
            }
            for c in calls
        ]
    )

    second, err = post(RESPONSES, "openai", turn2)
    if err:
        print(f"  {name}: turn2 FAILED {err}")
        return None

    kinds = sorted({i.get("type") for i in turn2["input"]})
    return write_case(
        name, RESPONSES, "openai", model, turn1, first, turn2, second, f"input items {kinds}"
    )


# ---------------------------------------------------------- OpenAI chat wire


def openai_rich_turn(name, model="gpt-4o-mini"):
    turn1 = {
        "model": model,
        "max_tokens": 300,
        "messages": [
            {"role": "system", "content": "Be terse."},
            {
                "role": "user",
                "content": [
                    {"type": "text", "text": ASK},
                    {"type": "image_url", "image_url": {"url": PNG_DATA_URL}},
                    {"type": "file", "file": {"filename": "notes.pdf", "file_data": PDF_DATA_URL}},
                ],
            },
        ],
        "tools": TOOL_OPENAI,
    }
    first, err = post(OPENAI_CHAT, "openai", turn1)
    if err:
        print(f"  {name}: turn1 FAILED {err}")
        return None

    answer = first["choices"][0]["message"]
    calls = answer.get("tool_calls") or []
    if not calls:
        print(f"  {name}: turn1 called no tool — nothing to capture")
        return None

    turn2 = dict(turn1)
    turn2["messages"] = (
        list(turn1["messages"])
        + [answer]
        + [{"role": "tool", "tool_call_id": c["id"], "content": TOOL_RESULT} for c in calls]
        + [{"role": "user", "content": FOLLOW_UP}]
    )
    second, err = post(OPENAI_CHAT, "openai", turn2)
    if err:
        print(f"  {name}: turn2 FAILED {err}")
        return None
    return write_case(
        name, OPENAI_CHAT, "openai", model, turn1, first, turn2, second,
        "system + text/image/file turn + assistant tool_calls + tool result + follow-up",
    )


# ------------------------------------------------------------- Anthropic wire


def anthropic_rich_turn(name, model="claude-sonnet-4-5-20250929", thinking=False):
    turn1 = {
        "model": model,
        "max_tokens": 3000 if thinking else 500,
        "system": "Be terse.",
        "tools": TOOL_ANTHROPIC,
        "messages": [
            {
                "role": "user",
                "content": [
                    {"type": "text", "text": ASK},
                    {
                        "type": "image",
                        "source": {"type": "base64", "media_type": "image/png", "data": PNG_B64},
                    },
                    {
                        "type": "document",
                        "source": {
                            "type": "base64",
                            "media_type": "application/pdf",
                            "data": PDF_B64,
                        },
                    },
                ],
            }
        ],
    }
    if thinking:
        # With thinking enabled the answer's tool_use is PRECEDED by a signed
        # thinking block, and the signature must come back verbatim on the next
        # turn or Anthropic rejects it. That is the whole point of this variant:
        # it is the only way to get a signed block into a request fixture.
        turn1["thinking"] = {"type": "enabled", "budget_tokens": 1024}
    first, err = post(ANTHROPIC_MESSAGES, "anthropic", turn1)
    if err:
        print(f"  {name}: turn1 FAILED {err}")
        return None

    uses = [b for b in first.get("content", []) if b.get("type") == "tool_use"]
    if not uses:
        print(f"  {name}: turn1 called no tool — nothing to capture")
        return None

    turn2 = dict(turn1)
    turn2["messages"] = (
        list(turn1["messages"])
        + [{"role": "assistant", "content": first["content"]}]
        + [
            {
                "role": "user",
                "content": [
                    {"type": "tool_result", "tool_use_id": u["id"], "content": TOOL_RESULT}
                    for u in uses
                ],
            }
        ]
        + [{"role": "user", "content": [{"type": "text", "text": FOLLOW_UP}]}]
    )
    second, err = post(ANTHROPIC_MESSAGES, "anthropic", turn2)
    if err:
        print(f"  {name}: turn2 FAILED {err}")
        return None
    return write_case(
        name, ANTHROPIC_MESSAGES, "anthropic", model, turn1, first, turn2, second,
        "system + text/image/document turn + tool_use + tool_result + follow-up",
    )


# ---------------------------------------------------------------- Gemini wire


def gemini_rich_turn(name, model="gemini-2.5-flash"):
    turn1 = {
        "systemInstruction": {"parts": [{"text": "Be terse."}]},
        "tools": TOOL_GEMINI,
        "contents": [
            {
                "role": "user",
                "parts": [
                    {"text": ASK},
                    {"inlineData": {"mimeType": "image/png", "data": PNG_B64}},
                    {"inlineData": {"mimeType": "application/pdf", "data": PDF_B64}},
                ],
            }
        ],
        "generationConfig": {"maxOutputTokens": 800},
    }
    first, err = post(GEMINI_GEN, "gemini", turn1)
    if err:
        print(f"  {name}: turn1 FAILED {err}")
        return None

    parts = first["candidates"][0]["content"].get("parts") or []
    calls = [p["functionCall"] for p in parts if "functionCall" in p]
    if not calls:
        print(f"  {name}: turn1 called no tool — nothing to capture")
        return None

    turn2 = dict(turn1)
    turn2["contents"] = (
        list(turn1["contents"])
        + [first["candidates"][0]["content"]]
        + [
            {
                "role": "user",
                "parts": [
                    {
                        "functionResponse": {
                            "name": c["name"],
                            "response": {"result": json.loads(TOOL_RESULT)},
                        }
                    }
                    for c in calls
                ],
            }
        ]
        + [{"role": "user", "parts": [{"text": FOLLOW_UP}]}]
    )
    second, err = post(GEMINI_GEN, "gemini", turn2)
    if err:
        print(f"  {name}: turn2 FAILED {err}")
        return None
    return write_case(
        name, GEMINI_GEN, "gemini", model, turn1, first, turn2, second,
        "systemInstruction + text/inlineData turn + functionCall + functionResponse + follow-up",
    )


# ---------------------------------------------------------------- Cohere wire


def cohere_rich_turn(name, model="command-a-03-2025"):
    """The tool half of Cohere. There is no fixture carrying both halves.

    Cohere's capabilities are SPLIT across models, and the split is not
    documented anywhere the gateway reads. Probed 2026-08-29:

      command-a-03-2025        + image  -> 400 image content is not supported
      command-a-vision-07-2025 + tools  -> 400 TOOL_USE_NOT_SUPPORTED
      command-a-vision-07-2025 + image  -> 200

    So one conversation cannot carry a tool call and an image, and pretending
    otherwise produces a fixture no Cohere model accepts. This case takes the
    tool turn plus documents[] — which is how this wire carries a file, a text
    passage rather than a media part — and cohere_vision_turn takes the image.
    """
    turn1 = {
        "model": model,
        "tools": TOOL_OPENAI,
        "documents": [
            {
                "id": "notes",
                "data": {
                    "title": "notes.txt",
                    "text": "Weather notes: readings are taken at 09:00 local time.",
                },
            }
        ],
        # documents[] and a tool answer the same question, and left to itself
        # the model reaches for the passage. tool_choice pins turn 1 to the
        # call this fixture exists to carry; turn 2 drops it, or the model
        # calls again instead of answering.
        "tool_choice": "REQUIRED",
        "messages": [
            {"role": "system", "content": "Be terse."},
            {"role": "user", "content": ASK},
        ],
    }
    first, err = post(COHERE_CHAT, "cohere", turn1)
    if err:
        print(f"  {name}: turn1 FAILED {err}")
        return None

    answer = first.get("message", {})
    calls = answer.get("tool_calls") or []
    if not calls:
        print(f"  {name}: turn1 called no tool — nothing to capture")
        return None

    assistant = {"role": "assistant", "tool_calls": calls}
    if answer.get("tool_plan"):
        assistant["tool_plan"] = answer["tool_plan"]

    turn2 = dict(turn1)
    turn2.pop("tool_choice", None)
    turn2["messages"] = (
        list(turn1["messages"])
        + [assistant]
        + [{"role": "tool", "tool_call_id": c["id"], "content": TOOL_RESULT} for c in calls]
        + [{"role": "user", "content": FOLLOW_UP}]
    )
    second, err = post(COHERE_CHAT, "cohere", turn2)
    if err:
        print(f"  {name}: turn2 FAILED {err}")
        return None
    return write_case(
        name, COHERE_CHAT, "cohere", model, turn1, first, turn2, second,
        "system + text turn + documents[] + tool_plan + tool_calls + tool result + follow-up",
    )


def cohere_vision_turn(name, model="command-a-vision-07-2025"):
    """The image half. No tools, because this model answers 400 for them.

    Kept separate rather than dropped: image_url is a content part the Cohere
    codec must build correctly, and the tool fixture above cannot carry one.
    """
    turn1 = {
        "model": model,
        "messages": [
            {"role": "system", "content": "Be terse."},
            {
                "role": "user",
                "content": [
                    {"type": "text", "text": "What colour is this image?"},
                    {"type": "image_url", "image_url": {"url": PNG_DATA_URL}},
                ],
            },
        ],
    }
    first, err = post(COHERE_CHAT, "cohere", turn1)
    if err:
        print(f"  {name}: turn1 FAILED {err}")
        return None

    answer = first.get("message", {})
    turn2 = dict(turn1)
    turn2["messages"] = (
        list(turn1["messages"])
        + [{"role": "assistant", "content": answer.get("content", [])}]
        + [{"role": "user", "content": "And is it light or dark?"}]
    )
    second, err = post(COHERE_CHAT, "cohere", turn2)
    if err:
        print(f"  {name}: turn2 FAILED {err}")
        return None
    return write_case(
        name, COHERE_CHAT, "cohere", model, turn1, first, turn2, second,
        "system + text/image turn + assistant text turn + follow-up",
    )


CASES = {
    "openai_responses_tool_turn": lambda n: responses_tool_turn(n, "gpt-4o-mini"),
    # gpt-5-mini emits a reasoning item beside the function_call, so replaying
    # its output exercises the input-item types a plain tool turn never shows.
    "openai_responses_reasoning_tool_turn": lambda n: responses_tool_turn(
        n, "gpt-5-mini", {"reasoning": {"summary": "auto"}}
    ),
    # Two cities in one question makes the model call the tool twice in ONE
    # turn. On the chat wire that is a single assistant message carrying two
    # tool_calls; on the Responses wire it is two separate function_call items.
    "openai_responses_parallel_tool_turn": lambda n: responses_tool_turn(
        n,
        "gpt-4o-mini",
        {"parallel_tool_calls": True},
        prompt="weather in Paris and in Tokyo? call the tool once per city",
    ),
    "openai_chat_rich_turn": openai_rich_turn,
    "anthropic_rich_turn": anthropic_rich_turn,
    "anthropic_thinking_rich_turn": lambda n: anthropic_rich_turn(n, thinking=True),
    "gemini_rich_turn": gemini_rich_turn,
    "cohere_rich_turn": cohere_rich_turn,
    "cohere_vision_turn": cohere_vision_turn,
}


def verify_roundtrip(name, wire_path):
    """Send a round-tripped request back to the provider and keep both halves.

    A codec's output is "correct" in exactly one sense that matters: the
    provider accepts it and answers about the same conversation. This posts
    what the codec produced from the captured request and stores the request
    it sent alongside the response it got, so the repo holds real bytes for
    both directions of the leg the unit test guards.
    """
    body = json.loads(pathlib.Path(wire_path).read_text())
    answer, err = post(RESPONSES, "openai", body)
    if err:
        print(f"  {name}: ROUND-TRIP REJECTED {err}")
        return False
    (OUT / f"{name}.roundtrip.request.json").write_text(
        json.dumps(body, indent=2, ensure_ascii=False) + "\n"
    )
    (OUT / f"{name}.roundtrip.response.json").write_text(
        json.dumps(answer, indent=2, ensure_ascii=False) + "\n"
    )
    text = "".join(
        c.get("text", "")
        for item in answer.get("output", [])
        for c in item.get("content", [])
    )
    print(f"  {name}: round-trip accepted, model answered {text[:70]!r}")
    return True


# `verify <name> <path>` re-sends a round-tripped body instead of capturing a
# new conversation; without it the script captures every case.
if sys.argv[1:2] == ["verify"]:
    if not verify_roundtrip(sys.argv[2], sys.argv[3]):
        sys.exit(1)
    sys.exit(0)

wanted = sys.argv[1:] or list(CASES)
index_path = OUT / "index.json"
index = json.loads(index_path.read_text()) if index_path.exists() else {}
for n in wanted:
    e = CASES[n](n)
    if e:
        index[n] = e
index_path.write_text(json.dumps(index, indent=2, ensure_ascii=False, sort_keys=True) + "\n")
print("->", OUT)
