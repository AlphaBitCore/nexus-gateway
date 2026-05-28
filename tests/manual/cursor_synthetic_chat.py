#!/usr/bin/env python3
"""Synthetic Cursor IDE chat traffic for end-to-end validation of the
Tier-1 cursor protobuf normalizer (E46-S12).

Sends a GetChatRequest protobuf (Connect-RPC content-type) to
api2.cursor.sh through the prod compliance proxy. Even if cursor's
upstream rejects our fake bearer token with HTTP 401/403, the proxy
will still:

  1. CONNECT-tunnel + TLS-bump (cursor.sh is in the prod
     interception_domain list as `adapter_id=cursor`),
  2. Run the cursor adapter's request hooks (now via the new
     adapter.Normalize path producing structured Messages),
  3. Forward upstream,
  4. Emit a traffic_event row with the bumped request body,
  5. Hub-side Tier-1 cursor.Normalize() decodes the protobuf and
     persists a NormalizedPayload with `Kind=ai-chat`,
     `DetectedSpec=cursor`, three Messages with user/assistant/user
     roles, and `Model=claude-sonnet-4-6`.

No third-party deps — uses urllib from the stdlib and hand-rolled
protobuf encoding via varint+tag bytes (matches what
protowire.Append* in the Go decoder reads).

Usage:
    python3 cursor_synthetic_chat.py [--insecure] [--proxy host:port] [--target url]

By default: --insecure (skip TLS verify; the Nexus CA may not be
trusted by Python's certifi bundle even when system trust accepts it),
proxy=localhost:3128 (override with --proxy or NEXUS_COMPLIANCE_PROXY_ADDR),
target=https://api2.cursor.sh/aiserver.v1.AiService/StreamUnifiedChatWithTools.
"""

from __future__ import annotations

import argparse
import json
import os
import ssl
import struct
import sys
import time
import urllib.error
import urllib.request


# ─── Protobuf wire encoding (just enough for the cursor schema) ─────────

WIRE_VARINT = 0
WIRE_LEN_DELIM = 2


def encode_varint(value: int) -> bytes:
    out = bytearray()
    while True:
        b = value & 0x7F
        value >>= 7
        if value:
            out.append(b | 0x80)
        else:
            out.append(b)
            return bytes(out)


def encode_tag(field_num: int, wire_type: int) -> bytes:
    return encode_varint((field_num << 3) | wire_type)


def encode_string(field_num: int, s: str) -> bytes:
    data = s.encode("utf-8")
    return encode_tag(field_num, WIRE_LEN_DELIM) + encode_varint(len(data)) + data


def encode_bytes(field_num: int, data: bytes) -> bytes:
    return encode_tag(field_num, WIRE_LEN_DELIM) + encode_varint(len(data)) + data


def encode_varint_field(field_num: int, value: int) -> bytes:
    return encode_tag(field_num, WIRE_VARINT) + encode_varint(value)


# ─── Cursor message schema ──────────────────────────────────────────────


def build_conversation_message(role_enum: int, text: str) -> bytes:
    """ConversationMessage protobuf:

      field 1 (string) → text
      field 2 (varint) → role (1=user, 2=assistant)
    """
    return encode_string(1, text) + encode_varint_field(2, role_enum)


def build_model_details(model_name: str) -> bytes:
    """ModelDetails sub-message: field 1 (string) → model_name."""
    return encode_string(1, model_name)


def build_get_chat_request(messages: list[tuple[str, str]], model_name: str | None = None) -> bytes:
    """Assemble a GetChatRequest payload.

    Field layout (matches packages/shared/traffic/adapters/cursor/cursor.go
    and the new normalize.go decoder):

      field 2  (repeated bytes) → ConversationMessage
      field 7  (bytes)          → ModelDetails

    `messages` is a list of (role, text) tuples where role is
    'user' or 'assistant'.
    """
    body = bytearray()
    for role, text in messages:
        role_enum = 1 if role == "user" else 2
        msg_bytes = build_conversation_message(role_enum, text)
        body += encode_bytes(2, msg_bytes)
    if model_name:
        body += encode_bytes(7, build_model_details(model_name))
    return bytes(body)


# ─── Optional Connect-RPC envelope wrap (for response-direction tests) ──


def wrap_connect_rpc_frame(payload: bytes, end_of_stream: bool = False) -> bytes:
    """Connect-RPC envelope: 1 flag byte + 4 BE length + payload."""
    flag = 0x01 if end_of_stream else 0x00
    return bytes([flag]) + struct.pack(">I", len(payload)) + payload


# ─── HTTPS-via-proxy request ────────────────────────────────────────────


def post_via_proxy(
    target_url: str,
    body: bytes,
    proxy: str,
    headers: dict[str, str],
    insecure: bool,
    timeout: int = 30,
) -> tuple[int, bytes, str]:
    """POST body to target_url via an HTTPS-CONNECT proxy at `proxy`.

    Returns (status_code, response_body, error_string). On
    network/connect failure the status is -1 and error_string
    describes what went wrong.
    """
    handlers: list[urllib.request.BaseHandler] = []
    handlers.append(
        urllib.request.ProxyHandler(
            {"http": f"http://{proxy}", "https": f"http://{proxy}"}
        )
    )
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
        return resp.status, resp.read(2048), ""
    except urllib.error.HTTPError as e:
        # Upstream returned a non-2xx — that's actually fine for our
        # test, the proxy still got to bump + audit before forwarding.
        return e.code, e.read(2048), ""
    except urllib.error.URLError as e:
        return -1, b"", f"URLError: {e}"
    except Exception as e:  # noqa: BLE001
        return -1, b"", f"{type(e).__name__}: {e}"


# ─── Main ───────────────────────────────────────────────────────────────


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n", 1)[0])
    ap.add_argument("--proxy", default=os.environ.get("NEXUS_COMPLIANCE_PROXY_ADDR", "localhost:3128"))
    ap.add_argument(
        "--target",
        default="https://api2.cursor.sh/aiserver.v1.AiService/StreamUnifiedChatWithTools",
    )
    ap.add_argument(
        "--secure",
        action="store_true",
        help="enforce TLS verify (default: insecure — Nexus CA may not be in certifi bundle)",
    )
    ap.add_argument(
        "--model",
        default="claude-sonnet-4-6",
        help="model name to embed in the synthetic ModelDetails sub-message",
    )
    args = ap.parse_args()

    messages = [
        ("user", "Hello Cursor — please explain Connect-RPC in one sentence."),
        (
            "assistant",
            "Connect-RPC is Buf's HTTP-friendly protobuf protocol that frames each message with a 5-byte envelope (1 flag + 4-byte big-endian length).",
        ),
        ("user", "And how is JSON-Patch used in chatgpt-web SSE deltas?"),
    ]

    payload = build_get_chat_request(messages, model_name=args.model)

    headers = {
        # Connect-RPC unary call content-type. Request side is typically
        # the bare protobuf (no envelope); streaming responses are
        # envelope-framed.
        "Content-Type": "application/connect+proto",
        "Connect-Protocol-Version": "1",
        # A fake bearer — cursor's upstream will 401 but the proxy still
        # MITMs the request and audits the bumped body.
        "Authorization": "Bearer cursor-synthetic-test-token-not-real",
        "User-Agent": "Cursor/0.42.0 (Nexus synthetic test; Macintosh; Intel Mac OS X 10_15_7)",
        # Nexus correlation header so you can grep this exact request in
        # the audit pipeline.
        "X-Nexus-Request-Id": f"cursor-synth-{int(time.time())}",
    }

    print("─" * 72)
    print(f"Cursor synthetic protobuf chat — Nexus E46-S12 Tier-1 cursor adapter")
    print("─" * 72)
    print(f"proxy:  {args.proxy}")
    print(f"target: {args.target}")
    print(f"trace:  {headers['X-Nexus-Request-Id']}")
    print(f"model:  {args.model}")
    print(f"messages ({len(messages)}):")
    for role, text in messages:
        snippet = text if len(text) <= 80 else text[:77] + "..."
        print(f"   [{role}] {snippet}")
    print(f"protobuf body: {len(payload)} bytes")
    print(f"  hex preview: {payload[:48].hex()}{'...' if len(payload) > 48 else ''}")
    print(f"  (decode-side: field 2 ×{len(messages)} + field 7 ModelDetails)")
    print()

    print("POST in flight…")
    status, body, err = post_via_proxy(
        args.target,
        payload,
        args.proxy,
        headers,
        insecure=not args.secure,
        timeout=30,
    )
    if err:
        print(f"transport error: {err}")
    else:
        print(f"upstream HTTP {status}")
        if body:
            # Show body as text if printable, else hex.
            try:
                txt = body.decode("utf-8")
                if txt.strip():
                    print(f"upstream response (first 2 KiB):")
                    print(txt[:2048])
            except UnicodeDecodeError:
                print(f"upstream binary response ({len(body)} B): {body[:64].hex()}…")

    print()
    print("─" * 72)
    print("Now verify in Control Plane:")
    print("─" * 72)
    print(
        f"""
  1. Go to  the Control Plane Traffic page (https://cp.<your-domain>/traffic)
     Filter: target_host = api2.cursor.sh:443
             (or grep by trace id {headers['X-Nexus-Request-Id']!r})

  2. Open the row. Expected on the Normalized tab:
       • green Tier-1 badge:  "Tier 1 · cursor · 0.95"  (model+messages both extracted)
       • Kind = ai-chat
       • Messages list:
            user      → "Hello Cursor — please explain Connect-RPC in one sentence."
            assistant → "Connect-RPC is Buf's HTTP-friendly protobuf protocol …"
            user      → "And how is JSON-Patch used in chatgpt-web SSE deltas?"
       • Model = {args.model}

  3. Raw tab should show the protobuf bytes as a BinaryRef (size +
     content-type), not human-readable — that's expected for binary
     protocol; the decoded Messages tab is where the value lives.

  If the badge shows Tier 2 (amber, "pattern:cursor"), cursor.Normalize
  returned ErrUnsupported on the body — pattern probe took over via
  Tier 2 fallback. Send the audit row id back here for diagnosis.

  If you don't see ANY new row, the proxy didn't MITM. Check:
    - cursor.sh in interception_domain table is enabled (it should be)
    - Source IP allowlist allows your IP (check Infrastructure → Overrides)
        """.rstrip()
    )

    return 0


if __name__ == "__main__":
    sys.exit(main())
