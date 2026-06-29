#!/usr/bin/env python3
"""Verify that response-stage hooks inflate CLIENT-OBSERVED streaming TTFT.

Background: the AI Gateway's default SSE mode (chunked_async / LivePipeline)
holds the first token back server-side until a compliance checkpoint
(~FirstInspectChars=400 accumulated chars) whenever ANY response-stage hook is
wired — the engine can't know statically whether a response hook will inspect or
mutate response bytes, so it buffers conservatively. With no response-stage hook
the probe drops hold-back and the stream passes through live.

Measurement design (each fix addresses a real methodology gap):

  * CLIENT-side TTFT, off the socket — NOT traffic_event. The gateway's
    upstreamTtfbMs/latencyBreakdown time the gateway<->upstream leg and exclude
    the hold-back (which sits after upstream first-byte, before client
    first-byte), so they cannot reveal client TTFT.

  * INTERLEAVED ON/OFF rounds (not all-A-then-all-B) so per-minute upstream/network
    variance cancels in the ON−OFF delta.

  * Per-request SERVER-SIDE attribution: for every gateway request we look up the
    traffic_event (by the x-nexus-request-id header) and print upstreamTtfbMs /
    upstreamTotalMs / latencyMs. If the client total >> the gateway's
    upstreamTotalMs, the slowness is the CLIENT<->gateway link (WAN / Nagle on
    small SSE writes), not the gateway — the script flags this and the absolute
    TTFT is then NOT production-representative. Run from an in-region client
    (e.g. the bench runner) for trustworthy absolutes.

  * Optional DIRECT-to-OpenAI arm (--openai-key) as a true no-gateway baseline,
    measured from the same client and interleaved with the gateway arms.

  * Guards: refuses to trust ARM-OFF if ANY OTHER response-stage hook is still
    enabled (hold-back would stay on); confirms the target hook's loaded state via
    /runtime before each measurement (no resync).

Verdict is on the ON−OFF TTFT delta (hold-back cost), with the WAN caveat
surfaced explicitly. Non-SSE totals are reported as a control (hold-back does not
apply to non-streaming; they should be ~equal across arms).

Usage:
    NEXUS_ADMIN_PASSWORD=... python3 tests/scripts/verify_response_hooks_latency.py \
        --url https://44.212.215.114 --model gpt-4o-mini --rounds 3 --max-tokens 160
    # optional true baseline:
    OPENAI_API_KEY=sk-... ... --openai-key env   (or --openai-key sk-...)
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import http.client
import json
import os
import re
import ssl
import subprocess
import sys
import tempfile
import time
import urllib.parse

CLIENT_ID = "cp-ui"


def _c(code: str, m: str) -> str:
    return f"\033[{code}m{m}\033[0m" if sys.stdout.isatty() else m


def ok(m): print(_c("32", "  ✓ " + m))
def info(m): print(_c("36", "  • " + m))
def warn(m): print(_c("33", "  ! " + m))
def err(m): print(_c("31", "  ✗ " + m))
def step(m): print(_c("1;35", "\n=== " + m + " ==="))


def _ctx(insecure):
    c = ssl.create_default_context()
    if insecure:
        c.check_hostname = False
        c.verify_mode = ssl.CERT_NONE
    return c


def median(xs):
    if not xs:
        return None
    xs = sorted(xs)
    return xs[len(xs) // 2]


class CP:
    def __init__(self, base_url, email, password, insecure):
        p = urllib.parse.urlparse(base_url)
        self.host, self.port = p.hostname, p.port or (443 if p.scheme == "https" else 80)
        self.https = p.scheme == "https"
        self.redirect_uri = base_url.rstrip("/") + "/auth/callback"
        self.email, self.password = email, password
        self._tok, self._exp = None, 0.0
        self.ctx = _ctx(insecure)

    def conn(self):
        return (http.client.HTTPSConnection(self.host, self.port, timeout=180, context=self.ctx)
                if self.https else http.client.HTTPConnection(self.host, self.port, timeout=180))

    def raw(self, method, path, body=None, ct="application/json", token=None):
        c = self.conn()
        h = {"Content-Type": ct}
        if token:
            h["Authorization"] = "Bearer " + token
        data = (json.dumps(body).encode() if (body is not None and ct == "application/json") else body)
        c.request(method, path, data, h)
        r = c.getresponse()
        hd = {k.lower(): v for k, v in r.getheaders()}
        raw = r.read().decode("utf-8", "replace")
        c.close()
        if r.status in (301, 302, 303, 307, 308):
            return r.status, {"_loc": hd.get("location", "")}, hd
        try:
            return r.status, (json.loads(raw) if raw.strip() else {}), hd
        except Exception:
            return r.status, {"_raw": raw[:300]}, hd

    def login(self):
        ver = base64.urlsafe_b64encode(os.urandom(32)).rstrip(b"=").decode()
        chal = base64.urlsafe_b64encode(hashlib.sha256(ver.encode()).digest()).rstrip(b"=").decode()
        qs = urllib.parse.urlencode({"response_type": "code", "client_id": CLIENT_ID,
                                     "redirect_uri": self.redirect_uri, "code_challenge": chal,
                                     "code_challenge_method": "S256", "state": "lat", "scope": "openid"})
        s, b, hd = self.raw("GET", "/oauth/authorize?" + qs)
        loc = hd.get("location", "") or (b.get("_loc", "") if isinstance(b, dict) else "")
        m = re.search(r"[?&]authctx=([^&]+)", loc)
        if not m:
            raise RuntimeError(f"/oauth/authorize: no authctx (status={s})")
        _, b2, _ = self.raw("POST", "/authserver/password",
                            {"authctx": m.group(1), "email": self.email, "password": self.password})
        mc = re.search(r"[?&]code=([^&]+)", b2.get("redirectUri", "") if isinstance(b2, dict) else "")
        if not mc:
            raise RuntimeError(f"/authserver/password failed: {b2}")
        form = urllib.parse.urlencode({"grant_type": "authorization_code", "code": mc.group(1),
                                       "redirect_uri": self.redirect_uri, "client_id": CLIENT_ID,
                                       "code_verifier": ver}).encode()
        _, b3, _ = self.raw("POST", "/oauth/token", body=form, ct="application/x-www-form-urlencoded")
        tok = b3.get("access_token") if isinstance(b3, dict) else None
        if not tok:
            raise RuntimeError(f"/oauth/token failed: {b3}")
        self._tok, self._exp = tok, time.time() + int(b3.get("expires_in", 3600)) - 60
        return tok

    def token(self):
        if not self._tok or time.time() >= self._exp:
            self.login()
        return self._tok

    def api(self, method, path, body=None):
        s, b, _ = self.raw(method, path, body, token=self.token())
        return s, b


def as_list(b):
    if isinstance(b, list):
        return b
    if isinstance(b, dict):
        for k in ("items", "data", "hooks", "nodes"):
            if isinstance(b.get(k), list):
                return b[k]
    return []


def curl_sse(url, token, body_json, insecure):
    """SSE timing via curl (NOT a python read loop).

    Returns (status, ttft_s, total_s, n_content_chunks, headers_dict) or None.
    ttft = curl time_starttransfer (first response byte; for this gateway the 200
    + first SSE event are flushed together, so it tracks the hold-back), total =
    curl time_total. Using curl avoids the http.client.readline()-per-line
    overhead that pathologically inflated full-stream totals in earlier revisions.
    """
    outf = tempfile.NamedTemporaryFile(delete=False); outf.close()
    hdrf = tempfile.NamedTemporaryFile(delete=False); hdrf.close()
    cmd = ["curl", "-sS", "-N", "--max-time", "180", "-o", outf.name, "-D", hdrf.name,
           "-w", "%{http_code}\t%{time_starttransfer}\t%{time_total}",
           url, "-H", "Content-Type: application/json", "-H", "Accept: text/event-stream",
           "-H", "Authorization: Bearer " + token, "-d", body_json]
    if insecure:
        cmd.insert(1, "-k")
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=200)
        parts = (r.stdout or "").strip().split("\t")
        if len(parts) < 3:
            return None
        status, ttft, total = int(parts[0]), float(parts[1]), float(parts[2])
        headers = {}
        for ln in open(hdrf.name, encoding="utf-8", errors="replace"):
            if ":" in ln and not ln.startswith("HTTP/"):
                k, v = ln.split(":", 1)
                headers[k.strip().lower()] = v.strip()
        nchunks = 0
        for ln in open(outf.name, encoding="utf-8", errors="replace"):
            if ln.startswith("data:") and '"content"' in ln:
                nchunks += 1
        return status, ttft, total, nchunks, headers
    except Exception:
        return None
    finally:
        for f in (outf.name, hdrf.name):
            try:
                os.unlink(f)
            except OSError:
                pass


def main() -> int:
    ap = argparse.ArgumentParser(description="Verify response-stage hooks inflate client SSE TTFT.")
    ap.add_argument("--url", default=os.environ.get("NEXUS_CP_URL", "https://44.212.215.114"))
    ap.add_argument("--gw-url", default=os.environ.get("NEXUS_AI_GW_URL"),
                    help="gateway data-plane base URL for /v1 (default: same as --url, i.e. the "
                         "appliance nginx). Set to http://localhost:3050 to bypass nginx.")
    ap.add_argument("--email", default=os.environ.get("NEXUS_ADMIN_EMAIL", "admin@nexus.ai"))
    ap.add_argument("--password", default=os.environ.get("NEXUS_ADMIN_PASSWORD", ""))
    ap.add_argument("--response-hook", default="response-quality-signals")
    ap.add_argument("--model", default="gpt-4o-mini")
    ap.add_argument("--rounds", type=int, default=3, help="interleaved ON/OFF rounds")
    ap.add_argument("--max-tokens", type=int, default=160)
    ap.add_argument("--openai-key", default="env",
                    help="OpenAI API key for the REQUIRED direct no-gateway baseline arm; "
                         "literal key or 'env' to read OPENAI_API_KEY (default)")
    ap.add_argument("--openai-base", default="api.openai.com")
    ap.add_argument("--insecure", action="store_true", default=True)
    args = ap.parse_args()
    if not args.password:
        ap.error("admin password required (--password or NEXUS_ADMIN_PASSWORD)")
    openai_key = os.environ.get("OPENAI_API_KEY") if args.openai_key == "env" else args.openai_key
    # Direct-OpenAI is a best-effort baseline arm: attempted when a key is present,
    # cleanly skipped otherwise. It must NEVER block the core gateway ON/OFF TTFT
    # measurement (and some hosts cannot reach api.openai.com for streaming at all).

    cp = CP(args.url, args.email, args.password, args.insecure)

    step("Phase 0 — login + discover + guards")
    cp.login(); ok(f"CP login OK ({args.email})")
    s, nodes = cp.api("GET", "/api/admin/nodes")
    node_id = next((n["id"] for n in as_list(nodes) if n.get("type") == "ai-gateway"), None)
    if not node_id:
        err("no ai-gateway node"); return 2
    ok(f"gateway node: {node_id}")
    s, hooks = cp.api("GET", "/api/admin/hooks")
    target = next((h for h in as_list(hooks) if h.get("name") == args.response_hook), None)
    if not target:
        err(f"response hook {args.response_hook!r} not found"); return 2
    if target.get("stage") != "response":
        warn(f"{args.response_hook} stage={target.get('stage')} (expected 'response')")
    hook_id, original_enabled = target["id"], bool(target.get("enabled"))

    # GUARD: any OTHER enabled response-stage hook keeps hold-back on => ARM OFF
    # would not actually drop hold-back, masking the effect.
    other_resp = [h["name"] for h in as_list(hooks)
                  if h.get("stage") == "response" and h.get("enabled") and h.get("id") != hook_id]
    if other_resp:
        err(f"other response-stage hooks still ENABLED: {other_resp}. ARM-OFF would keep hold-back "
            "engaged. Disable them first, or this test cannot isolate the effect.")
        return 2
    ok(f"target response hook: {args.response_hook} ({hook_id}); no other response hooks enabled")

    s, vk = cp.api("POST", "/api/admin/virtual-keys", {"name": "resp-hook-latency-probe", "vkType": "service"})
    if s not in (200, 201) or not vk.get("key"):
        err(f"create VK -> {s}: {vk}"); return 2
    vk_id, vk_key = vk["id"], vk["key"]
    ok(f"probe VK: {vk.get('keyPrefix')} | model: {args.model} | rounds: {args.rounds} | "
       f"direct-OpenAI arm: {'on' if openai_key else 'off'}")

    def runtime_loaded(name):
        s, b = cp.api("GET", f"/api/admin/nodes/{urllib.parse.quote(node_id)}/runtime")
        loaded = (((b.get("snapshot") or {}).get("sources") or {}).get("config.hooks") or {}).get("value") or []
        return name in {h.get("name") for h in loaded if isinstance(h, dict)}

    def ensure_state(enabled):
        cur = runtime_loaded(args.response_hook)
        if cur != enabled:
            cp.api("PUT", f"/api/admin/hooks/{hook_id}", {"enabled": enabled})
        t0 = time.time()
        while time.time() - t0 <= 30:
            if runtime_loaded(args.response_hook) == enabled:
                return True
            time.sleep(1)
        warn(f"/runtime did not reach {args.response_hook} loaded={enabled} in 30s")
        return False

    def upstream_timing(rid, timeout=25):
        """Server-side attribution from the traffic_event for this request id."""
        if not rid:
            return None
        t0 = time.time()
        while time.time() - t0 <= timeout:
            s, b = cp.api("GET", "/api/admin/traffic?limit=50")
            row = next((it for it in as_list(b) if it.get("traceId") == rid or it.get("id") == rid), None)
            if row:
                _, det = cp.api("GET", f"/api/admin/traffic/{row['id']}")
                return {"upstreamTtfbMs": det.get("upstreamTtfbMs"),
                        "upstreamTotalMs": det.get("upstreamTotalMs"),
                        "latencyMs": det.get("latencyMs"),
                        "routedProviderName": det.get("routedProviderName")}
            time.sleep(2)
        return None

    gw_data_url = (args.gw_url or args.url).rstrip("/") + "/v1/chat/completions"

    def _body(prompt, stream):
        return json.dumps({"model": args.model, "stream": stream, "max_tokens": args.max_tokens,
                           "messages": [{"role": "user", "content": prompt}]})

    def gw_stream(prompt):
        res = curl_sse(gw_data_url, vk_key, _body(prompt, True), args.insecure)
        if res is None:
            return 0, None, 0, 0, None
        st, ttft, total, nc, hd = res
        return st, ttft, total, nc, hd.get("x-nexus-request-id")

    def gw_nonsse(prompt):
        t0 = time.time()
        st, _, _ = cp.raw("POST", "/v1/chat/completions",
                          {"model": args.model, "stream": False, "max_tokens": args.max_tokens,
                           "messages": [{"role": "user", "content": prompt}]}, token=vk_key)
        return st, time.time() - t0

    direct_dead = {"reason": None if openai_key else "no OPENAI_API_KEY set (direct baseline skipped)"}

    def direct_stream(prompt):
        """Direct-to-OpenAI baseline via curl. Best-effort: a host that cannot
        reach api.openai.com (egress filtering) records the reason and the 3-way
        degrades to the gateway ON/OFF 2-way — it never crashes the run."""
        res = curl_sse("https://" + args.openai_base + "/v1/chat/completions",
                       openai_key, _body(prompt, True), False)
        if res is None or res[0] != 200:
            direct_dead["reason"] = (f"curl http={res[0]}" if res else "curl error / timeout")
            return None
        st, ttft, total, nc, _ = res
        return st, ttft, total, nc

    A_ttft, B_ttft, D_ttft = [], [], []
    A_up, B_up = [], []
    A_total, B_total = [], []
    a_ns = b_ns = None
    try:
        step("Phase 1 — warmup (discarded)")
        ensure_state(True); gw_stream("warmup ON")
        ensure_state(False); gw_stream("warmup OFF")
        if openai_key:
            direct_stream("warmup direct")

        step(f"Phase 2 — {args.rounds} INTERLEAVED rounds (ON / OFF [/ direct]) — no resync")
        for r in range(args.rounds):
            nonce = base64.urlsafe_b64encode(os.urandom(4)).decode()
            # ON
            ensure_state(True)
            st, ttft, total, nc, rid = gw_stream(f"Tell me a short story about gateways {r} {nonce}")
            up = upstream_timing(rid)
            if st == 200 and ttft:
                A_ttft.append(ttft); A_total.append(total)
                if up and up.get("upstreamTotalMs") is not None:
                    A_up.append(up["upstreamTotalMs"])
            info(f"[r{r} ON ] client TTFT={round(ttft*1000) if ttft else 'n/a'}ms total={round(total*1000)}ms "
                 f"chunks={nc} | server upstreamTtfb={up and up.get('upstreamTtfbMs')}ms "
                 f"upstreamTotal={up and up.get('upstreamTotalMs')}ms via={up and up.get('routedProviderName')}")
            # OFF
            ensure_state(False)
            st, ttft, total, nc, rid = gw_stream(f"Tell me a short story about gateways {r} {nonce}b")
            up = upstream_timing(rid)
            if st == 200 and ttft:
                B_ttft.append(ttft); B_total.append(total)
                if up and up.get("upstreamTotalMs") is not None:
                    B_up.append(up["upstreamTotalMs"])
            info(f"[r{r} OFF] client TTFT={round(ttft*1000) if ttft else 'n/a'}ms total={round(total*1000)}ms "
                 f"chunks={nc} | server upstreamTtfb={up and up.get('upstreamTtfbMs')}ms "
                 f"upstreamTotal={up and up.get('upstreamTotalMs')}ms via={up and up.get('routedProviderName')}")
            # direct OpenAI (no gateway; hook-state-independent true baseline)
            if not direct_dead["reason"]:
                d = direct_stream(f"Tell me a short story about gateways {r} {nonce}d")
                if d and d[0] == 200 and d[1]:
                    D_ttft.append(d[1])
                    info(f"[r{r} DIRECT openai] client TTFT={round(d[1]*1000)}ms total={round(d[2]*1000)}ms chunks={d[3]}")
                else:
                    warn(f"[r{r} DIRECT openai] unreachable: {direct_dead['reason'] or ('status='+str(d and d[0]))}")

        step("Phase 3 — non-SSE control (hold-back does not apply)")
        ensure_state(True);  _, a_ns = gw_nonsse("control story ON")
        ensure_state(False); _, b_ns = gw_nonsse("control story OFF")
        info(f"non-SSE total: ON={round(a_ns*1000)}ms  OFF={round(b_ns*1000)}ms")

        step("ANALYSIS (evidence)")
        a, b = median(A_ttft), median(B_ttft)   # gateway ON / OFF client TTFT (seconds)
        d = median(D_ttft)                       # direct OpenAI client TTFT (seconds, may be None)
        if a is None or b is None:
            err(f"missing gateway measurements: gw_off={b} gw_on={a}"); return 1
        # ALL latencies normalised to MILLISECONDS for display/compare. Client
        # timings come from time.time() (seconds → ×1000); upstream* come from
        # traffic_event already in ms. (Mixing the two was a prior unit bug.)
        a_ms, b_ms = a * 1000, b * 1000
        d_ms = d * 1000 if d is not None else None
        a_tot_ms = median(A_total) * 1000 if A_total else None   # ON  client SSE total
        b_tot_ms = median(B_total) * 1000 if B_total else None   # OFF client SSE total
        client_tot_ms = median(A_total + B_total) * 1000
        up_ms = median(A_up + B_up) if (A_up + B_up) else None    # server upstreamTotal (ms)
        n = min(len(A_ttft), len(B_ttft))

        print(f"  model={args.model}  max_tokens={args.max_tokens}  rounds={args.rounds} (interleaved)  "
              f"samples/arm≈{n}")
        print(f"  --- TIME-TO-FIRST-TOKEN (client-measured) ---")
        print(f"  {'arm':34}{'median TTFT':>14}")
        print(f"  {'① direct OpenAI, no gateway':34}"
              f"{(str(round(d_ms))+' ms') if d_ms is not None else 'UNREACHABLE':>14}"
              + ("" if d_ms is not None else f'   ({direct_dead["reason"]})'))
        print(f"  {'② gateway, response-hook OFF':34}{str(round(b_ms))+' ms':>14}")
        print(f"  {'③ gateway, response-hook ON':34}{str(round(a_ms))+' ms':>14}")
        print(f"   • response-hook HOLD-BACK TTFT cost (③−②) = {round(a_ms-b_ms):>6} ms   ({a/b:.2f}×)")
        if d_ms is not None:
            print(f"   • gateway passthrough overhead      (②−①) = {round(b_ms-d_ms):>6} ms")
        print()
        print(f"  --- FULL-STREAM TOTAL (client-measured) vs SERVER upstreamTotal ---")
        print(f"   • server upstreamTotal (gateway received full body) = {round(up_ms) if up_ms else 'n/a'} ms")
        print(f"   • client SSE total: ON={round(a_tot_ms) if a_tot_ms else 'n/a'} ms  "
              f"OFF={round(b_tot_ms) if b_tot_ms else 'n/a'} ms")
        delivery_slow = bool(up_ms and client_tot_ms and client_tot_ms > up_ms * 2.5)
        on_eq_off = bool(a_tot_ms and b_tot_ms and 0.6 < (a_tot_ms / b_tot_ms) < 1.6)
        if delivery_slow:
            print(f"   • client total (~{round(client_tot_ms)} ms) ≫ upstreamTotal (~{round(up_ms)} ms): the "
                  f"gateway had the full body in ~{round(up_ms)} ms but the client received it over "
                  f"~{round(client_tot_ms)} ms.")
            print(f"     → the gap is in CLIENT↔gateway DELIVERY (nginx SSE proxy-buffering and/or per-chunk "
                  "pacing), NOT generation.")
            if on_eq_off:
                print(f"     → ON total ≈ OFF total ⇒ this delivery slowness is COMMON-MODE, NOT caused by the "
                      "hook. Separate issue from the hold-back TTFT cost above.")

        step("VERDICT")
        if a_ms > b_ms * 1.3:
            ok(f"TTFT: CONFIRMED — a response-stage hook inflates client SSE TTFT by ~{round(a_ms-b_ms)} ms "
               f"({a/b:.2f}×) vs hook-OFF, no resync. Gated on the PRESENCE of a response-stage hook "
               "(SSE hold-back to the first ~400-char checkpoint), regardless of what the hook does.")
        else:
            warn(f"TTFT: no material hold-back effect (③−②={round(a_ms-b_ms)}ms) — check the model streams "
                 "multi-chunk and that no other response-stage hook keeps hold-back engaged.")
        if a_ns and b_ns:
            ns_delta = round((a_ns - b_ns) * 1000)
            print(_c("36", f"  • NOTE non-SSE total: ON={round(a_ns*1000)}ms OFF={round(b_ns*1000)}ms "
                  f"(Δ={ns_delta}ms). Non-SSE has no hold-back, so any Δ here is the hook's OWN compute "
                  "cost — i.e. the hook is NOT free; the streaming hold-back TTFT cost above is ON TOP of this."))
        if delivery_slow:
            warn(f"FULL-STREAM TOTAL is delivery-bound (~{round(client_tot_ms)}ms vs ~{round(up_ms)}ms upstream) "
                 "and common-mode across ON/OFF — a SEPARATE issue (nginx buffering / SSE pacing), not the "
                 "hook. TTFT deltas above are unaffected. Root-cause still open: confirm via gateway upstream "
                 "stream-duration logs and a direct-to-:3050 (no-nginx) comparison.")
        return 0 if a_ms > b_ms * 1.3 else 1
    finally:
        step("Teardown")
        cp.api("PUT", f"/api/admin/hooks/{hook_id}", {"enabled": original_enabled})
        info(f"restored {args.response_hook} enabled={original_enabled}")
        cp.api("DELETE", f"/api/admin/virtual-keys/{vk_id}")
        info(f"deleted probe VK {vk_id}")


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print("\ninterrupted"); sys.exit(130)
