#!/usr/bin/env python3
"""Verify hook-config changes reach the AI Gateway live (no resync).

This answers the AWS S-01 report's Bug 6 ("the AI gateway does not receive live
hook config changes; only a resync propagates them"). It proves the claim wrong
at TWO levels, with NO `POST /nodes/:id/resync` ever issued:

  (1) CONFIG LEVEL — GET /api/admin/nodes/:id/runtime exposes the gateway's
      in-memory loaded config. `snapshot.sources["config.hooks"].value` is the
      exact hook set the gateway has loaded (the DB loader selects
      `WHERE enabled = true`, so a disabled hook drops out). `meta.desired_ver`
      / `meta.reported_ver` are the Hub shadow versions; equal => the gateway
      applied the latest pushed config.

  (2) EXECUTION LEVEL — the authoritative signal. Send a real request through
      the data plane, then read its traffic_event detail
      (`requestHooksPipeline`) which records exactly which hooks executed for
      that request. Toggle the hook, send another request, and confirm the
      executed-hook set changed. Config-sync is necessary but not sufficient;
      this proves the change actually took effect on live traffic.

Experiment (default target hook: noop-baseline — a request-stage no-op that
reliably appears in requestHooksPipeline, so flipping it is side-effect free):

    baseline (enabled)  -> hook present in executed pipeline + in /runtime
    disable (no resync) -> hook ABSENT from the next request's pipeline
    re-enable (no resync)-> hook present again

Usage:
    NEXUS_ADMIN_PASSWORD=... python3 tests/scripts/verify_hooks_sync.py \
        --url https://44.212.215.114 --email admin@nexus.ai

    Flags:
      --url / --email / --password   admin login (or NEXUS_CP_URL/_ADMIN_EMAIL/_ADMIN_PASSWORD)
      --hook-name   hook to toggle (default noop-baseline)
      --model       data-plane model code (default mock-gpt-4o-mini)
      --insecure    skip TLS verify (default on, for the self-signed AMI cert)
"""
from __future__ import annotations

import argparse
import base64
import datetime
import hashlib
import http.client
import json
import os
import re
import ssl
import sys
import time
import urllib.parse
from typing import Any, Optional

CLIENT_ID = "cp-ui"


def _c(code: str, m: str) -> str:
    return f"\033[{code}m{m}\033[0m" if sys.stdout.isatty() else m


def ok(m): print(_c("32", "  ✓ " + m))
def info(m): print(_c("36", "  • " + m))
def warn(m): print(_c("33", "  ! " + m))
def err(m): print(_c("31", "  ✗ " + m))
def step(m): print(_c("1;35", "\n=== " + m + " ==="))


class CP:
    def __init__(self, base_url, email, password, insecure):
        p = urllib.parse.urlparse(base_url)
        self.host, self.port = p.hostname, p.port or (443 if p.scheme == "https" else 80)
        self.https = p.scheme == "https"
        self.redirect_uri = base_url.rstrip("/") + "/auth/callback"
        self.email, self.password = email, password
        self._tok, self._exp = None, 0.0
        self.ctx = ssl.create_default_context()
        if insecure:
            self.ctx.check_hostname = False
            self.ctx.verify_mode = ssl.CERT_NONE

    def _conn(self):
        return (http.client.HTTPSConnection(self.host, self.port, timeout=60, context=self.ctx)
                if self.https else http.client.HTTPConnection(self.host, self.port, timeout=60))

    def raw(self, method, path, body=None, ct="application/json", token=None):
        c = self._conn()
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
                                     "code_challenge_method": "S256", "state": "sync", "scope": "openid"})
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


def main() -> int:
    ap = argparse.ArgumentParser(description="Verify hook-config sync to the AI gateway (no resync).")
    ap.add_argument("--url", default=os.environ.get("NEXUS_CP_URL", "https://44.212.215.114"))
    ap.add_argument("--email", default=os.environ.get("NEXUS_ADMIN_EMAIL", "admin@nexus.ai"))
    ap.add_argument("--password", default=os.environ.get("NEXUS_ADMIN_PASSWORD", ""))
    ap.add_argument("--hook-name", default="noop-baseline")
    ap.add_argument("--model", default="mock-gpt-4o-mini")
    ap.add_argument("--insecure", action="store_true", default=True)
    args = ap.parse_args()
    if not args.password:
        ap.error("admin password required (--password or NEXUS_ADMIN_PASSWORD)")

    cp = CP(args.url, args.email, args.password, args.insecure)

    step("Phase 0 — login + discover")
    cp.login(); ok(f"CP login OK ({args.email})")
    s, nodes = cp.api("GET", "/api/admin/nodes")
    node_id = next((n["id"] for n in as_list(nodes) if n.get("type") == "ai-gateway"), None)
    if not node_id:
        err("no ai-gateway node"); return 2
    ok(f"gateway node: {node_id}")
    s, hooks = cp.api("GET", "/api/admin/hooks")
    target = next((h for h in as_list(hooks) if h.get("name") == args.hook_name), None)
    if not target:
        err(f"hook {args.hook_name!r} not found"); return 2
    hook_id, original_enabled = target["id"], bool(target.get("enabled"))
    ok(f"target hook: {args.hook_name} ({hook_id}), enabled={original_enabled}")

    # create throwaway service VK (admin-created service VKs are active immediately)
    s, vk = cp.api("POST", "/api/admin/virtual-keys", {"name": "hooks-sync-probe", "vkType": "service"})
    if s not in (200, 201) or not vk.get("key"):
        err(f"create VK -> {s}: {vk}"); return 2
    vk_id, vk_key, vk_name = vk["id"], vk["key"], vk["name"]
    ok(f"probe VK: {vk.get('keyPrefix')}")

    def runtime_state():
        s, b = cp.api("GET", f"/api/admin/nodes/{urllib.parse.quote(node_id)}/runtime")
        meta = b.get("meta") or {}
        loaded = (((b.get("snapshot") or {}).get("sources") or {}).get("config.hooks") or {}).get("value") or []
        names = {h["name"] for h in loaded if isinstance(h, dict) and "name" in h}
        return meta.get("desired_ver"), meta.get("reported_ver"), names

    def executed_hooks(timeout=30):
        """Send a request, correlate its traffic_event by the x-nexus-request-id header.

        The gateway returns `x-nexus-request-id` on the data-plane response, and the
        traffic_event's `traceId`/`id` equals it — an exact, background-traffic-proof
        key (the bench runs continuous mock load, so 'newest row' would be wrong).
        Also returns the client-observable `x-nexus-hook` header (e.g.
        'passed:noop-baseline,pii-scanner,keyword-blocker') as a second signal.
        """
        nonce = "N" + base64.urlsafe_b64encode(os.urandom(5)).decode().rstrip("=")
        st, _, hd = cp.raw("POST", "/v1/chat/completions",
                          {"model": args.model, "max_tokens": 16, "stream": False,
                           "messages": [{"role": "user", "content": f"hi {nonce} 2+2?"}]},
                          token=vk_key)
        rid = hd.get("x-nexus-request-id")
        hook_hdr = hd.get("x-nexus-hook", "")
        if not rid:
            return st, None, None, hook_hdr
        t0 = time.time()
        while time.time() - t0 <= timeout:
            s, b = cp.api("GET", "/api/admin/traffic?limit=50")
            row = next((it for it in as_list(b) if it.get("traceId") == rid or it.get("id") == rid), None)
            if row:
                _, det = cp.api("GET", f"/api/admin/traffic/{row['id']}")
                pl = det.get("requestHooksPipeline") or []
                return st, row["id"], [{"name": h.get("name"), "decision": h.get("decision"),
                                        "latencyMs": h.get("latencyMs")} for h in pl], hook_hdr
            time.sleep(2)
        return st, rid, None, hook_hdr

    def wait_propagation(want_loaded, timeout=30):
        """Gate on the gateway's loaded set reflecting the toggle (independent /runtime
        axis) instead of a blind sleep; returns seconds-to-propagate (no resync)."""
        t0 = time.time()
        while time.time() - t0 <= timeout:
            _, _, loaded = runtime_state()
            if (args.hook_name in loaded) == want_loaded:
                return time.time() - t0
            time.sleep(0.5)
        return None

    def leg(label):
        st, rid, hooks, hook_hdr = executed_hooks()
        dv, rv, loaded = runtime_state()
        names = [h["name"] for h in hooks] if hooks else []
        rec = {
            "label": label, "rid": rid, "desired_ver": dv, "reported_ver": rv,
            "exec_traffic": args.hook_name in names,       # gateway exec trace (persisted)
            "exec_header": args.hook_name in (hook_hdr or ""),  # gateway exec trace (live header, same source)
            "loaded_runtime": args.hook_name in loaded,    # independent axis: HookConfigCache snapshot
            "header": hook_hdr, "pipeline": hooks,
        }
        sync = "in-sync" if dv == rv else "SYNCING"
        info(f"[{label}] http={st} event={rid} desired_ver={dv} reported_ver={rv} ({sync})")
        info(f"          x-nexus-hook header: {hook_hdr!r}")
        info(f"          traffic_event requestHooksPipeline: {json.dumps(hooks, ensure_ascii=False)}")
        info(f"          {args.hook_name}: exec(traffic_event)={rec['exec_traffic']}  "
             f"exec(header)={rec['exec_header']}  loaded(/runtime)={rec['loaded_runtime']}")
        return rec

    recs = {}
    try:
        step(f"Phase 1 — baseline ({args.hook_name} enabled): expect it to EXECUTE")
        if not original_enabled:
            cp.api("PUT", f"/api/admin/hooks/{hook_id}", {"enabled": True}); wait_propagation(True)
        recs["baseline"] = leg("baseline")

        step(f"Phase 2 — DISABLE {args.hook_name} (NO resync): expect it to STOP executing")
        sc, _ = cp.api("PUT", f"/api/admin/hooks/{hook_id}", {"enabled": False})
        prop = wait_propagation(False)
        ok(f"PUT enabled=false -> {sc}; /runtime reflected it in "
           f"{('%.1fs' % prop) if prop is not None else '>30s'} (NO resync)")
        recs["after-disable"] = leg("after-disable")
        recs["after-disable"]["propagation_s"] = prop

        step(f"Phase 3 — RE-ENABLE {args.hook_name} (NO resync): expect it to EXECUTE again")
        sc, _ = cp.api("PUT", f"/api/admin/hooks/{hook_id}", {"enabled": True})
        prop2 = wait_propagation(True)
        ok(f"PUT enabled=true -> {sc}; /runtime reflected it in "
           f"{('%.1fs' % prop2) if prop2 is not None else '>30s'} (NO resync)")
        recs["after-enable"] = leg("after-enable")
        recs["after-enable"]["propagation_s"] = prop2

        # ── evidence-based analysis ──────────────────────────────────────────
        step("ANALYSIS (evidence)")
        b, d, e = recs["baseline"], recs["after-disable"], recs["after-enable"]
        print(f"  Target hook: {args.hook_name}   (toggled via PUT /api/admin/hooks/:id, NO resync ever issued)")
        print(f"  {'phase':14}{'shadow ver (des/rep)':>22}{'exec(traffic)':>15}{'exec(header)':>14}{'loaded(/runtime)':>18}")
        for k in ("baseline", "after-disable", "after-enable"):
            r = recs[k]
            ver = f"{r['desired_ver']}/{r['reported_ver']}"
            print(f"  {k:14}{ver:>22}{str(r['exec_traffic']):>15}"
                  f"{str(r['exec_header']):>14}{str(r['loaded_runtime']):>18}")
        # consistency of the three signals per phase
        tri = {k: (recs[k]["exec_traffic"], recs[k]["exec_header"], recs[k]["loaded_runtime"]) for k in recs}
        all_consistent = all(len(set(v)) == 1 for v in tri.values())
        expected = {"baseline": True, "after-disable": False, "after-enable": True}
        matches = all(recs[k]["exec_traffic"] == expected[k] for k in expected)
        hdr_eq_traffic = all(recs[k]["exec_header"] == recs[k]["exec_traffic"] for k in recs)
        ver_advanced = (b["desired_ver"] < d["desired_ver"] < e["desired_ver"]) if all(
            recs[k]["desired_ver"] is not None for k in recs) else False
        ver_in_sync = all(recs[k]["desired_ver"] == recs[k]["reported_ver"] for k in recs)
        print()
        print(f"  • executed set followed the toggle (on→off→on): {matches}")
        print(f"  • header == traffic_event every phase (both are the gateway's own exec trace): {hdr_eq_traffic}")
        print(f"  • /runtime loaded set agreed (independent axis: HookConfigCache snapshot): {all_consistent}")
        print(f"  • shadow desired_ver advanced each toggle and reported_ver kept up: "
              f"advanced={ver_advanced} in_sync={ver_in_sync}")
        def _ps(x):
            return f"{x:.1f}s" if isinstance(x, (int, float)) else "n/a"
        print(f"  • propagation latency with NO resync: disable={_ps(d.get('propagation_s'))} "
              f"enable={_ps(e.get('propagation_s'))}")
        print("  Note: header and traffic_event are the SAME gateway signal (one reqHookResult); the")
        print("        INDEPENDENT second axis is /runtime. Plus the code path "
              "(CP InvalidateConfig→Hub→OnConfigChanged→HookConfigCache.Reload).")

        step("VERDICT")
        if matches and hdr_eq_traffic and all_consistent and ver_advanced:
            ok(f"HOOK SYNC WORKS — Bug 6 NOT reproduced. {args.hook_name} stopped executing after "
               "DISABLE and resumed after RE-ENABLE, with NO resync, confirmed at the gateway "
               "execution level (requestHooksPipeline == x-nexus-hook) AND on the independent "
               "/runtime snapshot, while the shadow version advanced and the gateway acked it.")
            return 0
        err(f"Inconsistent/unexpected — matches={matches} hdr==traffic={hdr_eq_traffic} "
            f"3-axis-consistent={all_consistent} ver_advanced={ver_advanced}. "
            "If the executed set did NOT change without a resync, Bug 6 would be corroborated.")
        return 1
    finally:
        step("Teardown")
        cp.api("PUT", f"/api/admin/hooks/{hook_id}", {"enabled": original_enabled})
        info(f"restored {args.hook_name} enabled={original_enabled}")
        cp.api("DELETE", f"/api/admin/virtual-keys/{vk_id}")
        info(f"deleted probe VK {vk_id}")


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        print("\ninterrupted"); sys.exit(130)
