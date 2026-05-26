# Cursor Traffic Capture — Debug Runbook

Context saved: 2026-05-11. Use this doc to restore investigation state without re-reading logs from scratch.

---

## Goal

Intercept, decode, and audit Cursor IDE AI traffic through the Compliance Proxy MITM.
"Intercept" = TLS bump succeeds + traffic_event rows written.
"Decode" = protobuf payloads parsed to extract prompt/response text, model, token counts.

---

## Reference Material

| Resource | Link / Path |
|----------|-------------|
| Reverse-engineered Cursor client (Python) | https://github.com/eisbaw/cursor_api_demo |
| Speedscale protocol analysis | https://speedscale.com/blog/peeking-under-the-hood-of-cursor/ |
| Compliance proxy source | `packages/compliance-proxy/` |
| Domain policy engine | `packages/shared/policy/domain/engine.go` (shared library) |
| Forward handler (bump path) | `packages/compliance-proxy/internal/proxy/forward/forward.go` |
| Bump / TLS intercept | `packages/compliance-proxy/internal/tls/{cache,issuer,kms,pinning}/` (issuer mints leaves; cache is two-tier `lru` + `redis` — labels match `metrics.CertCacheHits.With("lru"|"redis")` at `tls/cache/cache.go:83,93`; pinning auto-exempts on `bad_certificate`) consumed by `forward/forward.go` |
| Cursor traffic adapter | `packages/shared/traffic/adapters/ide/cursor/{cursor.go,normalize.go}` (registered in `packages/shared/traffic/adapters/builtins.go:85`) |

---

## Prod Environment

Required env vars (set via maintainer's local `.env`; see `.env.example`):
`PROD_SSH_TARGET`. The DB password is read from `/etc/nexus-gateway/env`
on the prod host (`PROD_DB_PASS=$(grep ^DATABASE_URL /etc/nexus-gateway/env | sed 's|.*://[^:]*:||;s|@.*||')`)
so it never lives locally.

```bash
HOST=${PROD_SSH_TARGET}      # e.g. ec2-user@<your-prod-ip>
DB_USER=nexus
DB_NAME=nexus_gateway
```

### Quick connect shortcuts

```bash
# SSH
ssh -o StrictHostKeyChecking=no ${PROD_SSH_TARGET}

# Compliance proxy logs (live)
ssh -o StrictHostKeyChecking=no ${PROD_SSH_TARGET} \
  "sudo journalctl -u nexus-compliance-proxy -f --no-pager"

# Last N lines
ssh -o StrictHostKeyChecking=no ${PROD_SSH_TARGET} \
  "sudo journalctl -u nexus-compliance-proxy -n 200 --no-pager"

# Errors only
ssh -o StrictHostKeyChecking=no ${PROD_SSH_TARGET} \
  "sudo journalctl -u nexus-compliance-proxy --since '1 hour ago' --no-pager \
   | grep -i 'error\|warn\|fatal\|panic'"

# Cursor-specific lines
ssh -o StrictHostKeyChecking=no ${PROD_SSH_TARGET} \
  "sudo journalctl -u nexus-compliance-proxy --since '30 min ago' --no-pager \
   | grep 'cursor\|api2\.cursor\|api3\.cursor'"

# DB query shorthand (password read from prod host's /etc/nexus-gateway/env)
ssh -o StrictHostKeyChecking=no ${PROD_SSH_TARGET} \
  'PGPASSWORD=$(grep ^DATABASE_URL /etc/nexus-gateway/env | sed "s|.*://[^:]*:||;s|@.*||") psql -h localhost -U nexus -d nexus_gateway -c "<SQL>"'
```

### Relevant DB queries

```sql
-- Cursor interception domains. All three seed rows ship with
-- default_path_action=PASSTHROUGH (not PROCESS) because the cursor adapter
-- only inspects a curated subset of paths and lets everything else pass
-- straight through — see tools/db-migrate/seed/data/seed-baseline.sql:937.
-- on_adapter_error=FAIL_CLOSED is the cursor-specific tightening (cursor is
-- more sensitive to bump-failure semantics than the OpenAI-compat rows).
SELECT id, name, host_pattern, adapter_id, enabled, default_path_action,
       on_adapter_error
FROM interception_domain
WHERE host_pattern LIKE '%cursor%'
ORDER BY priority DESC;
-- Returns: cursor-api2 (api2.cursor.sh), cursor-api3 (api3.cursor.sh), cursor-com (cursor.com)

-- Recent compliance-proxy traffic events
SELECT timestamp, provider_id, model_id, status_code, source
FROM traffic_event
WHERE source = 'compliance-proxy'
ORDER BY timestamp DESC LIMIT 20;

-- Interception domain schema
\d interception_domain
-- Key columns: id, name, host_pattern, host_match_type, adapter_id, enabled,
--              default_path_action, network_zone, capture_request_body,
--              capture_response_body, raw_body_spill_enabled

-- Check killswitch shadow state
SELECT desired->>'killswitch', reported->>'killswitch'
FROM thing WHERE id LIKE 'proxy-%';
```

### Service restart (compliance proxy only)

```bash
ssh -o StrictHostKeyChecking=no ${PROD_SSH_TARGET} "
  PID=\$(sudo systemctl show -p MainPID nexus-compliance-proxy | grep -oP 'MainPID=\K[0-9]+')
  sudo kill \$PID
  sleep 3
  sudo systemctl start nexus-compliance-proxy
  sudo journalctl -u nexus-compliance-proxy -n 30 --no-pager
"
```

---

## Cursor Protocol

### Domains

| Domain | Purpose | adapter_id in DB |
|--------|---------|-----------------|
| `api2.cursor.sh` | Primary AI (chat, models) | `cursor` |
| `api3.cursor.sh` | Telemetry / sync | `cursor` |
| `agent.api5.cursor.sh` | Agent (privacy mode) | not yet configured |
| `agentn.api5.cursor.sh` | Agent (non-privacy) | not yet configured |
| `cursor.com` | Web UI | `cursor` |

Cursor does **not** call Anthropic/OpenAI directly — all AI requests go to `api2.cursor.sh`, which proxies to the model provider server-side.

### Transport stack

```
Client (Cursor IDE)
  → HTTPS CONNECT tunnel → Compliance Proxy :3128
    → TLS bump (MITM)
      → HTTP/2 + Connect-RPC
        → api2.cursor.sh → Anthropic / OpenAI / etc.
```

- **Transport**: HTTP/2 (mandatory; HTTP/1.1 will not work)
- **RPC protocol**: Connect-RPC (Buf build; gRPC-compatible)
- **Content-Type**: `application/connect+proto`
- **Payload**: Protobuf binary

### Key endpoints

```
POST /aiserver.v1.ChatService/StreamUnifiedChatWithTools   # streaming chat
POST /aiserver.v1.AiService/AvailableModels                # model list
POST /oauth/token                                           # token refresh
```

### Required request headers (from reverse engineering)

```
Authorization: Bearer {token}
Content-Type: application/connect+proto
Connect-Protocol-Version: 1
user-agent: connect-es/1.6.1
x-cursor-client-version: 2.6.22
x-cursor-client-type: ide
x-cursor-client-os: darwin | linux | win32
x-cursor-client-arch: x64 | arm64
x-cursor-client-os-version: {kernel}
x-cursor-client-device-type: desktop
x-cursor-checksum: {Jyh-cipher(timestamp)}{machine_id}
x-cursor-config-version: {uuid}
x-cursor-timezone: {IANA timezone}
x-ghost-mode: false
x-session-id: {uuid-v5 derived from token}
x-request-id: {uuid}
x-amzn-trace-id: Root={same uuid as x-request-id}
```

### Wire format — Connect-RPC framing

**Request body** (client → proxy → api2.cursor.sh):
```
[flags: 1 byte][length: 4 bytes big-endian][protobuf payload]

flags=0x00  raw protobuf
flags=0x01  gzip-compressed protobuf  (used when ≥3 messages in conversation)
```

**Response streaming frames** (api2.cursor.sh → proxy → client):
```
[msg_type: 1 byte][msg_len: 4 bytes big-endian][msg_data: msg_len bytes]

type=0  raw protobuf
type=1  gzip-compressed protobuf
type=2  raw JSON  (errors, stream-end marker {})
type=3  gzip-compressed JSON
```

### Protobuf schema (partial, from reverse engineering)

**StreamUnifiedChatWithToolsRequest** (top-level request):
- field 1 (message): **Request**
  - field 1: repeated **Message** { content(1), role(2), messageId(13), chatModeEnum(47) }
  - field 5: **Model** { name(1), empty_bytes(4) }
  - field 23: conversationId (string)
  - field 46: chatModeEnum (int32=1)
  - field 54: chatMode (string="Ask")
  - field 26: **Metadata** { os(1), arch(2), version(3), path(4), timestamp(5) }

**StreamUnifiedChatResponseWithTools** (top-level response frame payload):
- field: `stream_unified_chat_response`
  - `text` — streaming text token (the AI output)
  - `filled_prompt` — debug: what was sent to the model
  - `thinking` — extended thinking tokens (Claude 3.7+)
  - `web_citation` — web search results
- field: `client_side_tool_v2_call` — tool use calls

**ProtobufDecoder utility** (from cursor_api_demo):
- Parse frame: `[flag:1][length:4BE][payload]`
- Decode varint: standard protobuf LEB128
- Field decoder: handles wire types 0 (varint), 1 (fixed64), 2 (length-delimited), 5 (fixed32)

### Authentication

Cursor reads tokens from local SQLite:
- macOS: `~/Library/Application Support/Cursor/User/globalStorage/state.vscdb`
- Linux: `~/.config/Cursor/User/globalStorage/state.vscdb`
- Keys: `cursorAuth/accessToken`, `cursorAuth/refreshToken`, `storage.serviceMachineId`

Embedded API key (baked into client bundle):
```
0c6ae279ed8443289764825290e4f9e2-1a736e7c-1324-4338-be46-fc2a58ae4d14-7255
```

Token refresh: `POST https://api2.cursor.sh/oauth/token` with `grant_type=refresh_token`.

---

## Current Capture Status

### What works
- CONNECT tunnel to `api2.cursor.sh:443` is accepted by the proxy ✓
- Domain allowlist includes all three Cursor domains (64 total domains loaded) ✓
- `interception_domain` rows exist with `adapter_id='cursor'`, `enabled=true`, `default_path_action=PASSTHROUGH` (path-level rules under the cursor adapter promote inspected endpoints to PROCESS at request time) ✓
- TLS bump works for other domains (e.g. `claude.ai` shows forward handler passthrough logs) ✓
- Cursor traffic adapter is implemented at `packages/shared/traffic/adapters/ide/cursor/{cursor.go,normalize.go}` and registered in `packages/shared/traffic/adapters/builtins.go:85`. Covers `/aiserver.v1.AiService/StreamChat` and `/aiserver.v1.ChatService/StreamUnifiedChatWithTools` end-to-end (see `cursor_test.go`).

### What does NOT work
- No forward_handler / compliance logs appear for Cursor connections
- No `traffic_event` rows written for `source='compliance-proxy'` from Cursor
- Connections stay open ~5 minutes then close with `"connection closed normally"`

### Root cause hypotheses (ordered by likelihood)

**Hypothesis 1 — Connect-RPC streaming not recognized (most likely)**
The prod binary (deployed before commit `187ef206`) does NOT have the fix that adds
`application/connect+proto` to the `isSSE` check in `forward_handler.go`. So when
Cursor's streaming response arrives, the old code tries to buffer the entire body via
`io.ReadAll`, which blocks until timeout (5 minutes). The client closes after its idle
timeout with no usable data captured.

Historical local fix (deployed long ago; preserved for archaeology):

```go
// forward_handler.go (legacy) — extended the streaming detection check to
// recognise Connect-RPC's content-type variants:
isSSE := strings.Contains(contentType, "text/event-stream") ||
    strings.Contains(contentType, "application/connect+proto") ||
    strings.Contains(contentType, "application/connect+json")
```

Current code has refactored the streaming decision out of a boolean
`isSSE` flag entirely — there is no `isSSE` identifier in the current
compliance-proxy source. Streaming routing now lives in
`packages/compliance-proxy/internal/proxy/forward/forward.go`'s response
type detection plus the adapter's per-path action. Treat the snippet above
as historical context only.

**Hypothesis 2 — TLS bump upstream fails (uTLS replay)**
The uTLS replay (replaying the Cursor client's JA3 fingerprint to `api2.cursor.sh`)
may fail. Cursor's client uses specific TLS extensions (e.g. PSK session tickets)
that our uTLS implementation doesn't handle. The proxy would then have no upstream
connection to forward to, so no HTTP requests flow.
Commit `187ef206` stripped PSK from uTLS replay — not deployed yet.

**Hypothesis 3 — HTTP/2 ALPN mismatch**
Our TLS bump server might not advertise `h2` in ALPN when presenting the MITM cert
to the Cursor client. Cursor (which requires HTTP/2) would then either refuse or
fall back to a mode that doesn't work.

### Next debugging steps

1. **Deploy local fixes first** — the `application/connect+proto` streaming fix +
   killswitch semantics fix + adapter registry changes are all unreleased. Deploy
   these and observe whether Cursor traffic starts appearing in logs/DB.

2. **Trace a single connectionId** — pick one `connectionId` from the CONNECT log,
   grep for it across all log levels to see the full lifecycle (including any TLS
   errors that may be at DEBUG level).

3. **Check bump.go HTTP/2 path** — verify the TLS server advertises `h2` in ALPN
   (`tls.Config.NextProtos`) when bumping. If not, Cursor connections silently degrade.

4. **Test with curl** — simulate a Cursor Connect-RPC request directly through the
   proxy to isolate whether the issue is TLS or HTTP/2 parsing:
   ```bash
   curl -v --proxy http://proxy-host:3128 \
        --cacert <nexus-ca.pem> \
        -H "Content-Type: application/connect+proto" \
        -H "Connect-Protocol-Version: 1" \
        --http2 \
        https://api2.cursor.sh/aiserver.v1.AiService/AvailableModels
   ```

---

## Update 2026-05-12 — Confirmed root cause for missing chat traffic

After re-examining a live Cursor session locally (system WiFi proxy → `:3128`,
`HTTPS_PROXY` exported, VS Code `http.proxy` set), the asymmetry is now
explained: **the chat / Composer / Agent endpoints never reach the compliance
proxy at all** — not a parsing or buffering issue. Earlier hypotheses 1–3
above remain valid for *traffic that does reach the proxy*, but they are not
the reason chat is missing. Status: investigation complete, fix **deferred**
(see "Why this is parked" below).

### Cursor IDE process model

Cursor is Electron + VS Code's extension-host model. A live session typically
shows four Node.js child processes (visible in `ps -ef`):

| Process tag (last column of `ps`) | Role |
|---|---|
| `Cursor Helper (Plugin): extension-host (user)`         | User-installed plugins (Go, ESLint, Prettier, …) |
| `Cursor Helper (Plugin): extension-host (retrieval)`    | Cursor's code indexing / embedding / `@codebase` search |
| `Cursor Helper (Plugin): extension-host (always-local)` | Extensions marked "must run locally" (debuggers, etc.) |
| `Cursor Helper (Plugin): extension-host (agent-exec)`   | **Cursor Agent / Composer / Chat execution** |

The trailing `nexus-gateway [1-N]` suffix is `<workspace-name> [<window>-<host>]`.

The `agent-exec` host is where streaming chat requests originate. It is a plain
Node.js process and does **not** use the Electron / Chromium network stack, so
macOS system proxy is irrelevant to it.

### Two network paths inside the same client

Cursor issues Connect-RPC calls over two distinct transports, and only one of
them is interceptable with a CONNECT proxy:

| Call type | Examples | Transport (Node API) | Goes through CONNECT proxy? |
|---|---|---|---|
| **Unary** (req → resp) | `AvailableModels`, `oauth/token`, `DashboardService`, `AnalyticsService` | `https.request` (HTTP/1.1) | **Yes** — patched by `@vscode/proxy-agent`, honours `HTTPS_PROXY` and VS Code `http.proxy` |
| **Server-streaming** | `ChatService/StreamUnifiedChatWithTools`, `AiService/StreamChat`, `AiService/StreamComposer` | **`http2.connect()`** (HTTP/2, `@connectrpc/connect-node` streaming transport) | **No** — bypasses every proxy mechanism listed below |

Node.js `http2.connect()` does **not**:
- Read macOS system proxy (SystemConfiguration framework is never consulted).
- Read `HTTPS_PROXY` / `HTTP_PROXY` env vars on its own.
- Get patched by `@vscode/proxy-agent`, which only hooks `http.request` and
  `https.request` (the HTTP/1.x APIs). There is no public `http2.connect`
  monkey-patch path.

Net effect: every streaming chat TCP connection from `extension-host (agent-exec)`
opens directly to `api2.cursor.sh:443` and never produces a `CONNECT` line in
the compliance-proxy log. Unary traffic from the same process still flows
through the proxy and is captured normally — which is exactly the asymmetry
seen in `traffic_event`.

### Why forcing HTTP/1.1 doesn't help

`cursor-agent` CLI can be coerced into HTTP/1.1, but the Cursor IDE's
streaming chat assumes HTTP/2 frame-level control (Connect-RPC server
streaming over HTTP/1.1 isn't supported by `@connectrpc/connect-node` in the
IDE bundle). Downgrading is not a viable workaround.

### The only viable fix (deferred)

**Transparent redirect via macOS `pf`** is the one method that does not
require modifying Cursor:

1. Add a `pf` rule that matches outbound TCP/443 from the UID running Cursor
   (or from the Cursor binaries by path) and redirects it to the compliance
   proxy on a dedicated transparent port (separate from `:3128`).
2. Teach compliance-proxy to read the original destination IP/port from the
   redirected socket. On macOS that means `getsockopt` on the redirected
   socket exposes the pre-`rdr` peer (the BSD equivalent of Linux's
   `SO_ORIGINAL_DST`), and SNI from the ClientHello identifies the host.
3. Bump TLS normally and process as today (the existing `interception_domain`
   rows for `api2.cursor.sh` continue to apply).

Cross-platform note: Linux supports the same approach via `iptables -j REDIRECT`
+ `getsockopt(SO_ORIGINAL_DST)`. Windows has no clean transparent-redirect
equivalent for unprivileged userspace; WFP callout driver would be required.

### Why this is parked

The fix needs (a) a new transparent listener mode in compliance-proxy,
(b) a `pf` ruleset + install/uninstall scripts, (c) a deployment story for
the Desktop Agent to manage `pf` rules per-user. Estimated at multi-day work
and not on the current epic backlog (E41 reliability + E42 config templates
take precedence). Re-open when there is product demand for capturing Cursor
chat specifically.

### Things tried that did NOT work

| Attempt | Why it failed |
|---|---|
| `networksetup -setwebproxy / setsecurewebproxy` (macOS WiFi proxy) | Only affects apps using CFNetwork. Node child processes bypass it. |
| `export HTTPS_PROXY=http://localhost:3128 && open -a Cursor` | `http2.connect()` does not read this env var. |
| VS Code setting `http.proxy` + `http.proxySupport: "on"` | `@vscode/proxy-agent` does not patch `http2.connect`. |
| Forcing HTTP/1.1 on the IDE | Streaming chat requires HTTP/2 in the connect-node bundle. |

---

## Cursor Adapter (built)

The cursor traffic adapter is implemented and registered. Source:

- `packages/shared/traffic/adapters/ide/cursor/cursor.go` — `Adapter` struct, ID `"cursor"`, the `aiserver.v1.AiService/StreamChat` and `/aiserver.v1.ChatService/StreamUnifiedChatWithTools` path predicates, and the request / response detection entry points.
- `packages/shared/traffic/adapters/ide/cursor/normalize.go` — protobuf frame decoder + user/assistant message extraction; outputs the canonical Tier-1 normalized shape.
- `packages/shared/traffic/adapters/ide/cursor/cursor_test.go` — synthetic end-to-end test that hand-rolls a `GetChatRequest` protobuf and asserts the resulting `traffic_event_normalized` row shows `kind=ai-chat`, `detectedSpec=cursor`, `model=claude-sonnet-4-6`, `confidence≈0.95`, and three decoded user/assistant/user messages.
- `packages/shared/traffic/adapters/builtins.go:85` — `{"cursor", func() traffic.Adapter { return &cursor.Adapter{} }}` registration entry.

Verification: invoke `Skill('test-cursor-adapter')` (see the skill description in this repo) to drive a synthetic GetChatRequest through the prod compliance proxy and confirm the normalized row.

Reference for protobuf shape (kept because the adapter only covers a subset of Cursor's RPC surface today and future endpoints can be added with the same decoder pattern): `cursor_streaming_decoder.py` and `cursor_chat_proto.py` in https://github.com/eisbaw/cursor_api_demo.
