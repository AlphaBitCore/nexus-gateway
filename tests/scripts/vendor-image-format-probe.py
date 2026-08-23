#!/usr/bin/env python3
"""Probe 3 — ask a vendor's own wire, in the vendor's own documented shape,
which image media types it decodes.

This exists because the other two probes cannot answer the question. Through
the gateway, our own content gate refuses a format before the wire sees it, so
the result describes our whitelist rather than the wire. And a published format
list is a FLOOR, not the truth: Google documents png/jpeg/webp/heic/heif and
Gemini reads a GIF anyway. A whitelist copied from documentation therefore
refuses formats that work, with nothing in the error to explain why.

The output is a refusal set. A whitelist is the COMPLEMENT of that set, never
the set of formats that produced a correct answer: a format with no anchor in
the fixture, or one whose answer was truncated, was not shown to be unreadable,
and refusing it takes away a capability the wire has.

    python3 tests/scripts/vendor-image-format-probe.py --vendor anthropic

Keys are read from ~/.nexus/provider-keys.json and never printed, logged, or
passed on a command line.
"""
import argparse
import base64
import json
import os
import sys
import urllib.error
import urllib.request

KEYS = os.path.expanduser("~/.nexus/provider-keys.json")
FIXTURES = "tests/fixtures/media"
ANCHOR = "19452"
ASK = "What number is written in this image? Answer with just the number."

# (case, fixture, media type). The first four carry the anchor; heic and svg do
# not, so they can only ever produce ACCEPTED or REFUSED — which is exactly
# enough to decide whether WE should refuse them.
FORMATS = [
    ("png", "ocr-19452.png", "image/png"),
    ("jpeg", "ocr-19452.jpg", "image/jpeg"),
    ("gif", "ocr-19452.gif", "image/gif"),
    ("webp", "ocr-19452.webp", "image/webp"),
    ("bmp", "ocr-19452.bmp", "image/bmp"),
    ("tiff", "ocr-19452.tiff", "image/tiff"),
    ("heic", "image.heic", "image/heic"),
    ("svg", "hostile.svg", "image/svg+xml"),
]


def load_key(name):
    with open(KEYS) as f:
        return json.load(f)[name]


def post(url, headers, body, timeout=120):
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
            return e.code, {"raw": raw[:400]}
    except Exception as e:  # network, DNS, timeout — not a statement about the format
        return 0, {"transport": str(e)[:200]}


def get(url, headers, timeout=60):
    req = urllib.request.Request(url)
    for k, v in headers.items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        return e.code, {"raw": e.read().decode(errors="replace")[:400]}


def b64(root, name):
    with open(os.path.join(root, FIXTURES, name), "rb") as f:
        return base64.standard_b64encode(f.read()).decode()


# --- vendors -----------------------------------------------------------------

def anthropic(root, model, max_tokens):
    key = load_key("ANTHROPIC_API_KEY")
    headers = {"x-api-key": key, "anthropic-version": "2023-06-01"}
    if not model:
        code, body = get("https://api.anthropic.com/v1/models?limit=100", headers)
        if code != 200:
            sys.exit(f"could not list models: {code} {body}")
        ids = [m["id"] for m in body.get("data", [])]
        # A vision-capable, cheap, current model. Prefer haiku, else the first.
        model = next((i for i in ids if "haiku" in i), ids[0])
        print(f"# model: {model}   (of {len(ids)} listed)")

    def call(media_type, data):
        return post("https://api.anthropic.com/v1/messages", headers, {
            "model": model, "max_tokens": max_tokens,
            "messages": [{"role": "user", "content": [
                {"type": "image", "source": {"type": "base64",
                                             "media_type": media_type, "data": data}},
                {"type": "text", "text": ASK}]}]})

    def text_of(body):
        return " ".join(b.get("text", "") for b in body.get("content", []) if isinstance(b, dict))

    return model, call, text_of


def openai(root, model, max_tokens):
    key = load_key("OPENAI_API_KEY")
    headers = {"authorization": "Bearer " + key}
    model = model or "gpt-4o-mini"

    def call(media_type, data):
        return post("https://api.openai.com/v1/chat/completions", headers, {
            "model": model, "max_tokens": max_tokens,
            "messages": [{"role": "user", "content": [
                {"type": "image_url",
                 "image_url": {"url": f"data:{media_type};base64,{data}"}},
                {"type": "text", "text": ASK}]}]})

    def text_of(body):
        try:
            return body["choices"][0]["message"]["content"] or ""
        except (KeyError, IndexError, TypeError):
            return ""

    return model, call, text_of


def cohere(root, model, max_tokens):
    key = load_key("COHERE_API_KEY")
    headers = {"authorization": "Bearer " + key}
    model = model or "command-a-vision-07-2025"

    def call(media_type, data):
        return post("https://api.cohere.com/v2/chat", headers, {
            "model": model, "max_tokens": max_tokens,
            "messages": [{"role": "user", "content": [
                {"type": "image_url",
                 "image_url": {"url": f"data:{media_type};base64,{data}"}},
                {"type": "text", "text": ASK}]}]})

    def text_of(body):
        try:
            return " ".join(b.get("text", "") for b in body["message"]["content"])
        except (KeyError, TypeError):
            return ""

    return model, call, text_of


def gemini(root, model, max_tokens):
    """Gemini's refusal names only the offending type ("Unsupported MIME type:
    image/svg+xml") rather than enumerating what it accepts, so its accepted set
    cannot be read off a single error the way Anthropic's, OpenAI's and Cohere's
    can. It has to be swept format by format — which is the whole reason this
    probe exists and the reason no allow-list has been written for it yet."""
    key = load_key("GEMINI_API_KEY")
    model = model or "gemini-2.5-flash"
    base = "https://generativelanguage.googleapis.com/v1beta/models/"

    def call(media_type, data):
        return post(base + model + ":generateContent",
                    {"x-goog-api-key": key},
                    {"contents": [{"parts": [
                        {"inline_data": {"mime_type": media_type, "data": data}},
                        {"text": ASK}]}],
                     "generationConfig": {"maxOutputTokens": max_tokens}})

    def text_of(body):
        try:
            parts = body["candidates"][0]["content"]["parts"]
            return " ".join(p.get("text", "") for p in parts)
        except (KeyError, IndexError, TypeError):
            return ""

    return model, call, text_of


def moonshot(root, model, max_tokens):
    key = load_key("MOONSHOT_API_KEY")
    headers = {"authorization": "Bearer " + key}
    model = model or "moonshot-v1-128k-vision-preview"

    def call(media_type, data):
        return post("https://api.moonshot.cn/v1/chat/completions", headers, {
            "model": model, "max_tokens": max_tokens,
            "messages": [{"role": "user", "content": [
                {"type": "image_url",
                 "image_url": {"url": f"data:{media_type};base64,{data}"}},
                {"type": "text", "text": ASK}]}]})

    def text_of(body):
        try:
            return body["choices"][0]["message"]["content"] or ""
        except (KeyError, IndexError, TypeError):
            return ""

    return model, call, text_of


VENDORS = {"anthropic": anthropic, "openai": openai, "cohere": cohere,
           "gemini": gemini, "moonshot": moonshot}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--vendor", required=True, choices=sorted(VENDORS))
    ap.add_argument("--model", default="")
    ap.add_argument("--root", default=".")
    ap.add_argument("--max-tokens", type=int, default=1024)
    ap.add_argument("--only", default="", help="comma-separated case names")
    ap.add_argument("--out", default="")
    args = ap.parse_args()

    model, call, text_of = VENDORS[args.vendor](args.root, args.model, args.max_tokens)
    wanted = set(args.only.split(",")) if args.only else None

    rows = []
    print(f"{'format':<10} {'status':<7} {'verdict':<9} detail")
    for case, fixture, media_type in FORMATS:
        if wanted and case not in wanted:
            continue
        status, body = call(media_type, b64(args.root, fixture))
        if status == 0:
            verdict, detail = "INFRA", body.get("transport", "")
        elif 200 <= status < 300:
            answer = text_of(body).strip()
            verdict = "READ" if ANCHOR in answer else "ACCEPTED"
            detail = answer[:70].replace("\n", " ")
        else:
            verdict = "REFUSED"
            err = body.get("error") if isinstance(body, dict) else None
            detail = (err.get("message") if isinstance(err, dict) else None) or json.dumps(body)[:120]
            detail = detail[:120].replace("\n", " ")
        print(f"{media_type:<10} {status:<7} {verdict:<9} {detail}")
        rows.append({"vendor": args.vendor, "model": model, "media_type": media_type,
                     "status": status, "verdict": verdict, "detail": detail})

    refused = [r["media_type"] for r in rows if r["verdict"] == "REFUSED"]
    print("\nREFUSED BY THE WIRE (this, and only this, is what we may refuse):")
    print("  " + (", ".join(refused) if refused else "(none — an allow-list here would refuse blind)"))
    print("A whitelist is the complement of the line above, over the formats measured here.")

    if args.out:
        with open(args.out, "a") as f:
            for r in rows:
                f.write(json.dumps(r) + "\n")
        print(f"\nappended {len(rows)} rows to {args.out}")


if __name__ == "__main__":
    main()
