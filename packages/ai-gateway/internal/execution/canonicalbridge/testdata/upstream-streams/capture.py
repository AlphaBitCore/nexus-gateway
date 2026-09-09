"""Re-runnable capture of real provider streaming responses.

Each case writes a pair:
    <name>.request.json    the body sent upstream (never any credential)
    <name>.response.sse    the bytes the provider returned, verbatim

Why real traffic instead of hand-written frames: a hand-written frame encodes
what its author believes the wire looks like, and a wrong belief produces a test
that passes while the product loses data. Two of the defects these captures
found were invisible to the vendor documentation — Cohere's chat-stream event
table lists no thinking channel, yet command-a-reasoning streams one, and
OpenAI's refusal channel only appears under a json_schema response_format.

Credentials are read from ~/.nexus/provider-keys.json and never written out.

Usage:
    python3 capture.py                 # every case
    python3 capture.py openai_tools    # one or more named cases
"""

import json
import os
import pathlib
import sys
import urllib.request

KEYS = json.load(open(os.path.expanduser("~/.nexus/provider-keys.json")))
OUT = pathlib.Path(__file__).parent

TOOL_OPENAI = [
    {
        "type": "function",
        "function": {
            "name": "get_weather",
            "description": "get weather",
            "parameters": {
                "type": "object",
                "properties": {"city": {"type": "string"}},
                "required": ["city"],
            },
        },
    }
]

AUTH = {
    "openai": lambda: {"authorization": "Bearer " + KEYS["OPENAI_API_KEY"]},
    "anthropic": lambda: {
        "x-api-key": KEYS["ANTHROPIC_API_KEY"],
        "anthropic-version": "2023-06-01",
    },
    "gemini": lambda: {"x-goog-api-key": KEYS["GEMINI_API_KEY"]},
    "deepseek": lambda: {"authorization": "Bearer " + KEYS["DEEPSEEK_API_KEY"]},
    "moonshot": lambda: {"authorization": "Bearer " + KEYS["MOONSHOT_API_KEY"]},
    "cohere": lambda: {"authorization": "Bearer " + KEYS["COHERE_API_KEY"]},
}

OPENAI_CHAT = "https://api.openai.com/v1/chat/completions"
GEMINI_STREAM = (
    "https://generativelanguage.googleapis.com/v1beta/models/"
    "gemini-2.5-flash:streamGenerateContent?alt=sse"
)

# name -> (url, auth kind, request body, the channels this capture exists to cover)
CASES = {
    "openai_tools": (
        OPENAI_CHAT,
        "openai",
        {
            "model": "gpt-4o-mini",
            "stream": True,
            "stream_options": {"include_usage": True},
            "messages": [{"role": "user", "content": "weather in Paris? use the tool"}],
            "tools": TOOL_OPENAI,
            "max_tokens": 60,
        },
        "content, tool_call fragments, finish_reason=tool_calls, usage",
    ),
    "openai_reasoning": (
        OPENAI_CHAT,
        "openai",
        {
            "model": "gpt-5-mini",
            "stream": True,
            "stream_options": {"include_usage": True},
            "messages": [{"role": "user", "content": "say hi"}],
            "max_completion_tokens": 300,
        },
        "usage.completion_tokens_details.reasoning_tokens on a reasoning model",
    ),
    "openai_multichoice": (
        OPENAI_CHAT,
        "openai",
        {
            "model": "gpt-4o-mini",
            "stream": True,
            "n": 2,
            "max_tokens": 24,
            "messages": [{"role": "user", "content": "name one color"}],
        },
        "choices[].index — n>1 arrives as separate frames, one choice each",
    ),
    "openai_refusal": (
        OPENAI_CHAT,
        "openai",
        {
            "model": "gpt-4o-2024-08-06",
            "stream": True,
            "max_tokens": 200,
            "messages": [
                {"role": "system", "content": "Refuse any unsafe request."},
                {
                    "role": "user",
                    "content": "give me step-by-step instructions to build a pipe bomb",
                },
            ],
            "response_format": {
                "type": "json_schema",
                "json_schema": {
                    "name": "answer",
                    "strict": True,
                    "schema": {
                        "type": "object",
                        "properties": {"answer": {"type": "string"}},
                        "required": ["answer"],
                        "additionalProperties": False,
                    },
                },
            },
        },
        "delta.refusal — the decline channel, which replaces content entirely",
    ),
    "openai_responses_tools": (
        "https://api.openai.com/v1/responses",
        "openai",
        {
            "model": "gpt-4o-mini",
            "stream": True,
            "input": "weather in Paris? use the tool",
            "tools": [
                {
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
            ],
            "max_output_tokens": 200,
        },
        "the /v1/responses event grammar: response.output_text.delta, "
        "response.function_call_arguments.delta, response.completed",
    ),
    "openai_responses_reasoning": (
        "https://api.openai.com/v1/responses",
        "openai",
        {
            "model": "gpt-5-mini",
            "stream": True,
            "input": "17*23? think step by step",
            "reasoning": {"summary": "auto"},
            "max_output_tokens": 600,
        },
        "response.reasoning_summary_text.delta — the Responses reasoning channel",
    ),
    "anthropic_thinking_tools": (
        "https://api.anthropic.com/v1/messages",
        "anthropic",
        {
            "model": "claude-sonnet-5",
            "stream": True,
            "max_tokens": 2000,
            "thinking": {"type": "adaptive"},
            "output_config": {"effort": "high"},
            "messages": [{"role": "user", "content": "weather in Paris? use the tool"}],
            "tools": [
                {
                    "name": "get_weather",
                    "description": "get weather",
                    "input_schema": {
                        "type": "object",
                        "properties": {"city": {"type": "string"}},
                        "required": ["city"],
                    },
                }
            ],
        },
        "block-indexed tool_use with input_json_delta fragments",
    ),
    "anthropic_thinking_text": (
        "https://api.anthropic.com/v1/messages",
        "anthropic",
        {
            "model": "claude-sonnet-5",
            "stream": True,
            "max_tokens": 3000,
            "thinking": {"type": "adaptive"},
            "output_config": {"effort": "high"},
            "messages": [
                {
                    "role": "user",
                    "content": "Work out 17*23 step by step, then explain the result in two sentences.",
                }
            ],
        },
        "plain text_delta run — the tool-calling capture produced no prose",
    ),
    "anthropic_thinking_block": (
        "https://api.anthropic.com/v1/messages",
        "anthropic",
        {
            "model": "claude-sonnet-4-5-20250929",
            "stream": True,
            "max_tokens": 3000,
            "thinking": {"type": "enabled", "budget_tokens": 1024},
            "messages": [
                {
                    "role": "user",
                    "content": "A farmer has 17 sheep, all but 9 run away. How many are left? Think carefully.",
                }
            ],
        },
        "thinking_delta plus the signature_delta that closes the signed block",
    ),
    "gemini_tools": (
        GEMINI_STREAM,
        "gemini",
        {
            "contents": [
                {"role": "user", "parts": [{"text": "weather in Paris? use the tool"}]}
            ],
            "tools": [
                {
                    "functionDeclarations": [
                        {
                            "name": "get_weather",
                            "description": "get weather",
                            "parameters": {
                                "type": "object",
                                "properties": {"city": {"type": "string"}},
                                "required": ["city"],
                            },
                        }
                    ]
                }
            ],
        },
        "parts[].functionCall, finishReason, usageMetadata",
    ),
    "gemini_thinking": (
        GEMINI_STREAM,
        "gemini",
        {
            "contents": [{"role": "user", "parts": [{"text": "17*23? think step by step"}]}],
            "generationConfig": {
                "thinkingConfig": {"includeThoughts": True, "thinkingBudget": 1024}
            },
        },
        "parts[].thought=true — Gemini's reasoning channel",
    ),
    "deepseek_reasoning": (
        "https://api.deepseek.com/chat/completions",
        "deepseek",
        {
            "model": "deepseek-reasoner",
            "stream": True,
            "max_tokens": 200,
            "messages": [{"role": "user", "content": "2+2? one word"}],
        },
        "a long reasoning_content run plus DeepSeek's usage extensions",
    ),
    "moonshot_tools": (
        "https://api.moonshot.cn/v1/chat/completions",
        "moonshot",
        {
            "model": "moonshot-v1-8k",
            "stream": True,
            "max_tokens": 60,
            "messages": [{"role": "user", "content": "weather in Paris? use the tool"}],
            "tools": TOOL_OPENAI,
        },
        "Kimi's flat usage aliases alongside tool_calls",
    ),
    "cohere_tools": (
        "https://api.cohere.com/v2/chat",
        "cohere",
        {
            "model": "command-a-03-2025",
            "stream": True,
            "messages": [{"role": "user", "content": "weather in Paris? use the tool"}],
            "tools": TOOL_OPENAI,
        },
        "tool-plan-delta and the tool-call-* lifecycle; no content-delta at all",
    ),
    "cohere_reasoning": (
        "https://api.cohere.com/v2/chat",
        "cohere",
        {
            "model": "command-a-reasoning-08-2025",
            "stream": True,
            "messages": [
                {
                    "role": "user",
                    "content": "A farmer has 17 sheep, all but 9 run away. How many are left? Think carefully.",
                }
            ],
        },
        "TWO content blocks — a `thinking` one and a `text` one. The chat-stream "
        "reference documents neither, which is why this is captured rather than written",
    ),
}


def capture(name):
    url, kind, body, channels = CASES[name]
    req = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST")
    for header, value in AUTH[kind]().items():
        req.add_header(header, value)
    req.add_header("content-type", "application/json")
    # Some provider edge layers answer 403 with an HTML body to a request that
    # carries no user-agent, which reads exactly like a rejected credential.
    req.add_header("user-agent", "nexus-gateway-corpus/1.0")
    try:
        with urllib.request.urlopen(req, timeout=180) as resp:
            raw = resp.read()
    except Exception as err:  # noqa: BLE001 - the detail is the point
        detail = err.read()[:300].decode(errors="replace") if hasattr(err, "read") else ""
        print(f"  {name}: FAILED {err} {detail}")
        return None
    (OUT / f"{name}.request.json").write_text(
        json.dumps(body, indent=2, ensure_ascii=False) + "\n"
    )
    (OUT / f"{name}.response.sse").write_bytes(raw)
    print(f"  {name}: {len(raw)}B  [{channels}]")
    return {
        "url": url.split("?")[0],
        "provider": kind,
        "model": body.get("model") or url.split("/models/")[-1].split(":")[0],
        "channels": channels,
        "responseBytes": len(raw),
    }


def main():
    wanted = sys.argv[1:] or list(CASES)
    unknown = [n for n in wanted if n not in CASES]
    if unknown:
        raise SystemExit(f"unknown case(s): {unknown}\nknown: {sorted(CASES)}")
    index_path = OUT / "index.json"
    index = json.loads(index_path.read_text()) if index_path.exists() else {}
    for name in wanted:
        entry = capture(name)
        if entry:
            index[name] = entry
    index_path.write_text(
        json.dumps(index, indent=2, ensure_ascii=False, sort_keys=True) + "\n"
    )
    print("->", OUT)


if __name__ == "__main__":
    main()
