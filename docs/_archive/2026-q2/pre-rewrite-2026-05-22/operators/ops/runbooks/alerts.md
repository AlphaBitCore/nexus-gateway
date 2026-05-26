# Alerts Runbook

Operator guide for the unified alerting pipeline introduced in Epic 21.
All alert instances, rules, and channels are owned by Nexus Hub and surfaced
through the Control Plane UI at `/alerts`.

---

## 1. Common Actions

### State model

```
FIRING → ACKNOWLEDGED → RESOLVED
FIRING ──────────────→ RESOLVED (auto or manual)
```

| State | Meaning | Who sets it |
|---|---|---|
| `firing` | Condition is active; no operator has acknowledged | Raiser (Hub or alertclient) |
| `acknowledged` | Operator has seen the alert; condition may still be active | Operator via UI or API |
| `resolved` | Condition cleared or manually dismissed | Auto (CheckFunc flips) or operator |

Rules with `requiresAck: true` (e.g. `quota.threshold`) require an explicit ack
before resolution matters. After ack, a subsequent re-fire creates a **new**
alert row — the operator sees a fresh incident, not a duplicate counter update.

Rules with `requiresAck: false` (most proxy rules) auto-resolve when the
CheckFunc returns healthy. No ack is required.

### Acknowledge via UI

1. Open `/alerts` inbox.
2. Find the alert (use State, Severity, or SourceType filters).
3. Click the row to open the detail drawer.
4. Click **Acknowledge** (visible when state = `firing`).
5. Optionally enter a reason. Click Confirm.

### Resolve via UI

1. Open `/alerts` inbox.
2. Click the row → detail drawer.
3. Click **Resolve** (visible when state != `resolved`).
4. Optionally enter a reason. Click Confirm.

### Acknowledge or resolve via API (scripts and automation)

```bash
# Use cp_curl from the prod-login / local cp_login helpers (OAuth + PKCE bearer-token flow);
# see docs/developers/workflow/local-dev-debugging.md for the helper contract.

# Acknowledge
cp_curl -X POST "/api/admin/alerts/<ALERT_ID>/ack" \
  -H 'Content-Type: application/json' \
  -d '{"reason": "investigating"}'

# Resolve
cp_curl -X POST "/api/admin/alerts/<ALERT_ID>/resolve" \
  -H 'Content-Type: application/json' \
  -d '{"reason": "manually resolved after quota reset"}'

# Or, if invoking curl directly, use a Bearer header:
# curl -s -H "Authorization: Bearer $TOKEN" -X POST "http://<CP_HOST>/api/admin/alerts/<ALERT_ID>/ack" ...
```

Both return HTTP 204 on success. HTTP 404 means the alert does not exist or is
already in the terminal state for that operation.

---

## 2. How to Add a Notification Channel

1. Open `/alerts/channels` in the UI.
2. Click **Create channel**.
3. Fill in **Name** and **Type**. Required config fields by type:

| Type | Required config fields | Optional |
|---|---|---|
| `webhook` | `url` | `headers` (map of extra headers) |
| `slack` | `url` (Incoming Webhook URL) | — |
| `email` | `host`, `port`, `from`, `to` (array of addresses) | `username`, `smtpPassword`, `subject`, `useTLS` |
| `pagerduty` | `routingKey` (PagerDuty Events API v2 routing key) | `dedupKey` (defaults to alert ID) |

4. Set **Severities** — leave empty to receive all severities; pick specific
   values (critical / high / medium / low / info) to narrow.
5. Set **Source Types** — leave empty to receive alerts from all producers
   (quota, proxy, thing, provider, system, audit); select specific types to narrow.
6. Click **Test** before saving. A synthetic `system.channel_test` alert is
   dispatched through the channel. The result toast shows success or the error
   returned by the sink. The synthetic alert is automatically resolved and does
   not appear in the inbox.
7. Click **Save**.

Secrets (`botToken`, `smtpPassword`, `routingKey`, auth headers) are masked in
all GET responses (`xxxx-••••-` + last 4 chars). A PUT round-trip that sends
back a masked value preserves the original secret — no need to re-enter.

---

## 3. Triage: "Why Didn't I Get Paged?"

Work through this decision tree in order.

### Step 1: Is the rule enabled?

```sql
SELECT id, display_name, enabled, cooldown_sec
FROM alert_rule
WHERE id = '<ruleId>';
```

If `enabled = false`, the producer is not evaluating the condition. Enable it
via `/alerts/rules` → toggle or `PUT /api/admin/alerts/rules/<ruleId>` with
`{"enabled": true}`.

### Step 2: Is there a channel enabled with matching severity and source type?

```sql
SELECT id, name, type, enabled, severities, source_types
FROM alert_channel
WHERE enabled = true;
```

Check that at least one channel's `severities` array either is empty (= all) or
contains the alert's severity, **and** `source_types` is either empty or
contains the alert's `source_type`.

The dispatcher skips a channel when its filter list is non-empty and the alert
does not match. An empty array means "match all".

### Step 3: Did the channel's last test succeed?

Run a test send from the UI (`/alerts/channels` → row menu → Test) or via API:

```bash
cp_curl -X POST "/api/admin/alerts/channels/<CHANNEL_ID>/test"
# Or: curl -s -H "Authorization: Bearer $TOKEN" -X POST \
#       "http://<CP_HOST>/api/admin/alerts/channels/<CHANNEL_ID>/test"
```

The response includes `{"success": true/false, "error": "...", "dispatchId": "..."}`.
A failing test identifies the sink problem immediately (wrong URL, expired token,
firewall rule, etc.) without waiting for a real alert.

### Step 4: Is the alert still in cooldown?

`AlertRule.cooldownSec` is a **producer-side** throttle — it controls how often
a periodic evaluator re-fires `Raise` for the same `(rule, target)` while the
condition persists. If the condition just crossed the threshold, the producer may
not have run again yet.

```sql
SELECT id, cooldown_sec
FROM alert_rule
WHERE id = '<ruleId>';

-- Check when the current FIRING row was last seen
SELECT id, rule_id, target_key, state, fired_at, last_seen_at, duplicate_count
FROM alert
WHERE rule_id = '<ruleId>'
  AND target_key = '<targetKey>'
  AND state IN ('firing', 'acknowledged')
ORDER BY fired_at DESC
LIMIT 1;
```

If `now() - last_seen_at < cooldown_sec`, the producer has not re-evaluated
since the last fire — dispatch already happened on the initial `fired_at`.

### Step 5: Did the dispatcher write an alert_dispatch row?

This is the smoking gun. A row with `success = false` and a non-null `error_msg`
tells you exactly what went wrong at send time.

```sql
-- Find dispatches for a specific alert
SELECT d.id, d.channel_id, d.channel_name, d.success, d.status_code, d.error_msg, d.attempted_at
FROM alert_dispatch d
WHERE d.alert_id = '<alertId>'
ORDER BY d.attempted_at DESC;

-- Find recent failed dispatches across all alerts (last 24h)
SELECT d.alert_id, d.channel_name, d.success, d.status_code, d.error_msg, d.attempted_at
FROM alert_dispatch d
WHERE d.success = false
  AND d.attempted_at > now() - interval '24 hours'
ORDER BY d.attempted_at DESC
LIMIT 50;

-- Find alerts with no dispatch rows (dispatcher never ran or rule had no channels)
SELECT a.id, a.rule_id, a.target_key, a.severity, a.fired_at
FROM alert a
LEFT JOIN alert_dispatch d ON d.alert_id = a.id
WHERE d.id IS NULL
  AND a.fired_at > now() - interval '24 hours';
```

**No dispatch row at all** on a FIRING alert means either:
- All channels were filtered out (Step 2 above), or
- The alert was a duplicate Raise on an existing FIRING row (duplicate Raise
  updates `last_seen_at` only; it does not re-dispatch).

**Dispatch row with `success = false`** and `error_msg` — fix the channel config
(wrong URL, expired token, SMTP auth failure) and use the Test endpoint to verify
before the next real alert fires.

Use `POST /api/admin/alerts/channels/<id>/test` as the fastest feedback loop —
it exercises the full sender code path with a real HTTP call to the sink and
writes a `alert_dispatch` row you can query immediately.
