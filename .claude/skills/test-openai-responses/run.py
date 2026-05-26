#!/usr/bin/env python3
"""/test-openai-responses runner — drives 5 (or 8 with --cross-format)
arms against POST /v1/responses on the local AI Gateway, cross-checks
the resulting traffic_event DB rows, and emits a Markdown report.

Standalone — no nexus-gateway Python deps beyond the stdlib. Drives
HTTP / SSE directly via http.client; queries Postgres via the running
docker container (mirrors smoke-gateway.py / tests/lib/auth.sh).
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
import urllib.parse
from dataclasses import dataclass, field
from datetime import datetime, timezone
from http.client import HTTPConnection, HTTPSConnection
from typing import Any, Optional


# ─── HTTP helper ──────────────────────────────────────────────────────────────


def _conn(url: str, timeout: int = 120):
    parsed = urllib.parse.urlparse(url)
    host = parsed.hostname
    port = parsed.port or (443 if parsed.scheme == "https" else 80)
    if parsed.scheme == "https":
        import ssl
        return HTTPSConnection(host, port, timeout=timeout, context=ssl._create_unverified_context())
    return HTTPConnection(host, port, timeout=timeout)


def _auth_headers(vk: str, extra: Optional[dict] = None) -> dict:
    h = {"Authorization": f"Bearer {vk}", "Content-Type": "application/json"}
    if extra:
        h.update(extra)
    return h


def responses_non_stream(gw_url: str, vk: str, body: dict, timeout: int = 90) -> dict:
    """POST /v1/responses non-stream, returns {status, data, elapsed, request_id}."""
    payload = json.dumps(body).encode()
    c = _conn(gw_url, timeout)
    t0 = time.time()
    try:
        c.request("POST", "/v1/responses", payload, _auth_headers(vk))
        r = c.getresponse()
        raw = r.read().decode("utf-8", errors="replace")
        elapsed = time.time() - t0
        request_id = r.getheader("x-nexus-request-id", "") or r.getheader("X-Nexus-Request-Id", "")
        c.close()
    except Exception as e:
        return {"status": 0, "error": str(e), "elapsed": time.time() - t0}
    try:
        data = json.loads(raw)
    except Exception:
        data = {"_raw": raw[:1500]}
    return {"status": r.status, "data": data, "elapsed": elapsed, "raw": raw, "request_id": request_id}


def responses_stream(gw_url: str, vk: str, body: dict, timeout: int = 120) -> dict:
    """POST /v1/responses stream=true, returns full event log + last response payload."""
    body = {**body, "stream": True}
    payload = json.dumps(body).encode()
    c = _conn(gw_url, timeout)
    t0 = time.time()
    ttfb: Optional[float] = None
    try:
        c.request("POST", "/v1/responses", payload, _auth_headers(vk, {"Accept": "text/event-stream"}))
        r = c.getresponse()
        events = []
        content_parts = []
        completed_seen = False
        failed_seen = False
        usage = None
        last_response = None
        current_event = None
        request_id = r.getheader("x-nexus-request-id", "") or r.getheader("X-Nexus-Request-Id", "")
        for raw_line in r:
            if ttfb is None:
                ttfb = time.time() - t0
            line = raw_line.decode("utf-8", errors="replace").rstrip("\n")
            if line.startswith("event: "):
                current_event = line[7:].strip()
                continue
            if not line.startswith("data: "):
                continue
            data_str = line[6:].strip()
            try:
                payload_obj = json.loads(data_str)
            except Exception:
                continue
            evtype = current_event or payload_obj.get("type", "")
            events.append({"event": evtype, "data": payload_obj})
            if evtype == "response.output_text.delta":
                content_parts.append(payload_obj.get("delta", ""))
            if evtype == "response.completed":
                completed_seen = True
                last_response = payload_obj.get("response", {})
                usage = last_response.get("usage") if last_response else None
            if evtype == "response.failed":
                failed_seen = True
                last_response = payload_obj.get("response", {})
            current_event = None
        elapsed = time.time() - t0
        c.close()
        return {
            "status": r.status,
            "elapsed": elapsed,
            "ttfb": ttfb,
            "events": events,
            "completed_seen": completed_seen,
            "failed_seen": failed_seen,
            "content": "".join(content_parts),
            "usage": usage,
            "last_response": last_response,
            "request_id": request_id,
        }
    except Exception as e:
        return {"status": 0, "error": str(e), "elapsed": time.time() - t0}


# ─── DB helper ────────────────────────────────────────────────────────────────


def _postgres_container() -> str:
    out = subprocess.check_output(
        "docker ps --filter name=postgres -q | head -1",
        shell=True, text=True, timeout=10,
    ).strip()
    if not out:
        raise RuntimeError("no postgres container running")
    return out


def query_traffic_event(request_id: str) -> dict:
    """Returns {id, endpoint_type, prompt_tokens, completion_tokens,
                 reasoning_tokens, prompt_cache_tokens, status_code} or {}."""
    if not request_id:
        return {}
    try:
        cid = _postgres_container()
    except Exception as e:
        return {"_db_error": str(e)}
    # request_id appears in the id column or in trace_id depending on the
    # codepath; query both for robustness.
    sql = (
        "SELECT id, endpoint_type, COALESCE(prompt_tokens, 0), "
        "COALESCE(completion_tokens, 0), COALESCE(reasoning_tokens, 0), "
        "COALESCE(prompt_cache_tokens, 0), status_code "
        "FROM traffic_event WHERE id = '%s' OR trace_id = '%s' "
        "ORDER BY created_at DESC LIMIT 1;"
    ) % (request_id, request_id)
    out = subprocess.run(
        ["docker", "exec", cid, "psql", "-U", "postgres", "-d", "nexus_gateway",
         "-X", "-A", "-t", "-F", "|", "-c", sql],
        capture_output=True, text=True, timeout=15,
    )
    if out.returncode != 0:
        return {"_db_error": out.stderr.strip()[:400]}
    line = out.stdout.strip()
    if not line:
        return {}
    parts = line.split("|")
    if len(parts) < 7:
        return {"_raw": line}
    return {
        "id": parts[0],
        "endpoint_type": parts[1],
        "prompt_tokens": int(parts[2] or 0),
        "completion_tokens": int(parts[3] or 0),
        "reasoning_tokens": int(parts[4] or 0),
        "prompt_cache_tokens": int(parts[5] or 0),
        "status_code": int(parts[6] or 0),
    }


# ─── Metric helper ────────────────────────────────────────────────────────────


def fetch_responses_counter(gw_url: str) -> int:
    """Returns the current value of ai_gateway_request_total for
    endpoint_type="responses" labels — summed across all label combos
    matching that endpoint_type. Returns 0 if not yet observed."""
    c = _conn(gw_url, 10)
    try:
        c.request("GET", "/metrics", b"", {})
        r = c.getresponse()
        body = r.read().decode("utf-8", errors="replace")
        c.close()
    except Exception:
        return 0
    total = 0
    for line in body.splitlines():
        if not line.startswith("ai_gateway_request_total{"):
            continue
        if 'endpoint_type="responses"' not in line:
            continue
        try:
            total += float(line.rsplit(" ", 1)[-1])
        except Exception:
            pass
    return int(total)


# ─── Test arms ────────────────────────────────────────────────────────────────


@dataclass
class ArmResult:
    name: str
    passed: bool
    http_status: int
    elapsed: float
    request_id: str = ""
    db_row: dict = field(default_factory=dict)
    notes: list[str] = field(default_factory=list)
    diagnostic: str = ""


def arm_text_non_stream(gw_url: str, vk: str, model: str) -> ArmResult:
    body = {"model": model, "input": "haiku about clouds", "max_output_tokens": 40}
    if model.startswith(("o1", "o3", "o4")):
        # Reasoning models do not accept temperature; the body skip is
        # handled by the gateway codec, but we also drop the implicit 0
        # by NOT passing it. Nothing extra needed here.
        pass
    res = responses_non_stream(gw_url, vk, body)
    notes = []
    passed = True
    if res.get("status") != 200:
        passed = False
        notes.append(f"http status={res.get('status')} err={res.get('error', '-')}")
        return ArmResult("text non-stream", passed, res.get("status", 0), res.get("elapsed", 0), "", {}, notes,
                         diagnostic=res.get("raw", "")[:400])
    data = res["data"]
    if data.get("object") != "response":
        passed = False; notes.append(f"object={data.get('object')!r} (want response)")
    if data.get("status") not in ("completed", "incomplete"):
        passed = False; notes.append(f"status={data.get('status')!r}")
    if not isinstance(data.get("output"), list) or not data["output"]:
        passed = False; notes.append("output[] empty")
    usage = data.get("usage", {})
    if not isinstance(usage, dict) or usage.get("input_tokens", 0) <= 0:
        passed = False; notes.append(f"usage.input_tokens={usage.get('input_tokens') if isinstance(usage, dict) else 'NA'}")
    return ArmResult("text non-stream", passed, res["status"], res["elapsed"], res.get("request_id", ""), notes=notes)


def arm_text_sse(gw_url: str, vk: str, model: str) -> ArmResult:
    body = {"model": model, "input": "haiku about clouds", "max_output_tokens": 40}
    res = responses_stream(gw_url, vk, body)
    notes = []
    passed = True
    if res.get("status") != 200:
        passed = False
        notes.append(f"http status={res.get('status')} err={res.get('error', '-')}")
        return ArmResult("text SSE", passed, res.get("status", 0), res.get("elapsed", 0), "", {}, notes)
    if not res.get("completed_seen"):
        passed = False; notes.append("no response.completed event")
    if res.get("failed_seen"):
        passed = False; notes.append("response.failed observed mid-stream")
    if not any(e["event"] == "response.output_text.delta" for e in res["events"]):
        passed = False; notes.append("no response.output_text.delta")
    return ArmResult("text SSE", passed, res["status"], res["elapsed"], res.get("request_id", ""), notes=notes)


def arm_function_sse(gw_url: str, vk: str, model: str) -> ArmResult:
    body = {
        "model": model,
        "input": "What is the weather in Tokyo?",
        "max_output_tokens": 100,
        "tools": [{
            "type": "function",
            "name": "get_weather",
            "description": "Get the current weather for a city.",
            "parameters": {
                "type": "object",
                "properties": {"city": {"type": "string"}},
                "required": ["city"],
            },
        }],
    }
    res = responses_stream(gw_url, vk, body)
    notes = []
    passed = True
    if res.get("status") != 200:
        passed = False
        notes.append(f"http status={res.get('status')} err={res.get('error', '-')}")
        return ArmResult("function-call SSE", passed, res.get("status", 0), res.get("elapsed", 0), "", {}, notes)
    if not res.get("completed_seen"):
        passed = False; notes.append("no response.completed event")
    saw_function = any(
        e["event"] == "response.function_call_arguments.delta"
        for e in res["events"]
    )
    if not saw_function:
        # Some models may decline to call the function on tiny prompts;
        # surface as a soft note instead of fail (test the wire path,
        # not the model's choice).
        notes.append("no response.function_call_arguments.delta (model may have answered without calling)")
    return ArmResult("function-call SSE", passed, res["status"], res["elapsed"], res.get("request_id", ""), notes=notes)


def arm_structured_outputs(gw_url: str, vk: str, model: str) -> ArmResult:
    body = {
        "model": model,
        "input": "Meeting in Tokyo on March 5",
        "max_output_tokens": 100,
        "text": {
            "format": {
                "type": "json_schema",
                "json_schema": {
                    "name": "meeting_extract",
                    "schema": {
                        "type": "object",
                        "properties": {
                            "city": {"type": "string"},
                            "date": {"type": "string"},
                        },
                        "required": ["city", "date"],
                    },
                },
            },
        },
    }
    res = responses_non_stream(gw_url, vk, body)
    notes = []
    passed = True
    if res.get("status") != 200:
        passed = False
        notes.append(f"http status={res.get('status')} err={res.get('error', '-')}")
        return ArmResult("structured outputs", passed, res.get("status", 0), res.get("elapsed", 0), "", {}, notes,
                         diagnostic=res.get("raw", "")[:400])
    data = res["data"]
    output = data.get("output", [])
    text = ""
    for item in output:
        if item.get("type") == "message":
            for part in item.get("content", []):
                if part.get("type") == "output_text":
                    text += part.get("text", "")
    if not text:
        passed = False; notes.append("no output_text content")
    else:
        try:
            obj = json.loads(text)
        except Exception:
            passed = False; notes.append(f"output_text is not JSON: {text[:80]!r}")
            obj = {}
        if isinstance(obj, dict):
            if "city" not in obj:
                passed = False; notes.append("JSON missing 'city'")
            if "date" not in obj:
                passed = False; notes.append("JSON missing 'date'")
    return ArmResult("structured outputs", passed, res["status"], res["elapsed"], res.get("request_id", ""), notes=notes)


def arm_reasoning_high(gw_url: str, vk: str, model: str) -> ArmResult:
    body = {
        "model": model,
        "input": "Prove that sqrt(2) is irrational.",
        "max_output_tokens": 400,
        "reasoning": {"effort": "high"},
    }
    res = responses_non_stream(gw_url, vk, body)
    notes = []
    passed = True
    if res.get("status") != 200:
        passed = False
        notes.append(f"http status={res.get('status')} err={res.get('error', '-')}")
        return ArmResult("reasoning effort=high", passed, res.get("status", 0), res.get("elapsed", 0), "", {}, notes,
                         diagnostic=res.get("raw", "")[:400])
    data = res["data"]
    usage = data.get("usage", {}) or {}
    # Either there is a reasoning item in output[] or reasoning_tokens
    # in usage.output_tokens_details. Some non-reasoning models still
    # accept the field but return 0; soft note rather than fail.
    has_reasoning_item = any(it.get("type") == "reasoning" for it in data.get("output", []))
    rtokens = (usage.get("output_tokens_details") or {}).get("reasoning_tokens", 0)
    if not has_reasoning_item and rtokens == 0:
        notes.append("no reasoning item nor reasoning_tokens — model may be non-reasoning")
    return ArmResult("reasoning effort=high", passed, res["status"], res["elapsed"], res.get("request_id", ""), notes=notes)


def arm_cross_format_previous_response_id_rejected(gw_url: str, vk: str, model: str) -> ArmResult:
    """Cross-format arm — requires the routing rule to resolve to a
    non-OpenAI provider. Hand-crafted bodies with stateful fields must
    be rejected with HTTP 400 + the documented error envelope.
    """
    body = {
        "model": model,
        "input": "hello",
        "previous_response_id": "resp_fake",
    }
    res = responses_non_stream(gw_url, vk, body)
    notes = []
    passed = True
    if res.get("status") != 400:
        passed = False; notes.append(f"want 400 got {res.get('status')}")
        return ArmResult("cross-format previous_response_id rejection", passed, res.get("status", 0), res.get("elapsed", 0), "", {}, notes,
                         diagnostic=res.get("raw", "")[:400])
    err = (res.get("data") or {}).get("error") or {}
    if err.get("code") != "feature_requires_native_responses_target":
        passed = False; notes.append(f"error.code={err.get('code')!r} (want feature_requires_native_responses_target)")
    if err.get("param") != "previous_response_id":
        passed = False; notes.append(f"error.param={err.get('param')!r}")
    return ArmResult("cross-format previous_response_id rejection", passed, res["status"], res["elapsed"], res.get("request_id", ""), notes=notes)


# ─── Driver ───────────────────────────────────────────────────────────────────


def db_crosscheck(arm: ArmResult) -> None:
    """Populates arm.db_row + appends notes when the row is missing or
    its endpoint_type is wrong. Waits ≤5s for the row to materialise
    (audit pipeline is async)."""
    if not arm.request_id:
        return
    for _ in range(10):
        row = query_traffic_event(arm.request_id)
        if row and "id" in row:
            arm.db_row = row
            if row.get("endpoint_type") != "responses":
                arm.notes.append(f"traffic_event.endpoint_type={row.get('endpoint_type')!r} (want 'responses')")
                arm.passed = False
            return
        time.sleep(0.5)
    arm.notes.append("traffic_event row not observed within 5s")


def main():
    ap = argparse.ArgumentParser(description="/test-openai-responses runner")
    ap.add_argument("--vk", required=True)
    ap.add_argument("--model", default="gpt-5.2")
    ap.add_argument("--gw-url", default="http://localhost:3050")
    ap.add_argument("--cp-url", default="http://localhost:3001")
    ap.add_argument("--cp-user", default="admin@nexus.ai")
    ap.add_argument("--cp-pass", default="admin123")
    ap.add_argument("--cross-format", action="store_true")
    ap.add_argument("--report", default=f"/tmp/test-openai-responses-{datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ')}.md")
    args = ap.parse_args()

    print(f"[/test-openai-responses] starting → {args.report}")
    print(f"  gw={args.gw_url} model={args.model} vk={args.vk[:12]}…")

    m0 = fetch_responses_counter(args.gw_url)
    arms: list[ArmResult] = []

    arms.append(arm_text_non_stream(args.gw_url, args.vk, args.model))
    arms.append(arm_text_sse(args.gw_url, args.vk, args.model))
    arms.append(arm_function_sse(args.gw_url, args.vk, args.model))
    arms.append(arm_structured_outputs(args.gw_url, args.vk, args.model))
    arms.append(arm_reasoning_high(args.gw_url, args.vk, args.model))

    if args.cross_format:
        arms.append(arm_cross_format_previous_response_id_rejected(args.gw_url, args.vk, args.model))

    for arm in arms:
        if arm.http_status == 200:
            db_crosscheck(arm)

    m1 = fetch_responses_counter(args.gw_url)
    delta = m1 - m0
    expected_delta = sum(1 for a in arms if a.http_status == 200)

    # ─── Write report ────────────────────────────────────────────────────
    lines = []
    lines.append(f"# /test-openai-responses — {datetime.now(timezone.utc).isoformat()}")
    lines.append("")
    lines.append("## Summary")
    lines.append(f"- AI Gateway: `{args.gw_url}`")
    lines.append(f"- Model: `{args.model}`")
    lines.append(f"- Total arms: {len(arms)}")
    lines.append(f"- Passed: {sum(1 for a in arms if a.passed)}")
    lines.append(f"- Failed: {sum(1 for a in arms if not a.passed)}")
    lines.append(f"- Prometheus `ai_gateway_request_total{{endpoint_type=\"responses\"}}` delta: {delta} (expected ≥ {expected_delta})")
    lines.append("")
    for i, a in enumerate(arms, 1):
        lines.append(f"## Arm {i} — {a.name}")
        lines.append(f"- Status: **{'PASS' if a.passed else 'FAIL'}**")
        lines.append(f"- HTTP: {a.http_status}")
        lines.append(f"- Elapsed: {a.elapsed:.2f}s")
        lines.append(f"- request_id: `{a.request_id or '-'}`")
        if a.db_row:
            lines.append(f"- traffic_event.endpoint_type: `{a.db_row.get('endpoint_type', '?')}`")
            lines.append(f"- prompt_tokens: {a.db_row.get('prompt_tokens')}, completion_tokens: {a.db_row.get('completion_tokens')}, "
                         f"reasoning_tokens: {a.db_row.get('reasoning_tokens')}, prompt_cache_tokens: {a.db_row.get('prompt_cache_tokens')}")
        for n in a.notes:
            lines.append(f"- Note: {n}")
        if a.diagnostic and not a.passed:
            lines.append("```")
            lines.append(a.diagnostic)
            lines.append("```")
        lines.append("")
    overall = "PASS" if all(a.passed for a in arms) else "FAIL"
    lines.append(f"## Conclusion: **{overall}** ({sum(1 for a in arms if not a.passed)} issue(s))")

    with open(args.report, "w") as f:
        f.write("\n".join(lines))
    print(f"[/test-openai-responses] {overall} — report at {args.report}")
    sys.exit(0 if overall == "PASS" else 1)


if __name__ == "__main__":
    main()
