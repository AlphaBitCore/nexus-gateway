# Vendor bill reconciliation — enabling and troubleshooting

Turns on the daily job that compares the gateway's **estimated** spend against
each provider's **authoritative** billing API, and explains every failure mode
observed while first enabling it.

Report page: **Overview → Bill Reconciliation**. Alert: `vendor.bill_drift`
(see [alerts.md](./alerts.md)).

## What gets reconciled

| Provider | Reconciled | Finest attribution the vendor exposes |
| --- | --- | --- |
| OpenAI | yes | **API key** (`api_key_ids` filter) |
| Anthropic | yes | **Workspace** (no per-key cost, no filter params) |
| Gemini (direct), DeepSeek, Moonshot | no | no dollar-denominated cost API at day granularity |

The job (`vendor-bill-reconcile`) runs every 24 h and **also once at Hub
startup**, so restarting the Hub is a valid way to trigger a pass. It
reconciles the closed window `[today-5, today-2]` — today and yesterday never
appear, because vendor billing is not final that soon. On a **successful** fetch
each run re-upserts the whole window (refreshing the vendor numbers as the vendor
finalizes late-arriving days), preserving any review state you have set on a row.
A **failed** fetch is non-destructive: it only seeds a `fetch_failed` placeholder
for days that have no row yet, and never overwrites a vendor number already
recorded — a transient vendor 500 / timeout cannot erase a good day.

## Step 1 — Create the right kind of key

Both endpoints are **organization-management** APIs. An ordinary inference key
is rejected, and no amount of permission-granting on the wrong key type fixes
it.

**OpenAI** — create an **Organization Admin key** at
`platform.openai.com` → Settings → Organization → **Admin keys**. Requires the
**Owner** role; if you cannot see the page, you are not an Owner.

> A project key or a service-account key (`sk-svcacct-…`) fails with
> `403 … Missing scopes: api.usage.read` even when its permissions look
> correct — the scope is organization-level and cannot be granted to a
> project-scoped credential.

**Anthropic** — create an **Admin key** (`sk-ant-admin01-…`) in the Console
under Admin keys. A normal inference key (`sk-ant-api03-…`) fails with
`401 invalid x-api-key`.

These keys are read-only billing credentials and are **separate** from the
provider credentials the gateway uses for inference (those live encrypted in
the database). Adding them does not affect traffic.

## Step 2 — Decide the scope, then pin it

This is the step that decides whether the report is useful or decorative.

An admin key reports the **entire organization**. If the gateway is one
consumer among several — other API keys, other products, other teams, or
Claude/ChatGPT subscription seats — then the vendor total and the gateway's
spend are measuring different things. The job detects this and marks the row
`coverage = org_only`: displayed for reference, showing ~100% difference, and
**never alerted**. That is correct behaviour, not a bug.

To get comparable numbers, pin the scope:

**OpenAI** — set `OPENAI_COST_API_KEY_ID` to the **id** (`key_…`, not the
secret) of the key the gateway authenticates with. The job passes it as the
endpoint's `api_key_ids` filter, so the vendor itself narrows the bill. This is
exact per-key attribution and needs no account restructuring.

To find the id when the console does not show it plainly, group costs by key and
match the daily curve against the gateway's own estimate:

```bash
curl -s -G https://api.openai.com/v1/organization/costs \
  -H "Authorization: Bearer $OPENAI_COST_ADMIN_KEY" \
  --data-urlencode "start_time=$(date -u -d '7 days ago' +%s)" \
  --data-urlencode "bucket_width=1d" \
  --data-urlencode "group_by=api_key_id" \
  --data-urlencode "limit=31"
```

Compare against the gateway's estimate for the same days:

```sql
SELECT date_trunc('day', "bucketStart")::date AS day, round(sum(value)::numeric, 4)
FROM metric_rollup_1d
WHERE "metricName" = 'billed_cost_usd'
  AND "dimensionKey" = 'routed_provider=<Provider.id>'
GROUP BY 1 ORDER BY 1;
```

**Anthropic** — set `ANTHROPIC_COST_WORKSPACE_ID` to a **named** workspace
containing the gateway's key. Two constraints, both from the vendor:

- `cost_report` exposes **no per-key cost** — workspace is the finest unit.
- It accepts **no filter parameters**, so the job filters client-side.
- The **default workspace is reported with a null `workspace_id`**, so an
  account that never created a named workspace can never resolve to `scoped`.
  Create a workspace and move the gateway's key into it.

Set the variables in the Hub's environment (`/etc/nexus-gateway/env` on a
single-box install), then restart the Hub — `EnvironmentFile` is read only at
process start.

## Step 3 — Verify

```bash
sudo systemctl restart nexus-hub   # RunOnStart fires one pass
sleep 45
sudo journalctl -u nexus-hub --since '2 min ago' --no-pager | grep -i 'vendor fetch failed'
```

```sql
SELECT p.name, v.day, v.our_billed_usd, v.vendor_reported_usd,
       round(v.diff_pct * 100, 1) AS diff_pct, v.scope_kind, v.coverage
FROM vendor_bill_reconciliation v JOIN "Provider" p ON p.id = v.provider_id
ORDER BY p.name, v.day DESC;
```

Read `coverage` first — it tells you whether the numbers mean anything:

| `coverage` | Meaning | Alerts? |
| --- | --- | --- |
| `scoped` | Narrowed to the gateway. Comparable. | yes |
| `org_only` | Vendor reported an org-wide total. Reference only. | no |
| `fetch_failed` | The vendor call failed for a day that had no number yet; a placeholder is written. A transient failure never overwrites a day already reconciled. | not for drift; a failure that persists past ~25h raises `vendor.bill_sync_failed` (see alerts runbook) |

**No rows at all** means the provider was skipped entirely: its admin key is
unset, or no enabled `Provider` row has `adapter_type` exactly `openai` /
`anthropic`. A skipped provider is silent by design — check the env var first.

## Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| No rows for a provider | Admin key unset, or no enabled provider with that exact `adapter_type` | Set the key; check `SELECT name, adapter_type, enabled FROM "Provider"` |
| All rows `fetch_failed`, log shows `403 … Missing scopes: api.usage.read` | OpenAI key is a project / service-account key | Create an **Organization** Admin key (Step 1) |
| All rows `fetch_failed`, log shows `401 invalid x-api-key` | Anthropic key is an inference key | Create an Admin key (`sk-ant-admin01-…`) |
| All rows `coverage = org_only`, ~100% difference | No scope pin; the key reports the whole org | Pin the scope (Step 2) |
| Anthropic stuck `org_only` even with a workspace | Key sits in the **default** workspace (null `workspace_id`) | Create a named workspace, move the key, set `ANTHROPIC_COST_WORKSPACE_ID` |
| Vendor total far exceeds any plausible bill | Comparing against an invoice that includes **subscription seats** (Claude Max / ChatGPT), which the cost APIs do not report | Compare against the API-usage portion of the invoice only |
| Rows exist but `our_billed_usd` is 0 for a day with traffic | No `metric_rollup_1d` `billed_cost_usd` row for that provider/day | Check the rollup jobs; the reconcile job reads the rollup, not `traffic_event` |
| Sporadic `fetch_failed` with a timeout in the log, no vendor-side error | The vendor cost API is slow or the Hub's egress to it is blocked. Vendor-bill calls run on the shared outbound HTTP client with a 60s per-request budget; a page that does not answer inside it fails that day rather than stalling the job | Re-run the day once the vendor API recovers (a later successful run re-upserts the day, so the `fetch_failed` placeholder self-heals; a day that already had a good number keeps it through the outage); check egress to `api.openai.com` / `api.anthropic.com` |

### Upgrading from a build before the amount-scale / percentage fixes

Rows written by an earlier build carry Anthropic amounts **100× too high** and a
`diff_pct` computed against `max(vendor, our)` instead of the vendor value. Each
daily run re-upserts only the rolling `[today-5, today-2]` window, so rows
inside it self-correct on the next pass — **rows older than the window never
do.** They are pure derived data, so the safe cleanup is to drop them and let
the job rebuild what it can:

```sql
DELETE FROM vendor_bill_reconciliation WHERE day < current_date - 5;
```

If any row has been reviewed (`status`/`reviewed_by`/`note` set), export those
notes first — the delete discards them, and the rebuild cannot recover a review
made against a wrong number anyway.

### Amount scale

Anthropic's `cost_report` returns `amount` in **cents** as a decimal string;
the adapter divides by 100 (`anthropicAmountIsCents`). OpenAI returns
`amount.value` as a decimal **string** too, despite its docs showing a bare
number — the decoder accepts either. Both are pinned by contract tests, because
a wrong scale is a silent 100× money error in the report.

## Related

- [alerts.md](./alerts.md) — the `vendor.bill_drift` rule, thresholds, and why
  only `scoped` rows fire.
- [Bill Reconciliation report page](../../../users/features/cp-ui/overview.md)
