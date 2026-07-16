# LiteLLM Stability Guide

LiteLLM has repeatedly shown a high, erratic latency tail in our runs. Some of that
is real; some is an artifact of a cold connection pool, a rig/upstream limit, or
ordering. This guide records the controls the harness applies so a LiteLLM number is
either trustworthy or **honestly flagged** — never quietly quoted as "slow" when the
cause is unproven.

---

## 1. Run LiteLLM last

`config/global.yaml` carries a `run_order`; `engine/stability.order_gateways()`
honors it and **forces LiteLLM to the end** regardless of the requested order.

```
run_order: [nexus-hooks-on, nexus-hooks-off, bifrost, agentgateway, litellm]
```

Why: a shared upstream mock warms up over the course of a suite (connection pools,
JIT, page cache). Running LiteLLM last removes the "first gateway pays the cold-start
tax" confound. `validate_benchmark.py` WARNs if a LiteLLM result was not last.

## 2. Warm up before measuring

Warmup already exists in `engine/runner.py` (`warmup_duration_seconds`, warmup
requests dropped from metrics). For LiteLLM specifically, record the warmup as
`stability.warmup_record(duration_s)`:

```json
"warmup": {"duration_s": 30, "completed": true, "excluded_from_metrics": true}
```

A missing `warmup` record on a LiteLLM result is a WARN — a cold connection pool can
dominate its p95 and make the number meaningless.

## 3. Resource telemetry (null, never zero)

`engine/stability.ResourceSampler` samples `docker stats` for the gateway container
during the run and records `resource_observations`:

| field | source |
|-------|--------|
| `cpu_peak_pct`, `memory_peak_mb` | docker stats sampling |
| `container_restart_count` | docker inspect |
| `upstream_5xx_count`, `upstream_4xx_count`, `timeout_count` | folded from the run's own counters |
| `source_status` | `available` / `partial` / `unavailable` |

**When docker stats is unreadable, every numeric field is `null` — never `0`.** A
fabricated `0%` CPU would falsely imply "the gateway was idle, so the tail is its own
fault." `source_status: unavailable` says plainly "we could not measure the box," and
the validator WARNs that a gateway-vs-rig attribution cannot be made.

## 4. Anomaly classification — evidence, not vibes

`stability.classify_anomaly(metric, ratio_threshold=4.0)` labels a result
`anomaly_status == "anomalous"` **only when both** hold:

1. `ttft_p95 / ttft_p50 > threshold` (a fat tail), **and**
2. there were actual errors/timeouts (`failed`, `connection_timeouts`, …) > 0.

A big tail with **zero** errors is *not* called anomalous — it may be genuine
gateway behavior, and we do not get to dismiss a real number as "an anomaly." When a
result is flagged anomalous, `infrastructure_status` is set to `"unknown"`: we
observed instability but have not proven whether the cause is the gateway, the
upstream, or the rig. We never assert "LiteLLM is slow" from a tail alone.

## 5. What the validator says about LiteLLM

`scripts/validate_benchmark.py` emits **WARN** (not BLOCK) for LiteLLM when:

- no warmup record,
- it did not run last,
- resource telemetry is unavailable,
- `ttft_p95/p50` exceeds the ratio threshold (prints the computed ratio +
  `anomaly_status` + `infrastructure_status`),
- there were nonzero timeouts/5xx.

These are caveats a human must read before quoting the LiteLLM number — deliberately
WARN, because a stability caveat does not invalidate the whole run the way a
governance/methodology violation does.

See also: [HOOKS_MODE_METHODOLOGY.md](HOOKS_MODE_METHODOLOGY.md), [RUNBOOK.md](RUNBOOK.md).
