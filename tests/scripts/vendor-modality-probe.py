#!/usr/bin/env python3
"""Ask the vendor's own wire what a model reads, for the cells where our
capability guard answered first.

The sweep records these as REFUSED/CATALOG: the routing guard read the catalog
row under test and refused before any request left the building. That verdict is
a fact about the catalog, never about the model — and the catalog is the thing
being audited, so those cells measure nothing. There is no way to settle them
except to go around ourselves entirely.

Two different questions are answered here, and conflating them is how a wire
limitation gets written down as a model limitation:

  WIRE   Does this provider's wire have a content part for this modality at all?
         Per PROVIDER. Answered once. When the wire has no form for it, the
         refusal enumerates the tags it does have — that enumeration is the
         evidence, and it is why a claim of absence here is a measurement rather
         than an assumption.
  MODEL  Does THIS model read what the wire can carry? Per MODEL, and only
         asked when the wire has a form, because otherwise the model never gets
         the chance to answer.

    python3 tests/scripts/vendor-modality-probe.py --catalog models.json \\
        --cells cells.json --out measured-direct.ndjson

Keys are read from ~/.nexus/provider-keys.json and never printed or logged.
"""
import argparse
import base64
import json
import os
import sys
import time
import urllib.error
import urllib.request

KEYS = os.path.expanduser("~/.nexus/provider-keys.json")
FIXTURES = "tests/fixtures/media"

# modality -> (fixture, media type, prompt, anchor). The anchors are the ones
# baked into the fixtures by the sweep; a reply carrying the anchor is the only
# thing that shows the attachment was READ rather than merely accepted.
PROBES = {
    "image": ("ocr-19452.png", "image/png",
              "Reply with only the number shown in this image, digits only.", "19452"),
    "file":  ("doc.md", "text/markdown",
              "What is the reference number in the attached document? Digits only.", "52903"),
    "audio": ("speech.wav", "audio/wav",
              "Transcribe the attached audio exactly. Reply with the transcript only.",
              "regression fixture"),
    "video": ("clip.mp4", "video/mp4",
              "Reply with only the number shown in this video, digits only.", "74128"),
    "text":  (None, None, "Reply with only: ok", "ok"),
}

KEY_OF = {
    "anthropic": "ANTHROPIC_API_KEY", "openai": "OPENAI_API_KEY",
    "google-gemini": "GEMINI_API_KEY", "cohere": "COHERE_API_KEY",
    "moonshot": "MOONSHOT_API_KEY", "deepseek": "DEEPSEEK_API_KEY",
}

# Providers whose wire is OpenAI-shaped. Same body builder, different host.
OPENAI_COMPAT = {
    "openai": "https://api.openai.com/v1/chat/completions",
    "moonshot": "https://api.moonshot.cn/v1/chat/completions",
    "deepseek": "https://api.deepseek.com/v1/chat/completions",
}

TRANSIENT = {0, 408, 409, 425, 429, 500, 502, 503, 504}


def load_keys():
    with open(KEYS) as f:
        return json.load(f)


def b64(root, name):
    with open(os.path.join(root, FIXTURES, name), "rb") as f:
        return base64.standard_b64encode(f.read()).decode()


def http(url, headers, body, timeout):
    req = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST")
    for k, v in headers.items():
        req.add_header(k, v)
    req.add_header("content-type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        raw = e.read().decode(errors="replace")
        try:
            return e.code, json.loads(raw)
        except json.JSONDecodeError:
            return e.code, {"raw": raw[:500]}
    except Exception as e:
        return 0, {"transport": str(e)[:200]}


# --- per-provider body builders ---------------------------------------------
# Each returns (url, headers, body, text_extractor). A builder returns None when
# the wire has no documented part for that modality — the caller then sends a
# deliberately shaped probe anyway, because a 400 enumerating the valid tags is
# the evidence that the form is absent. "We could not find one" is not the same
# statement as "there is none", and only the wire can make the second.

def build_openai(host, key, model, modality, data, media, prompt, max_tokens):
    if modality == "image":
        part = {"type": "image_url", "image_url": {"url": f"data:{media};base64,{data}"}}
    elif modality == "file":
        part = {"type": "file", "file": {"filename": "doc.md",
                                         "file_data": f"data:{media};base64,{data}"}}
    elif modality == "audio":
        part = {"type": "input_audio", "input_audio": {"data": data, "format": "wav"}}
    elif modality == "video":
        part = {"type": "video_url", "video_url": {"url": f"data:{media};base64,{data}"}}
    else:
        part = None
    content = [part, {"type": "text", "text": prompt}] if part else prompt
    body = {"model": model, "messages": [{"role": "user", "content": content}],
            "max_completion_tokens": max_tokens}
    return host, {"authorization": "Bearer " + key}, body, _openai_text


def _openai_text(b):
    try:
        m = b["choices"][0]["message"]
        return (m.get("content") or "") + " " + json.dumps(m.get("audio", {}))
    except (KeyError, IndexError, TypeError):
        return ""


def build_anthropic(key, model, modality, data, media, prompt, max_tokens):
    if modality == "image":
        part = {"type": "image", "source": {"type": "base64", "media_type": media, "data": data}}
    elif modality == "file":
        # This wire's textual document source takes the characters, not base64.
        part = {"type": "document",
                "source": {"type": "text", "media_type": "text/plain",
                           "data": base64.b64decode(data).decode("utf-8", "replace")}}
    elif modality in ("audio", "video"):
        # No documented block for either. Sent so the 400 enumerates what IS
        # valid — the enumeration is the proof of absence.
        part = {"type": modality, "source": {"type": "base64", "media_type": media, "data": data}}
    else:
        part = None
    content = [part, {"type": "text", "text": prompt}] if part else prompt
    return ("https://api.anthropic.com/v1/messages",
            {"x-api-key": key, "anthropic-version": "2023-06-01"},
            {"model": model, "max_tokens": max_tokens,
             "messages": [{"role": "user", "content": content}]},
            lambda b: " ".join(p.get("text", "") for p in b.get("content", [])
                               if isinstance(p, dict)))


def build_gemini(key, model, modality, data, media, prompt, max_tokens):
    parts = [{"text": prompt}]
    if modality != "text":
        parts.insert(0, {"inline_data": {"mime_type": media, "data": data}})
    return ("https://generativelanguage.googleapis.com/v1beta/models/" + model + ":generateContent",
            {"x-goog-api-key": key},
            {"contents": [{"parts": parts}],
             "generationConfig": {"maxOutputTokens": max_tokens}},
            _gemini_text)


def _gemini_text(b):
    try:
        return " ".join(p.get("text", "") for p in b["candidates"][0]["content"]["parts"])
    except (KeyError, IndexError, TypeError):
        return ""


def build_cohere(key, model, modality, data, media, prompt, max_tokens):
    if modality == "image":
        part = {"type": "image_url", "image_url": {"url": f"data:{media};base64,{data}"}}
    elif modality == "file":
        part = {"type": "document", "document": {"data": base64.b64decode(data).decode("utf-8", "replace")}}
    elif modality in ("audio", "video"):
        part = {"type": "input_audio" if modality == "audio" else "video_url"}
    else:
        part = None
    content = [part, {"type": "text", "text": prompt}] if part else prompt
    return ("https://api.cohere.com/v2/chat", {"authorization": "Bearer " + key},
            {"model": model, "max_tokens": max_tokens,
             "messages": [{"role": "user", "content": content}]},
            lambda b: " ".join(p.get("text", "") for p in b.get("message", {}).get("content", [])))


def build(vendor, key, model, modality, data, media, prompt, max_tokens):
    if vendor in OPENAI_COMPAT:
        return build_openai(OPENAI_COMPAT[vendor], key, model, modality, data, media,
                            prompt, max_tokens)
    if vendor == "anthropic":
        return build_anthropic(key, model, modality, data, media, prompt, max_tokens)
    if vendor == "google-gemini":
        return build_gemini(key, model, modality, data, media, prompt, max_tokens)
    if vendor == "cohere":
        return build_cohere(key, model, modality, data, media, prompt, max_tokens)
    raise KeyError(vendor)


def err_message(body):
    if not isinstance(body, dict):
        return str(body)[:200]
    e = body.get("error")
    if isinstance(e, dict):
        return str(e.get("message") or e)[:300]
    if isinstance(e, str):
        return e[:300]
    return json.dumps(body)[:300]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--catalog", required=True, help="the /api/admin/models payload")
    ap.add_argument("--cells", required=True, help='JSON list of {"model","modality"}')
    ap.add_argument("--root", default=".")
    ap.add_argument("--out", required=True)
    ap.add_argument("--max-tokens", type=int, default=512)
    ap.add_argument("--timeout", type=int, default=180)
    ap.add_argument("--retries", type=int, default=2)
    args = ap.parse_args()

    keys = load_keys()
    catalog = {}
    raw = json.load(open(args.catalog))
    for group in (raw.get("data") if isinstance(raw, dict) else raw):
        vendor = group["provider"]["name"]
        for m in group["models"]:
            catalog[m["code"]] = {
                "vendor": vendor, "provider_model_id": m["providerModelId"],
                "declared_in": [d.lower() for d in (m.get("inputModalities") or [])],
                "declared_required": [d.lower() for d in (m.get("requiredModalities") or [])],
            }

    # Resume: a cell already recorded is not paid for twice.
    done = set()
    if os.path.exists(args.out):
        for line in open(args.out):
            line = line.strip()
            if line:
                r = json.loads(line)
                done.add((r["model"], r["modality"]))

    cells = json.load(open(args.cells))
    todo = [c for c in cells if (c["model"], c["modality"]) not in done]
    print(f"{len(cells)} cells, {len(done)} already recorded, {len(todo)} to probe\n")
    print(f"{'model':<34} {'modality':<7} {'status':<7} {'verdict':<9} detail")

    out = open(args.out, "a")
    for c in todo:
        model, modality = c["model"], c["modality"]
        meta = catalog.get(model)
        if not meta:
            print(f"{model:<34} {modality:<7} {'-':<7} {'NOCAT':<9} not in the catalog payload")
            continue
        vendor = meta["vendor"]
        key = keys.get(KEY_OF.get(vendor, ""))
        if not key:
            print(f"{model:<34} {modality:<7} {'-':<7} {'NOKEY':<9} no key for {vendor}")
            continue

        fixture, media, prompt, anchor = PROBES[modality]
        data = b64(args.root, fixture) if fixture else ""
        url, headers, body, text_of = build(vendor, key, meta["provider_model_id"],
                                            modality, data, media, prompt, args.max_tokens)

        for attempt in range(args.retries + 1):
            status, resp = http(url, headers, body, args.timeout)
            if status not in TRANSIENT or attempt == args.retries:
                break
            time.sleep(2 * (attempt + 1))

        if status in TRANSIENT:
            verdict, detail = "INFRA", err_message(resp)
        elif 200 <= status < 300:
            answer = (text_of(resp) or "").strip()
            verdict = "READ" if anchor.lower() in answer.lower() else "ACCEPTED_NOT_READ"
            detail = answer[:90].replace("\n", " ")
        else:
            verdict, detail = "REFUSED", err_message(resp).replace("\n", " ")

        row = {"model": model, "vendor": vendor,
               "provider_model_id": meta["provider_model_id"], "modality": modality,
               "status": status, "verdict": verdict, "detail": detail,
               "declared_in": meta["declared_in"], "declared_required": meta["declared_required"]}
        out.write(json.dumps(row) + "\n")
        out.flush()
        print(f"{model:<34} {modality:<7} {status:<7} {verdict:<9} {detail[:80]}")
        time.sleep(0.2)
    out.close()

    print("\nA READ here against a catalog row that does not declare the modality is an "
          "UNDERSTATEMENT: routing hides a model that works.")
    print("A REFUSED here against a row that DOES declare it is an OVERSTATEMENT: routing "
          "sends work to a model that cannot do it.")


if __name__ == "__main__":
    main()
