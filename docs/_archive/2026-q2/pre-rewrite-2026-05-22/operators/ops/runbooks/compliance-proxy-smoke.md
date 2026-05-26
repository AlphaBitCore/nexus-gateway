# Compliance Proxy Real-Flow Smoke Test

End-to-end probe of the compliance proxy MITM pipeline using **real provider
traffic** (OpenAI, Anthropic, DeepSeek, Moonshot). Verifies that a `curl`
client routed through the proxy on `:3128` is correctly intercepted, MITMed,
forwarded to the upstream, and that the resulting event reaches PostgreSQL
(`traffic_event` + `traffic_event_payload`), Prometheus, and the Hub MQ
consumer over JetStream.

**When to run:**
- After changes to `packages/compliance-proxy/internal/proxy/*`,
  `packages/shared/traffic/*`, `packages/shared/policy/payloadcapture/*`, or the
  `interception_domain` / `payload_capture` config plane.
- Before cutting a release build of compliance-proxy.
- During incident triage when the audit pipeline appears stuck.

The probe is **not** in CI: it requires live provider credentials and incurs
real (small) provider cost, so it is run by hand.

---

## 1. Prerequisites

All of the following must be running locally:

| Component | How to check |
|---|---|
| PostgreSQL (port 55532) | `docker ps \| grep postgres` |
| Redis (port 6437) | `docker ps \| grep redis` |
| NATS + JetStream (port 4222, monitor 8222) | `docker ps \| grep nats` and `curl -s http://127.0.0.1:8222/jsz` |
| Nexus Hub (port 3060) | `curl -s http://127.0.0.1:3060/healthz` |
| Control Plane (port 3001) | a JWT can reach an admin endpoint (see §2) |
| Compliance Proxy (CONNECT 3128, runtime 3040, metrics 9090) | `curl -s http://127.0.0.1:3040/healthz` returns `bumpEnabled=true` |
| Compliance Proxy MITM root CA | `packages/compliance-proxy/dev-certs/ca.crt` exists |

You also need an admin JWT for the Control Plane. The simplest way is to log
into the dashboard at `http://127.0.0.1:3000`, open DevTools → Application →
Cookies, copy the access token, and save it:

```bash
echo '<jwt>' > /tmp/nexus_jwt
```

Validate the token:

```bash
curl -sS -H "Authorization: Bearer $(cat /tmp/nexus_jwt)" \
  http://127.0.0.1:3001/api/admin/credentials -w "\nHTTP %{http_code}\n" | tail -3
```

Expect `HTTP 200`.

---

## 2. Obtain Plaintext Provider Keys

The `Credential` table stores AES-256-GCM ciphertext. There is no
`/credentials/:id/reveal` admin endpoint by design — keys never leave the
vault during normal operation. For this smoke test we decrypt locally using
the dev encryption key from `packages/control-plane/control-plane.dev.yaml`
(`crypto.encryptionKey`).

Build a one-off decryptor under `/tmp` (kept outside the repo):

```bash
mkdir -p /tmp/decrypt-creds && cat > /tmp/decrypt-creds/main.go <<'GO'
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"

	_ "github.com/lib/pq"
)

func main() {
	key, _ := hex.DecodeString("deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCMWithTagSize(block, 16)

	db, err := sql.Open("postgres",
		"postgresql://postgres:postgres@localhost:55532/nexus_gateway?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	rows, _ := db.Query(`
		SELECT c.name, p.name, p."baseUrl",
		       c."encryptedKey", c."encryptionIv", c."encryptionTag"
		  FROM "Credential" c
		  JOIN "Provider" p ON p.id = c."providerId"
		 ORDER BY p.name`)
	defer rows.Close()
	for rows.Next() {
		var name, prov, base, ct, iv, tag string
		_ = rows.Scan(&name, &prov, &base, &ct, &iv, &tag)
		ctB, _ := hex.DecodeString(ct)
		ivB, _ := hex.DecodeString(iv)
		tagB, _ := hex.DecodeString(tag)
		pt, err := aead.Open(nil, ivB, append(append([]byte{}, ctB...), tagB...), nil)
		if err != nil {
			log.Fatalf("decrypt %s: %v", name, err)
		}
		fmt.Printf("%s|%s|%s|%s\n", name, prov, base, string(pt))
	}
}
GO

cat > /tmp/decrypt-creds/go.mod <<'MOD'
module decryptcreds

go 1.25

require github.com/lib/pq v1.10.9
MOD

(cd /tmp/decrypt-creds && go run . 2>/dev/null \
  | awk -F'|' '{gsub(/-prod$/,"",$1); print $1"="$4}' > /tmp/nexus_keys.env)
chmod 600 /tmp/nexus_keys.env

awk -F= '{print $1"=<"length($2)" chars>"}' /tmp/nexus_keys.env
```

Expect four lines: `anthropic`, `deepseek`, `moonshot`, `openai`.

> **Note:** This step is dev-only. The hard-coded encryption key is the value
> shipped in `control-plane.dev.yaml`. In production the vault key comes from
> `CREDENTIAL_ENCRYPTION_KEY` and is not shippable.

---

## 3. Verify Interception Domains

The compliance proxy only MITMs hosts listed in `interception_domain`. A
fresh seed (`tools/db-migrate/seed/data/seed-baseline.sql:934+`) already
covers OpenAI, Anthropic, Gemini, Mistral, Cohere, **DeepSeek**
(`deepseek-public` → `api.deepseek.com`, adapter `deepseek`), and three
Moonshot rows (`moonshot-public-cn` → `api.moonshot.cn`,
`moonshot-public-international` → `api.moonshot.ai`, both with adapter
`moonshot`). All seeded rows ship with `default_path_action='PASSTHROUGH'`
because the consumer-surface adapters (cursor, claude-web, gemini-web, the
*-public OpenAI-compat rows) intentionally only inspect a curated subset of
paths and let everything else passthrough — see
`tools/db-migrate/seed/data/seed-baseline.sql:937` for the cursor rows that
use `FAIL_CLOSED` (cursor is more sensitive to bump-failure semantics) vs
`FAIL_OPEN` for the OpenAI-compat rows.

Confirm the rows exist on this DB (sanity check only; no admin write is
needed against a fresh seed):

```bash
docker exec $(docker ps --filter "name=postgres" -q | head -1) \
  psql -U postgres -d nexus_gateway -c \
  "SELECT name, host_pattern, adapter_id, default_path_action
     FROM interception_domain
    WHERE adapter_id IN ('deepseek','moonshot','openai-compat','anthropic')
    ORDER BY name;"
```

If the four rows are missing (you reseeded with a stale dump, or you're on a
custom dataset), add them through the admin API — never direct SQL, because
the admin path triggers `Hub.InvalidateConfig` and the compliance-proxy
hot-reloads via WebSocket shadow push without a restart:

```bash
TOKEN=$(cat /tmp/nexus_jwt)

# DeepSeek — adapter is the per-provider slug, not openai-compat
curl -sS -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -X POST "http://127.0.0.1:3001/api/admin/interception-domains" \
  -d '{
    "name":"deepseek-public","hostPattern":"api.deepseek.com",
    "hostMatchType":"EXACT","adapterId":"deepseek","enabled":true,
    "priority":0,"defaultPathAction":"PASSTHROUGH","onAdapterError":"FAIL_OPEN",
    "networkZone":"PUBLIC","source":"admin"
  }' -w "\nHTTP %{http_code}\n" | tail -3

# Moonshot (CN endpoint)
curl -sS -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -X POST "http://127.0.0.1:3001/api/admin/interception-domains" \
  -d '{
    "name":"moonshot-public-cn","hostPattern":"api.moonshot.cn",
    "hostMatchType":"EXACT","adapterId":"moonshot","enabled":true,
    "priority":0,"defaultPathAction":"PASSTHROUGH","onAdapterError":"FAIL_OPEN",
    "networkZone":"PUBLIC","source":"admin"
  }' -w "\nHTTP %{http_code}\n" | tail -3
```

`HTTP 201` on first create, `HTTP 500` with a duplicate-key message if the
row already exists (safe to ignore — the row is in the seed).

Confirm compliance-proxy applied the new shadow if you did write:

```bash
tail -50 packages/compliance-proxy/logs/compliance-proxy.log \
  | grep -E "interception_domains|payload_capture" | tail -5
```

Expect `event=config_apply_success config_key=interception_domains` with the
freshly bumped `desired_ver`.

---

## 4. Enable Payload Capture

Default is `storeRequestBody=false / storeResponseBody=false`. To verify the
full body pipeline, switch both flags on (the change propagates via Hub
shadow — no restart):

```bash
TOKEN=$(cat /tmp/nexus_jwt)

# Inspect current state
curl -sS -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:3001/api/admin/settings/payload-capture"

# Enable capture, raise body cap to 256 KiB
curl -sS -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -X PUT "http://127.0.0.1:3001/api/admin/settings/payload-capture" \
  -d '{"storeRequestBody":true,"storeResponseBody":true,"maxBodyBytes":262144}' \
  -w "\nHTTP %{http_code}\n"
```

Compliance-proxy log should show:

```
msg="payload capture config reloaded" storeRequestBody=true storeResponseBody=true maxBodyBytes=262144
```

> **Remember to revert** when done if your environment is shared (see §8).

---

## 5. Capture Baselines

Snapshot Prometheus metrics and the `traffic_event` row count before
generating any traffic, so deltas are unambiguous:

```bash
curl -sS http://127.0.0.1:9090/metrics > /tmp/metrics_before.txt

docker exec $(docker ps --filter "name=postgres" -q | head -1) \
  psql -U postgres -d nexus_gateway -tAc \
  "SELECT COUNT(*) FROM traffic_event WHERE source='compliance-proxy';" \
  > /tmp/te_count_before.txt
echo "traffic_event(compliance-proxy) before: $(cat /tmp/te_count_before.txt)"
```

---

## 6. Run the Four Provider Probes

The prompt is intentionally open-ended so each provider returns a real
self-introduction (~50–300 tokens), exercising the response body pipeline,
not just a one-token reply. Responses are saved under `/tmp/` for manual
inspection.

```bash
set -a; source /tmp/nexus_keys.env; set +a
CA=packages/compliance-proxy/dev-certs/ca.crt
PROXY=http://127.0.0.1:3128
PROMPT='hello, who are you? and what can you do for me?'

# OpenAI — Authorization: Bearer
curl -sS --proxy "$PROXY" --cacert "$CA" \
  -H "Authorization: Bearer $openai" -H "Content-Type: application/json" \
  -d "{\"model\":\"gpt-4o-mini\",\"max_tokens\":300,\"messages\":[{\"role\":\"user\",\"content\":\"$PROMPT\"}]}" \
  "https://api.openai.com/v1/chat/completions" \
  -o /tmp/resp_openai.json -w "openai HTTP %{http_code} %{time_total}s\n"

# Anthropic — x-api-key + anthropic-version (NOT Bearer)
curl -sS --proxy "$PROXY" --cacert "$CA" \
  -H "x-api-key: $anthropic" -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d "{\"model\":\"claude-haiku-4-5-20251001\",\"max_tokens\":300,\"messages\":[{\"role\":\"user\",\"content\":\"$PROMPT\"}]}" \
  "https://api.anthropic.com/v1/messages" \
  -o /tmp/resp_anthropic.json -w "anthropic HTTP %{http_code} %{time_total}s\n"

# DeepSeek — OpenAI-compatible
curl -sS --proxy "$PROXY" --cacert "$CA" \
  -H "Authorization: Bearer $deepseek" -H "Content-Type: application/json" \
  -d "{\"model\":\"deepseek-chat\",\"max_tokens\":300,\"messages\":[{\"role\":\"user\",\"content\":\"$PROMPT\"}]}" \
  "https://api.deepseek.com/v1/chat/completions" \
  -o /tmp/resp_deepseek.json -w "deepseek HTTP %{http_code} %{time_total}s\n"

# Moonshot — OpenAI-compatible
curl -sS --proxy "$PROXY" --cacert "$CA" \
  -H "Authorization: Bearer $moonshot" -H "Content-Type: application/json" \
  -d "{\"model\":\"moonshot-v1-8k\",\"max_tokens\":300,\"messages\":[{\"role\":\"user\",\"content\":\"$PROMPT\"}]}" \
  "https://api.moonshot.cn/v1/chat/completions" \
  -o /tmp/resp_moonshot.json -w "moonshot HTTP %{http_code} %{time_total}s\n"
```

Expect `HTTP 200` from all four, with response body sizes in the 0.5 – 2 KB
range. Inspect a response with:

```bash
python3 -c "import json; d=json.load(open('/tmp/resp_openai.json')); \
  print(d['choices'][0]['message']['content'])"
```

> **Anthropic model gotcha:** the API rejects model strings the account is
> not entitled to with `HTTP 404 not_found_error`. Read the live model list
> from `Model` joined to `Provider` (`name='anthropic'`) before the run if
> the default `claude-haiku-4-5-20251001` is not available.

---

## 7. Verify the Audit Pipeline

### 7.1 PostgreSQL — `traffic_event` + `traffic_event_payload`

```bash
docker exec $(docker ps --filter "name=postgres" -q | head -1) \
  psql -U postgres -d nexus_gateway -c "
SELECT to_char(te.timestamp, 'HH24:MI:SS.MS') AS ts,
       te.target_host, te.status_code, te.latency_ms,
       te.hook_decision, te.bump_status,
       octet_length(p.request_body::text)  AS req_bytes,
       octet_length(p.response_body::text) AS resp_bytes
  FROM traffic_event te
  LEFT JOIN traffic_event_payload p ON p.traffic_event_id = te.id
 WHERE te.source='compliance-proxy'
 ORDER BY te.timestamp DESC LIMIT 4;"
```

Expected for each row: `status_code=200`, `hook_decision=APPROVE`,
`bump_status=BUMP_SUCCESS`, `req_bytes ≈ 130–160`, `resp_bytes` matches the
`/tmp/resp_*.json` size on disk (within JSON re-serialization noise).

### 7.2 Prometheus — counter deltas

The compliance-proxy uses the **dotted opsmetrics** naming convention. The
old `nexus_compliance_proxy_*` namespace prefix has been **dropped** per
the no-backcompat rule (see the comment at
`packages/compliance-proxy/internal/metrics/prometheus.go:11`). When
Prometheus scrapes the dotted names, the underlying client library
translates `.` to `_` for the wire format, so metric line names appear as
e.g. `cert_cache_hits_total{layer="l1"}` — but the canonical names live in
`metrics/prometheus.go` and are reproduced below.

```bash
curl -sS http://127.0.0.1:9090/metrics > /tmp/metrics_after.txt

# Inspect a sampling of the real exported metrics
grep -E '^(tunnels_(active|total)|cert_cache_(hits|misses)_total|cert_sign_ms|cert_prewarm_duration_ms|pinning_passthrough_total|killswitch_active|attestation_verify_total|redis_available)' \
  /tmp/metrics_after.txt
```

The compliance-proxy-owned instruments registered by
`metrics.Register` (per `packages/compliance-proxy/internal/metrics/prometheus.go`):

| Canonical name | Type | Labels | Notes |
|---|---|---|---|
| `tunnels.active` | gauge | — | Active CONNECT tunnels right now. |
| `tunnels.total` | counter | `result` | Lifetime CONNECTs by accept / deny / error. |
| `cert_cache.hits_total` | counter | `layer` (`l1` / `l2`) | Cache hit by tier. |
| `cert_cache.misses_total` | counter | — | Cache miss → mint. |
| `cert_cache.size` | gauge | — | Current entry count. |
| `cert_sign_ms` | histogram | — | Per-mint cert sign latency. |
| `cert_prewarm.duration_ms` | gauge | — | Last prewarm run duration. |
| `pinning.passthrough_total` | counter | `status` | Auto-exemption decisions. |
| `killswitch.active` | gauge | — | 0 / 1; flipped by runtime API. |
| `attestation.verify_total` | counter | `outcome` | Agent-attestation verification outcomes. |
| `redis.available` | gauge | — | L2 health. |

TLS handshake + upstream-request latency metrics were moved to the shared
`shared/tlsbump` package in E55 — they're registered by
`tlsbump.RegisterMetrics` so the agent shares the same instruments. Look for
`tls_handshake_ms` and `upstream_request_ms` in the scrape output, not under
the compliance-proxy metrics package.

There is no `audit_batch_size_count`, `audit_bump_status_total`, or
`upstream_request_duration_seconds_count` in the current code; if a runbook
update wants those, register them in `metrics/prometheus.go` first.

### 7.3 NATS JetStream — Hub consumer caught up

```bash
curl -sS "http://127.0.0.1:8222/jsz?streams=true&consumers=true" \
  | python3 -c "
import json,sys
d=json.load(sys.stdin)
for acct in d.get('account_details',[]):
  for s in acct.get('stream_detail',[]):
    for c in s.get('consumer_detail',[]):
      if 'compliance' in c.get('name',''):
        print(f\"{c['name']}  delivered={c.get('delivered',{}).get('consumer_seq')}  \" \
              f\"acked={c.get('ack_floor',{}).get('consumer_seq')}  \" \
              f\"pending={c.get('num_pending')}  \" \
              f\"redelivered={c.get('num_redelivered')}\")
"
```

Expect a durable named `hub-db-writer__nexus_event_compliance` (the
underscore-slug form produced by `jetstreamDurableName(group, subject)` in
`packages/shared/transport/mq/consumer.go:195`) with
`delivered ≥ <baseline+4>`, `acked = delivered`, `pending = 0`,
`redelivered = 0`.

A non-zero `pending` or growing `redelivered` indicates Hub is failing to
process events — check `packages/nexus-hub/logs/nexus-hub.log` for
`compliance` consumer errors.

---

## 8. Cleanup

If your environment is shared, restore defaults:

```bash
TOKEN=$(cat /tmp/nexus_jwt)

# Disable payload capture again
curl -sS -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -X PUT "http://127.0.0.1:3001/api/admin/settings/payload-capture" \
  -d '{"storeRequestBody":false,"storeResponseBody":false,"maxBodyBytes":65536}'

# (Leave deepseek-public / moonshot-public in place — they are useful seed
#  data for any subsequent run, and they only cost a few KB on disk.)
```

Remove temp artifacts:

```bash
rm -rf /tmp/decrypt-creds \
       /tmp/nexus_keys.env /tmp/nexus_jwt \
       /tmp/resp_*.json \
       /tmp/metrics_before.txt /tmp/metrics_after.txt \
       /tmp/te_count_before.txt
```

---

## 9. Troubleshooting

| Symptom | Likely cause | Where to look |
|---|---|---|
| curl exits with `SSL certificate problem` | `--cacert` missing or proxy regenerated its CA | Reissue `dev-certs/`; restart compliance-proxy |
| `HTTP 502/504` from proxy | upstream provider unreachable | Probe `curl https://<host>/` directly without `--proxy` |
| `HTTP 403` from proxy with `domain not allowed` | host not in `interception_domain` | §3 — POST a row, watch shadow apply log |
| 200 response but no `traffic_event` row | audit batch not flushed yet | Wait 1 s (default `flushIntervalMs=500`); confirm `audit_queue_depth` returns to 0 |
| 200 response but `traffic_event_payload` missing bodies | payload capture still off | §4 — re-PUT `storeRequestBody=true` |
| `traffic_event` rows missing after probes | NATS JetStream stream missing or wrong subject; producer failure in audit pipeline | `curl http://127.0.0.1:8222/jsz?streams=true`; ensure `NEXUS_EVENTS` stream covers `nexus.event.compliance`; check `packages/compliance-proxy/logs/compliance-proxy.log` for MQ writer errors |
| `pending > 0` on Hub consumer | Hub failing to write to PG; back-pressure | `packages/nexus-hub/logs/nexus-hub.log`; check PG connectivity |
| `bump_status=BUMP_FAILED` | TLS handshake to upstream failed (cert pinning hit?) | `cert_sign_ms_count` vs `cert_cache_misses_total`; check pinning exemptions; consult `pinning_passthrough_total{status}` |
