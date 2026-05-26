# Ops Metrics + Diag End-to-End Smoke Test

End-to-end probe of the ops-metrics + diag pipeline introduced by the
2026-04-27 ops-metrics-and-diag rollout. Verifies that every Thing
(Hub, Control Plane, AI Gateway, Compliance Proxy, Agent) produces
samples via `shared/opsmetrics`, pushes them to Hub over the
thingclient WebSocket, and that the resulting rows surface in
PostgreSQL (`metric_ops_raw`, `metric_ops_rollup_*`,
`thing_diag_event`, `thing_diag_mode_window`,
`metric_ops_retention_config`) and in the Control Plane UI.

**When to run:**

- After changes to `packages/shared/runtime/opsmetrics/*`,
  `packages/shared/transport/thingclient/*`, the Hub opsmetrics writers/handlers,
  the Hub ops-rollup-1h / ops-rollup-1d / ops-rollup-1mo /
  ops-retention / diag-mode-expiry jobs, or any of the CP
  `/api/admin/ops-metrics`, `/api/admin/diag-events`,
  `/api/admin/agents/.../diagnostic-mode`, or
  `/api/admin/observability/retention` handlers.
- Before cutting a release build.
- During incident triage when Status / Recent Errors / Crash Reports /
  Agent Diag Mode / Retention pages render empty or stale.

The probe is **not** in CI: it requires a running cluster + an agent
binary connected to the local Hub, and several of the assertions need
~5 minutes of wall-clock for the rollup window to close.

---

## 1. Prerequisites

All of the following must be running locally:

| Component | How to check |
|---|---|
| PostgreSQL (port 55532) | `docker ps \| grep postgres` |
| Redis (port 6437) | `docker ps \| grep redis` |
| NATS + JetStream (port 4222, monitor 8222) | `docker ps \| grep nats` |
| Nexus Hub (port 3060) | `curl -s http://127.0.0.1:3060/healthz` |
| Control Plane (port 3001) | `curl -s http://127.0.0.1:3001/healthz` |
| AI Gateway (port 3050) | `curl -s http://127.0.0.1:3050/healthz` |
| Compliance Proxy (port 3040) | `curl -s http://127.0.0.1:3040/healthz` |
| Control Plane UI (port 3000) | `curl -sI http://127.0.0.1:3000/` |

Bring the stack up via the standard one-command bootstrap:

```bash
./scripts/dev-start.sh
```

Then, in separate shells (or VS Code launch configs), start the four Go
services. Each writes to its own log file (see CLAUDE.md → "Service
log files"):

```bash
cd packages/nexus-hub        && go run ./cmd/nexus-hub        -config nexus-hub.dev.yaml
cd packages/control-plane    && go run ./cmd/control-plane    -config control-plane.dev.yaml
cd packages/ai-gateway       && go run ./cmd/ai-gateway       -config ai-gateway.dev.yaml
cd packages/compliance-proxy && go run ./cmd/compliance-proxy -config compliance-proxy.dev.yaml
```

Capture a `psql` shorthand for the rest of the runbook:

```bash
PG_CID=$(docker ps --filter "name=postgres" -q | head -1)
alias pg="docker exec $PG_CID psql -U postgres -d nexus_gateway -At -c"
```

If `pg` is unavailable in your shell, the literal form is:

```bash
docker exec $PG_CID psql -U postgres -d nexus_gateway -c "<SQL>"
```

Authenticate against the Control Plane admin API. CP uses OAuth + PKCE
bearer tokens (NOT cookie-session); the password-login endpoint is
`POST /authserver/password` (`packages/control-plane/internal/identity/authserver/mount.go:248`).
For runbook execution, use the `cp_login` / `cp_curl` helpers wired in
`tests/lib/auth.sh`:

```bash
source tests/lib/loadenv.sh local      # loads tests/.env.local + .example defaults
source tests/lib/auth.sh
cp_login                                # idempotent; caches token at /tmp/nexus_test_token_local

cp_curl /api/admin/me | jq -r '.email'
# → admin@nexus.ai
```

If `cp_login` returns 401, the Control Plane is not running or the seed
admin password is different — re-run the seed
(`cd tools/db-migrate && npx prisma db seed`).

---

## 2. Bring Up an Agent (Optional but Recommended)

Steps 6, 7 and most UI checks need at least one Agent Thing connected
to the Hub. From a fresh checkout:

```bash
cd packages/agent
go run ./cmd/agent run -config agent.dev.yaml
```

Confirm the agent registered as a Thing:

```bash
pg "SELECT id, type, status FROM thing WHERE type = 'agent' ORDER BY enrolled_at DESC LIMIT 3"
```

Expect at least one row with `status = 'online'`.

If you cannot run the agent locally (no host TUN, no admin keychain,
etc.), skip steps 6 and 7 and rely on the cluster-service samples for
the rest of the runbook — note this in the report.

---

## 3. Verify Sample Flow (Per-Service Rows in `metric_ops_raw`)

Wait ~30 seconds after the four services boot, then:

```bash
pg "
  SELECT thing_type, COUNT(*) AS samples, COUNT(DISTINCT thing_id) AS things
    FROM metric_ops_raw
   WHERE sampled_at > now() - interval '5 min'
   GROUP BY thing_type
   ORDER BY thing_type
"
```

Expected (counts vary; the shape is what matters):

```
thing_type        | samples | things
------------------+---------+--------
agent             |     ... |      1
ai_gateway        |     ... |      1
compliance_proxy  |     ... |      1
control_plane     |     ... |      1
hub               |     ... |      1
```

If a `thing_type` is missing:

- Tail the corresponding service log
  (`packages/<service>/logs/<service>.log`) — look for
  `opsmetrics push`, `thingclient connected`, or
  `metrics_sample dropped` lines.
- Confirm the service is registered as a Thing:
  `pg "SELECT id, type, status FROM thing WHERE type = '<thing_type>'"`.
- If `type` is correct but `status = 'offline'`, the thingclient
  WebSocket is not establishing — verify Hub `:3060` is reachable from
  the service's runtime config.

---

## 4. Verify `staticInfo` Identity Payload

Each Thing publishes a one-shot `static_info` snapshot at startup
(version, build SHA, OS info, sampler interval). It lives under
`thing.metadata.staticInfo`:

```bash
pg "
  SELECT id, type, metadata->'staticInfo' AS static_info
    FROM thing
   WHERE metadata ? 'staticInfo'
   ORDER BY type
"
```

Expected: every running Thing has a non-null JSON object with at least
`version`, `goVersion`, and a `samplerInterval` field. If a Thing is
missing the key, its constructor never called the static-info publish
helper — review `cmd/<service>/main.go` for the
`thingclient.PublishStaticInfo` call.

---

## 5. Verify 1-Hour Rollup

The `ops-rollup-1h` Hub job runs every 5 minutes and closes any window
whose hour boundary has passed. Wait until the **next full hour
elapses** plus 5–10 minutes after first booting the stack, then:

```bash
pg "
  SELECT bucket_start, thing_type, COUNT(*) AS rows
    FROM metric_ops_rollup_1h
   WHERE bucket_start > now() - interval '2 hours'
   GROUP BY bucket_start, thing_type
   ORDER BY bucket_start DESC, thing_type
"
```

Expected: at least one closed hour with rows for every `thing_type`
seen in step 3. If `metric_ops_rollup_1h` is empty after the second
hour boundary:

```bash
curl -sb /tmp/nexus_cookie -X POST \
  http://127.0.0.1:3001/api/admin/jobs/ops-rollup-1h/trigger | jq .
```

The trigger is admin-audited. Then re-run the SELECT.

If still empty, the `ops_metrics_watermark` table is likely stuck —
check `pg "SELECT * FROM ops_metrics_watermark"` and the Hub log for
`ops-rollup-1h` errors.

---

## 6. Verify Agent Diag Pipeline (Synthetic ERROR)

> Skip this step if no agent is running — note the skip in the report.

The agent's slog ERROR sink converts every `slog.Error` into a
DiagEvent that buffers locally (encrypted SQLCipher) and drains to Hub
via the `/api/hub/things/<id>/diag-events:batch` endpoint. The agent
binary exposes only the `run`, `enroll`, and `unenroll` subcommands —
**there is no `agent diag-emit` debug helper**. To exercise the
read-side pipeline (Hub → DB → CP → UI) without needing a real agent
ERROR, insert a synthetic row directly:

```bash
THING_ID=$(pg "SELECT id FROM thing WHERE type = 'agent' AND status = 'online' ORDER BY last_seen_at DESC NULLS LAST LIMIT 1")

pg "
  INSERT INTO thing_diag_event
    (id, thing_id, thing_type, occurred_at, level, event_type, source,
     message, message_hash, repeat_count)
  VALUES (
    gen_random_uuid(),
    '$THING_ID', 'agent', NOW(),
    'error', 'error', 'smoke-test',
    'ops-metrics smoke synthetic error',
    md5('error|smoke-test|ops-metrics smoke synthetic error'),
    1
  )
"
```

> **Note:** This SQL bypasses the agent → Hub WS path; production
> ERROR/FATAL events are emitted by the agent's slog sink and require
> either a real ERROR-level log line in the agent process or the
> agent's debug RPC (not exposed today). The direct insert is for
> runbook verification of the read-side surfaces only.

Verify:

```bash
pg "
  SELECT level, source, message, occurred_at
    FROM thing_diag_event
   WHERE source = 'smoke-test'
     AND occurred_at > now() - interval '5 min'
   ORDER BY occurred_at DESC
   LIMIT 5
"
```

Expected: a row with `level = 'error'` and the synthetic message.

For the **crash** (FATAL) variant, repeat the insert with
`level = 'fatal'`, `event_type = 'crash'`, and a different message
(so `message_hash` does not collide):

```bash
pg "
  INSERT INTO thing_diag_event
    (id, thing_id, thing_type, occurred_at, level, event_type, source,
     message, message_hash, repeat_count, agent_version, os_info)
  VALUES (
    gen_random_uuid(),
    '$THING_ID', 'agent', NOW(),
    'fatal', 'crash', 'smoke-test',
    'smoke crash test',
    md5('fatal|smoke-test|smoke crash test'),
    1,
    '0.0.0-smoke',
    '{\"os\":\"darwin\",\"arch\":\"arm64\"}'::jsonb
  )
"
```

Confirm:

```bash
pg "SELECT id FROM thing_diag_event WHERE level = 'fatal' AND source = 'smoke-test' AND occurred_at > now() - interval '5 min'"
```

Failure modes (real-traffic path, when an actual agent error is the
trigger rather than this insert):

- **No row, no agent log error** — drainer is queued but not flushing;
  check `packages/agent/logs/agent.log` for `diag-events drain` lines
  and the Hub log for `/diag-events:batch` 4xx / 5xx responses.
- **No row, `messageHash` collision** — the dedup writer skipped this
  event because an identical hash arrived in the same minute. Re-run
  with a unique `message` suffix.

---

## 7. Verify Diag-Mode Window Flow

> Skip this step if no agent is running — note the skip in the report.

Pick the agent Thing ID from step 2:

```bash
THING_ID=$(pg "SELECT id FROM thing WHERE type = 'agent' AND status = 'online' ORDER BY last_seen_at DESC NULLS LAST LIMIT 1")
echo "$THING_ID"
```

Open a 2-minute diag window:

```bash
curl -sb /tmp/nexus_cookie -X POST \
  "http://127.0.0.1:3001/api/admin/agents/$THING_ID/diagnostic-mode" \
  -H 'Content-Type: application/json' \
  -d '{"durationMinutes":2,"reason":"ops-metrics smoke test"}' | jq .
```

Expected: `200` with `diagModeUntil` ~2 minutes in the future.

Confirm the metadata flag and the window row:

```bash
pg "SELECT metadata->'diagModeUntil' FROM thing WHERE id = '$THING_ID'"
pg "
  SELECT started_at, ended_at, reason
    FROM thing_diag_mode_window
   WHERE thing_id = '$THING_ID'
   ORDER BY started_at DESC
   LIMIT 1
"
```

Expected: `metadata->'diagModeUntil'` is a non-null timestamp and the
window row matches.

Wait ~3 minutes (window duration + scheduler tick). Then trigger the
expiry job manually instead of waiting for the 5-minute schedule:

```bash
curl -sb /tmp/nexus_cookie -X POST \
  http://127.0.0.1:3001/api/admin/jobs/diag-mode-expiry/trigger | jq .
```

Re-check:

```bash
pg "SELECT metadata ? 'diagModeUntil' AS still_set FROM thing WHERE id = '$THING_ID'"
```

Expected: `f` (false) — the expiry job cleared the metadata flag.

---

## 8. Verify Retention Flow

Set `runtime_raw` to its minimum (1 day) so we can age out a row in a
single command:

```bash
curl -sb /tmp/nexus_cookie -X PUT \
  http://127.0.0.1:3001/api/admin/observability/retention \
  -H 'Content-Type: application/json' \
  -d '{"runtime_raw":1}' | jq '.runtime_raw'
# → {"value": 1, "min": 1, "max": 30, ...}
```

Insert a synthetic too-old sample (the unique constraint requires a
matching `(sampled_at, thing_id, metric_name, dimension_key)` tuple
that does not yet exist — a 2-day-old timestamp guarantees that):

```bash
THING_ID=$(pg "SELECT id FROM thing WHERE type = 'hub' LIMIT 1")
pg "
  INSERT INTO metric_ops_raw
    (sampled_at, thing_id, thing_type, metric_name, metric_kind, dimension_key, value)
  VALUES
    (now() - interval '2 days', '$THING_ID', 'hub', 'smoke_synthetic_total', 'counter', '', 1)
"
pg "SELECT count(*) FROM metric_ops_raw WHERE metric_name = 'smoke_synthetic_total'"
# → 1
```

Trigger the retention job:

```bash
curl -sb /tmp/nexus_cookie -X POST \
  http://127.0.0.1:3001/api/admin/jobs/ops-retention/trigger | jq .
```

Wait ~5 seconds for the run to complete, then:

```bash
pg "SELECT count(*) FROM metric_ops_raw WHERE metric_name = 'smoke_synthetic_total'"
# → 0
```

Expected: the synthetic row was purged. Reset retention to a sane
default for the next runbook iteration:

```bash
curl -sb /tmp/nexus_cookie -X PUT \
  http://127.0.0.1:3001/api/admin/observability/retention \
  -H 'Content-Type: application/json' \
  -d '{"runtime_raw":7}' | jq '.runtime_raw.value'
# → 7
```

Failure modes:

- **`PUT` returns `400`** — the value is outside the allowed
  `[Min, Max]` for the layer (see
  `packages/control-plane/internal/handler/observability_retention.go`).
- **Row not purged** — check Hub log for `ops-retention` job errors;
  confirm `metric_ops_retention_config` actually persisted the
  1-day setting (`pg "SELECT * FROM metric_ops_retention_config WHERE layer='runtime_raw'"`).

---

## 9. Verify Control Plane UI Surfaces

Open `http://127.0.0.1:3000` in a browser, log in as `admin@nexus.ai`,
and walk through:

| Page | What to check |
|---|---|
| **Status** (`/status`) | Service Metrics cards for AI Gateway / Compliance Proxy / Control Plane / Hub render with non-zero `requests`, `latency`, and `errors` counters sourced from `/api/admin/ops-metrics/current`. The Recent Errors widget at the bottom lists the synthetic event from step 6 (or is empty if step 6 was skipped). |
| **Observability → Recent Errors** (`/observability/recent-errors`) | Filtering by `level = error` and `source = smoke-test` returns the row from step 6. Clicking a row opens the detail drawer with `messageHash`, `repeatCount`, and `attrs`. |
| **Observability → Crash Reports** (`/observability/crash-reports`) | The FATAL row from step 6 appears. The crash cohort grouping shows the count by `messageHash`. |
| **Observability → Agent Diag Mode** (`/observability/agent-diag-mode`) | Within the 2-minute window from step 7, the agent Thing is listed as "Active". After the expiry trigger, the row disappears (or moves to the historical-windows view if implemented). |
| **Settings → Observability → Retention** (`/settings/observability`) | The `runtime_raw` row reflects the value last set in step 8. Editing through the UI submits a `PUT` and returns the canonical layer object. |
| **Nodes detail** (`/infrastructure/nodes/<thingId>`) | The Metrics tab renders the per-Thing time series from `/api/admin/ops-metrics/timeseries`. The Logs tab renders that Thing's diag events. Selecting a brush on either tab updates the other tab's time axis (cross-tab sync). |

If a card or page renders empty:

- DevTools → Network tab — confirm the underlying API call returned
  `200` with a non-empty body. A `401` means the cookie expired; log
  in again. A `200` with `[]` means the underlying SQL is empty —
  re-run the relevant data step above.
- Console — TypeError on `groupOpsSamples` usually means the
  `/ops-metrics/current` shape changed; confirm the CP build is at
  the same commit as the UI bundle.

---

## 10. Cleanup

After a successful run:

```bash
# Drop synthetic ops rows
pg "DELETE FROM metric_ops_raw WHERE metric_name = 'smoke_synthetic_total'"

# Drop synthetic diag events
pg "DELETE FROM thing_diag_event WHERE source = 'smoke-test'"

# Restore retention defaults — the seed defaults are described in
# tools/db-migrate/seed.ts under the metric_ops_retention_config block.
curl -sb /tmp/nexus_cookie -X PUT \
  http://127.0.0.1:3001/api/admin/observability/retention \
  -H 'Content-Type: application/json' \
  -d '{"runtime_raw":7,"runtime_1h":90,"runtime_1d":365,"runtime_1mo":1095,"diag_warn":30,"diag_error":180,"diag_fatal":365}' | jq '.runtime_raw.value'

# Close the diag-mode window if it is still open from step 7
THING_ID=$(pg "SELECT id FROM thing WHERE type = 'agent' ORDER BY enrolled_at DESC LIMIT 1")
curl -sb /tmp/nexus_cookie -X DELETE \
  "http://127.0.0.1:3001/api/admin/agents/$THING_ID/diagnostic-mode" | jq .

# Remove the cookie file
rm -f /tmp/nexus_cookie
```

Stop the four Go services with `Ctrl-C` (or kill them per the CLAUDE.md
"Service lifecycle" rules — graceful `SIGTERM` first, escalate only on
hang).

The Docker stack (Postgres / Redis / NATS) is left running — the
runbook does **not** destroy local state.
