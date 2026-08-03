#!/usr/bin/env python3
"""Live regression arms for the compliance-proxy CONNECT+bump path.

Why this exists: the compliance proxy's hot path is not in packages/compliance-proxy,
it is packages/shared/transport/{tlsbump,streaming}. This branch changed 24 files
there, and unit tests cannot prove the wiring. Each arm below names the changed
area it exercises and the observation that proves it, so a pass is a specific
column or field rather than "no error".

Usage:
    python3 tests/scripts/bump-regression.py            # all arms
    python3 tests/scripts/bump-regression.py --arm sse  # one arm

Requires: the local stack up, the proxy on :3128, and OPENAI_API_KEY in
tests/.env.local. Reads nothing from the network except the provider itself.
"""

from __future__ import annotations

import argparse
import collections
import json
import os
import subprocess
import sys
import time
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
CA = os.path.join(ROOT, "packages", "compliance-proxy", "dev-certs", "ca.crt")
PROXY = os.environ.get("NEXUS_PROXY_URL", "http://127.0.0.1:3128")
PG = ("nexus-postgres", "postgres", "nexus_gateway")

PASS, FAIL, SKIP = "PASS", "FAIL", "SKIP"
results: list[tuple[str, str, str]] = []


def record(arm: str, verdict: str, evidence: str) -> None:
    results.append((arm, verdict, evidence))
    mark = {PASS: "\033[32m✓\033[0m", FAIL: "\033[31m✗\033[0m", SKIP: "\033[33m~\033[0m"}[verdict]
    print(f"  {mark} {arm}: {evidence}")


def load_env() -> None:
    path = os.path.join(ROOT, "tests", ".env.local")
    if not os.path.exists(path):
        sys.exit(f"missing {path}")
    for line in open(path):
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            k, v = line.split("=", 1)
            # OVERRIDE, not setdefault. tests/.env.local is the authority for a
            # test run; setdefault silently defers to whatever the ambient shell
            # already exported, which is how this harness spent three rounds
            # reporting 401 "Incorrect API key" against a stale key while the
            # same request from a shell that sourced this file returned 200.
            os.environ[k.strip()] = v.strip().strip('"').strip("'")


def sql(query: str) -> str:
    """One-shot psql through the container; returns the raw tuple-only output."""
    out = subprocess.run(
        ["docker", "exec", "-e", "PGPASSWORD=postgres", PG[0],
         "psql", "-U", PG[1], "-d", PG[2], "-tAc", query],
        capture_output=True, text=True, timeout=30,
    )
    return out.stdout.strip()


# The client is curl, deliberately. urllib's ProxyHandler returned 401 from the
# provider where curl returned 200 through the SAME proxy, in the same second,
# with byte-identical key material — so the difference was urllib's request
# construction, not the proxy. A test harness whose client behaves differently
# from every real client is not measuring the thing under test.
CURL_BASE = ["curl", "-sS", "--proxy", PROXY, "--cacert", CA]


def curl_chat(body: dict, timeout: int = 90, stream: bool = False) -> tuple[int, bytes]:
    """POST to the provider THROUGH the proxy; returns (status, body)."""
    args = CURL_BASE + [
        "-m", str(timeout),
        "-o", "-", "-w", "\n%{http_code}",
        "-X", "POST", "https://api.openai.com/v1/chat/completions",
        "-H", f"Authorization: Bearer {os.environ['OPENAI_API_KEY']}",
        "-H", "Content-Type: application/json",
        "-d", json.dumps(body),
    ]
    if stream:
        args += ["-H", "Accept: text/event-stream", "--no-buffer"]
    out = subprocess.run(args, capture_output=True, timeout=timeout + 30)
    raw = out.stdout
    nl = raw.rfind(b"\n")
    if nl == -1:
        return 0, raw
    return int(raw[nl + 1:] or 0), raw[:nl]


def chat(body: dict, timeout: int = 90) -> tuple[int, bytes]:
    return curl_chat(body, timeout)


def marker(tag: str) -> str:
    """A per-request token the audit row can be found by."""
    return f"{tag}-{int(time.time() * 1000)}"


def row_for_marker(cols: str, mark: str, timeout: float = 90) -> str:
    """Find THIS request's audit row by a token the request itself carried.

    Replaces a "sleep N, then take the newest row" helper, which is the single
    mistake this program made most often: audit is an async side path, so the newest
    row is whatever landed last, not the one you asked about. It produced three wrong
    conclusions — twice reading a pre-fix row as evidence a fix had failed, once
    reading NULL tokens off a different request entirely.

    Two details that are not optional:
      * inline_request_body is BYTEA. Casting it with ::text yields the hex encoding,
        so a LIKE on the marker matches nothing and the row looks lost. encode(...,
        'escape') is used because it cannot throw on a non-UTF8 body the way
        convert_from can, which would abort the whole query.
      * It POLLS instead of sleeping a fixed interval. Measured settle time on this
        host ranges from ~2 s idle to >13 s after a few hundred concurrent requests
        drain through NATS and the Hub consumer — a fixed sleep turns that ordinary
        back-pressure into a spurious "row missing" failure.
    """
    q = (f"SELECT {cols} FROM traffic_event te "
         "JOIN traffic_event_payload p ON p.traffic_event_id = te.id "
         "WHERE te.source='compliance-proxy' "
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


# --------------------------------------------------------------------------
# Arms
# --------------------------------------------------------------------------

def arm_nonstream() -> None:
    """tlsbump/forward_*.go + audit emitter: a bumped request produces an audit row.

    Proves: the CONNECT+bump path completes, and C-3/C-34's upstream timings are
    stamped (they were the sibling program's deferred-audit-row change).
    """
    mark = marker("BUMP-NS")
    code, raw = chat({"model": "gpt-4o-mini",
                      "messages": [{"role": "user",
                                    "content": f"{mark} reply with the single word BUMP"}],
                      "max_tokens": 5})
    if code != 200:
        record("nonstream", FAIL, f"HTTP {code}: {raw[:160]!r}")
        return
    body = json.loads(raw)
    content = body["choices"][0]["message"]["content"]

    row = row_for_marker(
        "te.status_code||'|'||COALESCE(te.upstream_ttfb_ms::text,'NULL')||'|'"
        "||COALESCE(te.upstream_total_ms::text,'NULL')||'|'||COALESCE(te.latency_ms::text,'NULL')", mark)
    if not row:
        record("nonstream", FAIL,
               f"reply {content!r} but no compliance-proxy traffic_event carried marker {mark} within 90s")
        return
    status, ttfb, total, latency = row.split("|")
    if ttfb == "NULL" or total == "NULL":
        record("nonstream", FAIL, f"row exists but upstream timings NULL (ttfb={ttfb} total={total}) — C-34 regressed")
        return
    ok_lat = latency != "NULL" and int(latency) >= int(total)
    record("nonstream", PASS if ok_lat else FAIL,
           f"reply={content!r} status={status} upstream_ttfb={ttfb}ms upstream_total={total}ms "
           f"latency={latency}ms (latency>=upstream_total: {ok_lat})")


def arm_sse() -> None:
    """streaming/{parser,live,serialize,locked_buffer}.go + tlsbump/sse*.go.

    Proves the whole SSE chain the sibling program rewrote (C-30 reader-goroutine
    removal, C-12 pooled scan buffer, C-21 serializer, C-20 usage accumulator):
    frames arrive, [DONE] terminates, and the usage trailer survives.
    """
    mark = marker("BUMP-SSE")
    frames, saw_done, usage = 0, False, None
    text = []
    try:
        code, raw = curl_chat({
            "model": "gpt-4o-mini",
            "messages": [{"role": "user",
                          "content": f"{mark} count from 1 to 8, one number per line"}],
            "stream": True, "stream_options": {"include_usage": True}, "max_tokens": 60,
        }, timeout=120, stream=True)
        if code != 200:
            record("sse", FAIL, f"HTTP {code}: {raw[:160]!r}")
            return
        for line in raw.splitlines():
            t = line.decode("utf-8", "replace").strip()
            if not t.startswith("data:"):
                continue
            payload = t[5:].strip()
            if payload == "[DONE]":
                saw_done = True
                continue
            frames += 1
            try:
                ev = json.loads(payload)
            except Exception:
                continue
            if ev.get("usage"):
                usage = ev["usage"]
            for ch in ev.get("choices") or []:
                text.append((ch.get("delta") or {}).get("content") or "")
    except Exception as e:
        record("sse", FAIL, f"stream error after {frames} frames: {e}")
        return

    joined = "".join(text)
    problems = []
    if frames < 3:
        problems.append(f"only {frames} frames")
    if not saw_done:
        problems.append("no [DONE] terminator")
    if usage is None:
        problems.append("usage trailer missing (C-20 accumulator)")
    if "8" not in joined:
        problems.append(f"content looks truncated: {joined[:60]!r}")

    # The audit row is an assertion, not a note. It is the only evidence that S-13's
    # redact-gate fix holds on the STREAMING path — the proxy persisted no response
    # body at all before it — and the 5b checklist cites exactly these columns. It
    # previously reported "no-row" without failing, which is how a NULL that was
    # really the recency trap got read as a defect.
    row = row_for_marker(
        "COALESCE(te.prompt_tokens::text,'NULL')||'|'||COALESCE(te.completion_tokens::text,'NULL')"
        "||'|'||COALESCE(p.response_size_bytes::text,'NULL')", mark)
    if not row:
        problems.append(f"no audit row carried marker {mark} within 90s")
        tokens, rbytes = "no-row", "no-row"
    else:
        ptok, ctok, rbytes = row.split("|")
        tokens = f"{ptok}|{ctok}"
        if ptok == "NULL" or ctok == "NULL":
            problems.append(f"audit row has NULL usage ({tokens}) — the SSE usage trailer "
                            "was parsed on the wire but never reached the row")
        if rbytes == "NULL" or int(rbytes) <= 0:
            problems.append(f"audit row carries no response body (response_size_bytes={rbytes}) "
                            "— S-13 regressed on the streaming path")

    if problems:
        record("sse", FAIL, "; ".join(problems))
    else:
        record("sse", PASS,
               f"{frames} frames, [DONE] seen, usage={usage.get('total_tokens')} tok, "
               f"content len={len(joined)}, audit tokens(prompt|completion)={tokens}, "
               f"response_size_bytes={rbytes}")


def arm_large_body() -> None:
    """shared/transport/bodyread (C-11) + spillstore emit.

    Proves the bounded reader returns the WHOLE body — the 160x-amplification fix
    bounded growth to a doubling, and a regression there shows up as a truncated
    or mis-sized captured request body.
    """
    mark = marker("BUMP-LG")
    filler = "The quick brown fox. " * 3000          # ~63 KB of prompt
    sent = {"model": "gpt-4o-mini",
            "messages": [{"role": "user",
                          "content": f"{mark} " + filler + "\nReply with the word LONG only."}],
            "max_tokens": 5}
    sent_bytes = len(json.dumps(sent).encode())
    code, raw = chat(sent)
    if code != 200:
        record("large-body", FAIL, f"HTTP {code}: {raw[:160]!r}")
        return
    row = row_for_marker(
        "COALESCE(te.prompt_tokens::text,'NULL')||'|'||COALESCE(te.status_code::text,'NULL')", mark)
    if not row:
        record("large-body", FAIL, f"sent {sent_bytes}B, no audit row carried marker {mark} within 90s")
        return
    ptok, status = row.split("|")
    # ~63 KB of English is well over 10k tokens; a truncated capture would show far fewer.
    ok = ptok != "NULL" and int(ptok) > 5000
    record("large-body", PASS if ok else FAIL,
           f"sent {sent_bytes}B request, audit status={status} prompt_tokens={ptok} "
           f"(>5000 means the full body reached the tokenizer, not a truncated prefix)")


def arm_scan_scale() -> None:
    """shared/policy/hooks/matcher (re2.go) — the parallel per-pattern scan.

    The RE2 matcher now fans its per-pattern scans out across cores once the work
    is large enough (measured flip point: >=4 patterns and >=2048 byte x pattern
    units). That cut a 400 KB body's added proxy latency from ~3 s to ~0.4 s, and
    the ONLY way it can be wrong is by changing a verdict at scale: a merge that
    drops a slot means "this rule stops firing on big bodies", which no unit test
    on a 30-byte fixture would notice.

    So this arm sends the SAME sensitive value twice — once in a tiny body, once in
    a ~200 KB body with the value at the very END (past every early-exit and any
    truncated prefix) — and requires identical treatment: same hook decision, and
    in both stored bodies the raw value absent while the marker survives.

    200 KB, not 400: the lookup finds a row by its marker inside
    inline_request_body, and past the 256 KiB threshold the body spills and the
    inline column is empty (that path is arm_spill_readback's job). 200 KB x 423
    seeded rules is far above the fan-out threshold, which is what this arm is for.
    """
    ssn = "412-88-7391"
    cols = ("COALESCE(te.request_hook_decision,'NULL')||'|'||"
            "COALESCE(te.request_hook_reason_code,'NULL')||'|'||"
            "position('" + ssn + "' in encode(p.inline_request_body,'escape'))::text")

    outcomes = {}
    for label, filler in (("tiny", ""), ("200kb", "Quarterly notes, nothing sensitive. " * 5800)):
        mark = marker("BUMP-SCALE-" + label)
        # The sensitive value goes LAST so a scan that stops early or reads a
        # prefix would miss it — the failure this arm is built to catch.
        body = {"model": "gpt-4o-mini", "max_tokens": 5,
                "messages": [{"role": "user",
                              "content": f"{mark} {filler}my ssn is {ssn}"}]}
        sent = len(json.dumps(body).encode())
        code, raw = chat(body)
        if code not in (200, 400, 403, 451):
            record("scan-scale", FAIL, f"{label}: unexpected HTTP {code}: {raw[:160]!r}")
            return
        row = row_for_marker(cols, mark)
        if not row:
            record("scan-scale", FAIL, f"{label}: sent {sent}B, no audit row carried {mark} within 90s")
            return
        decision, reason, ssn_pos = row.split("|")
        outcomes[label] = (decision, reason, ssn_pos, sent, code)

    tiny, big = outcomes["tiny"], outcomes["200kb"]
    # The raw value must be absent from BOTH stored bodies. A non-zero position is
    # a live leak, not a test detail.
    leaks = [lbl for lbl, o in outcomes.items() if o[2] != "0"]
    same = tiny[0] == big[0] and tiny[1] == big[1]
    verdict = PASS if same and not leaks else FAIL
    record("scan-scale", verdict,
           f"tiny({tiny[3]}B, HTTP {tiny[4]}) decision={tiny[0]}/{tiny[1]} ssn_pos={tiny[2]} vs "
           f"200kb({big[3]}B, HTTP {big[4]}) decision={big[0]}/{big[1]} ssn_pos={big[2]}"
           + ("" if same else " — DECISIONS DIVERGED WITH SIZE")
           + ("" if not leaks else f" — RAW VALUE PRESENT IN STORED BODY: {leaks}"))


def arm_spill_readback() -> None:
    """spillstore write + the Control Plane read path, end to end (S-1/S-2/S-3).

    This arm used to claim "proves a spilled body is fetchable end to end" and then
    check only that some .bin files existed on disk with a size, never reading a
    body back through anything. It also counted spill refs across ALL sources, so
    its evidence came from the gateway: filtered to the proxy, the count was 0 of
    770 payload rows, and nothing here had ever spilled at all.

    Nothing spilled because nothing was big enough. The inline-vs-spill cutoff is
    payloadcapture.DefaultMaxInlineBodyBytes = 256 KiB and the large-body arm sends
    63 KB. So this arm now sends a body OVER the cutoff and follows it all the way
    back:

      1. a >256 KiB request through the bump, carrying a marker,
      2. the audit row must carry a request_spill_ref and NOT the inline body,
      3. GET /api/admin/traffic/:id must return that body with the marker intact.

    Step 3 is the one that matters. A spilled body whose ref is recorded but whose
    bytes cannot be read back is worse than no capture at all: the row promises an
    operator something the operator cannot get.

    The model is deliberately invalid so the upstream refuses immediately — the
    request body is captured and spilled regardless, and a 300 KB prompt that
    reached a real model would cost real tokens on every run.
    """
    mark = marker("spillarm")
    filler = "x" * 300_000
    body = {
        "model": "nexus-invalid-model-for-spill-probe",
        "messages": [{"role": "user", "content": f"{mark} {filler}"}],
        "max_tokens": 4,
    }
    status, _ = chat(body, timeout=120)
    if status == 0:
        record("spill-readback", FAIL, "request through the proxy produced no HTTP status")
        return

    row = row_for_marker(
        "te.id || '|' || coalesce(p.request_spill_ref->>'key','') || '|' || "
        "coalesce(p.request_size_bytes::text,'0') || '|' || "
        "coalesce(octet_length(p.inline_request_body)::text,'0')",
        mark, timeout=120)
    if not row:
        # The marker lives in the SPILLED object, not the inline column, so
        # row_for_marker cannot see it once spilling works. Fall back to the
        # newest proxy row carrying a request spill ref.
        row = sql("SELECT te.id || '|' || coalesce(p.request_spill_ref->>'key','') || '|' || "
                  "coalesce(p.request_size_bytes::text,'0') || '|' || "
                  "coalesce(octet_length(p.inline_request_body)::text,'0') "
                  "FROM traffic_event te JOIN traffic_event_payload p ON p.traffic_event_id = te.id "
                  "WHERE te.source='compliance-proxy' AND p.request_spill_ref IS NOT NULL "
                  "AND te.\"timestamp\" > NOW() - INTERVAL '10 minutes' "
                  "ORDER BY te.\"timestamp\" DESC LIMIT 1")
    if not row:
        record("spill-readback", FAIL,
               f"no compliance-proxy row with a request_spill_ref after a {len(json.dumps(body))}B request "
               f"(cutoff is 256 KiB) — the body was neither spilled nor recorded")
        return

    event_id, ref, size, inline_len = (row.split("|") + ["", "", "", ""])[:4]
    if not ref:
        record("spill-readback", FAIL,
               f"row {event_id[:8]} has no request_spill_ref for a {size}B body — it stayed inline past the cutoff")
        return
    if inline_len not in ("0", ""):
        record("spill-readback", FAIL,
               f"row {event_id[:8]} spilled to {ref} but ALSO kept {inline_len}B inline — "
               "the two-tier store must move the body, not copy it")
        return

    detail = cp_admin(f"/api/admin/traffic/{event_id}")
    if not detail:
        record("spill-readback", FAIL, f"GET /api/admin/traffic/{event_id} returned nothing")
        return
    if mark not in detail:
        record("spill-readback", FAIL,
               f"row {event_id[:8]} records spill_ref={ref} ({size}B) but GET /api/admin/traffic/:id "
               f"did not return the body — the marker is absent from {len(detail)}B of response. "
               "The ref promises an operator bytes they cannot read")
        return

    record("spill-readback", PASS,
           f"{size}B body spilled to {ref}, inline column empty, and read back through "
           f"GET /api/admin/traffic/{event_id[:8]} with the marker intact")


def arm_exemption() -> None:
    """compliance-proxy/internal/exemption/{match,store}.go (C-26) — persistence only.

    Scope stated honestly: this arm reads the DB table the grants live in. It proves
    the grant surface is queryable; it does NOT touch the proxy's in-memory store, so
    it cannot say anything about C-26's lock-free read path. The arm that does is
    exemption-rebuild below.
    """
    n = sql("SELECT count(*) FROM compliance_exemption_grant")
    if n == "":
        record("exemption", SKIP, "compliance_exemption_grant not queryable")
        return
    record("exemption", PASS if n.isdigit() else FAIL,
           f"compliance_exemption_grant queryable, {n} rows (persistence only — the in-proxy "
           "store is exercised by the exemption-rebuild arm)")


PROXY_LOG = os.path.join(ROOT, "packages", "compliance-proxy", "logs", "compliance-proxy.log")


def cp_admin(path: str, *args: str) -> str:
    """Call the Control Plane admin API through the repo's own auth helper.

    Deliberately shells out to tests/lib/auth.sh rather than reimplementing OAuth +
    PKCE here. This program has now been burned three times by a probe that
    reimplements production behaviour and is itself the thing that is wrong; the
    helper IS the real consumer of that flow.
    """
    script = (
        "set -e; export NEXUS_TEST_TARGET=local; "
        f"cd {ROOT!r}; . tests/lib/loadenv.sh >/dev/null; . tests/lib/auth.sh >/dev/null; "
        "cp_login >/dev/null 2>&1; "
        "cp_curl " + " ".join([path] + [f"{a!r}" for a in args])
    )
    out = subprocess.run(["bash", "-c", script], capture_output=True, text=True, timeout=90)
    return out.stdout.strip()


def arm_exemption_rebuild() -> None:
    """compliance-proxy/internal/exemption/store.go (C-26) under a CONCURRENT rebuild.

    C-26 replaced the store's sync.RWMutex with copy-on-write behind an atomic.Pointer:
    IsExempt loads a snapshot with no lock while Rebuild builds a new one and swaps it.
    The failure modes that shape can have are (a) a reader observing a snapshot that is
    not internally consistent, and (b) a swap disturbing an interception already in
    flight. Neither is reachable by the boot-load arm, because nothing swaps there.

    So this arm drives real bumped traffic CONCURRENTLY with two rebuilds pushed
    through the production path — CP admin API -> Hub shadow -> thingclient ->
    ApplyActiveExemptions -> Store.Rebuild — and correlates on the grant ID the
    request itself logs, never on recency.

    Four observations, each of which fails on its own:
      1. >=2 "exemption store rebuilt from shadow" lines land INSIDE the load window
         (the swaps really were concurrent with traffic, not before or after it);
      2. >=1 request logs exemptionId=<this grant>  (the granting snapshot was
         observed by the lock-free read path);
      3. >=1 request after the revoke logs no exemption (the empty snapshot was
         observed too — so the swap propagated in both directions);
      4. every request returned 200 (no interception was broken by either swap).
    """
    import threading

    if not os.path.exists(PROXY_LOG):
        record("exemption-rebuild", SKIP, f"proxy structured log absent at {PROXY_LOG}")
        return
    offset = os.path.getsize(PROXY_LOG)

    stop = threading.Event()
    codes: list[int] = []
    codes_lock = threading.Lock()

    def worker() -> None:
        while not stop.is_set():
            code, _ = curl_chat({"model": "gpt-4o-mini",
                                 "messages": [{"role": "user", "content": "say C26"}],
                                 "max_tokens": 5}, timeout=45)
            with codes_lock:
                codes.append(code)

    threads = [threading.Thread(target=worker, daemon=True) for _ in range(4)]
    for t in threads:
        t.start()

    grant_id = ""
    try:
        time.sleep(4)                                   # let traffic reach steady state
        resp = cp_admin("/api/admin/compliance/exemption-grants", "-X", "POST",
                        "-H", "Content-Type: application/json",
                        "-d", json.dumps({"sourceIP": "127.0.0.1",
                                          "targetHost": "api.openai.com",
                                          "durationMinutes": 10,
                                          "reason": "bump-regression C-26 concurrent rebuild"}))
        try:
            grant_id = (json.loads(resp).get("grant") or {}).get("id", "")
        except json.JSONDecodeError:
            pass
        if not grant_id:
            record("exemption-rebuild", FAIL, f"grant POST returned no id: {resp[:200]}")
            return
        time.sleep(12)                                  # traffic under the GRANTED snapshot
        cp_admin(f"/api/admin/compliance/exemption-grants/{grant_id}", "-X", "PATCH",
                 "-H", "Content-Type: application/json", "-d", '{"inactive":true}')
        time.sleep(12)                                  # traffic under the EMPTY snapshot
    finally:
        stop.set()
        for t in threads:
            t.join(timeout=60)
        # Best-effort cleanup so a re-run starts from a known state. Already-inactive
        # is idempotent, so this is safe even on the happy path.
        if grant_id:
            cp_admin(f"/api/admin/compliance/exemption-grants/{grant_id}", "-X", "PATCH",
                     "-H", "Content-Type: application/json", "-d", '{"inactive":true}')

    with open(PROXY_LOG, "rb") as fh:
        fh.seek(offset)
        appended = fh.read().decode("utf-8", "replace")
    # Byte offset, not recency: a log file is not a per-process channel, and reading
    # "the tail" is how this program repeatedly attributed one process's lines to
    # another. Everything below is by construction from THIS arm's window.
    lines = appended.splitlines()
    rebuilds = [ln for ln in lines if "exemption store rebuilt from shadow" in ln]
    exempted = [ln for ln in lines
                if "request exempt from compliance hooks" in ln and grant_id in ln]
    # A revoke rebuild is the one carrying active=0; requests logged after it must not
    # be exempt. Find its position and check no exempt line follows.
    revoke_idx = next((i for i, ln in enumerate(lines)
                       if "exemption store rebuilt from shadow" in ln and "active=0" in ln), -1)
    after_revoke_exempt = [ln for ln in lines[revoke_idx + 1:]
                           if "request exempt from compliance hooks" in ln and grant_id in ln] \
        if revoke_idx >= 0 else []
    completed = [ln for ln in lines[revoke_idx + 1:] if "connectionId=" in ln] if revoke_idx >= 0 else []

    with codes_lock:
        total, non200 = len(codes), [c for c in codes if c != 200]

    problems = []
    if len(rebuilds) < 2:
        problems.append(f"only {len(rebuilds)} rebuild lines inside the load window (need >=2: "
                        "grant and revoke); the swaps were not concurrent with traffic")
    if not exempted:
        problems.append(f"no request logged exemptionId={grant_id}: the granting snapshot was "
                        "never observed by the lock-free read path")
    if revoke_idx < 0:
        problems.append("no active=0 rebuild line: the revoke never reached the store")
    elif after_revoke_exempt:
        problems.append(f"{len(after_revoke_exempt)} requests still exempt AFTER the active=0 "
                        "rebuild: the empty snapshot was not observed")
    elif not completed:
        problems.append("no interception activity logged after the revoke, so 'no longer exempt' "
                        "is vacuous rather than observed")
    if non200:
        problems.append(f"{len(non200)}/{total} requests failed across the swaps: {sorted(set(non200))}")

    evidence = (f"{total} bumped requests, {len(non200)} non-200; "
                f"{len(rebuilds)} in-window rebuilds "
                f"({' '.join(ln.split('active=')[1].split()[0] for ln in rebuilds)} active); "
                f"{len(exempted)} requests correlated to grant {grant_id[:8]}; "
                f"{len(after_revoke_exempt)} exempt after revoke (want 0) "
                f"with {len(completed)} interceptions still flowing")
    record("exemption-rebuild", FAIL if problems else PASS,
           evidence + (" | " + "; ".join(problems) if problems else ""))


def arm_log_volume() -> None:
    """Log volume on the bumped path (C-9 / C-4 / A-2 / C-14 / C-23).

    Five findings cut per-request logging. Their common failure mode is a regression
    nothing else notices: a line demoted to Debug gets restored to Info, or a new one is
    added, and the only symptom is that a busy proxy writes several times more log than it
    should. No test fails, no metric moves. On the agent — same shared code — it is battery
    and disk on a user's laptop.

    So the observation is a COUNT, measured on the real path, plus the absence of each
    specific message these findings removed from default level. Read by byte offset from
    the live structured log, so nothing from an earlier run can be counted.

    C-23's guarantee is checked directly rather than by proxy: a marker carried in the
    request prompt must appear NOWHERE in the log at any level. The SSE diagnostics used to
    echo up to 1 MiB of remotely-controlled bytes per offending line, which on a
    compliance/DLP product is a path for prompt text to reach the logs.
    """
    if not os.path.exists(PROXY_LOG):
        record("log-volume", SKIP, f"proxy structured log absent at {PROXY_LOG}")
        return

    # Messages these findings took off the default level. Each is asserted absent at INFO
    # by its own name, so a partial regression names itself instead of moving a total.
    DEMOTED = [
        ("post-upstream response routing", "C-9"),
        ("response stage: non-SSE arm", "C-9"),
        ("runtimeNormalize: Registry.Normalize CLAIM", "C-9"),
        ("Registry.Normalize FELL-THROUGH", "C-9"),
        ("request entry", "C-4"),
        ("domain/path resolved", "C-4"),
        ("SSE parser: unrecognized field", "C-14"),
        ("SSE parser: malformed retry", "C-14"),
    ]

    offset = os.path.getsize(PROXY_LOG)
    mark = marker("LOGVOL")
    requests = 0
    for i in range(4):
        code, _ = curl_chat({"model": "gpt-4o-mini",
                             "messages": [{"role": "user", "content": f"{mark}-{i} reply ok"}],
                             "max_tokens": 5}, timeout=60)
        if code == 200:
            requests += 1
    # One SSE request too: C-14/C-23 live on the streaming parser, so a non-stream-only
    # sample would leave exactly the noisiest path unmeasured.
    code, _ = curl_chat({"model": "gpt-4o-mini",
                         "messages": [{"role": "user", "content": f"{mark}-sse count to 5"}],
                         "stream": True, "max_tokens": 40}, timeout=90, stream=True)
    if code == 200:
        requests += 1
    if requests == 0:
        record("log-volume", FAIL, "no bumped request returned 200; nothing to measure")
        return

    time.sleep(2)  # the proxy writes its close line after the client sees the response
    with open(PROXY_LOG, "rb") as fh:
        fh.seek(offset)
        appended = fh.read().decode("utf-8", "replace")
    lines = [ln for ln in appended.splitlines() if ln.strip()]
    info = [ln for ln in lines if "level=INFO" in ln]
    warn = [ln for ln in lines if "level=WARN" in ln]
    err = [ln for ln in lines if "level=ERROR" in ln]

    problems = []
    for msg, finding in DEMOTED:
        hits = sum(1 for ln in info if msg in ln)
        if hits:
            problems.append(f"{finding}: {hits} INFO lines still say {msg!r} — it was demoted to "
                            "Debug and is back at default level")
    # The bound is per REQUEST, not absolute, so the arm keeps meaning if the request count
    # changes. Connection lifecycle is 2 lines (CONNECT accepted + connection closed
    # normally); 3 leaves headroom for one variant without admitting a whole new per-request
    # line, which is the thing being guarded.
    per_request = len(info) / requests
    if per_request > 3:
        top = collections.Counter(
            ln.split('msg="')[1].split('"')[0] for ln in info if 'msg="' in ln
        ).most_common(4)
        problems.append(f"{per_request:.1f} INFO lines per bumped request (bound 3); most "
                        f"frequent: {top}")
    if mark in appended:
        problems.append(f"the request's own prompt marker {mark} appears in the proxy log — "
                        "wire content is being echoed (C-23)")
    if err:
        problems.append(f"{len(err)} ERROR lines: {err[0][:160]}")

    # The headline states only what was actually observed. Asserting "all N demoted
    # messages absent" unconditionally would make the summary contradict its own problem
    # list on a failure — a line claiming something that did not happen, which is the
    # defect class this whole program exists to remove.
    verdict = FAIL if problems else PASS
    headline = (f"{requests} bumped requests -> {len(info)} INFO ({per_request:.1f}/req), "
                f"{len(warn)} WARN, {len(err)} ERROR")
    if verdict == PASS:
        headline += (f"; all {len(DEMOTED)} demoted messages absent at INFO; "
                     "prompt marker not echoed")
    record("log-volume", verdict, headline + (" | " + "; ".join(problems) if problems else ""))


def arm_runtime_source() -> None:
    """compliance-proxy wiring/health.go: the storage.spill runtime source (S-3)."""
    tok = os.environ.get("INTERNAL_SERVICE_TOKEN") or os.environ.get("NEXUS_HUB_SERVICE_TOKEN", "")
    req = urllib.request.Request("http://127.0.0.1:9090/debug/runtime",
                                 headers={"Authorization": f"Bearer {tok}"})
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            snap = json.loads(r.read())
    except Exception as e:
        record("runtime-source", FAIL, f"/debug/runtime on :9090 -> {e}")
        return
    src = (snap.get("sources") or {}).get("storage.spill")
    if src is None:
        record("runtime-source", FAIL,
               f"storage.spill absent; present: {sorted((snap.get('sources') or {}))[:8]}")
        return
    v = src.get("value") or {}
    if not v.get("effect"):
        record("runtime-source", FAIL, f"storage.spill has an EMPTY effect (S-10 regressed): {v}")
        return
    record("runtime-source", PASS,
           f"configured={v.get('configured')} backend={v.get('backend')!r} "
           f"hostLocal={v.get('hostLocal')} effect={len(v['effect'])} chars")


ARMS = {
    "nonstream": arm_nonstream,
    "sse": arm_sse,
    "large-body": arm_large_body,
    "scan-scale": arm_scan_scale,
    "spill-readback": arm_spill_readback,
    "exemption": arm_exemption,
    "exemption-rebuild": arm_exemption_rebuild,
    "log-volume": arm_log_volume,
    "runtime-source": arm_runtime_source,
}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--arm", action="append", choices=sorted(ARMS), help="run only these arms")
    args = ap.parse_args()
    load_env()
    if not os.path.exists(CA):
        sys.exit(f"missing proxy CA at {CA} — run the dev-certs block from scripts/dev-start.sh")
    if not os.environ.get("OPENAI_API_KEY"):
        sys.exit("OPENAI_API_KEY not in tests/.env.local")

    chosen = args.arm or list(ARMS)
    print(f"compliance-proxy bump regression — proxy={PROXY}, {len(chosen)} arms\n")
    for name in chosen:
        try:
            ARMS[name]()
        except Exception as e:  # an arm must never take the suite down
            record(name, FAIL, f"arm raised {type(e).__name__}: {e}")

    npass = sum(1 for _, v, _ in results if v == PASS)
    nfail = sum(1 for _, v, _ in results if v == FAIL)
    nskip = sum(1 for _, v, _ in results if v == SKIP)
    print(f"\n  {npass} pass, {nfail} fail, {nskip} skip")
    return 1 if nfail else 0


if __name__ == "__main__":
    sys.exit(main())
