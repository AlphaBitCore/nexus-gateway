#!/usr/bin/env python3
"""C-32 regression: the normalize chokepoint, proved on all four public ingresses.

C-32 swept ~30 goccy decode sites in shared/transport/normalize/codecs/** behind one
convergence point. The claim that needs a standing guard is not "normalization runs"
but "**every** ingress wire shape converges on it and comes out canonical" — a codec
that silently stopped extracting text for one ingress would leave the other three
green, and nothing else in the suite compares the four against each other.

The observation is deliberately the strongest one available: a marker string carried in
the request is looked for **inside the normalized projection's extracted text**. A row
that merely exists proves the audit path ran; the marker appearing in the canonical
messages proves the codec for that specific wire shape decoded and extracted it. The
projection is read through GET /api/admin/traffic/:id/normalized — the same view-time
recompute the UI's Normalized tab runs, i.e. the real consumer, not a reimplementation.

Usage:
    python3 tests/scripts/normalize-ingress-parity.py

Requires the local stack up, the gateway on :3050, and NEXUS_TEST_VK in tests/.env.local.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
GW = os.environ.get("NEXUS_AI_GW_URL", "http://127.0.0.1:3050")
PG = ("nexus-postgres", "postgres", "nexus_gateway")
MODEL = os.environ.get("NEXUS_NORMALIZE_MODEL", "gpt-4o-mini")

PASS, FAIL = "PASS", "FAIL"
results: list[tuple[str, str, str]] = []


def record(name: str, verdict: str, evidence: str) -> None:
    results.append((name, verdict, evidence))
    mark = {PASS: "\033[32m✓\033[0m", FAIL: "\033[31m✗\033[0m"}[verdict]
    print(f"  {mark} {name}: {evidence}")


def load_env() -> None:
    path = os.path.join(ROOT, "tests", ".env.local")
    if not os.path.exists(path):
        sys.exit(f"missing {path}")
    for line in open(path):
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            k, v = line.split("=", 1)
            # Override, never setdefault — tests/.env.local is the authority for a run.
            os.environ[k.strip()] = v.strip().strip('"').strip("'")


def sql(query: str) -> str:
    out = subprocess.run(
        ["docker", "exec", "-e", "PGPASSWORD=postgres", PG[0],
         "psql", "-U", PG[1], "-d", PG[2], "-tAc", query],
        capture_output=True, text=True, timeout=30,
    )
    return out.stdout.strip()


def cp_curl(path: str) -> str:
    """Admin GET through the repo's own auth helper rather than a reimplemented OAuth flow."""
    script = (
        "set -e; export NEXUS_TEST_TARGET=local; "
        f"cd {ROOT!r}; . tests/lib/loadenv.sh >/dev/null; . tests/lib/auth.sh >/dev/null; "
        f"cp_login >/dev/null 2>&1; cp_curl {path!r}"
    )
    out = subprocess.run(["bash", "-c", script], capture_output=True, text=True, timeout=90)
    return out.stdout.strip()


def post(path: str, body: dict, extra_headers: dict | None = None) -> tuple[int, bytes]:
    req = urllib.request.Request(
        GW + path, data=json.dumps(body).encode(), method="POST",
        headers={"Content-Type": "application/json",
                 "Authorization": f"Bearer {os.environ['NEXUS_TEST_VK']}",
                 **(extra_headers or {})},
    )
    try:
        with urllib.request.urlopen(req, timeout=90) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


# Each entry is (ingress name, path builder, body builder). The four PUBLIC ingresses the
# gateway serves; each has its own wire shape and therefore its own codec into canonical.
INGRESSES = [
    ("chat", lambda m: "/v1/chat/completions",
     lambda m, mark: {"model": m, "max_tokens": 8,
                      "messages": [{"role": "user", "content": f"{mark} reply with OK"}]}),
    ("responses", lambda m: "/v1/responses",
     lambda m, mark: {"model": m, "max_output_tokens": 16,
                      "input": [{"role": "user",
                                 "content": [{"type": "input_text",
                                              "text": f"{mark} reply with OK"}]}]}),
    ("messages", lambda m: "/v1/messages",
     lambda m, mark: {"model": m, "max_tokens": 8,
                      "messages": [{"role": "user", "content": f"{mark} reply with OK"}]}),
    ("gemini", lambda m: f"/v1beta/models/{m}:generateContent",
     lambda m, mark: {"contents": [{"role": "user",
                                    "parts": [{"text": f"{mark} reply with OK"}]}],
                      "generationConfig": {"maxOutputTokens": 8}}),
]


def event_id_for_marker(mark: str, timeout: float = 120) -> str:
    """Find THIS request's traffic_event by a token the request itself carried.

    Never by recency: audit is an async side path, and identifying a row by "the newest
    one" produced three wrong conclusions in this program. inline_request_body is BYTEA,
    so encode(...,'escape') is required — a ::text cast yields hex and matches nothing.
    """
    q = ("SELECT te.id FROM traffic_event te "
         "JOIN traffic_event_payload p ON p.traffic_event_id = te.id "
         "WHERE te.source='ai-gateway' "
         f"AND position('{mark}' in encode(p.inline_request_body,'escape')) > 0 "
         "ORDER BY te.created_at DESC LIMIT 1")
    deadline = time.time() + timeout
    while True:
        raw = sql(q)
        if raw:
            return raw
        if time.time() >= deadline:
            return ""
        time.sleep(2)


def canonical_text(projection: dict) -> str:
    """Text carried by the CANONICAL message structure only.

    Deliberately not a blind walk of the whole document. The point of the marker is to
    prove the codec decoded this wire shape and placed the prompt into canonical
    messages[].content[].text; a whole-document walk would also match the marker if it
    survived anywhere else, which would make the assertion vacuous. Verified against a
    live response that GET /normalized returns only the two projections and echoes no
    raw body — so this narrowing is what keeps the marker meaningful, not decoration.
    """
    out: list[str] = []
    for msg in projection.get("messages") or []:
        content = msg.get("content")
        if isinstance(content, str):
            out.append(content)
        elif isinstance(content, list):
            for part in content:
                if isinstance(part, dict) and isinstance(part.get("text"), str):
                    out.append(part["text"])
    return "\n".join(out)


def run_ingress(name: str, path_of, body_of, specs: dict[str, str]) -> None:
    mark = f"C32-{name.upper()}-{int(time.time() * 1000)}"
    code, raw = post(path_of(MODEL), body_of(MODEL, mark))
    if code != 200:
        record(name, FAIL, f"HTTP {code} from the ingress: {raw[:200]!r}")
        return

    eid = event_id_for_marker(mark)
    if not eid:
        record(name, FAIL, f"200 from the ingress but no ai-gateway traffic_event carried {mark}")
        return

    body = cp_curl(f"/api/admin/traffic/{eid}/normalized")
    try:
        doc = json.loads(body)
    except json.JSONDecodeError:
        record(name, FAIL, f"event {eid[:8]}: /normalized returned non-JSON: {body[:200]}")
        return

    req = doc.get("requestNormalized") or {}
    resp = doc.get("responseNormalized") or {}
    problems = []

    if doc.get("requestStatus") != "ok" or doc.get("responseStatus") != "ok":
        problems.append(f"status request={doc.get('requestStatus')!r} response={doc.get('responseStatus')!r}")

    req_text = canonical_text(req)
    if mark not in req_text:
        problems.append(
            f"the canonical request messages do NOT contain the request's own marker {mark}: the "
            f"audit row exists, so the path ran — this says the codec for the {name} wire shape "
            f"did not extract the prompt into canonical form ({len(req_text)} chars present)")

    # The response half converges too, or only half the chokepoint is guarded.
    if not canonical_text(resp).strip():
        problems.append("the canonical response messages carry no text: the response half of the "
                        "chokepoint produced an empty projection")

    spec = req.get("detectedSpec") or ""
    if not spec:
        problems.append("requestNormalized carries no detectedSpec")
    else:
        specs[name] = spec

    record(name, FAIL if problems else PASS,
           f"event {eid[:8]} kind={req.get('kind')!r} protocol={req.get('protocol')!r} "
           f"detectedSpec={spec!r} confidence={req.get('confidence')} "
           f"marker recovered from canonical messages; response usage="
           f"{(resp.get('usage') or {}).get('totalTokens')} tok"
           + (" | " + "; ".join(problems) if problems else ""))


def main() -> int:
    load_env()
    if not os.environ.get("NEXUS_TEST_VK"):
        sys.exit("NEXUS_TEST_VK not in tests/.env.local")
    print(f"C-32 normalize chokepoint — gateway={GW}, model={MODEL}, "
          f"{len(INGRESSES)} ingresses\n")
    specs: dict[str, str] = {}
    for name, path_of, body_of in INGRESSES:
        try:
            run_ingress(name, path_of, body_of, specs)
        except Exception as e:  # one ingress must never take the suite down
            record(name, FAIL, f"raised {type(e).__name__}: {e}")

    # The chokepoint claim is four DISTINCT wire shapes converging on one canonical
    # shape. If two ingresses report the same detectedSpec, one of them is being decoded
    # by the other's codec — the projections can still look plausible while a field
    # unique to the collapsed shape is silently dropped, which is precisely the
    # cross-ingress asymmetry that unit tests cannot see.
    if len(specs) == len(INGRESSES) and len(set(specs.values())) != len(specs):
        record("distinct-specs", FAIL,
               f"two ingresses resolved to the SAME detectedSpec: {specs}")
    elif len(specs) == len(INGRESSES):
        record("distinct-specs", PASS,
               "four ingresses -> four distinct detected specs, one canonical shape: "
               + ", ".join(f"{k}={v}" for k, v in specs.items()))

    npass = sum(1 for _, v, _ in results if v == PASS)
    nfail = sum(1 for _, v, _ in results if v == FAIL)
    print(f"\n  {npass} pass, {nfail} fail")
    return 1 if nfail else 0


if __name__ == "__main__":
    sys.exit(main())
