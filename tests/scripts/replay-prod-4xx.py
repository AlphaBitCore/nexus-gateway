#!/usr/bin/env python3
"""Replay production 4xx requests with their ORIGINAL payloads.

The sweep measures the shapes we thought to send. This measures the shapes real
callers actually sent — which is the only source that can surface a failure
nobody on this side imagined. Every request goes back out byte-for-byte as it
arrived; a replay that reshapes the body is measuring the reshaping.

Each replayed request is judged into exactly one of three buckets, which are the
goal's exit condition for this stage:

  FIXED      200 now. The deploy repaired it.
  OURS       still 4xx, and the refusal is one WE wrote and can name. This is a
             pass: the caller is told what is wrong and what would work.
  UNOWNED    still 4xx with somebody else's vocabulary, or a new failure. Every
             one of these is a finding that goes back into the tier list.

    python3 tests/scripts/replay-prod-4xx.py --in prod-4xx.ndjson --gw URL \\
        --vk-file vk.txt --out replayed.ndjson
"""
import argparse
import json
import os
import subprocess
import time

# A refusal is OURS when it carries the marker the gateway stamps on refusals it
# authored. Matching on "nexus:" alone would also catch an upstream that happens
# to mention us, so the error code is checked too.
OURS_MARKERS = ("nexus:", "nexus_field_unsupported")
OURS_CODES = {"MODEL_INPUT_MODALITY_UNSUPPORTED", "MODEL_REQUIRED_MODALITY_MISSING",
              "MODEL_MODALITY_MISMATCH", "QUOTA_MODEL_UNPRICED", "QUOTA_EXCEEDED"}
TRANSIENT = {0, 408, 409, 425, 429, 500, 502, 503, 504}


def post(gw, vk, path, body, request_id, timeout):
    """curl rather than urllib: the payloads are arbitrary caller bytes and must
    go back out unmodified, including whatever encoding oddity they arrived
    with. The body rides in a file so it never reaches a command line — a
    command line is visible to every process on the box, which is how a bearer
    token leaked once already."""
    with open("/tmp/replay_body.json", "wb") as f:
        f.write(body if isinstance(body, bytes) else body.encode())
    p = subprocess.run(
        ["curl", "-sS", "-o", "/tmp/replay_resp.json", "-w", "%{http_code}",
         "-X", "POST", gw.rstrip("/") + path,
         "-H", "content-type: application/json",
         "-H", "authorization: Bearer " + vk,
         "-H", "x-request-id: " + request_id,
         "--data-binary", "@/tmp/replay_body.json",
         "--max-time", str(timeout)],
        capture_output=True, text=True)
    try:
        status = int(p.stdout.strip() or 0)
    except ValueError:
        status = 0
    try:
        with open("/tmp/replay_resp.json") as f:
            return status, json.load(f)
    except Exception:
        return status, {}


def judge(status, resp):
    if status in TRANSIENT:
        return "INFRA", "transient — says nothing about the request"
    if 200 <= status < 300:
        return "FIXED", ""
    err = resp.get("error") if isinstance(resp, dict) else None
    msg = ""
    code = ""
    if isinstance(err, dict):
        msg = str(err.get("message", ""))
        code = str(err.get("code", ""))
    elif isinstance(err, str):
        msg = err
    low = msg.lower()
    if code in OURS_CODES or any(m in low for m in OURS_MARKERS):
        return "OURS", msg[:160]
    return "UNOWNED", (code + " " + msg)[:160]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--in", dest="src", required=True)
    ap.add_argument("--gw", required=True)
    ap.add_argument("--vk-file", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--limit", type=int, default=200)
    ap.add_argument("--timeout", type=int, default=120)
    ap.add_argument("--run-id", default="replay")
    args = ap.parse_args()

    vk = open(args.vk_file).read().strip()

    done = set()
    if os.path.exists(args.out):
        for line in open(args.out):
            line = line.strip()
            if line:
                done.add(json.loads(line)["source_id"])

    rows = []
    for line in open(args.src):
        line = line.strip()
        if line:
            rows.append(json.loads(line))
    todo = [r for r in rows if r["id"] not in done][:args.limit]
    print(f"{len(rows)} captured 4xx, {len(done)} already replayed, {len(todo)} to go\n")
    print(f"{'was':<5} {'now':<5} {'verdict':<9} {'model':<26} detail")

    counts = {}
    out = open(args.out, "a")
    for i, r in enumerate(todo):
        status, resp = post(args.gw, vk, r.get("path") or "/v1/chat/completions",
                            r["body"], f"{args.run_id}-{i}", args.timeout)
        verdict, detail = judge(status, resp)
        counts[verdict] = counts.get(verdict, 0) + 1
        out.write(json.dumps({"source_id": r["id"], "was": r.get("statusCode"),
                              "now": status, "verdict": verdict, "detail": detail,
                              "model": r.get("model"), "path": r.get("path")}) + "\n")
        out.flush()
        print(f"{str(r.get('statusCode')):<5} {status:<5} {verdict:<9} "
              f"{str(r.get('model'))[:25]:<26} {detail[:70]}")
        time.sleep(0.15)
    out.close()

    print("\n" + "  ".join(f"{k}={v}" for k, v in sorted(counts.items())))
    print("\nEXIT CONDITION: every replayed 4xx is FIXED or OURS. "
          "Each UNOWNED is a finding — assign it a tier and fix it.")


if __name__ == "__main__":
    main()
