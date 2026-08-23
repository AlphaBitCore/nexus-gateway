# Alerts

What fires, what it means, and what to do. Architecture: [alerting-architecture.md](../../../developers/architecture/cross-cutting/observability/alerting-architecture.md).

## How alerting works here, in one paragraph

Alerts are **not** evaluated in Prometheus. The Hub's alert engine reads `traffic_event` and the metric rollups, evaluates 30 built-in rules, writes `Alert` rows (`state = FIRING`), and dispatches to the `AlertChannel` rows you configure — webhook, Slack, email, PagerDuty. Rules live in the `AlertRule` table with a per-tenant `params` blob, a `cooldownSec`, and an optional group filter, so **thresholds are tunable without a deploy**.

## First moves for any alert

```
1. What is the trace_id?      → traffic_event.trace_id is the key to everything
2. Is this new, or chronic?   → GET /api/admin/alerts?ruleId=<id>
3. Did WE change something?   → correlate against the deploy timestamp
```

```bash
# what is firing right now
cp_curl '/api/admin/alerts?state=FIRING'

# the rule's current thresholds
cp_curl '/api/admin/alerts/rules/<ruleId>'
```

(`cp_curl` — see [local-dev-debugging.md](../../../developers/workflow/local-dev-debugging.md); for prod use `Skill('prod-login')`.)

## ⚠️ Two rules are newly alive as of 2026-07-16

**`provider.upstream_error` and `credential.auth_failures_cascade` had never fired in their entire deployed life.** Both selected events on `error_code` being empty — a condition the gateway made unreachable by classifying every upstream failure. Verified against production: **zero 5xx rows carried an empty `error_code`**.

They now select on the canonical error code and work.

**If either fires in the days after that change, it is not a regression.** It is signal that was always warranted and never delivered. **Resist retuning the threshold on day one** — these rules have never been tuned against real traffic because they have never seen any. Find out what the real rate is first.

## The rules you are most likely to be woken by

### `provider.upstream_error` — an upstream is failing

**Means:** ≥`thresholdPct` (default 10%) of requests to one provider came back as an upstream 5xx over `windowSec` (default 300s), min 20 samples. Gateway-side rejects are excluded — this is the provider's fault, not ours.

**Do:**
```promql
sum(rate(nexus_requests_total{status=~"5.."}[5m])) by (provider)
sum(rate(nexus_errors_total[5m])) by (error_type)     # upstream_error vs timeout vs …
```
```sql
SELECT status_code, error_code, count(*), max(created_at)
FROM traffic_event
WHERE routed_provider_name = '<provider>' AND created_at > now() - interval '15 minutes'
GROUP BY 1,2 ORDER BY 3 DESC;
```
Check the provider's own status page. If it is them, the routing rules' fallback chain is the mitigation — confirm traffic is failing over.

### `credential.auth_failures_cascade` — a key is dying

**Means:** ≥`thresholdPct` (default 20%) of one credential's requests came back **401/403 from the upstream** over `windowSec` (default 600s). The caller's virtual key being wrong does **not** count — this is our credential being rejected.

**Do:** it is almost always revoked, expired, or billing-suspended. Test the key against the provider directly, then rotate it. **Replacing the API key auto-clears the circuit** — no separate reset needed.

### `credential.circuit_open` — read the reason before you act

**Means:** a credential's circuit breaker is open. **The reason decides what this costs you:**

| reason | What it means | Impact |
| --- | --- | --- |
| `rate_limit` | one 429 | self-heals after a 60s cooldown; **the breaker does not gate a single-credential provider** |
| `auth_fail` | 3 consecutive 401/403 | **never self-heals** — needs a key replacement or an explicit reset |

**Do:** for `auth_fail`, treat it like the cascade above — test the key, rotate it. For `rate_limit`, wait; if it recurs constantly, the provider's quota is genuinely too small for the traffic.

```bash
# after confirming the key is good
cp_curl -X POST '/api/admin/credentials/<id>/circuit-reset'
```

**Do not clear an `auth_fail` circuit blind.** If the key really is dead, you are re-arming a silent failure instead of surfacing it.

### `thing.offline` — a service stopped reporting

**Do:** `systemctl status nexus-<service>`, then its logs. Check `sum(nexus_scheduler_leader)` — if the Hub is the offline Thing, no scheduled job is running (rollups, circuit flush, expiry all stop).

### `proxy.cost_spike` / `quota.threshold` — spend moved

**Do:**
```sql
SELECT vk_name, sum(estimated_cost_usd), count(*)
FROM traffic_event
WHERE created_at > now() - interval '1 hour'
GROUP BY 1 ORDER BY 2 DESC LIMIT 10;
```
**Careful:** `estimated_cost_usd` on a **cache HIT** is the would-have-paid price, not spend. Real spend is `estimated_cost_usd - gateway_cache_savings_usd`. A naive `SUM(estimated_cost_usd)` **overstates**.

### `vendor.bill_drift` — our estimate disagrees with the provider's bill

Our recorded vendor spend for a provider on a given UTC day drifted from what the provider's own billing API reported for that day, past **both** floors (`thresholdPct` **and** `thresholdUsd` — default 5% and $1). This means our pricing table, tokenizer, or cache accounting may be wrong, or the provider changed prices.

The "ours" figure in the alert message and the `ourVendorSpendUsd` detail is `our_vendor_spend_usd` — all spend we caused the vendor to charge, internal ops included. The payload also carries `ourBilledUsd` (the customer-quota basis, unchanged in meaning); the two differ by the gateway's own overhead, so do not compare the percentage against it.

**Do:** look at the row the `vendor-bill-reconcile` job wrote.
```sql
SELECT provider_id, day, our_vendor_spend_usd, our_internal_ops_usd, our_billed_usd,
       vendor_reported_usd, diff_usd, diff_pct, coverage
FROM vendor_bill_reconciliation
WHERE coverage = 'scoped' AND day > now() - interval '7 days'
ORDER BY abs(diff_pct) DESC;
```

If `diff_usd` is close to `our_internal_ops_usd`, the overhead is being billed but not attributed — check the rollout state of the vendor-spend series before suspecting the pricing table.
Then open **Overview → Bill Reconciliation** in the UI to review/ack the row.

**Note:** `diff_usd` / `diff_pct` are computed from `vendor_reported_usd` and
`our_vendor_spend_usd` rounded to the cent, not their raw values — see "Diff
basis is rounded to the cent" in
[vendor-bill-reconciliation.md](./vendor-bill-reconciliation.md). One
consequence: a day where the vendor amount rounds to $0.00 while
`our_vendor_spend_usd` rounds to a non-zero figure reports `diff_pct = -1.0`
(-100%). That is expected, not a pricing bug — the `thresholdUsd` floor keeps
such a sub-cent day from firing this alert.

**Careful:** the drift alert **only fires on `coverage = 'scoped'` rows.** `org_only` (the provider reported an org-wide total we couldn't narrow to the gateway), `fetch_failed` (the vendor lookup failed), and `no_basis` (no vendor spend recorded for the day) are display-only for *drift* and never raise `vendor.bill_drift` — a difference there is expected, not a defect. A *persistently* failing sync is caught separately by `vendor.bill_sync_failed` below, not here. Only OpenAI and Anthropic are reconciled; a drift on any other provider will never appear here. A late vendor revision self-resolves on the next daily run. A day with no `vendor_spend_usd` rollup rows has no comparable basis, so the job does not compute a difference for it and the drift alert never fires on it — it lands as `coverage = 'no_basis'` and is caught by `vendor.bill_sync_failed` below instead. A day the `rollup-correction` pass has not re-aggregated yet produces no row at all: its rollup rows may still be partial, and a difference computed from a partial basis would fire this alert with a fabricated under-record. Such a day is reconciled by a later run, so neither alert fires on it in the meantime. A row that still carries an old-basis difference beside `our_vendor_spend_usd = 0` (today−6 / today−7, outside the 4-day rewrite window) is likewise treated as non-breaching and **resolved**, so an alert raised on the old basis clears rather than firing forever on "ours $0.00".

### `vendor.bill_sync_failed` — vendor cost-API sync persistently failing

The `vendor-bill-reconcile` job could not produce a comparable figure for a provider, and the gap has persisted past a full run cycle (`staleHours`, default 25). Two coverages count toward it:

- **`fetch_failed`** — the **vendor** side is broken: a revoked or wrong-type admin key, a missing scope, or blocked egress.
- **`no_basis`** — **our** side is broken: the vendor billed the day, but no `vendor_spend_usd` rollup rows exist for it, so nothing could be compared. On a recent day that means the cost-stamping path recorded nothing — 100% of that day's vendor spend went unrecorded.

Unlike a one-off transient — which the next successful run overwrites — a persisting gap means the reconciliation is broken. While it persists, that provider gets **no** drift detection at all, so this alert exists to make the silent gap loud. It fires **per provider** (severity `medium`) and auto-resolves once reconciliation recovers.

A `fetch_failed` placeholder heals on a day the gateway sent **no** traffic too, because the vendor's reported zero for such a day is reconciled into a `scoped` $0 row rather than skipped. Before 2026-08-21 it was skipped, and a placeholder landing on an idle day could therefore never be overwritten by anything: it aged out of the trailing re-reconcile window frozen at `fetch_failed` and kept this alert firing on a day with nothing wrong with it (production OpenAI / 2026-08-16, raised 2026-08-20). An alert that will not clear on an idle day is the symptom of a build predating that fix.

Placeholder rows are inserted `ON CONFLICT DO NOTHING`, so an existing placeholder is never re-stamped and its `updated_at` age keeps accumulating — without that, every daily run would reset the staleness clock and this alert could never fire.

**Do:** find which provider, and how long it has been failing.
```sql
SELECT provider_id, day, coverage, updated_at
FROM vendor_bill_reconciliation
WHERE coverage IN ('fetch_failed', 'no_basis')
ORDER BY updated_at;
```
Then read the underlying reason from the Hub log — `fetch_failed` and `no_basis`
log different lines, and which one you get tells you which side is broken:
```bash
sudo journalctl -u nexus-hub --since '2 days ago' --no-pager \
  | grep -iE 'vendor fetch failed|no recorded vendor spend'
```

**Fix**, by the logged error:
- `403 … Missing scopes: api.usage.read` / `401 invalid x-api-key` — the admin key is the wrong type or lacks the cost scope. Re-issue it (see the [vendor-bill-reconciliation runbook](vendor-bill-reconciliation.md) Step 1) and set `OPENAI_COST_ADMIN_KEY` / `ANTHROPIC_COST_ADMIN_KEY`.
- timeout / connection error — the Hub's egress to `api.openai.com` / `api.anthropic.com` is blocked.

The alert clears on the next daily run once a fetch succeeds. To tolerate a slower vendor before alerting, raise `staleHours` (see *Tuning a threshold*).

## Tuning a threshold

```bash
cp_curl -X PUT '/api/admin/alerts/rules/<ruleId>' \
  -H 'Content-Type: application/json' \
  -d '{"params":{"thresholdPct":15,"windowSec":300,"minSamples":20}}'
```

Validated against the rule's `paramsSchema`; a bad value is rejected rather than silently ignored. Prefer raising `minSamples` over raising `thresholdPct` when a rule is noisy at low volume — it is usually a small-sample artefact, not a real threshold problem.

## An alert that never fires looks exactly like a healthy system

That is the failure mode this runbook exists because of, and it is not hypothetical — two rules lived it for their whole deployment, with green tests around them the entire time.

If a rule has been silent for a suspiciously long time, prove it *can* fire rather than assuming it would have:

```sql
-- does the event shape the rule selects on actually occur?
SELECT status_code, COALESCE(NULLIF(error_code,''),'<empty>') AS error_code, count(*)
FROM traffic_event
WHERE created_at > now() - interval '7 days' AND status_code >= 400
GROUP BY 1,2 ORDER BY 3 DESC;
```

Compare that distribution against the aggregator's predicate (`packages/nexus-hub/internal/alerts/eval/aggregators/`). If no real row can satisfy it, the rule is dead — and no amount of threshold tuning will wake it.

## Full rule list

31 built-in rules in `packages/nexus-hub/internal/alerts/engine/rules/builtin.go`, seeded into `AlertRule`. Grouped by `sourceType`: `quota`, `proxy`, `thing`, `provider`, `audit`, `system`. `cp_curl '/api/admin/alerts/rules'` lists them live with current params.

This runbook covers the ones that page. The rest are documented at their definition site.
