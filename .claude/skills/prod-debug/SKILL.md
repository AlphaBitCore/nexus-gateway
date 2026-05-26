---
name: prod-debug
description: >
  Diagnose production issues on taskforce10x.com. Covers: service logs,
  DB queries (traffic, analytics, cache, credentials, nodes), Redis inspection,
  NATS stream status, config/shadow inspection, metrics, and common failure
  patterns with known fixes. Trigger keywords: debug prod, prod issue,
  investigate prod, prod is broken, prod logs, check prod, /prod-debug.
user-invocable: true
---

# Prod Debug

Diagnose issues on the production Nexus Gateway (`taskforce10x.com`).

## Domain → service map (nginx /etc/nginx/conf.d/nexus.conf)

| Public domain | Reverse-proxied to | Service |
|---|---|---|
| `nexus.taskforce10x.com` | 127.0.0.1:3001 | **Control Plane** (admin BFF + SPA UI at /, API under /api /oauth /idp /.well-known /healthz /metrics /ready /debug) |
| `api.taskforce10x.com`   | 127.0.0.1:3050 | **AI Gateway** (OpenAI-compatible /v1/* surface — what end-user LLM clients hit) |
| `hub.taskforce10x.com`   | 127.0.0.1:3060 | **Hub** (WS at /ws for thingclient + REST under /api/internal/*) |

NOTE: `cp.taskforce10x.com` is NOT a separate domain — the CP lives on `nexus.taskforce10x.com`. DNS resolves the cp subdomain to the same IP, but nginx has no `cp.*` server_name, so https://cp.taskforce10x.com hangs on TLS.

Compliance Proxy (`nexus-compliance-proxy.service`) runs on the same EC2 instance but is reached as a TLS CONNECT proxy on a separate port (not behind public nginx); it's a transparent intercept point for org-managed devices, not a public web endpoint.

## Connection details

```bash
HOST=ec2-user@18.204.174.212   # passwordless (id_rsa.pub deployed)

# Shorthand for all commands below
alias prod="ssh -o StrictHostKeyChecking=no $HOST"
alias proddb="ssh -o StrictHostKeyChecking=no $HOST 'PGPASSWORD=VclwRVYAAadpVPJfY9hzd0cM psql -h localhost -U nexus -d nexus_gateway'"
```

**Prod API auth (for CP API calls from local machine):**
```bash
bash -c 'source tests/lib/loadenv.sh prod && source tests/lib/auth.sh && cp_login && cp_curl "/api/admin/..."'
```
All prod credentials/URLs come from `tests/.env.prod`. See `/prod-login` for
the full flow + failure-pattern table.

---

## Service logs

```bash
# Tail a service live
ssh -o StrictHostKeyChecking=no $HOST "sudo journalctl -u nexus-hub -f --no-pager"
ssh -o StrictHostKeyChecking=no $HOST "sudo journalctl -u nexus-ai-gateway -f --no-pager"
ssh -o StrictHostKeyChecking=no $HOST "sudo journalctl -u nexus-control-plane -f --no-pager"
ssh -o StrictHostKeyChecking=no $HOST "sudo journalctl -u nexus-compliance-proxy -f --no-pager"

# Last N lines
ssh -o StrictHostKeyChecking=no $HOST "sudo journalctl -u nexus-hub -n 100 --no-pager"

# Errors only
ssh -o StrictHostKeyChecking=no $HOST "sudo journalctl -u nexus-hub --since '1 hour ago' --no-pager | grep -i 'error\|fatal\|panic'"

# All services, errors, last 30 min
ssh -o StrictHostKeyChecking=no $HOST "sudo journalctl -u 'nexus-*' --since '30 min ago' --no-pager | grep -i 'ERROR\|FATAL\|panic'"

# Since last restart
ssh -o StrictHostKeyChecking=no $HOST "sudo journalctl -u nexus-ai-gateway --since today --no-pager | head -50"
```

---

## Service status

```bash
# Quick status of all 4
ssh -o StrictHostKeyChecking=no $HOST "sudo systemctl status nexus-hub nexus-control-plane nexus-ai-gateway nexus-compliance-proxy --no-pager | grep -E 'nexus-|Active:|Main PID'"

# Uptime / restart count
ssh -o StrictHostKeyChecking=no $HOST "sudo systemctl show nexus-hub --property=ActiveEnterTimestamp,NRestarts"
```

---

## Database queries

DB: `nexus_gateway`, user: `nexus`, host: `localhost`, password: `VclwRVYAAadpVPJfY9hzd0cM`

```bash
ssh -o StrictHostKeyChecking=no $HOST "PGPASSWORD=VclwRVYAAadpVPJfY9hzd0cM psql -h localhost -U nexus -d nexus_gateway -c \"<SQL>\""
```

### Node / thing status

```bash
# All real service nodes
SELECT id, type, status, version, last_seen_at FROM thing WHERE id LIKE '%-ip-172%' ORDER BY type;

# Any offline nodes
SELECT id, type, status, last_seen_at FROM thing WHERE status != 'online' ORDER BY last_seen_at DESC;

# Shadow state (desired vs reported)
SELECT id, type, desired_ver, reported_ver, desired_ver - reported_ver AS drift FROM thing WHERE id LIKE '%-ip-172%';

# Config values for a specific thing
SELECT id, desired, reported FROM thing WHERE id = 'gw-ip-172-31-1-117.ec2.internal-3050';
```

### Traffic / analytics

```bash
# Recent requests
SELECT timestamp, provider_id, model_id, status_code, error_code, cache_status,
       input_tokens, output_tokens, cost_usd
FROM traffic_event ORDER BY timestamp DESC LIMIT 20;

# Requests by adapter in last 24h
SELECT p.adapter_type, count(*) as reqs, sum(cost_usd) as cost
FROM traffic_event te JOIN "Provider" p ON p.id = te.provider_id
WHERE te.timestamp > now() - interval '24 hours'
GROUP BY p.adapter_type ORDER BY reqs DESC;

# Cache hits by adapter
SELECT p.adapter_type,
       count(*) FILTER (WHERE te.cache_status IN ('HIT','HIT_LIVE')) AS gw_hits,
       sum(te.gateway_cache_savings_usd) AS gw_savings,
       sum(te.cache_read_tokens) AS prompt_cache_read_tokens
FROM traffic_event te JOIN "Provider" p ON p.id = te.provider_id
WHERE te.timestamp > now() - interval '7 days'
GROUP BY p.adapter_type ORDER BY gw_hits DESC;

# Error breakdown
SELECT error_code, error_reason, count(*)
FROM traffic_event WHERE error_code IS NOT NULL
  AND timestamp > now() - interval '24 hours'
GROUP BY error_code, error_reason ORDER BY count DESC;

# Rollup data health
SELECT 'metric_rollup_5m' AS tbl, count(*), min(bucket), max(bucket) FROM metric_rollup_5m
UNION ALL
SELECT 'metric_rollup_1h', count(*), min(bucket), max(bucket) FROM metric_rollup_1h
UNION ALL
SELECT 'metric_rollup_1d', count(*), min(bucket), max(bucket) FROM metric_rollup_1d;
```

### Credentials

```bash
# List credentials (check encrypted_value looks like base64, not plaintext)
SELECT id, name, provider_id, created_at, length(encrypted_value) AS enc_len
FROM "Credential" ORDER BY created_at DESC LIMIT 20;

# Provider + credential pairs
SELECT p.name, p.adapter_type, c.name AS cred_name, c.created_at
FROM "Provider" p JOIN "Credential" c ON c.provider_id = p.id ORDER BY p.adapter_type;
```

### Virtual keys

```bash
# Active VKs
SELECT id, name, status, quota_usd, used_usd, expires_at
FROM "VirtualKey" WHERE status = 'active' ORDER BY created_at DESC LIMIT 20;

# Quota usage
SELECT name, quota_usd, used_usd, (quota_usd - used_usd) AS remaining
FROM "VirtualKey" WHERE status = 'active' ORDER BY used_usd DESC;
```

### Migrations status

```bash
# What's applied on prod
SELECT migration_name, finished_at FROM _prisma_migrations ORDER BY finished_at DESC LIMIT 15;

# Pending (compare with local migrations dir)
# Local: ls tools/db-migrate/migrations/ | sort
```

---

## Redis inspection

```bash
ssh -o StrictHostKeyChecking=no $HOST "redis-cli -h localhost"

# Inside redis-cli:
INFO server            # version, uptime
INFO memory            # used_memory_human
DBSIZE                 # total keys
KEYS nexus:*           # list nexus keys (careful on prod with large keyspaces — use SCAN)

# SCAN safely (non-blocking)
redis-cli -h localhost --scan --pattern 'nexus:*' | head -30

# Inspect a specific key
redis-cli -h localhost GET "nexus:session:<token>"
redis-cli -h localhost TTL "nexus:session:<token>"

# Cache ROI keys (response cache)
redis-cli -h localhost --scan --pattern 'rc:*' | wc -l   # response cache entry count

# Rate limit counters
redis-cli -h localhost --scan --pattern 'rl:*' | head -20
redis-cli -h localhost GET "rl:<vk_id>:<window>"

# Flush a specific key (targeted, not FLUSHALL)
redis-cli -h localhost DEL "nexus:iam:<user_id>"
```

---

## NATS inspection

```bash
# NATS server info
ssh -o StrictHostKeyChecking=no $HOST "curl -s http://localhost:8222/varz | python3 -m json.tool | grep -E 'version|connections|in_msgs|out_msgs'"

# Stream list
ssh -o StrictHostKeyChecking=no $HOST "curl -s http://localhost:8222/jsz?streams=true | python3 -m json.tool | grep -E 'name|messages|bytes|subjects'"

# Consumer lag (check if consumers are behind)
ssh -o StrictHostKeyChecking=no $HOST "curl -s 'http://localhost:8222/jsz?consumers=true' | python3 -m json.tool | grep -E 'name|num_pending|num_redelivered'"

# Specific stream info
ssh -o StrictHostKeyChecking=no $HOST "curl -s 'http://localhost:8222/jsz?stream=NEXUS_EVENTS' | python3 -m json.tool"

# NATS CLI (if installed)
ssh -o StrictHostKeyChecking=no $HOST "nats stream ls --server nats://localhost:4222"
ssh -o StrictHostKeyChecking=no $HOST "nats stream info NEXUS_EVENTS --server nats://localhost:4222"
```

---

## Metrics / Prometheus

```bash
# Hub metrics
ssh -o StrictHostKeyChecking=no $HOST "curl -s http://localhost:3060/metrics | grep -v '^#' | grep nexus_ | head -40"

# AI Gateway metrics
ssh -o StrictHostKeyChecking=no $HOST "curl -s http://localhost:3050/metrics | grep -v '^#' | grep nexus_ | head -40"

# Specific counter (e.g. request count)
ssh -o StrictHostKeyChecking=no $HOST "curl -s http://localhost:3050/metrics | grep 'nexus_gateway_requests_total'"

# Cache hit rate
ssh -o StrictHostKeyChecking=no $HOST "curl -s http://localhost:3050/metrics | grep 'nexus_cache'"
```

---

## Config / shadow inspection

The `thing.desired` and `thing.reported` JSONB columns hold the full config for each service.

```bash
# AI Gateway desired config (routing rules, VKs, etc.)
SELECT jsonb_pretty(desired) FROM thing WHERE id = 'gw-ip-172-31-1-117.ec2.internal-3050';

# Config sync state for all real things
SELECT id, desired_ver, reported_ver,
       CASE WHEN desired_ver = reported_ver THEN 'in-sync' ELSE 'DRIFTED' END AS sync_state
FROM thing WHERE id LIKE '%-ip-172%';

# Routing rules currently desired by Hub
SELECT jsonb_array_length(desired->'routing_rules') AS rule_count,
       desired_ver
FROM thing WHERE id = 'gw-ip-172-31-1-117.ec2.internal-3050';
```

---

## Common failure patterns

| Symptom | Where to look | Likely cause |
|---------|--------------|--------------|
| AI Gateway returns 503 | CP logs + Credential table | Credential decryption failure — key mismatch between `CREDENTIAL_ENCRYPTION_KEY` env and DB |
| All nodes offline in UI | Hub logs + thing table | Seed transaction error deleted real `thing` rows; restart all 4 services |
| Analytics shows no data | `metric_rollup_5m` count | Rollup pipeline stalled; Hub restart usually fixes |
| Cache ROI breakdown missing adapters | `traffic_event` + API response | Frontend filter bug or `gatewayCacheHitCount` missing from API response |
| Hub WARN: column does not exist | Hub logs + `_prisma_migrations` | Pending migration not applied on prod; apply targeted SQL (see prod-deploy skill) |
| Service won't die on SIGTERM | `kill -9 <pid>` | Some binaries (ai-gateway has happened) don't honor SIGTERM in time; escalate |
| Config not propagating | `thing.desired_ver` vs `reported_ver` | Service reconnect needed or WebSocket drop; restart service |
| JWT / auth 401 on CP API | CP logs | Token expired or wrong `NEXUS_OAUTH_REDIRECT_URI`; re-run `cp_login` |
| 401/403 between services after deploy | EnvironmentFile [MUST MATCH] env vars | One of `INTERNAL_SERVICE_TOKEN` / `ADMIN_KEY_HMAC_SECRET` / `CREDENTIAL_ENCRYPTION_KEY` / `COMPLIANCE_PROXY_API_TOKEN` / `AUTH_SERVER_ISSUER` drifted between services (e.g. one service was redeployed with a fresh secret, others still hold the old value). Re-issue from secrets manager and restart all services so every EnvironmentFile carries the same value. See `docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md` §6. |
| Service won't register as Thing (no row in `thing` table, Hub logs show no thingclient WS hello) | EnvironmentFile `NEXUS_HUB_URL` | If the pre-2026-05-20 name `CONTROL_PLANE_URL` is set instead, the new binary won't read it and falls back to yaml `registry.nexusHubUrl` (often empty in prod). Rename the env var to `NEXUS_HUB_URL`. Same class of regression for `NEXUS_HUB_CP_*` → `AUTH_SERVER_*` (Hub JWT verification 401s) and `NEXUS_HUB_AGENTCA_*` → `AGENT_CA_*` (agent enrollment fails). Full rename table: `docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md` §6. |

---

## Restart a single service (when needed)

```bash
ssh -o StrictHostKeyChecking=no $HOST "
  PID=\$(sudo systemctl show -p MainPID nexus-ai-gateway | grep -oP 'MainPID=\K[0-9]+')
  sudo kill \$PID
  sleep 3
  sudo systemctl start nexus-ai-gateway
  sudo journalctl -u nexus-ai-gateway -n 20 --no-pager
"
```

## Restart all 4 services (full restart)

Hub first, 5s gap, then the rest:
```bash
ssh -o StrictHostKeyChecking=no $HOST "
  sudo kill \$(sudo systemctl show -p MainPID nexus-hub nexus-control-plane nexus-ai-gateway nexus-compliance-proxy | grep -oP 'MainPID=\K[0-9]+') 2>/dev/null
  sleep 4
  sudo systemctl start nexus-hub && sleep 5
  sudo systemctl start nexus-control-plane nexus-ai-gateway nexus-compliance-proxy
  sleep 3
  sudo systemctl status nexus-hub nexus-control-plane nexus-ai-gateway nexus-compliance-proxy --no-pager | grep -E 'Active:|Main PID'
"
```
