# Operations Day 2 Cheatsheet

*Audience: operators managing a running Nexus Gateway deployment.*

This page is the operator's one-pager for the most common day-2 tasks: killing and restarting services, viewing logs, checking health, draining a node from routing, querying Prometheus metrics, and viewing the audit log. Each operation lists the exact command pattern for both a single-node EC2 deployment (the current baseline) and a local development stack.

---

## Service kill and restart

Services start and stop in dependency order. Nexus Hub must start before the services that register with it; stop in reverse order.

**Start order:** Hub → Control Plane → AI Gateway → Compliance Proxy

**Stop order:** Compliance Proxy → AI Gateway → Control Plane → Hub

### Production (systemd)

```bash
# Stop in reverse-dependency order
systemctl stop nexus-compliance-proxy
systemctl stop nexus-aigw
systemctl stop nexus-control-plane
systemctl stop nexus-hub

# Start in dependency order
systemctl start nexus-hub
sleep 3
systemctl start nexus-control-plane
sleep 3
systemctl start nexus-aigw
sleep 3
systemctl start nexus-compliance-proxy
```

Check whether a service came up:

```bash
systemctl is-active nexus-hub nexus-control-plane nexus-aigw nexus-compliance-proxy
```

Expected output: `active` for each.

### Local development (Go run)

Find a running service by port and send graceful `SIGTERM`:

```bash
# Identify the PID listening on the service port
lsof -nP -iTCP:3060 -sTCP:LISTEN   # Hub
lsof -nP -iTCP:3001 -sTCP:LISTEN   # Control Plane
lsof -nP -iTCP:3050 -sTCP:LISTEN   # AI Gateway
lsof -nP -iTCP:3040 -sTCP:LISTEN   # Compliance Proxy

# Send SIGTERM; escalate to SIGKILL only if not gone after ~3 s
kill <pid>
```

Restart with the same config file:

```bash
cd packages/nexus-hub        && go run ./cmd/nexus-hub        -config nexus-hub.dev.yaml &
cd packages/control-plane    && go run ./cmd/control-plane    -config control-plane.dev.yaml &
cd packages/ai-gateway       && go run ./cmd/ai-gateway       -config ai-gateway.dev.yaml &
cd packages/compliance-proxy && go run ./cmd/compliance-proxy -config compliance-proxy.dev.yaml &
```

---

## Viewing service logs

All Go services write structured JSON logs. Production logs land in the systemd journal; local dev services write to on-disk log files.

### Production (journald)

```bash
journalctl -u nexus-hub             -f --since "10 minutes ago"
journalctl -u nexus-control-plane   -f --since "10 minutes ago"
journalctl -u nexus-aigw            -f --since "10 minutes ago"
journalctl -u nexus-compliance-proxy -f --since "10 minutes ago"
```

Filter for errors only:

```bash
journalctl -u nexus-aigw -p err --since "1 hour ago"
```

### Local development (on-disk log files)

| Service | Log file |
|---|---|
| Nexus Hub | `packages/nexus-hub/logs/nexus-hub.log` |
| Control Plane | `packages/control-plane/logs/control-plane.log` |
| AI Gateway | `packages/ai-gateway/logs/ai-gateway.log` |
| Compliance Proxy | `packages/compliance-proxy/logs/compliance-proxy.log` |
| Agent | `packages/agent/logs/agent.log` |

Tail with JSON pretty-print (requires `jq`):

```bash
tail -f packages/ai-gateway/logs/ai-gateway.log | jq .
```

### Key log messages to watch

| Message pattern | Level | Meaning |
|---|---|---|
| `"job panic recovered"` | error | Scheduled Hub job crashed and recovered |
| `"redis get failed"` / `"redis set failed"` | warn | Valkey/Redis connectivity issue |
| `"compliance hook error"` | warn | Hook execution failure |
| `"NDJSON fallback"` | warn | Audit writing to local files — DB unreachable |
| `"config_apply_success"` | info | Shadow config applied; services reloaded without restart |

---

## Checking service health

Every service exposes `/healthz`. A 200 response means the service is alive.

```bash
# Production — replace <hostname> with your prod host/IP
curl -s https://<hostname>/healthz              # AI Gateway (via nginx)
curl -s http://<internal-ip>:3060/healthz       # Hub
curl -s http://<internal-ip>:3001/healthz       # Control Plane
curl -s http://<internal-ip>:3040/healthz       # Compliance Proxy

# Local development
curl -s http://127.0.0.1:3060/healthz
curl -s http://127.0.0.1:3001/healthz
curl -s http://127.0.0.1:3050/healthz
curl -s http://127.0.0.1:3040/healthz
```

A 503 from the Compliance Proxy `/healthz` means it is draining. A missing response (connection refused) means the process is not running.

Check that services registered with Hub (Hub's node registry is the canonical "what is online" view):

```bash
# Against local DB; adapt connection string for prod
docker exec $(docker ps --filter "name=postgres" -q | head -1) \
  psql -U postgres -d nexus_gateway -c \
  "SELECT id, type, status, version FROM thing ORDER BY type;"
```

Expected `status = 'online'` for every running service.

---

## Draining a node

"Draining" in Nexus Gateway means preventing the AI Gateway or Compliance Proxy from receiving new routing traffic while in-flight requests complete. The current single-node deployment has no built-in drain command; the operator workflow is:

1. Enable the emergency passthrough or kill switch via the Control Plane UI (`/infrastructure/kill-switch`) to stop new AI Gateway traffic immediately. See [Emergency Passthrough](Emergency-Passthrough) and [Kill Switch](Kill-Switch) for IAM requirements.
2. Wait for in-flight requests to complete — watch `nexus_aigw_requests_in_flight` (Prometheus gauge) drop to 0.
3. Apply changes, upgrade the binary, or perform maintenance.
4. Disable emergency passthrough / kill switch to resume traffic.

For planned maintenance windows, the `prod-deploy` skill automates this sequence — see [Operations Migrations On Prod](Operations-Migrations-On-Prod).

---

## Checking Prometheus metrics

All four Go services expose `/metrics` on their primary port.

| Service | Metrics endpoint |
|---|---|
| Nexus Hub | `http://<host>:3060/metrics` |
| Control Plane | `http://<host>:3001/metrics` |
| AI Gateway | `http://<host>:3050/metrics` |
| Compliance Proxy | `http://<host>:9090/metrics` |

Sample metric scrapes:

```bash
# Check AI Gateway request rate
curl -s http://127.0.0.1:3050/metrics | grep nexus_aigw_requests_total

# Check compliance proxy active tunnels
curl -s http://127.0.0.1:9090/metrics | grep tunnels_active

# Check Valkey availability (compliance proxy view)
curl -s http://127.0.0.1:9090/metrics | grep redis_available

# Check audit queue depth (compliance proxy; alert if sustained > 100)
curl -s http://127.0.0.1:9090/metrics | grep audit_queue_depth
```

Metrics to watch for immediate action:

| Metric | Condition | Action |
|---|---|---|
| `audit_ndjson_active` | = 1 | PostgreSQL unreachable; audit in fallback — investigate DB |
| `audit_write_errors_total` | rising | Persistent DB write failures |
| `tunnels_active` | > 80% of `maxConcurrentTunnels` | Near capacity — scale or reduce load |
| `redis_available` | = 0 | Valkey unreachable — cert cache degraded |

---

## Viewing the audit log

The admin audit log captures every admin API action (credential updates, routing rule changes, IAM policy changes, etc.). Query via the admin API or directly from PostgreSQL.

### Via admin API

```bash
# Source the auth helper (local dev):
source tests/lib/loadenv.sh
source tests/lib/auth.sh
cp_login

# Recent admin audit events (last 20)
cp_curl "/api/admin/audit?limit=20&order=desc"

# Filter by event type
cp_curl "/api/admin/audit?eventType=admin%3Acredential.update&limit=10"
```

### Via direct SQL

```bash
docker exec $(docker ps --filter "name=postgres" -q | head -1) \
  psql -U postgres -d nexus_gateway -c \
  "SELECT event_type, actor_id, emitted_at, resource_id
     FROM admin_audit
     ORDER BY emitted_at DESC
     LIMIT 20;"
```

Filter for security-sensitive events (credential rotations, IAM changes):

```sql
SELECT event_type, actor_id, emitted_at, resource_id
  FROM admin_audit
 WHERE event_type LIKE 'admin:credential%'
    OR event_type LIKE 'admin:iam%'
 ORDER BY emitted_at DESC
 LIMIT 50;
```

The audit log is append-only — rows are never updated or deleted by application code. Retention is configurable via the Control Plane UI at `/settings/observability`.

---

## Canonical docs

- [`local-dev-debugging.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/local-dev-debugging.md) — service log paths, kill/restart authority, `cp_login` / `cp_curl` helper contract
- [`monitoring.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/monitoring.md) — full Prometheus metrics catalog and health endpoint reference

**Adjacent wiki pages**: [Operations Runbook Index](Operations-Runbook-Index) · [Operations Logs Metrics Traces](Operations-Logs-Metrics-Traces) · [Operations FAQ](Operations-FAQ) · [Emergency Passthrough](Emergency-Passthrough) · [Kill Switch](Kill-Switch)
