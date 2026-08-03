# Monitoring

What exists, where it lives, and what to do when a number looks wrong.

Architecture background: [observability-architecture.md](../../developers/architecture/cross-cutting/observability/observability-architecture.md). This page is the operator's view.

## The five surfaces, and which one answers your question

| Question | Surface | Where you look |
| --- | --- | --- |
| What is happening right now, in aggregate? | Metrics | Prometheus (`/metrics` scrape) |
| What happened to **this one request**? | Audit | `traffic_event`, keyed by `trace_id` |
| Why did a service misbehave internally? | Diag | `thing_diag_event`, same `trace_id` |
| Span-by-span across services? | Traces | Your OTel collector — **off unless an endpoint is configured**; Nexus stores no spans |
| What must reach our SIEM? | SIEM bridge | Your external sink |

They share one correlation key: **`X-Nexus-Request-Id`**, echoed on every response and landing in `traffic_event.trace_id` and `thing_diag_event.trace_id`. Start there — it is the only id that crosses all of them.

## Scraping

**Config: [`deploy/prometheus/prometheus.yml`](../../../deploy/prometheus/prometheus.yml).** Read its header before deploying — the AI Gateway's `/metrics` is token-gated and a scrape without the bearer silently returns 401.

| job_name | target | Auth |
| --- | --- | --- |
| `ai-gateway` | `127.0.0.1:3050` | **Bearer** (`INTERNAL_SERVICE_TOKEN`) |
| `nexus-hub` | `127.0.0.1:3060` | none |
| `control-plane` | `127.0.0.1:3001` | none |
| `compliance-proxy` | `127.0.0.1:9090` | **Bearer** — note the port, **not** 3128 |

**The `job` label is load-bearing.** Metric names deliberately exclude the service (`nexus_requests_total` is emitted by more than one binary), so `job` is the only thing separating them — see [prometheus-naming-architecture.md](../../developers/architecture/cross-cutting/observability/prometheus-naming-architecture.md) §1. Rename a `job_name` and every per-service query breaks.

**First check when a dashboard empties out:** `up{job="ai-gateway"}`. `0` means the scrape is failing, and the overwhelmingly common cause is a token mismatch between `/etc/prometheus/nexus-internal-token` and the service's `INTERNAL_SERVICE_TOKEN` — a silent 401.

## The metrics you will actually reach for

Everything is prefixed `nexus_`. Not exhaustive — `curl` a `/metrics` endpoint for the full list (435 series on a live gateway).

**Traffic and latency** (job `ai-gateway`)

```promql
sum(rate(nexus_requests_total[5m])) by (provider, model)          # throughput
sum(rate(nexus_requests_total{status=~"5.."}[5m])) by (provider)  # server-side failure rate
histogram_quantile(0.95, sum(rate(nexus_request_duration_ms_bucket[5m])) by (le, provider))
sum(rate(nexus_tokens_total[5m])) by (direction)                  # prompt vs completion
sum(rate(nexus_errors_total[5m])) by (error_type)                 # failure BY CAUSE
```

`nexus_errors_total{error_type}` carries the canonical code (`auth_failed`, `rate_limited`, `context_overflow`, …) — this is the metric that answers *"why are we failing"*, not just *"how much"*.

**Overload** (job `ai-gateway`)

```promql
rate(nexus_admission_shed_total[5m])   # > 0 means the in-flight gate is rejecting
```

Non-zero means arrival rate is above what the box can serve and requests are being shed with 429 to keep the heap bounded. The cap is `1024 × GOMAXPROCS`, overridable with `AI_GATEWAY_MAX_INFLIGHT`. **Shedding is the gate working**, not a bug — but it means capacity is short.

**Routing and retries** (job `ai-gateway`)

```promql
sum(rate(nexus_router_retry_total[5m])) by (outcome)
```

Outcomes: `retried_succeeded`, `failover_class_excluded`, `exhausted`, `failover_context_overflow`, `failover_no_credential`.

**Leadership** (job `nexus-hub`)

```promql
sum(nexus_scheduler_leader)    # MUST equal 1
```

`0` = no leader, so no scheduled job is running — rollups, credential-circuit flush, passthrough expiry all stop. `>1` = duplicate leaders running jobs independently, which is worse than none.

**Admin API** (job `control-plane`)

```promql
sum(rate(nexus_http_requests_total[5m])) by (route_class, status_class)
rate(nexus_auth_attempts_total{result="failure"}[5m])
rate(nexus_admin_audit_log_failed_total[5m])    # > 0 = audit writes are being lost
```

## Cardinality: the one thing that will hurt you

`nexus_requests_total` and `nexus_tokens_total` carry a **`model` label populated from the caller's request**. It is not bounded. A client sending garbage model strings at volume creates a new series per string.

Watch it:

```promql
count(count by (model) (nexus_requests_total))    # distinct models seen
```

Compare against your catalog size. A number climbing without new models being added is the leading indicator. (The Control Plane deliberately bounds its own `route_class` label by feeding the Echo route template rather than the concrete URL; the gateway has no equivalent bound on `model` — tracked as an open governance item.)

## Where alerting lives, and where it does not

**Not in Prometheus.** The Hub evaluates alerts against `traffic_event` and the metric rollups, writes `Alert` rows, and fans out to webhook/Slack/email/PagerDuty. Rules are DB-backed and tunable per tenant. See [alerting-architecture.md](../../developers/architecture/cross-cutting/observability/alerting-architecture.md) and the [alerts runbook](runbooks/alerts.md).

Prometheus here is for ad-hoc query, dashboards, and capacity work. Adding `rule_files` means running a second alerting system — do it deliberately or not at all.

## Known gaps

An honest list, so you do not go hunting for something that is not there:

- **No dashboards in-repo.** No Grafana JSON is shipped. The queries above are the starting set.
- **No SLOs.** Nothing defines a target or an error budget.
- **Tracing is off** unless an OTel endpoint is configured, and Nexus persists no spans regardless — cross-service correlation is via `trace_id` in the database, not via traces.
- **The Agent has no business metrics.** It emits diag events and a WebSocket `metrics_sample`, but has no `/metrics` endpoint and no `Recorder`. Do not expect agent traffic in Prometheus.
