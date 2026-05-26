#!/usr/bin/env python3
"""Synthetic Gemini Web (gemini.google.com) chat traffic for
end-to-end validation of the batchexecute Tier-1 normalizer (E46-S12).

Sends a hand-rolled batchexecute POST (`f.req=` form-urlencoded JSON
envelope) to gemini.google.com's StreamGenerate endpoint through the
prod compliance proxy. Google upstream will reject our cookie-less
request with HTTP 401/403, but the proxy still:

  1. CONNECT-tunnels + TLS-bumps (gemini.google.com is in the prod
     interception_domain list as `adapter_id=gemini-web`),
  2. Runs the adapter's request hooks (now via adapter.Normalize),
  3. Forwards upstream,
  4. Emits a traffic_event row with the bumped request body,
  5. Hub-side Tier-1 geminiweb.Normalize() decodes f.req → prompt
     and persists a NormalizedPayload with `Kind=ai-chat`,
     `DetectedSpec=gemini-web`, one user Message.

No third-party deps — stdlib urllib + json + url.

Usage:
    python3 geminiweb_synthetic_chat.py [--insecure] [--proxy host:port] [--prompt TEXT]
"""

from __future__ import annotations

import argparse
import json
import ssl
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


def build_batchexecute_body(prompt: str, locale: str = "en") -> bytes:
    """Encode a gemini.google.com chat POST body.

    Wire format reverse-engineered from prod capture
    traffic_event 78911179-d123-4810-bf31-7bf4defde85a:

      f.req=<URL-ENCODED [null, "<INNER>"]>&at=<CSRF>

      inner = [[ "<PROMPT>", 0, null, null, null, null, 0 ],
               [ "<locale>" ],
               ...]

    The packages/shared/normalize/extract/detector.go BatchExecuteDetector
    reads inner[0][0] as the user prompt.
    """
    inner = [
        [prompt, 0, None, None, None, None, 0],
        [locale],
    ]
    inner_json = json.dumps(inner, separators=(",", ":"))
    outer = [None, inner_json]
    outer_json = json.dumps(outer, separators=(",", ":"))
    form = urllib.parse.urlencode({
        "f.req": outer_json,
        "at": "AOOh0PGQVV_oCeqqld81UdRSGItv:synthetic-test",
    })
    return form.encode("utf-8")


def post_via_proxy(target_url, body, proxy, headers, insecure, timeout=30):
    handlers = [
        urllib.request.ProxyHandler({
            "http": f"http://{proxy}",
            "https": f"http://{proxy}",
        }),
    ]
    if insecure:
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        handlers.append(urllib.request.HTTPSHandler(context=ctx))
    opener = urllib.request.build_opener(*handlers)
    req = urllib.request.Request(target_url, data=body, method="POST")
    for k, v in headers.items():
        req.add_header(k, v)
    try:
        resp = opener.open(req, timeout=timeout)
        return resp.status, resp.read(4096), ""
    except urllib.error.HTTPError as e:
        return e.code, e.read(4096), ""
    except urllib.error.URLError as e:
        return -1, b"", f"URLError: {e}"
    except Exception as e:  # noqa: BLE001
        return -1, b"", f"{type(e).__name__}: {e}"


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n", 1)[0])
    ap.add_argument("--proxy", default="cp.taskforce10x.com:3128")
    ap.add_argument(
        "--target",
        default="https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate",
    )
    ap.add_argument("--prompt", default="great do do do do do — synthetic Nexus test")
    ap.add_argument("--locale", default="en")
    ap.add_argument(
        "--secure",
        action="store_true",
        help="enforce TLS verify (default: insecure — Nexus CA may not be in certifi bundle)",
    )
    args = ap.parse_args()

    body = build_batchexecute_body(args.prompt, args.locale)

    headers = {
        # Google batchexecute uses form-urlencoded; the proxy + adapter
        # both sniff this byte-by-byte rather than trusting CT, so the
        # header is mostly cosmetic but we set it correctly anyway.
        "Content-Type": "application/x-www-form-urlencoded;charset=UTF-8",
        "User-Agent": (
            "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5_0) AppleWebKit/537.36 "
            "(KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 NexusSyntheticTest/1.0"
        ),
        "X-Nexus-Request-Id": f"geminiweb-synth-{int(time.time())}",
    }

    print("─" * 72)
    print("Gemini Web synthetic batchexecute — E46-S12 Tier-1 gemini-web adapter")
    print("─" * 72)
    print(f"proxy:  {args.proxy}")
    print(f"target: {args.target}")
    print(f"trace:  {headers['X-Nexus-Request-Id']}")
    print(f"prompt: {args.prompt!r}")
    print(f"body:   {len(body)} bytes form-urlencoded")
    print()
    print("POST in flight…")

    status, resp_body, err = post_via_proxy(
        args.target, body, args.proxy, headers, insecure=not args.secure, timeout=30
    )
    if err:
        print(f"transport error: {err}")
    else:
        print(f"upstream HTTP {status}")
        if resp_body:
            try:
                txt = resp_body.decode("utf-8", errors="replace")
                snippet = txt[:512] + ("…" if len(txt) > 512 else "")
                print(f"upstream response (truncated):\n{snippet}")
            except Exception:
                print(f"binary response ({len(resp_body)} B)")

    print()
    print("─" * 72)
    print("Verify in Control Plane:")
    print("─" * 72)
    print(
        f"""
  1. Open  https://nexus.taskforce10x.com/traffic
     Filter: target_host = gemini.google.com:443
             OR grep trace id  {headers['X-Nexus-Request-Id']!r}

  2. Open the row. Normalized panel should show:
       • Tier-1 badge (green):  Tier 1 · gemini-web · 0.85
       • Kind = ai-chat
       • Messages = 1
            user → {args.prompt!r}
       • Model — empty for request side (Gemini web doesn't put model
         in the request body; the response side would carry "3 Flash" /
         "2.5 Pro" etc.)

  3. If you see Tier 2 amber  "pattern:google-batchexecute-chat"
     instead — the gemini-web adapter's Tier 1 Normalize returned
     ErrUnsupported but the Tier 2 BatchExecuteDetector caught it.
     Still works; just lower attribution precision. Likely cause:
     the batchexecute envelope changed (Google evolves it; field
     numbers / structure drift).

  4. If no row shows up, the proxy didn't MITM. Check:
        - gemini.google.com in interception_domain table is enabled
        - Source IP allowlist allows your IP (Infrastructure → Overrides)
        """.rstrip()
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
