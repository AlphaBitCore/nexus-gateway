# Operations FAQ

*Audience: operators managing a running Nexus Gateway deployment.*

Answers to common day-2 operational questions. For step-by-step procedures, see [Operations Day 2 Cheatsheet](Operations-Day-2-Cheatsheet) and [Operations Runbook Index](Operations-Runbook-Index).

---

## Service health and restarts

**How do I check whether all services are running?**

Query the Hub node registry — it is the canonical source of which services are online:

```bash
docker exec $(docker ps --filter "name=postgres" -q | head -1) \
  psql -U postgres -d nexus_gateway -c \
  "SELECT id, type, status, version FROM thing ORDER BY type;"
```

Expected `status = 'online'` for every deployed service. Also check each health endpoint directly (see [Operations Day 2 Cheatsheet](Operations-Day-2-Cheatsheet)).

**A service is showing `offline` in Hub but the process is running. What is wrong?**

The service has lost its WebSocket connection to Hub. Check: (1) Hub is reachable on `:3060`; (2) `INTERNAL_SERVICE_TOKEN` matches between Hub and the service — a mismatch causes the service to fail the Hub handshake silently; (3) the service log for connection errors (`thingclient`).

**The AI Gateway restarted but is not picking up new routing rules. What do I check?**

The AI Gateway pulls config from Hub on startup and on each Hub change-signal. Check: (1) `systemctl status nexus-hub` — if Hub is down, the gateway cannot pull config; (2) `INTERNAL_SERVICE_TOKEN` matches; (3) the AI Gateway log for `config_apply_success` — if absent, the shadow pull is failing.

---

## Traffic and routing

**Requests are returning 401 even though the virtual key is correct. What could be wrong?**

Check in order: (1) the virtual key status in the UI — it may be revoked or expired; (2) `CREDENTIAL_ENCRYPTION_KEY` matches between AI Gateway and Control Plane — a mismatch means the gateway cannot decrypt the credential; (3) clock skew — VK expiry is time-based; (4) the AI Gateway log for `vkauth` errors.

**Requests are returning 403 with "IAM deny". How do I debug this?**

The `iam_denials_total` Prometheus counter increments on every IAM deny. The detailed reason is in the Control Plane log (level `info`). Check that the virtual key's scope (`allowed_models`, `allowed_providers`) covers the requested model, and that the routing rule's IAM policy allows the request.

**All requests to a specific provider are failing. How do I diagnose?**

1. Check credential health: `cp_curl "/api/admin/credentials"` — look for `circuitState: "open"` or `healthStatus: "unavailable"`.
2. Check the provider's upstream status page independently.
3. Query recent traffic events for that provider:

```sql
SELECT status_code, COUNT(*) FROM traffic_event
WHERE provider = 'openai' AND timestamp > NOW() - INTERVAL '30 minutes'
GROUP BY status_code;
```

4. If the circuit is open due to a transient issue and the provider is healthy, reset it: `cp_curl -X POST "/api/admin/credentials/<credId>/reset-circuit"`.

---

## Audit and compliance

**Where are traffic events stored? How long are they retained?**

Traffic events are in the `traffic_event` table in PostgreSQL. Retention is configurable via the Control Plane UI at **Settings → Observability → Retention**. The default retention depends on the deployment configuration; check `metric_ops_retention_config` and the traffic event retention settings in the UI.

**An alert fired for a compliance event. How do I acknowledge it?**

Open the Control Plane UI at **Alerts**, find the alert row, click to open the detail drawer, and click **Acknowledge**. Via API:

```bash
cp_curl -X POST "/api/admin/alerts/<alertId>/ack" \
  -H 'Content-Type: application/json' \
  -d '{"reason": "investigating"}'
```

See the [alerts runbook](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/alerts.md) for the full triage workflow.

**How do I look up who made a specific admin change?**

Query the admin audit log:

```bash
cp_curl "/api/admin/audit?limit=50&order=desc"
```

Or directly:

```sql
SELECT event_type, actor_id, emitted_at, resource_id
FROM admin_audit
WHERE emitted_at > NOW() - INTERVAL '24 hours'
ORDER BY emitted_at DESC;
```

Every admin API write records an audit event. Sensitive operations (credential updates, IAM changes, kill switch toggles) are always audited — the plaintext of any credential is never logged.

---

## Database and migrations

**How do I check which migrations have been applied to production?**

```bash
ssh $PROD_SSH_TARGET 'PGPASSWORD=... psql -h localhost -U nexus -d nexus_gateway \
  -c "SELECT migration_name FROM _prisma_migrations ORDER BY migration_name DESC LIMIT 10;"'
```

**A Prisma migration appears to have been skipped. What happened?**

Two migration folders sharing the same `YYYYMMDDHHMMSS` prefix cause Prisma to silently skip one. Check for prefix collisions:

```bash
ls tools/db-migrate/migrations/ | cut -c1-14 | sort | uniq -d
```

A non-empty result means a collision exists. Rename the migration folder with a unique timestamp and coordinate with the prod migration checklist.

**I need to run a one-off SQL fix on production. What is the safe procedure?**

Use the pattern in `docs/operators/ops/runbooks/prod-deploy-data-changes.md` Section 3: write an idempotent SQL script, take a `pg_dump` backup first, run the script via `psql` over SSH, verify the results. Never run one-off SQL that modifies data without a backup and a verification query.

---

## Valkey / cache

**Valkey restarted and services cannot connect. What should I check?**

1. Confirm Valkey is running: `docker exec nexus-valkey valkey-cli ping` → `PONG`.
2. Check that `NEXUS_REDIS_HOST` in each service's environment file matches the Valkey container name or IP.
3. Check that the `valkey-search` module is loaded (required for the semantic cache): `docker exec nexus-valkey valkey-cli MODULE LIST | grep -i search`.
4. Check Prometheus: `redis_available` gauge on the compliance proxy and AI Gateway.

**Response cache hit rate dropped to zero. Why?**

Check in order: (1) Valkey connectivity — `redis_available` gauge; (2) the cache TTL setting — if TTL was recently lowered, all entries may have expired; (3) incoming traffic pattern change — fully random prompts produce zero cache hits by design; (4) the AI Gateway log for cache-related errors.

---

## Desktop Agent

**An agent device is showing offline. How do I diagnose?**

1. Check the Hub node registry: `SELECT id, status, last_seen_at FROM thing WHERE type = 'agent' AND id = '<device-id>'`.
2. If `last_seen_at` is recent (within the heartbeat interval, typically 60s), the device is reconnecting — wait one cycle.
3. If `last_seen_at` is stale, the device may be powered off or has lost network access to Hub.
4. On the device itself, check the agent log (`platform.DefaultPaths().LogDir/agent.log`) for connection errors.

**How do I remotely enable verbose logging on an agent for incident triage?**

Use the diagnostics mode API:

```bash
THING_ID=<agent-device-id>
cp_curl -X POST "/api/admin/agents/$THING_ID/diagnostic-mode" \
  -H 'Content-Type: application/json' \
  -d '{"durationMinutes": 60, "reason": "incident investigation"}'
```

Diag mode auto-expires after the configured duration. The agent emits verbose logs that upload to Hub and surface in the Control Plane UI at **Infrastructure → Recent Errors**.

---

## Kill switch and emergency passthrough

**How do I immediately stop all AI Gateway traffic?**

Toggle the kill switch from the Control Plane UI at **Infrastructure → Kill Switch**. The kill switch is a Hub shadow Category A key — it propagates to the AI Gateway within seconds without a restart. Requires the `admin:kill-switch.write` IAM action. See [Kill Switch](Kill-Switch) for the full procedure and IAM requirements.

**What is the difference between the kill switch and emergency passthrough?**

The kill switch stops all traffic through the AI Gateway — requests receive an error response immediately. Emergency passthrough allows traffic to continue to providers while bypassing hooks, compliance checks, and audit — useful when a hook or compliance plugin is causing failures and needs to be bypassed temporarily. See [Emergency Passthrough](Emergency-Passthrough) for the three-tier safety model.

---

## Canonical docs

- [`docs/operators/ops/`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/) — full operators documentation directory
- [`monitoring.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/monitoring.md) — Prometheus metrics catalog and health endpoints
- [`backup-dr.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/backup-dr.md) — backup and disaster recovery procedures

**Adjacent wiki pages**: [Operations Day 2 Cheatsheet](Operations-Day-2-Cheatsheet) · [Operations Runbook Index](Operations-Runbook-Index) · [Operations Logs Metrics Traces](Operations-Logs-Metrics-Traces) · [Operations Credential Rotation](Operations-Credential-Rotation) · [Operations Migrations On Prod](Operations-Migrations-On-Prod)
