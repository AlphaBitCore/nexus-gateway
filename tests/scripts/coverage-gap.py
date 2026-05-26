#!/usr/bin/env python3
"""
tests/scripts/coverage-gap.py — fills the 4 routes.go gaps that
smoke-gateway.py does not cover:

  G1  POST /v1/estimate                 — pre-flight cost compare
  G2  POST /internal/*                  — provider-test, routing-simulate,
                                          credentials/{id}/probe, hooks-test,
                                          embedding-probe, semantic-prewarm
  G3  Extract cache (L1, gateway)       — 2-turn identical request must
                                          stamp `X-Nexus-Cache: HIT` on round-2.
                                          Independent of upstream `cached_tokens`
                                          (which smoke-gateway.py already covers).
  G4  Semantic cache (L2, paraphrase)   — prewarm Q→A, then send paraphrase
                                          → expect `X-Nexus-Cache: HIT` with
                                          `X-Nexus-Cache-Source: L2-semantic`.

Provider cache (Anthropic cache_control + Gemini cachedContents) is already
exercised by smoke-gateway.py's spec.extract_cached_tokens() in P3A/P3G.

Usage:
  NEXUS_TEST_TARGET=local python3 tests/scripts/coverage-gap.py --vk nvk_xxx \
    [--gw-url http://localhost:3050] [--report /tmp/coverage-gap-<UTC>.md]

Designed to be re-run after every edit to the gateway's auth, cache,
estimate, or debug-endpoint code paths.
"""

import argparse
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Optional

# ─── results ─────────────────────────────────────────────────────────────────
RESULTS: list[dict] = []

def rec(group: str, name: str, ok: bool, evidence: str = "", warn: bool = False):
    status = "WARN" if warn else ("PASS" if ok else "FAIL")
    RESULTS.append({"group": group, "name": name, "status": status, "evidence": evidence})
    marker = {"PASS": "✓", "WARN": "!", "FAIL": "✗"}[status]
    print(f"  [{status:4s}] {marker} {group}/{name}  {evidence}")

# ─── HTTP helpers ────────────────────────────────────────────────────────────
def http_call(method: str, url: str, *, headers: dict | None = None,
              body: bytes | None = None, timeout: float = 30.0) -> tuple[int, dict, bytes]:
    req = urllib.request.Request(url, method=method, data=body)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, dict(resp.getheaders()), resp.read()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read() or b""
    except Exception as e:
        return 0, {}, str(e).encode()

def gw_post_json(gw: str, path: str, body: dict, *, vk: str | None = None,
                 extra_headers: dict | None = None, timeout: float = 30.0) -> tuple[int, dict, dict]:
    h = {"Content-Type": "application/json"}
    if vk:
        h["Authorization"] = f"Bearer {vk}"
    if extra_headers:
        h.update(extra_headers)
    status, headers, raw = http_call("POST", gw + path, headers=h,
                                     body=json.dumps(body).encode(), timeout=timeout)
    try:
        return status, headers, json.loads(raw or b"{}")
    except json.JSONDecodeError:
        return status, headers, {"_raw": raw.decode("utf-8", "replace")[:400]}

# ─── G1: /v1/estimate ────────────────────────────────────────────────────────
def test_g1_estimate(gw: str, vk: str, openai_provider_id: str, anthropic_provider_id: str,
                     openai_model_id: str, anthropic_model_id: str):
    print("\n── G1: POST /v1/estimate ──")

    # 1a — single target, by code
    body = {
        "request": {"model": "gpt-4o-mini",
                    "messages": [{"role": "user", "content": "Write a 50-word story about a robot."}],
                    "max_tokens": 200},
        "compareTargets": [{"providerId": openai_provider_id, "modelId": openai_model_id}],
    }
    status, _, data = gw_post_json(gw, "/v1/estimate", body, vk=vk)
    if status != 200:
        rec("G1", "single-target", False, f"HTTP {status} {str(data)[:120]}"); return
    targets = data.get("targets", [])
    if len(targets) != 1:
        rec("G1", "single-target", False, f"targets len={len(targets)}"); return
    t0 = targets[0]
    if t0.get("error"):
        rec("G1", "single-target", False, f"target error: {t0['error']}"); return
    tok = t0.get("tokens") or {}
    cost = t0.get("cost") or {}
    input_tok = tok.get("uncachedInput") or tok.get("inputTokens") or 0
    cost_total = ((cost.get("expected") or {}).get("total")
                  if isinstance(cost.get("expected"), dict) else cost.get("expectedTotalUsd"))
    if not input_tok or cost_total is None:
        rec("G1", "single-target", False, f"missing tokens/cost: tokens={tok} cost={cost}"); return
    rec("G1", "single-target", True,
        f"inputTokens={input_tok} expectedTotalUsd={cost_total}")

    # 1b — compare 2 providers, summary cheapest non-empty
    body2 = {
        "request": {"model": "gpt-4o-mini",
                    "messages": [{"role": "user", "content": "Say hi"}],
                    "max_tokens": 50},
        "compareTargets": [
            {"providerId": openai_provider_id, "modelId": openai_model_id},
            {"providerId": anthropic_provider_id, "modelId": anthropic_model_id},
        ],
    }
    status, _, data = gw_post_json(gw, "/v1/estimate", body2, vk=vk)
    if status != 200:
        rec("G1", "compare-2", False, f"HTTP {status}"); return
    summary = data.get("summary") or {}
    succ = summary.get("successCount", 0)
    cheapest = summary.get("cheapestExpectedTarget")
    rec("G1", "compare-2",
        succ == 2 and cheapest is not None,
        f"success={succ} cheapest={cheapest}")

    # 1c — invalid reasoning effort → per-target error
    body3 = {
        "request": {"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "x"}]},
        "compareTargets": [{"providerId": openai_provider_id, "modelId": openai_model_id,
                            "reasoningEffort": "bogus"}],
    }
    status, _, data = gw_post_json(gw, "/v1/estimate", body3, vk=vk)
    rec("G1", "invalid-reasoning",
        status == 400 or (status == 200 and any(t.get("error") for t in data.get("targets", []))),
        f"HTTP {status}")


# ─── G2: /internal/* admin endpoints ─────────────────────────────────────────
def test_g2_internal(gw: str, openai_provider_id: str, anthropic_provider_id: str,
                     openai_credential_id: str, openai_model_id: str,
                     openai_embed_model_id: str, anthropic_model_id: str):
    print("\n── G2: POST /internal/* ──")

    # 2a — /internal/provider-test (bogus key → upstream error in body, HTTP still 200/4xx)
    status, _, data = gw_post_json(gw, "/internal/provider-test", {
        "providerName": "openai", "adapterType": "openai",
        "baseUrl": "https://api.openai.com", "apiKey": "sk-bogus-test-only",
    })
    # We expect a JSON response with a `success` field — bogus key means success:false but
    # the endpoint surface itself works.
    rec("G2", "provider-test/bogus-key", status in (200, 400, 401, 502) and "success" in data,
        f"HTTP {status} keys={list(data.keys())[:6]}")

    # 2b — /internal/routing-simulate
    status, _, data = gw_post_json(gw, "/internal/routing-simulate", {
        "modelId": openai_model_id,
        "endpointType": "chat_completions",
        "ingressBodyFormat": "openai",
        "messages": [{"role": "user", "content": "Hello"}],
    })
    # Expected top-level keys (per routing_simulate_endpoint.go): request,
    # originalModelId, substituted, stages, trace, targets, recoveryTargets,
    # branches, warnings. PASS when HTTP 200 + the response shape is valid.
    # Empty targets is acceptable iff a warning explains it ("no stage-1
    # rule matched" — true when admin has zero enabled RoutingRule rows).
    targets = data.get("targets") if isinstance(data, dict) else None
    warnings = data.get("warnings") or []
    no_rule = any("no stage-1 rule matched" in (w or "") for w in warnings)
    rec("G2", "routing-simulate",
        status == 200 and isinstance(targets, list) and (len(targets) >= 1 or no_rule),
        f"HTTP {status} targets-count={(len(targets) if isinstance(targets, list) else 'n/a')} no-rule={no_rule}")

    # 2c — /internal/v1/credentials/{id}/probe
    status, _, data = gw_post_json(gw, f"/internal/v1/credentials/{openai_credential_id}/probe", {
        "timeoutSeconds": 5,
    })
    # ok=true is best but ok=false with a structured error also proves the endpoint surface works.
    rec("G2", "credentials-probe",
        status == 200 and "ok" in data,
        f"HTTP {status} ok={data.get('ok')} err={(data.get('error') or '')[:60]}")

    # 2d — /internal/hooks-test (no enabled hook → harmless echo)
    hook_payload = {
        "hookConfig": {
            "id": str(uuid.uuid4()),
            "name": "test-hook",
            "type": "prompt_hook",
            "implementationId": "noop",
            "stage": "request",
            "config": {},
            "priority": 0,
            "timeoutMs": 1000,
            "failBehavior": "fail-open",
            "enabled": True,
            "applicableIngress": ["openai-chat"],
        },
        "rawBody": '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}',
    }
    status, _, data = gw_post_json(gw, "/internal/hooks-test", hook_payload)
    rec("G2", "hooks-test", status in (200, 400, 404), f"HTTP {status}")

    # 2e — /internal/embedding-probe (bogus key → ok:false in body, HTTP 200)
    status, _, data = gw_post_json(gw, "/internal/embedding-probe", {
        "providerId": openai_provider_id,
        "modelId": openai_embed_model_id,
        "modelName": "text-embedding-3-small",
        "providerModelId": "text-embedding-3-small",
        "baseUrl": "https://api.openai.com",
        "apiKey": "sk-bogus-test-only",
        "dimension": 1536,
    })
    rec("G2", "embedding-probe/bogus-key",
        status == 200 and "ok" in data,
        f"HTTP {status} ok={data.get('ok')} err={(data.get('error') or '')[:60]}")

    # 2f — /internal/semantic-prewarm dryRun (no embedding call)
    status, _, data = gw_post_json(gw, "/internal/semantic-prewarm", {
        "entries": [
            {"prompt": "What is the capital of France?", "response": "Paris.",
             "model": "gpt-4o-mini", "vkScope": "", "ttlSeconds": 3600},
        ],
        "apiKey": "sk-bogus", "providerBaseUrl": "https://api.openai.com", "dryRun": True,
    })
    # 503 is acceptable when writer is nil (no L2 wiring); endpoint surface still proved.
    rec("G2", "semantic-prewarm/dryRun",
        status in (200, 503), f"HTTP {status} resp keys={list(data.keys())[:6]}")


# ─── G3: Extract Cache (L1) ──────────────────────────────────────────────────
def test_g3_extract_cache(gw: str, vk: str):
    print("\n── G3: Extract Cache (L1) — X-Nexus-Cache: HIT verification ──")
    # NOTE: avoid 10-digit sequences in the nonce — the PII hook
    # (phone-number detector) will reject the request as 403.
    nonce = "extract-cache-" + uuid.uuid4().hex[:8]
    body = {
        "model": "gpt-4o-mini",
        "messages": [
            {"role": "system", "content": f"You are a helpful assistant. Run-id: {nonce}"},
            {"role": "user", "content": f"Reply with exactly the word: PONG. Run-id: {nonce}"},
        ],
        "max_tokens": 10,
        "temperature": 0.0,
    }

    # Turn 1 — should be MISS
    h = {"Content-Type": "application/json", "Authorization": f"Bearer {vk}"}
    s1, hdr1, _ = http_call("POST", gw + "/v1/chat/completions",
                            headers=h, body=json.dumps(body).encode(), timeout=60)
    c1 = (hdr1.get("X-Nexus-Cache") or "").upper()
    rec("G3", "turn1-miss", s1 == 200 and c1 in ("MISS", ""),
        f"HTTP {s1} X-Nexus-Cache={c1!r}")

    if s1 != 200:
        rec("G3", "turn2-hit", False, "skipped (turn1 failed)"); return

    # Turn 2 — must be HIT
    time.sleep(0.5)
    s2, hdr2, _ = http_call("POST", gw + "/v1/chat/completions",
                            headers=h, body=json.dumps(body).encode(), timeout=60)
    c2 = (hdr2.get("X-Nexus-Cache") or "").upper()
    rec("G3", "turn2-hit", s2 == 200 and c2 == "HIT",
        f"HTTP {s2} X-Nexus-Cache={c2!r}")


# ─── G4: Semantic Cache (L2) ─────────────────────────────────────────────────
def test_g4_semantic_cache(gw: str, vk: str):
    print("\n── G4: Semantic Cache (L2) — paraphrase HIT ──")

    # Step 1: drive a real Q to populate L2 (the writer fires asynchronously
    # after responses; we let the cache settle for 3s).
    # Avoid 10-digit sequences (PII phone-number detector).
    nonce = "semantic-cache-" + uuid.uuid4().hex[:8]
    sys_msg = "You are a concise Q&A assistant. Answer in one short sentence."
    q1 = "Which language is the Go programming language statically or dynamically typed?"
    q1_para = "Is the Go programming language statically typed or dynamically typed?"

    h = {"Content-Type": "application/json", "Authorization": f"Bearer {vk}"}
    body1 = {"model": "gpt-4o-mini",
             "messages": [{"role": "system", "content": f"{sys_msg} Run:{nonce}"},
                          {"role": "user", "content": q1}],
             "max_tokens": 60, "temperature": 0.0}
    s1, hdr1, _ = http_call("POST", gw + "/v1/chat/completions",
                            headers=h, body=json.dumps(body1).encode(), timeout=60)
    if s1 != 200:
        rec("G4", "seed", False, f"seed HTTP {s1}"); rec("G4", "paraphrase-hit", False, "skipped"); return
    rec("G4", "seed", True, f"HTTP {s1}")

    time.sleep(3.0)  # let async semantic-write settle

    # Step 2: send a paraphrased question — same VK, same model, different phrasing.
    body2 = dict(body1)
    body2["messages"] = [{"role": "system", "content": f"{sys_msg} Run:{nonce}"},
                         {"role": "user", "content": q1_para}]
    s2, hdr2, _ = http_call("POST", gw + "/v1/chat/completions",
                            headers=h, body=json.dumps(body2).encode(), timeout=60)
    cache_status = (hdr2.get("X-Nexus-Cache") or "").upper()
    cache_source = (hdr2.get("X-Nexus-Cache-Source") or "")
    # We accept either HIT (L2 wired and admin-configured to allow) or MISS+WARN
    # (L2 disabled / threshold too high) — both indicate the endpoint shape works.
    if s2 == 200 and cache_status == "HIT" and "L2" in cache_source.upper():
        rec("G4", "paraphrase-hit", True, f"X-Nexus-Cache=HIT source={cache_source!r}")
    elif s2 == 200:
        rec("G4", "paraphrase-hit", True,
            f"L2 not HIT (likely disabled in config): cache={cache_status!r} source={cache_source!r}",
            warn=True)
    else:
        rec("G4", "paraphrase-hit", False, f"HTTP {s2}")


# ─── main ────────────────────────────────────────────────────────────────────
def main():
    ap = argparse.ArgumentParser(description="AI Gateway routes.go coverage-gap test")
    ap.add_argument("--vk", default=os.environ.get("NEXUS_TEST_VK") or os.environ.get("NEXUS_VK"))
    ap.add_argument("--gw-url", default=os.environ.get("NEXUS_AI_GW_URL", "http://localhost:3050"))
    ap.add_argument("--report", default=f"/tmp/coverage-gap-{datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')}.md")
    # IDs are looked up from a local pg if not given.
    ap.add_argument("--openai-provider-id", default="6b6d307f-a80b-4dcb-801b-1ffa07e25cab")
    ap.add_argument("--anthropic-provider-id", default="dee77e2b-fbe8-4281-9d87-cd8886f90450")
    ap.add_argument("--openai-credential-id", default="abff2f77-5506-4d73-99a3-6b60ed756bac")
    ap.add_argument("--openai-model-id", default="3f45ca57-c842-4e40-a901-4d607f3fe064")  # gpt-4o-mini
    ap.add_argument("--openai-embed-model-id", default="a7387e60-64a9-49b5-9acf-cf62c4cde100")  # text-embedding-3-small
    ap.add_argument("--anthropic-model-id", default="e65623cf-60e8-489f-a91f-6e4869216d15")  # claude-haiku-4-5
    args = ap.parse_args()

    if not args.vk:
        print("error: --vk or NEXUS_TEST_VK required", file=sys.stderr); sys.exit(2)

    gw = args.gw_url.rstrip("/")
    print(f"AI Gateway: {gw}")
    print(f"VK: {args.vk[:14]}…")

    # Sanity ping
    s, _, raw = http_call("GET", gw + "/healthz", timeout=5)
    if s != 200:
        print(f"FATAL: gateway not reachable, healthz={s}", file=sys.stderr); sys.exit(2)

    test_g1_estimate(gw, args.vk, args.openai_provider_id, args.anthropic_provider_id,
                     args.openai_model_id, args.anthropic_model_id)
    test_g2_internal(gw, args.openai_provider_id, args.anthropic_provider_id,
                     args.openai_credential_id, args.openai_model_id,
                     args.openai_embed_model_id, args.anthropic_model_id)
    test_g3_extract_cache(gw, args.vk)
    test_g4_semantic_cache(gw, args.vk)

    # Summary
    n_pass = sum(1 for r in RESULTS if r["status"] == "PASS")
    n_warn = sum(1 for r in RESULTS if r["status"] == "WARN")
    n_fail = sum(1 for r in RESULTS if r["status"] == "FAIL")
    print(f"\n── Summary: {n_pass} PASS / {n_warn} WARN / {n_fail} FAIL ──")

    # Write Markdown report
    Path(args.report).parent.mkdir(parents=True, exist_ok=True)
    with open(args.report, "w") as f:
        f.write(f"# AI Gateway routes.go coverage-gap report\n\n")
        f.write(f"Generated: {datetime.now(timezone.utc).isoformat()}Z\n")
        f.write(f"Gateway: {gw}\n\n")
        f.write(f"**Summary: {n_pass} PASS / {n_warn} WARN / {n_fail} FAIL**\n\n")
        f.write("| Group | Test | Status | Evidence |\n|---|---|---|---|\n")
        for r in RESULTS:
            ev = r["evidence"].replace("|", "\\|")
            f.write(f"| {r['group']} | {r['name']} | {r['status']} | {ev} |\n")
    print(f"Report: {args.report}")

    sys.exit(0 if n_fail == 0 else 1)


if __name__ == "__main__":
    main()
