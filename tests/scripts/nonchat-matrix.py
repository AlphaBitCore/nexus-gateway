#!/usr/bin/env python3
"""Non-chat endpoint sweep: every stt / tts / image / embedding / rerank model.

One cell per model, judged on business evidence rather than the status code:

  stt        the fixture's spoken sentence comes back in the transcript
  tts        the response is audio bytes of plausible size, not JSON in a coat
  image      the response carries an image (b64 with a real image magic, or a URL)
  embedding  a non-empty float vector of stated dimensionality
  rerank     the on-topic document ranks first — a rerank that returns scores
             but sorts nothing useful has not done its job

Realtime models speak WebSocket and are exercised by a separate probe; this
instrument covers the request/response endpoints only and says so in its
report rather than counting realtime as covered.

Models come from the admin catalog (enabled only), or --models to scope.
NDJSON resume + --report, same conventions as the other matrices.
"""

import argparse
import base64
import json
import os
import subprocess
import sys
import time
import uuid

STT_ANCHOR = "regression fixture"
RERANK_QUERY = "How do I rotate a virtual key in the gateway?"
RERANK_DOCS = [
    "The weather in Lisbon is mild in October, with occasional rain.",
    "Virtual keys are rotated from the admin console: revoke the old key, mint a replacement, and update the client's credential.",
    "Chocolate cake requires flour, sugar, cocoa, and patience.",
]
RERANK_ONTOPIC = 1  # index of the document that answers the query


def curl_json(url, vk, body, timeout, rid, extra=None):
    cmd = ["curl", "-sS", "-o", "-", "-w", "\n%{http_code}",
           url, "-H", "Authorization: Bearer " + vk,
           "-H", "X-Request-ID: " + rid, "--max-time", str(timeout)]
    if body is not None:
        cmd += ["-H", "Content-Type: application/json", "-d", json.dumps(body)]
    cmd += extra or []
    p = subprocess.run(cmd, capture_output=True, text=True)
    out = p.stdout
    payload, _, code = out.rpartition("\n")
    return (int(code) if code.isdigit() else 0), payload


def curl_bytes(url, vk, body, timeout, rid):
    """For endpoints that answer with binary (tts)."""
    tmp = f"/tmp/nonchat-{rid}.bin"
    cmd = ["curl", "-sS", "-o", tmp, "-w", "%{http_code}",
           url, "-H", "Authorization: Bearer " + vk,
           "-H", "X-Request-ID: " + rid,
           "-H", "Content-Type: application/json",
           "--max-time", str(timeout), "-d", json.dumps(body)]
    p = subprocess.run(cmd, capture_output=True, text=True)
    code = int(p.stdout) if p.stdout.strip().isdigit() else 0
    data = b""
    if os.path.exists(tmp):
        with open(tmp, "rb") as f:
            data = f.read()
        os.unlink(tmp)
    return code, data


def audio_magic(b):
    return (b[:4] == b"RIFF" or b[:3] == b"ID3" or b[:2] in (b"\xff\xfb", b"\xff\xf3", b"\xff\xf2")
            or b[:4] == b"OggS" or b[:4] == b"fLaC")


def image_magic(b):
    return (b[:8] == b"\x89PNG\r\n\x1a\n" or b[:3] == b"\xff\xd8\xff"
            or b[:6] in (b"GIF87a", b"GIF89a") or (b[:4] == b"RIFF" and b[8:12] == b"WEBP"))


def run_cell(mtype, model, gw, vk, root, timeout, rid):
    if mtype == "stt":
        audio = os.path.join(root, "tests/fixtures/media/speech.mp3")
        code, payload = curl_json(
            f"{gw}/v1/audio/transcriptions", vk, None, timeout, rid,
            extra=["-F", f"model={model}", "-F", f"file=@{audio};type=audio/mpeg"])
        if code != 200:
            return {"verdict": "REFUSED", "status": code, "error": payload[:300]}
        text = (json.loads(payload).get("text") or "") if payload.startswith("{") else payload
        ok = STT_ANCHOR.lower() in text.lower()
        return {"verdict": "TRANSCRIBED" if ok else "TEXT_WITHOUT_ANCHOR",
                "status": 200, "text_head": text[:120]}

    if mtype == "tts":
        code, data = curl_bytes(f"{gw}/v1/audio/speech", vk,
                                {"model": model, "voice": "alloy",
                                 "input": "The gateway regression fixture speaks."},
                                timeout, rid)
        if code != 200:
            return {"verdict": "REFUSED", "status": code, "error": data[:300].decode(errors="replace")}
        return {"verdict": "AUDIO" if (len(data) > 1024 and audio_magic(data)) else "NOT_AUDIO",
                "status": 200, "bytes": len(data), "magic": data[:4].hex()}

    if mtype == "image":
        code, payload = curl_json(f"{gw}/v1/images/generations", vk,
                                  {"model": model, "prompt": "a plain white square", "n": 1,
                                   "size": "1024x1024"}, timeout, rid)
        if code != 200:
            return {"verdict": "REFUSED", "status": code, "error": payload[:300]}
        d = json.loads(payload) if payload.startswith("{") else {}
        item = (d.get("data") or [{}])[0]
        if item.get("b64_json"):
            raw = base64.b64decode(item["b64_json"][:64] + "==", validate=False)
            return {"verdict": "IMAGE" if image_magic(raw) else "B64_NOT_IMAGE",
                    "status": 200, "kind": "b64"}
        if item.get("url"):
            return {"verdict": "IMAGE", "status": 200, "kind": "url"}
        return {"verdict": "NO_IMAGE_PAYLOAD", "status": 200, "body_head": payload[:160]}

    if mtype == "embedding":
        code, payload = curl_json(f"{gw}/v1/embeddings", vk,
                                  {"model": model, "input": "gateway regression fixture"},
                                  timeout, rid)
        if code != 200:
            return {"verdict": "REFUSED", "status": code, "error": payload[:300]}
        d = json.loads(payload) if payload.startswith("{") else {}
        vec = ((d.get("data") or [{}])[0].get("embedding")) or []
        return {"verdict": "VECTOR" if len(vec) > 0 else "EMPTY_VECTOR",
                "status": 200, "dims": len(vec)}

    if mtype == "rerank":
        code, payload = curl_json(f"{gw}/v1/rerank", vk,
                                  {"model": model, "query": RERANK_QUERY,
                                   "documents": RERANK_DOCS, "top_n": 3}, timeout, rid)
        if code != 200:
            return {"verdict": "REFUSED", "status": code, "error": payload[:300]}
        d = json.loads(payload) if payload.startswith("{") else {}
        results = d.get("results") or d.get("data") or []
        if not results:
            return {"verdict": "NO_RESULTS", "status": 200, "body_head": payload[:160]}
        top = results[0].get("index")
        return {"verdict": "RANKED_ONTOPIC" if top == RERANK_ONTOPIC else "RANKED_OFFTOPIC",
                "status": 200, "top_index": top,
                "scores": [round(r.get("relevance_score") or r.get("relevanceScore") or 0, 4)
                           for r in results]}

    return {"verdict": "UNSUPPORTED_TYPE"}


def load_done(path):
    done = {}
    if path and os.path.exists(path):
        with open(path) as f:
            for line in f:
                try:
                    r = json.loads(line)
                except Exception:
                    continue
                if "model" in r:
                    done[(r["type"], r["model"])] = r
    return done


def report(done):
    counts = {}
    print(f"=== non-chat sweep: {len(done)} cells ===")
    for (mtype, model), r in sorted(done.items()):
        v = r["result"]["verdict"]
        counts[v] = counts.get(v, 0) + 1
        extra = {k: r["result"][k] for k in ("dims", "bytes", "top_index", "kind")
                 if k in r["result"]}
        print(f"  {mtype:<10} {model:<28} {v} {extra if extra else ''}")
    print("=== totals ===")
    for v, n in sorted(counts.items()):
        print(f"  {v:<20} {n}")
    print("NOTE: realtime models are NOT covered here — they need the WebSocket probe.")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--gw", default=os.environ.get("NEXUS_AI_GW_URL", ""))
    ap.add_argument("--vk-file", default=os.environ.get("NEXUS_VK_FILE", ""))
    ap.add_argument("--cp-url", default=os.environ.get("NEXUS_CP_URL", ""))
    ap.add_argument("--cp-token", default=os.environ.get("NEXUS_CP_TOKEN", ""))
    ap.add_argument("--models", default="", help="comma-separated subset of model codes")
    ap.add_argument("--root", default=".")
    ap.add_argument("--timeout", type=int, default=180)
    ap.add_argument("--report", action="store_true")
    ap.add_argument("--run-id", default="ncx")
    args = ap.parse_args()

    done = load_done(args.out)
    if args.report:
        report(done)
        return
    if not args.gw or not args.vk_file or not args.cp_url or not args.cp_token:
        sys.exit("--gw, --vk-file, --cp-url and --cp-token are required")
    vk = open(args.vk_file).read().strip()

    code, payload = (lambda p: (int(p.stdout.rpartition("\n")[2]) if p.stdout.rpartition("\n")[2].isdigit() else 0,
                                p.stdout.rpartition("\n")[0]))(
        subprocess.run(["curl", "-sS", "-o", "-", "-w", "\n%{http_code}",
                        f"{args.cp_url}/api/admin/models/flat?limit=1000",
                        "-H", "Authorization: Bearer " + args.cp_token],
                       capture_output=True, text=True))
    rows = (json.loads(payload).get("data") or []) if code == 200 else []
    if not rows:
        sys.exit(f"could not list models: HTTP {code}")
    wanted = {m.strip() for m in args.models.split(",") if m.strip()}
    cells = [(m["type"], m["code"]) for m in rows
             if m.get("enabled") and m.get("type") in ("stt", "tts", "image", "embedding", "rerank")
             and (not wanted or m["code"] in wanted)]
    todo = [c for c in cells if c not in done]
    print(f"{len(cells)} cells, {len(done)} already measured, {len(todo)} to run")

    with open(args.out, "a") as out:
        for i, (mtype, model) in enumerate(sorted(todo), 1):
            rid = f"{args.run_id}-{mtype}-{model}-{uuid.uuid4().hex[:8]}"
            t0 = time.time()
            result = run_cell(mtype, model, args.gw, vk, args.root, args.timeout, rid)
            row = {"type": mtype, "model": model, "request_id": rid,
                   "latency_s": round(time.time() - t0, 1), "result": result}
            out.write(json.dumps(row, ensure_ascii=False) + "\n")
            out.flush()
            print(f"[{i}/{len(todo)}] {mtype} {model}: {result['verdict']}")
    report(load_done(args.out))


if __name__ == "__main__":
    main()
