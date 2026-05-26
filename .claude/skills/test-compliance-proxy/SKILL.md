---
name: test-compliance-proxy
description: >
  End-to-end smoke test for the Compliance Proxy. Use when the user wants
  to verify that the running compliance-proxy correctly MITM-intercepts
  HTTPS provider traffic on `:3128`, applies the compliance pipeline, and
  writes matching `traffic_event` rows (`source = 'compliance-proxy'`)
  plus Prometheus counters. Trigger keywords: test compliance proxy, smoke
  cp, verify compliance proxy, MITM smoke, /v1/messages proxy test, agent
  upstream simulation. No required input — credentials come from the DB
  (`Credential` table). Output: a Markdown report at
  `/tmp/test-compliance-proxy-<UTC-timestamp>.md`.
user-invocable: true
---

# Test Compliance Proxy

End-to-end smoke test against a running Compliance Proxy:

- MITM proxy on **`localhost:3128`** (this is the proxy port — `:3040` is
  the runtime API, **not** the proxy)
- Runtime API on `127.0.0.1:3040` (healthz, killswitch)
- Metrics on `:9090`
- CA cert at `packages/compliance-proxy/dev-certs/ca.crt`

For each enabled `(Provider, Credential)` pair in the DB whose adapter
type uses **bearer / API-key auth** (openai / anthropic / gemini / glm /
deepseek / minimax — bedrock & vertex are out of scope, they use cloud
SDK auth), this skill issues a non-streaming and a streaming chat call
through the proxy using a curl form like:

```bash
curl --proxy http://localhost:3128 \
     --cacert packages/compliance-proxy/dev-certs/ca.crt \
     -H "Authorization: Bearer <plaintext-provider-key>" \
     <baseUrl><pathPrefix><adapter-specific-chat-path>
```

then verifies a matching `traffic_event` row exists with `source =
'compliance-proxy'` and `target_host` set to the provider host.

## When to use

- User typed `/test-compliance-proxy` or asked something like "smoke
  test the compliance proxy", "verify CP MITM works".
- After changing code under `packages/compliance-proxy/` or
  `packages/shared/{policy/pipeline,policy/hooks,transport/streaming,audit}/`.
- When debugging why CP `traffic_event` rows are missing fields.

## Inputs

| Arg | Required | Notes |
|---|---|---|
| `--provider <name>` | no | Scope to one provider by name (`Provider.name`). Default: all eligible. |
| `--dry-run` | no | Print the planned (provider, model) test matrix without firing requests. |
| `--no-stream` | no | Skip the SSE pass for every provider. |

## Workflow

```
preflight → metrics t0 → resolve test matrix → obtain plaintext keys →
domain allowlist check → per-provider chat (non-stream + stream) →
DB verify → metrics delta → report → on-failure: fix-build-restart loop
```

### 1. Preflight

```bash
curl -fsS http://127.0.0.1:3040/healthz                     # runtime API
curl -fsS http://localhost:9090/metrics | head -1            # metrics
test -f packages/compliance-proxy/dev-certs/ca.crt           # CA present
openssl x509 -in packages/compliance-proxy/dev-certs/ca.crt -noout -subject -dates
```

If the proxy isn't up, do **not** silently start it. Tell the user. If
they confirm, start with:

```bash
cd packages/compliance-proxy && go run ./cmd/compliance-proxy/ \
  -config compliance-proxy.dev.yaml &
```

then tail `packages/compliance-proxy/logs/compliance-proxy.log` until
"listener.*:3128" or equivalent appears.

Capture `t0 = $(date -u +%FT%TZ)` and snapshot metrics:

```bash
curl -fsS http://localhost:9090/metrics > /tmp/cp-metrics-t0.prom
```

### 2. Resolve the test matrix

Query Postgres for eligible (provider, credential, sample model) rows.
The container name and DSN (dev): `postgres` container,
`postgresql://postgres:postgres@localhost:55532/nexus_gateway`.

```sql
SELECT
  p.id, p.name, p.adapter_type, p.base_url, p.path_prefix,
  c.id          AS cred_id,
  c."encryptedKey", c."encryptionIv", c."encryptionTag",
  c.encryption_key_id,
  -- cheapest enabled model belonging to this provider, by code:
  (SELECT m.code FROM "Model" m
    WHERE m."providerId" = p.id AND m.enabled
    ORDER BY m.code LIMIT 1)  AS sample_model_code
FROM "Provider" p
JOIN "Credential" c ON c."providerId" = p.id
WHERE p.enabled
  AND c.enabled
  AND p.adapter_type IN ('openai','anthropic','gemini','glm','deepseek','minimax')
ORDER BY p.name;
```

Run it via `docker exec ... psql -A -F$'\t' -t -c "..."`. If a row has no
`sample_model_code`, skip that provider and note "no enabled model".

### 3. Obtain plaintext provider keys

This is the only place the skill needs to "decrypt" something. The
encryption format used by the running CP/AI Gateway is **AES-256-GCM**
with hex-encoded ciphertext, IV, and tag (see
`packages/control-plane/internal/crypto/aes_gcm.go` and
`packages/ai-gateway/internal/credentials/decrypt.go` for the
authoritative format).

You have two strategies; pick whichever the local environment supports
without adding code, scripts, or dependencies to the repo:

**Strategy A — environment variable shortcut (preferred when it works).**
Before doing any decryption, check whether the user has already exported
plaintext keys for the providers in scope. The convention this skill
documents is `NEXUS_TEST_<UPPER_PROVIDER_NAME>_KEY`, e.g.
`NEXUS_TEST_OPENAI_KEY`. If set, use it directly and skip B for that
provider. This avoids touching the master encryption key entirely.

**Strategy B — runtime decryption via Node's built-in `crypto`.** Node
is already a project-level dependency (the `control-plane-ui` workspace
requires it), so `node` is available without installing anything new.
Read the master key from the same env vars the running services use:
`CREDENTIAL_KEY_MAP` (multi-key, comma-separated `id:hex,id:hex`) wins
over `CREDENTIAL_ENCRYPTION_KEY` (single 64-hex-char). Both formats are
defined in
`packages/control-plane/internal/crypto/aes_gcm.go` —
match that exactly.

A single-line decrypt invocation looks like:

```bash
plaintext=$(node -e '
const { createDecipheriv } = require("node:crypto");
const [encKey, encIV, encTag, encKeyID] = process.argv.slice(1);
let keyHex;
if (process.env.CREDENTIAL_KEY_MAP) {
  const map = Object.fromEntries(
    process.env.CREDENTIAL_KEY_MAP.split(",")
      .map(p => p.trim()).filter(Boolean)
      .map(p => p.split(":").map(s => s.trim()))
  );
  keyHex = map[encKeyID];
  if (!keyHex) {
    process.stderr.write(`unknown encryption_key_id: ${encKeyID}\n`);
    process.exit(2);
  }
} else if (process.env.CREDENTIAL_ENCRYPTION_KEY) {
  keyHex = process.env.CREDENTIAL_ENCRYPTION_KEY;
} else {
  process.stderr.write("CREDENTIAL_ENCRYPTION_KEY or CREDENTIAL_KEY_MAP env required\n");
  process.exit(2);
}
const d = createDecipheriv("aes-256-gcm", Buffer.from(keyHex, "hex"),
                           Buffer.from(encIV, "hex"));
d.setAuthTag(Buffer.from(encTag, "hex"));
process.stdout.write(Buffer.concat([
  d.update(Buffer.from(encKey, "hex")),
  d.final()
]));
' "$encKey" "$encIV" "$encTag" "$encKeyID")
```

If neither strategy yields a key for some provider, **skip that
provider** with a clear "no plaintext key available — set
`NEXUS_TEST_<PROVIDER>_KEY` or `CREDENTIAL_ENCRYPTION_KEY`" note in the
report. **Never** print plaintext keys to logs or to the report. Only
fingerprint them: `printf %s "$plaintext" | shasum -a 256 | cut -c1-8`.

### 4. InterceptionDomain allowlist check

CP only MITMs hosts that match an **enabled** row in the
`interception_domain` table (see schema for `host_pattern` +
`host_match_type`). Before issuing any request to provider X with host
H, query:

```sql
SELECT host_pattern, host_match_type, enabled, default_path_action
FROM interception_domain
WHERE enabled = true
  AND (
    (host_match_type = 'EXACT' AND host_pattern = $H)
    OR (host_match_type = 'SUFFIX' AND $H LIKE '%' || host_pattern)
    OR (host_match_type = 'REGEX' AND $H ~ host_pattern)
  );
```

(Match the actual enum values used in code; if you're unsure, read
`packages/compliance-proxy/internal/...` to confirm.) If no row matches:

- Note in the report: `"skipped: <host> not in interception_domain
  allowlist"`.
- Print the SQL the user can run to add it (do NOT run the insert
  yourself — config writes need explicit user approval per CLAUDE.md).

### 5. Per-adapter chat call (non-streaming)

Each adapter speaks its own protocol. Bundle these mappings inline (do
not re-resolve per call by reading source — the adapter set is stable):

| `adapter_type` | Chat path (appended to `<baseUrl><pathPrefix>`) | Auth header | Body shape (excerpt) |
|---|---|---|---|
| `openai` | `/chat/completions` | `Authorization: Bearer <key>` | `{model, messages, max_tokens:16, stream:false}` |
| `anthropic` | `/messages` | `x-api-key: <key>`, `anthropic-version: 2023-06-01` | `{model, max_tokens:16, messages, stream:false}` |
| `gemini` | `/models/<model>:generateContent?key=<key>` | (key in query) | `{contents:[{parts:[{text:"ok"}]}]}` |
| `glm` | `/chat/completions` | `Authorization: Bearer <key>` | `{model, messages, max_tokens:16, stream:false}` |
| `deepseek` | `/chat/completions` | `Authorization: Bearer <key>` | `{model, messages, max_tokens:16, stream:false}` |
| `minimax` | `/text/chatcompletion_v2` | `Authorization: Bearer <key>` | `{model, messages, max_tokens:16, stream:false}` |

Notes:
- `Provider.baseUrl` is **origin-only** (no version path). `pathPrefix`
  carries the version path (`/v1`, `/v1beta`, `/api/paas/v4`, …). Concat
  them then append the adapter-specific path. This matches the existing
  Provider schema convention (see CLAUDE.md memory
  "Provider baseUrl must be origin-only").
- Use the `sample_model_code` you got from the matrix query — that is
  the customer-facing model code clients send in `{"model": "..."}`.
- Always send a tiny prompt (max 16 tokens, single-turn `"reply with
  the single word: ok"`) to keep cost negligible.

Curl shape:

```bash
curl -fsS -w '\n%{http_code} %{time_total}\n' \
  --proxy http://localhost:3128 \
  --cacert packages/compliance-proxy/dev-certs/ca.crt \
  -H "$AUTH_HEADER" \
  -H 'Content-Type: application/json' \
  -X POST "$baseUrl$pathPrefix$chatPath" \
  -d "$body"
```

Record HTTP status, latency, response token usage if present, response
id. Save full response body for any non-2xx. Track the **target host**
(extract from `baseUrl`) for the DB lookup in step 7.

### 6. Per-adapter chat call (streaming, SSE)

Same mapping but with the streaming flag flipped per protocol:

| `adapter_type` | Streaming flag | Stream terminator |
|---|---|---|
| `openai`, `glm`, `deepseek`, `minimax` | body `stream: true` | `data: [DONE]` |
| `anthropic` | body `stream: true` | `event: message_stop` |
| `gemini` | path `:streamGenerateContent` instead of `:generateContent` | server closes stream |

Curl needs `-N` (`--no-buffer`) and `-H 'Accept: text/event-stream'`.
Record TTFB, chunk count, total bytes, terminator seen. Skip streaming
for any provider whose non-stream pass failed (note "skipped:
non-stream failed").

### 7. DB verification

For every chat call (stream or not), verify a `traffic_event` row landed
with `source = 'compliance-proxy'`:

```sql
SELECT id, status_code, target_host, model_name,
       prompt_tokens, completion_tokens, total_tokens,
       usage_extraction_status, hook_decision,
       api_key_fingerprint
FROM traffic_event
WHERE source = 'compliance-proxy'
  AND timestamp >= '$t0'::timestamptz
  AND target_host = '$providerHost'
ORDER BY timestamp DESC LIMIT 5;
```

For each call, you must find a matching row. Flag:

- No row → CP audit pipeline broken or `Compliance.enabled = false`.
- `api_key_fingerprint` empty → caller-key fingerprinting regression
  (this column is the SHA256[:8] of the **caller's real provider key**
  on this `source`; see `traffic_event` schema comments at
  `tools/db-migrate/schema.prisma:1004-1007`).
- `usage_extraction_status` ∉ `{ok, streaming_reported,
  streaming_estimated}` → token-extraction regression in the proxy.
- `target_host` empty / wrong → CONNECT/SNI parsing regression.

### 8. Metrics delta

```bash
curl -fsS http://localhost:9090/metrics > /tmp/cp-metrics-t1.prom
diff /tmp/cp-metrics-t0.prom /tmp/cp-metrics-t1.prom \
  | grep -E '^(>|<) (nexus_compliance_proxy_)' || true
```

Expect `nexus_compliance_proxy_connections_total`,
`nexus_compliance_proxy_upstream_request_duration_seconds_count`, and
`nexus_compliance_proxy_audit_enqueue_total` to all increase by ~N.
Flag any `*_errors_total` that incremented.

### 9. Render report

Write to `/tmp/test-compliance-proxy-<UTC-iso-timestamp>.md`:

```markdown
# Compliance Proxy Smoke — <timestamp>

## Environment
- Proxy: http://localhost:3128
- Runtime API: http://127.0.0.1:3040
- Metrics: http://localhost:9090/metrics
- CA: packages/compliance-proxy/dev-certs/ca.crt (subject=…, notAfter=…)
- Postgres container: <name> (DSN ...:55532/nexus_gateway)
- t0 / t1: <UTC>

## Test matrix (N providers)
| Provider | Adapter | Host | Sample model | Key source | Allowlist |
|---|---|---|---|---|---|
| openai-prod | openai | api.openai.com | gpt-4o-mini | env (NEXUS_TEST_OPENAI_KEY) | ✅ EXACT |
| anthropic-prod | anthropic | api.anthropic.com | claude-3-5-haiku | DB+CREDENTIAL_KEY_MAP | ✅ SUFFIX |
| zhipu-prod | glm | open.bigmodel.cn | glm-4-flash | DB+CREDENTIAL_KEY_MAP | ❌ skipped |

## Results (per provider)
| Provider | Chat | Stream | TE row | extraction | Notes |
|---|---|---|---|---|---|
| openai-prod | 200 (412 ms) | 200 (1.2 s, 7 chunks) | ✅ | ok | |
| anthropic-prod | 200 (520 ms) | 200 (1.4 s, 9 chunks) | ✅ | streaming_reported | |
| zhipu-prod | — | — | — | — | skipped: not in interception_domain |

## Metrics delta
| Counter | Δ |
|---|---|
| nexus_compliance_proxy_connections_total | +6 |
| nexus_compliance_proxy_audit_enqueue_total | +6 |

## Failures
(none / per-failure detail with sanitized request line + response body)

## Skipped providers + suggested SQL
For zhipu-prod, add to allowlist:
```sql
INSERT INTO interception_domain (id, name, host_pattern, host_match_type,
  adapter_id, enabled, priority)
VALUES (gen_random_uuid(), 'zhipu', 'open.bigmodel.cn', 'EXACT',
        '<adapterId>', true, 100);
```

## Result
**PASS** (2/3 providers green, 1 skipped — see allowlist suggestion)
```

Print the absolute path on the last line of stdout.

## When something fails — fix / build / restart authorization

Same authorization as the AI Gateway skill (per CLAUDE.md "Service
lifecycle (local dev / debugging)"). Scope of services this skill is
allowed to bounce: **compliance-proxy only** (and possibly nexus-hub if
the failure is in shadow / config sync). **Never** restart Postgres,
Redis, NATS, or unrelated Go services.

1. **Investigate** — read the relevant Go source. Files most often
   implicated: `packages/compliance-proxy/cmd/compliance-proxy/main.go`,
   `packages/compliance-proxy/internal/{audit,config,metrics,proxy,tls,compliance}/`,
   and the shared MITM bits under
   `packages/shared/{policy/pipeline,policy/hooks,transport/streaming,transport/tlsbump,audit}/`. Read
   `packages/compliance-proxy/logs/compliance-proxy.log` for stack
   traces (the dev config sets `stackOnError: true`).
2. **Form a hypothesis with the user** before editing — even a one-line
   summary. Do not silently rewrite intercept logic.
3. **Edit production code** following CLAUDE.md rules: real fix, no
   stubs, no TODOs, English only. If the fix changes wire behaviour,
   update the relevant SDD/architecture doc in the same change. Memory
   note: pre-GA, no backwards-compat shims (delete old code outright).
4. **Build** — `(cd packages/compliance-proxy && go build ./...)` and
   `go test -race -count=1 ./...` for affected packages.
5. **Restart** — `lsof -nP -iTCP:3128 -sTCP:LISTEN` to find the proxy
   process, confirm path under `packages/compliance-proxy/`, `kill
   <pid>`, escalate to `kill -9` only if stuck. Never kill `dlv` or a
   debugger — surface to user.
6. **Relaunch** — `cd packages/compliance-proxy && go run
   ./cmd/compliance-proxy/ -config compliance-proxy.dev.yaml &`, tail
   the log until the proxy listener is ready.
7. **Rerun** the failing single-provider scenario, confirm green, then
   rerun the full skill workflow to make sure you didn't introduce a
   regression for another adapter.
8. **Never** `git stash`, **never** `docker compose down`, **never**
   delete the dev-certs CA without asking — re-issuing the CA
   invalidates all client trust stores.

After fixes land and the report is green, end your final reply with the
mandatory commit reminder per CLAUDE.md. Do not auto-commit.

## Common mistakes

| Symptom | Likely cause |
|---|---|
| `curl: (35) … self-signed cert` even with `--cacert` | The running CP loaded a different CA than `dev-certs/ca.crt`. Run `openssl x509 -in dev-certs/ca.crt -serial -noout` and compare with what CP logs at startup. |
| `curl: (56) Recv failure` after CONNECT | Host not in `interception_domain`; CP's `default_path_action` is FAIL_CLOSED for the unknown domain. Check the allowlist. |
| Chat returns 200 but no `traffic_event` row | Audit pipeline backed up; check `nexus_compliance_proxy_audit_queue_depth`. |
| `target_host` is empty in the row | SNI parse failure on the CONNECT path — likely a bug in the proxy's TLS handshake handler. |
| Streaming stalls after first chunk | SSE accumulator regression in `packages/shared/transport/streaming/`. |
| `api_key_fingerprint` empty | Auth header extraction in CP regressed; check `packages/compliance-proxy/internal/.../auth*`. |

## Red flags — STOP and surface to user

- `Credential.encryptedKey` decrypts to a string that doesn't look like
  a provider key (`sk-…`, `sk-ant-…`, `AIza…`, `nvk_…`). Either the
  master key is wrong or the credential was seeded with a placeholder.
- The CP process you found via `lsof` is **not** under
  `packages/compliance-proxy/` (e.g. it's a system squid). Don't kill
  it — surface to user.
- `dev-certs/ca.crt` is missing or `dev-certs/ca.key` exists with
  different permissions than expected. Re-issuing the CA is destructive
  (every running client loses trust); ask before regenerating.
- Multiple `traffic_event` rows with `source = 'compliance-proxy'`
  written for a single chat call. That points to a double-write bug —
  surface, do not "fix" by deduping.

## What this skill does NOT do

- Does not test the AI Gateway or the Agent. Use `test-ai-gateway` for
  the gateway. The agent's MITM smoke would be a separate skill.
- Does not exercise bedrock / vertex adapters. Both use cloud-SDK auth
  (SigV4 / GCP service accounts) — out of scope for a curl-driven
  smoke. Add a follow-up skill if needed.
- Does not seed `Provider`, `Credential`, or `interception_domain`
  rows. Surfaces gaps; never auto-inserts.
- Does not run upstream provider correctness checks (token quality,
  jailbreak resilience, etc.). The point is the **proxy plumbing**:
  CONNECT, MITM, hooks pipeline, audit, metrics.
