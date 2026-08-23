# Vendor bill reconciliation — enabling and troubleshooting

Turns on the daily job that compares the gateway's **recorded vendor spend**
against each provider's **authoritative** billing API, and explains every failure
mode observed while first enabling it.

The comparison basis is `metric_rollup_1d` `vendor_spend_usd` — every dollar the
gateway caused that vendor to charge, including internal-ops calls (smart-router
decisions, the ai-guard classifier, L2 embedding lookups) attributed to the
provider actually charged. The older customer-quota figure (`billed_cost_usd`) is
still recorded on each row as `our_billed_usd`, but it omits internal ops, so it
is **not** what `diff_usd` / `diff_pct` are computed from.

Report page: **Overview → Bill Reconciliation**. Alert: `vendor.bill_drift`
(see [alerts.md](./alerts.md)).

## What gets reconciled

| Provider | Reconciled | Finest attribution the vendor exposes |
| --- | --- | --- |
| OpenAI | yes | **API key** (`api_key_ids` filter) |
| Anthropic | yes | **Workspace** (no per-key cost, no filter params) |
| Gemini (direct), DeepSeek, Moonshot | no | no dollar-denominated cost API at day granularity |

A provider is reconciled only when **both** its `adapter_type` has a bill source
**and** its `baseUrl` names that vendor's own API host. `adapter_type` is a wire
format, not a vendor: a self-hosted model, an OpenAI-compatible appliance, or a
local inference box is configured as `adapter_type = 'openai'` while costing the
OpenAI organization nothing. Such a provider is skipped, with the Hub logging
`provider shares an adapter type with a covered vendor but is served from a
different host`. Without that check it would be handed the real vendor's daily
total as its own `vendor_reported_usd` — the same vendor dollars counted twice,
under a provider that spent none of them, and a drift alert on a bill that was
never issued. A row with an empty `baseUrl` uses the adapter's default endpoint,
which *is* the vendor's host, so it is reconciled.

The job (`vendor-bill-reconcile`) runs every 24 h and **also once at Hub
startup**, so restarting the Hub is a valid way to trigger a pass. It
reconciles the closed window `[today-9, today-2]` — today and yesterday never
appear, because vendor billing is not final that soon. The window is 8 days
wide so that a day cannot age out of it while `rollup-correction` is stalled:
a day may only be reconciled once the correction pass has rebuilt it, so every
day that pass fails to advance is a day this window must still be covering when
it recovers. It also means **one pass after a deploy backfills the preceding
week**, rather than only the two days either side of the restart.

The two jobs share a scheduler tick, and the reconcile pass is ~50× faster than
the correction pass, so before deciding which days are comparable it waits up to
10 minutes for the correction cursor to reach the end of its window. If the
cursor has not even reached the *start* of the window, the run **fails** instead
of quietly writing nothing — see the troubleshooting table.

On a **successful** fetch
each run re-upserts the whole window (refreshing the vendor numbers as the vendor
finalizes late-arriving days), preserving any review state you have set on a row.
A **failed** fetch is non-destructive: it only seeds a `fetch_failed` placeholder
for days that have no row yet, and never overwrites a vendor number already
recorded — a transient vendor 500 / timeout cannot erase a good day.

A day the gateway sent **no traffic** on still gets a row: `coverage = scoped`
with every money column at `$0.00` and a `0.0%` difference. Both cost APIs
answer such a day with a bucket that is present but carries no in-scope result
— OpenAI returns `"results": []`, Anthropic returns the other workspaces' rows
and none of yours — and that is the vendor **stating it charged nothing**, not
an absence of data. Writing it matters twice over:

- **The report stays readable.** An absent row and a zero row are the same
  blank, so without it you cannot tell "no traffic that day" from "the job never
  reconciled that day" — which is the question `coverage` exists to answer.
- **A `fetch_failed` placeholder on such a day can heal.** The vendor has no
  non-zero figure to report for an idle day, ever, so unless the zero itself is
  reconciled the placeholder is never overwritten: it ages out of the trailing
  window frozen at `fetch_failed`, still raising `vendor.bill_sync_failed`.
  Production sat exactly there on OpenAI / 2026-08-16.

Only a day the vendor leaves **out of its response entirely** is still treated
as "not finalized" and left for a later run.

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
SELECT p.name, v.day,
       v.our_vendor_spend_usd,   -- comparison basis: all spend we caused
       v.our_internal_ops_usd,   -- the router / classifier / embedding subset
       v.our_billed_usd,         -- customer-quota basis, for continuity only
       v.vendor_reported_usd,
       round(v.diff_pct * 100, 1) AS diff_pct, v.scope_kind, v.coverage
FROM vendor_bill_reconciliation v JOIN "Provider" p ON p.id = v.provider_id
ORDER BY p.name, v.day DESC;
```

`our_internal_ops_usd` is the part of `our_vendor_spend_usd` that is gateway
overhead rather than customer traffic. It is what tells a residual difference
apart from routing overhead: a difference roughly equal to it usually means the
overhead is real and expected, while a difference unrelated to it points at the
pricing tables.

Read `coverage` first — it tells you whether the numbers mean anything:

| `coverage` | Meaning | Alerts? |
| --- | --- | --- |
| `scoped` | Narrowed to the gateway. Comparable. | yes |
| `org_only` | Vendor reported an org-wide total. Reference only. | no |
| `fetch_failed` | The vendor call failed for a day that had no number yet; a placeholder is written. Rate-limited (429) and 5xx responses are retried with backoff first, so this means the vendor stayed unavailable across 4 attempts. A transient failure never overwrites a day already reconciled. | not for drift; a failure that persists past ~25h raises `vendor.bill_sync_failed` (see alerts runbook) |
| `no_basis` | The vendor reported a real number but **we** recorded no vendor spend for a day the `rollup-correction` pass has already rebuilt, so there is nothing to compare it against. `vendor_reported_usd` is real; our three money columns are placeholder zeros and the report shows them as `—`. **It means our cost stamping produced nothing for that day, which is a 100% under-record of real vendor money.** A day the correction pass has not reached yet is *deferred* instead — no row at all, see the troubleshooting table. | not for drift; unhealed past ~25h raises `vendor.bill_sync_failed` exactly like `fetch_failed` |

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
| A day shows `coverage = no_basis` | The `rollup-correction` pass rebuilt that day and it still has no `metric_rollup_1d` `vendor_spend_usd` row, so the day has no comparable basis. Absence is **not** read as zero spend — computing a difference against 0 would report a fabricated −100% and fire the drift alert. The Hub logs `vendor billed a day we have no recorded vendor spend for` with the provider, day, and vendor amount | The cost-stamping path produced nothing for that day: check `SELECT count(*) FROM metric_rollup_1d WHERE "metricName" = 'vendor_spend_usd' AND "bucketStart" = '<day>'`, look for a missing pricing row zeroing `estimated_cost_usd`, then re-run `rollup-correction` for the window. A run of these unhealed past ~25h raises `vendor.bill_sync_failed` |
| A day in the window has **no row at all**, Hub logs `day not yet re-aggregated by rollup correction, deferring` | The day is newer than the `rollup-correction` watermark, so its `vendor_spend_usd` rows may be missing or partial and the day cannot be judged either way. Both jobs run on the same 24h tick, so before reading the cursor the reconcile job now waits up to 10 minutes for the correction pass to reach the end of its window (`waiting for the rollup correction pass to reach the reconcile window`, then `rollup correction reached the reconcile window while waiting`). A day still past the cursor after that wait is deferred | Normally none — the day is inside an 8-day trailing window, so a later run picks it up. If it persists, the correction job is failing or not running: `SELECT "watermark" FROM rollup_watermark WHERE "jobName" = 'rollup-correction'` and check `job_run` for `rollup-correction` |
| `vendor-bill-reconcile` **fails** with `no day in the window can be reconciled` | The correction cursor has not reached even the *start* of the reconcile window, so the run could not have reconciled anything. Most often the cursor has never been published at all (`correctedThrough=never`), which is the state on the first boot after the vendor-spend rollout and any time `rollup-correction` has not yet completed a single successful run. **This is deliberately a failed run**: before 2026-08-09 it returned success while writing nothing, and on stg that hid three consecutive empty runs (2026-08-06..08-08) behind a green job status while the report silently stopped advancing | Fix the correction job, then trigger `rollup-correction` and `vendor-bill-reconcile` in that order. Check `job_run` for `rollup-correction` errors — a Postgres deadlock there leaves the cursor unadvanced and starves this job |
| `our_vendor_spend_usd` is 0 while `our_billed_usd` is not | The row was written before the vendor-spend rollout. Its difference was computed on the old basis and is not comparable with rows written since | None — router cost was never recorded historically, so no backfill can reconstruct it. The report marks these rows *not comparable* |
| `fetch_failed`, log shows `status 429 … rate_limit_exceeded … (after 4 attempts)` | OpenAI meters the whole **organization** at 30 admin-API requests per minute in **fixed one-minute windows**, so the costs endpoint can be limited by traffic this job never issued. A 429 therefore backs off past the window — 65s, then up to 90s per further attempt, with `Retry-After` honoured and preferred — rather than on the sub-second exponential schedule 5xx and transport faults use. Before 2026-08-21 every attempt fell inside the window (0.5s, 1s, 2s), so the retry could not outlive the condition it retried and the provider's whole window became placeholders | Re-run the day; the placeholder self-heals on the next successful run, idle days included. If it recurs daily, something else is spending the same organization's admin-API budget — most often **a second deployment sharing the admin key**. Give each environment its own admin key; the reconcile job itself issues only one request per page per provider per day |
| A day with **no gateway traffic** shows `$0.00` across the row | Expected. The vendor reported a bucket charging nothing and the gateway recorded nothing, so the day is consistent and is written as a real `scoped` zero row rather than left blank | None. A blank would be ambiguous with "never reconciled" — that is why the row is written |
| A provider has no rows and the Hub logs `served from a different host` | The provider shares a covered vendor's `adapter_type` but its `baseUrl` points elsewhere, so it is not billed by that vendor | Expected for self-hosted / OpenAI-compatible endpoints. If the provider really is served by the vendor, correct its `baseUrl` |
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

### `diff_usd` is rounded to the cent; `diff_pct` is not

The two diff figures answer the same question at deliberately different
resolutions.

**`diff_usd` is computed from `vendor_reported_usd` and `our_vendor_spend_usd`
rounded to cents.** The vendor's own cost APIs are only meaningful to the cent
(see "Amount scale" above), so a sub-cent disagreement in the *amount* is the
vendor's own rounding, not drift — there is no dollar figure the vendor could
have billed differently. A day where the two numbers agree to the cent
reconciles to exactly `diff_usd = 0`.

**`diff_pct` is computed from the raw, unrounded pair.** A ratio taken from
cent-rounded operands inherits that rounding as error scaled by `1/vendor`,
which is unbounded as the day's spend shrinks: on a sub-dollar day both operands
land in the same cent and the percentage reads exactly `0.0%` — "perfectly
reconciled" — on precisely the days where the dollar figure is too small to
notice and the percentage is the only visible signal. A real ~1.3% over-estimate
on a $1.28 day displayed as `0.0%`.

Rounding noise and real drift are indistinguishable on any single small day
either way. What separates them is the **sign holding across many days**, and a
column flattened to `0.0%` cannot show a sign at all. That is the trade this
takes: the percentage is noisier per-day and informative in aggregate.

Two consequences, both intended:

- A sub-cent day can show `diff_usd = $0.00` beside a non-zero `diff_pct`. The
  columns are not inconsistent — they are answering at their own resolutions.
- `pctDiff`'s vendor-zero branch (`-100%`) now fires only when the vendor
  genuinely reported `0`, not merely when their amount rounds to zero. A day
  where the vendor billed $0.004 against our $0.02 reports `-400%`, the true
  relative error, rather than a flat `-100%`.

The drift alert's dual threshold (`|diff%| > 5%` **and** `|diff$| > $1.00`, see
the `vendor.bill_drift` section of [alerts.md](./alerts.md)) keeps sub-cent days
from alerting on either figure — the dollar leg is what gates them.

The four money columns (`our_billed_usd`, `our_vendor_spend_usd`,
`our_internal_ops_usd`, `vendor_reported_usd`) keep their full unrounded
precision — only `diff_usd` is quantised. Rows keep whatever basis was in force
when written until the trailing `[today-5, today-2]` window re-reconciles them.

## Related

- [alerts.md](./alerts.md) — the `vendor.bill_drift` rule, thresholds, and why
  only `scoped` rows fire.
- [Bill Reconciliation report page](../../../users/features/cp-ui/overview.md)
