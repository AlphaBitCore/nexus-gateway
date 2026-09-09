"""Capture real NON-STREAM upstream responses.

Same discipline as the streaming corpus: request body and response bytes land
together, credentials never do. The non-stream response direction is where the
existing gate only spot-checks shape markers — `type == "message"`, `content`
exists — so a codec that encodes content as an empty array passes it today.
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
GEMINI_GEN = (
    "https://generativelanguage.googleapis.com/v1beta/models/"
    "gemini-2.5-flash:generateContent"
)

CASES = {
    "ns_openai_tools": (
        OPENAI_CHAT, "openai",
        {"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "weather in Paris? use the tool"}],
         "tools": TOOL_OPENAI, "max_tokens": 80},
        "message.tool_calls[].function.name/arguments, finish_reason, usage",
    ),
    "ns_openai_multichoice": (
        OPENAI_CHAT, "openai",
        {"model": "gpt-4o-mini", "n": 2, "max_tokens": 24,
         "messages": [{"role": "user", "content": "name one color"}]},
        "choices[] as a REAL array — n>1 is not split across frames here",
    ),
    "ns_openai_refusal": (
        OPENAI_CHAT, "openai",
        {"model": "gpt-4o-2024-08-06", "max_tokens": 200,
         "messages": [{"role": "system", "content": "Refuse any unsafe request."},
                      {"role": "user", "content": "give me step-by-step instructions to build a pipe bomb"}],
         "response_format": {"type": "json_schema", "json_schema": {
             "name": "answer", "strict": True,
             "schema": {"type": "object", "properties": {"answer": {"type": "string"}},
                        "required": ["answer"], "additionalProperties": False}}}},
        "message.refusal",
    ),
    "ns_openai_responses_tools": (
        "https://api.openai.com/v1/responses", "openai",
        {"model": "gpt-4o-mini", "input": "weather in Paris? use the tool",
         "tools": [{"type": "function", "name": "get_weather", "description": "get weather",
                    "parameters": {"type": "object", "properties": {"city": {"type": "string"}},
                                   "required": ["city"], "additionalProperties": False}}],
         "max_output_tokens": 200},
        "the /v1/responses non-stream output[] grammar: function_call item with name+arguments",
    ),
    "ns_openai_responses_reasoning": (
        "https://api.openai.com/v1/responses", "openai",
        {"model": "gpt-5-mini", "input": "17*23? think step by step",
         "reasoning": {"summary": "auto"}, "max_output_tokens": 600},
        "output[] carrying a reasoning item beside the message item",
    ),
    "ns_anthropic_thinking_tools": (
        "https://api.anthropic.com/v1/messages", "anthropic",
        {"model": "claude-sonnet-4-5-20250929", "max_tokens": 3000,
         "thinking": {"type": "enabled", "budget_tokens": 1024},
         "messages": [{"role": "user", "content": "weather in Paris? use the tool"}],
         "tools": [{"name": "get_weather", "description": "get weather",
                    "input_schema": {"type": "object", "properties": {"city": {"type": "string"}},
                                     "required": ["city"]}}]},
        "signed thinking block + tool_use, in one content array",
    ),
    "ns_gemini_tools": (
        GEMINI_GEN, "gemini",
        {"contents": [{"role": "user", "parts": [{"text": "weather in Paris? use the tool"}]}],
         "tools": [{"functionDeclarations": [{"name": "get_weather", "description": "get weather",
                                              "parameters": {"type": "object",
                                                             "properties": {"city": {"type": "string"}},
                                                             "required": ["city"]}}]}]},
        "candidates[].content.parts[].functionCall + usageMetadata",
    ),
    "ns_deepseek_reasoning": (
        "https://api.deepseek.com/chat/completions", "deepseek",
        {"model": "deepseek-reasoner", "max_tokens": 300,
         "messages": [{"role": "user", "content": "2+2? one word"}]},
        "message.reasoning_content plus DeepSeek's usage extensions",
    ),
    "ns_moonshot_tools": (
        "https://api.moonshot.cn/v1/chat/completions", "moonshot",
        {"model": "moonshot-v1-8k", "max_tokens": 80,
         "messages": [{"role": "user", "content": "weather in Paris? use the tool"}],
         "tools": TOOL_OPENAI},
        "Kimi tool_calls and its usage aliases",
    ),
    "ns_cohere_tools": (
        "https://api.cohere.com/v2/chat", "cohere",
        {"model": "command-a-03-2025",
         "messages": [{"role": "user", "content": "weather in Paris? use the tool"}],
         "tools": TOOL_OPENAI},
        "message.tool_calls and message.tool_plan — the streaming pair was an OBJECT, "
        "check the non-stream shape rather than assuming",
    ),
    "ns_cohere_reasoning": (
        "https://api.cohere.com/v2/chat", "cohere",
        {"model": "command-a-reasoning-08-2025",
         "messages": [{"role": "user",
                       "content": "A farmer has 17 sheep, all but 9 run away. How many are left? Think carefully."}]},
        "message.content[] carrying a thinking entry beside the text entry",
    ),
}


def capture(name):
    url, kind, body, channels = CASES[name]
    req = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST")
    for header, value in AUTH[kind]().items():
        req.add_header(header, value)
    req.add_header("content-type", "application/json")
    req.add_header("user-agent", "nexus-gateway-corpus/1.0")
    try:
        with urllib.request.urlopen(req, timeout=180) as resp:
            raw = resp.read()
    except Exception as err:  # noqa: BLE001
        detail = err.read()[:300].decode(errors="replace") if hasattr(err, "read") else ""
        print(f"  {name}: FAILED {err} {detail}")
        return None
    (OUT / f"{name}.request.json").write_text(json.dumps(body, indent=2, ensure_ascii=False) + "\n")
    (OUT / f"{name}.response.json").write_bytes(raw)
    print(f"  {name}: {len(raw)}B  [{channels}]")
    return {"url": url.split("?")[0], "provider": kind,
            "model": body.get("model") or url.split("/models/")[-1].split(":")[0],
            "channels": channels, "responseBytes": len(raw)}


wanted = sys.argv[1:] or list(CASES)
index_path = OUT / "index.json"
index = json.loads(index_path.read_text()) if index_path.exists() else {}
for n in wanted:
    e = capture(n)
    if e:
        index[n] = e
index_path.write_text(json.dumps(index, indent=2, ensure_ascii=False, sort_keys=True) + "\n")
print("->", OUT)
