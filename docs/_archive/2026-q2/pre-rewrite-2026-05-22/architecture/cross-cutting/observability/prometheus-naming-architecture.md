---
doc: prometheus-naming-architecture
area: cross-cutting
service: observability
tier: 1
updated: 2026-05-20
---

# Prometheus Metric Naming Convention

> **Tier 2 architecture doc.** Read when adding a new Prometheus metric, refactoring an existing one, or designing the metric surface for a new subsystem. The Go-side helper lives at `packages/shared/core/metrics/` and is split into `instruments/` (primitive types + bucket helpers), `platform/` (per-service registration glue) and `registry/` (the shared registry seam). Metrics are registered via `promauto`.

Consistent metric naming makes dashboards composable and PromQL queries portable across services. This doc is the canonical reference for the naming convention Nexus uses.

---

## 1. Three-part naming

```
<namespace>_<subsystem>_<measurement>[<unit_suffix>]
```

| Part | Example | Notes |
|---|---|---|
| `namespace` | `nexus` (always) | Identifies the project; constant across services |
| `subsystem` | `gateway`, `hub`, `compliance_proxy`, `agent`, `cp` | Identifies the service emitting |
| `measurement` | `requests_total`, `hook_latency_seconds`, `cache_hit_ratio` | What's being measured |
| `unit_suffix` | `_seconds`, `_bytes`, `_total` (counters), `_ratio` (0-1) | Follows Prometheus convention |

Examples (real metric names from the codebase):

```
nexus_agent_pf_flows_accepted_total          // packages/agent/internal/platform/darwin/pfintercept/listener/types.go
nexus_agent_pf_udp_blocked_total             // same
nexus_hub_agent_normalize_*                  // emitted via normalizecore.MustRegisterPrometheus(..., "nexus_hub_agent") at packages/nexus-hub/cmd/nexus-hub/wiring/observability.go:162
```

The compliance proxy and parts of the AI Gateway use a sibling dotted-name `registry` package (`cert_cache.hits_total`, `tunnels.total`, `cache.hit_total`, …) registered via the shared `registry` seam. New code in those services should follow the dotted convention there; new code in Hub / Agent services follows the `<namespace>_<subsystem>_<measurement>` form documented above.

## 2. Counters use `_total` suffix

Counter metrics end in `_total`. Histograms / gauges do not.

```
✓ requests_total          (counter)
✓ request_duration_seconds (histogram)
✓ queue_depth             (gauge)

✗ requests                 (counter without _total)
✗ request_duration_seconds_total (histogram with _total)
```

Prometheus tooling (and rate() / increase() functions) work better when this convention is followed.

## 3. Unit suffixes

Use SI / standard units, expressed in seconds / bytes / etc.:

- `_seconds` for durations (NOT `_ms` or `_microseconds`).
- `_bytes` for sizes (NOT `_kb` / `_mb`).
- `_ratio` for 0-1 ratios.
- `_count` is implicit on `_total`; not used elsewhere.

```
✓ request_duration_seconds
✓ request_size_bytes
✓ cache_hit_ratio

✗ request_duration_ms
✗ request_size_kb
```

## 4. Labels

Labels add dimensions. Conventions:

- **Lowercase, underscore-separated**: `provider`, `model`, `error_class`, `org_id`.
- **Bounded cardinality**: prefer enums + IDs over free-form strings.
- **Stable**: don't rename labels without a major version bump.

| ✓ Good label | ✗ Bad label | Why |
|---|---|---|
| `provider="openai"` | `provider="OpenAI"` | Always lowercase |
| `error_class="Rate429"` | `error_message="rate limit exceeded"` | High cardinality on free-form text |
| `org_id="org-acme"` | `org_email="admin@acme.com"` | PII in labels is forbidden |
| `model="gpt-4o"` | `model="gpt-4o (latest)"` | Stable identifiers, no free-form |

## 5. Cardinality budget

Each metric's label cardinality is `product(unique_values_per_label)`. Keep under ~1000 unique series per metric per service per tenant. Above 10K series, dashboards slow and ingestion costs spike.

Rough budget:

- `provider` × `model` × `error_class` × `org_id` for a 5-provider × 10-model × 8-class × 100-org tenant = 40,000 series. Too high. Solution: split — high-cardinality `org_id` lives on the analytics path (Postgres), not on Prometheus.

A useful pattern: emit a metric WITHOUT org-level dimension to Prometheus (low cardinality) and a separate audit / analytics path to Postgres (where high cardinality is fine).

## 6. Histogram bucket choice

There are no `ShortDurationBuckets` / `LongDurationBuckets` / `SizeBuckets` exported constants in `packages/shared/core/metrics/` today. Each instrument call passes its own explicit bucket slice — short-duration HTTP-style histograms generally use `prometheus.DefBuckets`; long-duration job-style histograms use a hand-rolled slice on the registration site.

The pragmatic convention:

- HTTP-request-like latency → `prometheus.DefBuckets` unless the instrument has a documented reason to diverge.
- Long-running job duration → an explicit hand-rolled slice on the registration; document the reasoning in a comment on that line.
- Size histograms → an explicit slice with `_bytes` suffix on the metric name; comment with the rationale.

When a third use of the same custom slice appears, promote it to a shared `instruments/` constant.

## 7. Registration pattern

```go
var (
    requestsTotal = promauto.NewCounterVec(
        prometheus.CounterOpts{
            Namespace: "nexus",
            Subsystem: "gateway",
            Name:      "requests_total",
            Help:      "Total number of /v1/* requests handled.",
        },
        []string{"provider", "model", "error_class"},
    )
)
```

The constructor accepting a `namespace string` parameter (per CLAUDE.md Go conventions §Metrics) makes the constants explicit and substitution easy in tests.

## 8. What NOT to emit as a metric

- **PII**: any user identifier beyond stable IDs. `user_email` is OUT; `user_id` is OK.
- **Request bodies**: way too high cardinality.
- **Free-form error messages**: use `error_class` enum.
- **Timestamps as labels**: time is the X axis; don't put it in a label.

## 9. Documentation

Each metric MUST carry a `Help` string. The help text:

- Says what's being measured.
- Names the unit (in case the metric name elides it).
- Notes the cardinality of labels.

```go
Help: "Number of /v1/* requests handled, labelled by provider+model+error_class. " +
      "High cardinality; downsample if extracting to long-term storage.",
```

## 10. Dashboard conventions (briefly)

Dashboards live in `docs/operators/ops/grafana/` (when wired). Naming:

- One dashboard per high-level concern (`AI Gateway Traffic`, `Compliance Proxy Health`, `Audit Pipeline`, ...).
- Use the same canonical labels across panels for filter consistency.
- Include a top-level filter for `tenant` / `org_id` so a single dashboard serves all tenants.

<!-- 💡 harvest: a CI lint that grep-scans Prometheus metric registrations for naming violations would catch drift. Promauto registrations are static so regex-feasible. Adding to the harvest pile; not urgent. -->

## 11. Sources

- `packages/shared/core/metrics/instruments/` — primitive metric types + bucket helpers (no top-level `types.go`; types live in `instruments/types.go`).
- `packages/shared/core/metrics/registry/` — the shared registry seam.
- `packages/shared/core/metrics/platform/` — per-service registration glue.
- `packages/*/internal/metrics/` — per-service metric definitions.
- CLAUDE.md "Go § Metrics" — `promauto` + `namespace` parameter convention.

## 12. Cross-references

- `otel-pipeline-architecture.md` — sibling tracing pipeline; same naming conventions for span attribute keys.
- `alerting-architecture.md` — alert rules consume these metrics.
- `metrics-rollup-architecture.md` — per-Thing rollup of these metrics.
- `docs/users/features/cp-ui/overview.md` — Metrics Explorer UI.
