# E53 — Cursor StreamChat Traffic Capture

**Status:** parked. Investigation memory at
`memory/project_cursor_capture_investigation.md`. Verification test
parked at `memory/project_ne_cursor_streamchat_verification.md`.

## Background

Cursor IDE's chat traffic to `api2.cursor.sh` uses HTTP/2 with
`:method = CONNECT, :protocol = websocket`. The compliance-proxy
intercepts only HTTP/1.1 CONNECT requests and unary HTTPS. Result:
Cursor StreamChat traffic flows around the proxy entirely and is
neither inspected nor logged.

Captured today: Cursor's unary calls (auth, file uploads, completions
metadata) hit the proxy correctly. Only the long-lived streaming chat
channel escapes.

## Why this matters

- Compliance gap: Cursor users' AI conversation content is the
  HIGHEST-VALUE traffic to monitor (it carries the prompts +
  responses) and we currently see none of it.
- This isn't a Cursor-specific problem — any client using HTTP/2
  Extended CONNECT (RFC 8441) will bypass us the same way.

## Story 1 — pf transparent proxy intercept (macOS)

**User story:** As an admin, I want compliance-proxy to capture
StreamChat traffic transparently, without depending on each client
honoring HTTP_PROXY env vars.

**Approach:**
- Use macOS `pf` rules to redirect outbound traffic to known LLM
  destinations through compliance-proxy on a transparent intercept
  port. The agent already drops a pf anchor on enrollment for the
  audit/proxy wiring — extend it.
- On the proxy side: add a TPROXY-equivalent listener. Linux uses
  `IP_TRANSPARENT` socket option; macOS uses `pf rdr-to`.
- Recover the original destination via `getsockopt(IPV6_ORIGINAL_DST)`
  (Linux) or `pf state lookup` (macOS).
- Once the original `host:port` is known, the normal CONNECT
  upgrade + bump flow takes over.

**Tasks:**
- T1.1 Linux: add `IP_TRANSPARENT` listener + `getsockopt SO_ORIGINAL_DST`
  recovery in `compliance-proxy/internal/proxy/listener.go`.
- T1.2 macOS: pf rdr-to rule generation in agent platform/darwin;
  expand the existing pf anchor to redirect 443 → proxy:transparent_port
  for the LLM-destinations allowlist.
- T1.3 Proxy: when a connection arrives on the transparent listener,
  synthesize an internal CONNECT request and route through the
  existing handler.
- T1.4 Tests: e2e test that simulates HTTP/2 Extended CONNECT and
  asserts the resulting traffic_event captures the body.

**Acceptance:**
- Cursor's StreamChat → api2.cursor.sh captured in traffic_event
  with non-empty request/response bodies.
- Normal HTTP/1.1 traffic unchanged (regression-test the existing
  CONNECT path).
- pf anchor uninstalled cleanly on agent uninstall (no stale rules).

**Estimate:** 2-3 days for the Linux path; macOS pf wiring +
uninstall safety adds another 1-2 days. Total ~1 week.

**Risk:** Medium-high. Transparent intercept changes the routing
fabric. Wrong pf rules can take a user's network OFFLINE. Critical
acceptance: agent uninstall removes every pf rule it created.

## Story 2 — NE-based capture verification (macOS)

**User story:** Before shipping S1's pf approach, verify the
existing macOS NETransparentProxy agent path actually captures
HTTP/2 Extended CONNECT.

**Approach:**
- The agent's NETransparentProxy already sees raw IP packets pre-routing.
  StreamChat traffic SHOULD be visible there.
- Test: install the production-signed .pkg, run Cursor chat with
  the agent's payload-capture on, query `traffic_event` for any
  rows with `dest_host = api2.cursor.sh` and non-empty body.

**Tasks:**
- T2.1 Install latest signed agent .pkg on a test Mac with Cursor
  installed.
- T2.2 Trigger a Cursor chat session.
- T2.3 Query traffic_event filtering by deviceId + dest_host.
- T2.4 If body captured → S1 (pf intercept) becomes lower priority;
  use NE-based capture as the canonical macOS path.
- T2.5 If body NOT captured → confirm gap, prioritize S1.

**Acceptance:** Verification outcome documented in
`memory/project_ne_cursor_streamchat_verification.md` with the
exact query that proves capture (or absence).

**Estimate:** 1 hour. No code change; just running the test.

## Priority

S2 (verification) should run first — it's an hour vs S1's week.
The result determines whether S1 is needed at all on macOS.
S1 is needed on Linux regardless (no NE equivalent).
