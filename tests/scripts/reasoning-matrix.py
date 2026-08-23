#!/usr/bin/env python3
"""Reasoning sweep: every reasoning-capable model, streaming and non-streaming.

Each cell sends one canonical /v1/chat/completions request that explicitly asks
for reasoning (reasoning_effort) and judges what came back on three independent
signals rather than the status code alone:

  - usage: reasoning token count in the normalized usage block
  - transcript: reasoning deltas / reasoning_content in the body or the stream
  - anchor: the puzzle has one right answer, and producing it is evidence the
    model worked the problem rather than pattern-matching the prompt

Verdicts:
  REASONED            200 + reasoning tokens > 0 (the accounting saw reasoning)
  REASONED_UNBILLED   200 + reasoning transcript present but zero/absent count —
                      the wire reasoned and our usage extraction did not see it;
                      each of these is a defect candidate, not a pass
  ANSWERED_ONLY       200 + correct anchor, no reasoning evidence at all
  EMPTY               200 + no content — budget burned before any output, or a
                      dropped translation; never counted as a pass
  REFUSED             non-200, with who said so (nexus vs upstream) and why

The runbook (tests/scripts/README or the deploy handoff) decides which verdicts
constitute a pass per model family; this instrument only measures.

Resume: rows land in --out as NDJSON keyed by (model, mode); an interrupted run
re-runs only what is missing. --report summarises without calling anything.
"""

import argparse
import json
import os
import subprocess
import sys
import time
import uuid

PROMPT = (
    "A farmer has 17 sheep. All but 9 run away. Then he buys twice as many "
    "sheep as remained. How many sheep does he have now? Think it through, "
    "then answer with just the final number."
)
ANCHOR = "27"  # 9 remained, bought 18 more

MODES = ("nonstream", "stream")


def call(gw, vk, body, timeout, request_id):
    p = subprocess.run(
        ["curl", "-sS", "-o", "-", "-w", "\n%{http_code}",
         gw + "/v1/chat/completions",
         "-H", "Authorization: Bearer " + vk,
         "-H", "Content-Type: application/json",
         "-H", "X-Request-ID: " + request_id,
         "--max-time", str(timeout),
         "-d", json.dumps(body)],
        capture_output=True, text=True)
    out = p.stdout
    if "\n" not in out:
        return 0, out or p.stderr
    payload, code = out.rsplit("\n", 1)
    try:
        return int(code), payload
    except ValueError:
        return 0, out


def judge_nonstream(payload):
    try:
        d = json.loads(payload)
    except Exception:
        return {"verdict": "EMPTY", "note": "unparseable body", "raw": payload[:200]}
    usage = d.get("usage") or {}
    det = usage.get("completion_tokens_details") or {}
    rtok = det.get("reasoning_tokens") or usage.get("reasoning_tokens") or 0
    msg = ((d.get("choices") or [{}])[0].get("message")) or {}
    content = msg.get("content") or ""
    transcript = bool(msg.get("reasoning_content") or msg.get("reasoning"))
    return _verdict(rtok, transcript, content)


def judge_stream(payload):
    rtok = 0
    transcript = False
    content = []
    for line in payload.splitlines():
        if not line.startswith("data:"):
            continue
        data = line[5:].strip()
        if not data or data == "[DONE]":
            continue
        try:
            d = json.loads(data)
        except Exception:
            continue
        usage = d.get("usage") or {}
        det = usage.get("completion_tokens_details") or {}
        rtok = max(rtok, det.get("reasoning_tokens") or usage.get("reasoning_tokens") or 0)
        for ch in d.get("choices") or []:
            delta = ch.get("delta") or {}
            if delta.get("reasoning_content") or delta.get("reasoning"):
                transcript = True
            if delta.get("content"):
                content.append(delta["content"])
    return _verdict(rtok, transcript, "".join(content))


def _verdict(rtok, transcript, content):
    anchored = ANCHOR in (content or "")
    if rtok and int(rtok) > 0:
        v = "REASONED"
    elif transcript:
        v = "REASONED_UNBILLED"
    elif content.strip():
        v = "ANSWERED_ONLY"
    else:
        v = "EMPTY"
    return {"verdict": v, "reasoning_tokens": int(rtok or 0),
            "transcript": transcript, "anchored": anchored,
            "content_head": (content or "")[:120]}


def refusal(code, payload):
    who = "nexus" if '"nexus' in payload or "nexus:" in payload else "upstream-or-unlabelled"
    try:
        msg = (json.loads(payload).get("error") or {}).get("message", "")
    except Exception:
        msg = payload
    return {"verdict": "REFUSED", "status": code, "who": who, "error": str(msg)[:300]}


def load_done(path):
    done = {}
    if path and os.path.exists(path):
        with open(path) as f:
            for line in f:
                try:
                    r = json.loads(line)
                except Exception:
                    continue
                if "model" in r and "mode" in r:
                    done[(r["model"], r["mode"])] = r
    return done


def report(done):
    by = {}
    for (model, mode), r in sorted(done.items()):
        by.setdefault(model, {})[mode] = r
    counts = {}
    print(f"=== reasoning sweep: {len(by)} models ===")
    for model, modes in sorted(by.items()):
        cells = []
        for mode in MODES:
            r = modes.get(mode)
            v = r["result"]["verdict"] if r else "MISSING"
            counts[v] = counts.get(v, 0) + 1
            extra = ""
            if r and v == "REASONED":
                extra = f"({r['result']['reasoning_tokens']}tok)"
            if r and v == "REFUSED":
                extra = f"({r['result'].get('status')} {r['result'].get('who')})"
            cells.append(f"{mode}={v}{extra}")
        print(f"  {model:<34} {'  '.join(cells)}")
    print("=== totals ===")
    for v, n in sorted(counts.items()):
        print(f"  {v:<20} {n}")
    bad = [(m, mo) for (m, mo), r in done.items()
           if r["result"]["verdict"] in ("REASONED_UNBILLED", "EMPTY", "REFUSED")]
    if bad:
        print(f"=== cells needing triage: {len(bad)} ===")
        for m, mo in sorted(bad):
            r = done[(m, mo)]["result"]
            print(f"  {m} {mo}: {r['verdict']} {r.get('error', '')[:160]}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True, help="NDJSON results; the run resumes from it")
    ap.add_argument("--gw", default=os.environ.get("NEXUS_AI_GW_URL", ""))
    ap.add_argument("--vk-file", default=os.environ.get("NEXUS_VK_FILE", ""))
    ap.add_argument("--models", required=False, default="",
                    help="comma-separated reasoning-capable model codes; the capability "
                         "lives in the admin catalog, not /v1/models, so the caller "
                         "supplies the list it measured there")
    ap.add_argument("--models-file", default="", help="file with one model code per line")
    ap.add_argument("--effort", default="low", help="reasoning_effort to request")
    ap.add_argument("--max-tokens", type=int, default=2048,
                    help="generous: reasoning spends budget before any content")
    ap.add_argument("--timeout", type=int, default=180)
    ap.add_argument("--report", action="store_true")
    ap.add_argument("--run-id", default="rsx")
    args = ap.parse_args()

    done = load_done(args.out)
    if args.report:
        report(done)
        return

    models = [m.strip() for m in args.models.split(",") if m.strip()]
    if args.models_file:
        with open(args.models_file) as f:
            models += [l.strip() for l in f if l.strip() and not l.startswith("#")]
    if not models:
        sys.exit("no models given — pass --models or --models-file")
    if not args.gw or not args.vk_file:
        sys.exit("--gw and --vk-file are required (or NEXUS_AI_GW_URL / NEXUS_VK_FILE)")
    vk = open(args.vk_file).read().strip()

    todo = [(m, mode) for m in models for mode in MODES if (m, mode) not in done]
    print(f"{len(models)} models × {len(MODES)} modes = {len(models) * len(MODES)} cells, "
          f"{len(done)} already measured, {len(todo)} to run")

    with open(args.out, "a") as out:
        for i, (model, mode) in enumerate(todo, 1):
            rid = f"{args.run_id}-{model}-{mode}-{uuid.uuid4().hex[:8]}"
            body = {
                "model": model,
                "messages": [{"role": "user", "content": PROMPT}],
                "reasoning_effort": args.effort,
                "max_tokens": args.max_tokens,
            }
            if mode == "stream":
                body["stream"] = True
                body["stream_options"] = {"include_usage": True}
            t0 = time.time()
            code, payload = call(args.gw, vk, body, args.timeout, rid)
            if code == 200:
                result = judge_stream(payload) if mode == "stream" else judge_nonstream(payload)
            else:
                result = refusal(code, payload)
            row = {"model": model, "mode": mode, "request_id": rid,
                   "effort": args.effort, "status": code,
                   "latency_s": round(time.time() - t0, 1), "result": result}
            out.write(json.dumps(row, ensure_ascii=False) + "\n")
            out.flush()
            print(f"[{i}/{len(todo)}] {model} {mode}: {result['verdict']}")
    report(load_done(args.out))


if __name__ == "__main__":
    main()
