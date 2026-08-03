# Prometheus naming architecture

Every Nexus service exposes a Prometheus `/metrics` endpoint. This document is the naming contract for that surface: how a metric name is formed, where the name comes from, and the one rule that keeps the whole fleet consistent — **service identity is a dimension, never part of the metric name.**

The metric *universes* (real-time Prometheus vs the Hub-bound rollup pipeline) are introduced in [observability-architecture.md](observability-architecture.md) §5; the rollup side is detailed in [metrics-rollup-architecture.md](metrics-rollup-architecture.md). This doc owns naming.

## 1. The root rule: `nexus_<subsystem>_<name>`

Every series shares the single application prefix `nexus`, followed by an optional subsystem segment and the measurement name with a unit suffix (`_total`, `_seconds`, `_bytes`, `_ms`). Examples: `nexus_requests_total`, `nexus_cache_lookups_total`, `nexus_credential_health_rollup_cycles_total`.

**Enforced by `scripts/check-prometheus-naming.sh`** — `npm run check:prometheus-naming`, and pre-commit on staged Go files. It rejects a service name inside a metric name and any `Namespace:` other than `"nexus"` (a namespace is prefixed onto the name, so `"nexus_hub"` reintroduces the service by another route). The service list is derived from `packages/shared/schemas/thingtype`, so a new service is covered without touching the script. The rule went unenforced long enough to accumulate two violations — `nexus_ai_gateway_admission_shed_total` and `Namespace: "nexus_hub"` — both fixed when the check landed.

**The service is not in the name.** Which binary emitted a metric is carried by the Prometheus `job` label (set by the scrape config), not by a `nexus_<service>_…` prefix. This mirrors how the rollup side already works: `metric_ops_raw` records the service as the `thing_type` column and keeps a flat `metric_name`. Both surfaces treat service as a dimension, so the same subsystem metric emitted by two services (for example MQ counters on the Control Plane and the Compliance Proxy) is one series name distinguished by `job`.

## 2. Two registration paths

A metric reaches `/metrics` through one of two paths. They share the root rule above but differ in what else they feed.

### Path 1 — the opsmetrics Registry (dual-surface)

`packages/shared/core/metrics/registry/registry.go` is the "one registration site, two surfaces" path. A single `NewCounter` / `NewGauge` / `NewHistogram` call both registers a real Prometheus `*Vec` (scraped at `/metrics`) **and** records a binding so the per-tick `Collect()` can emit the same value as a `metrics_sample` over WebSocket to the Hub (the rollup pipeline).

- Instruments are declared with **dotted** logical names (`requests_total`, `hook.pipeline_total`). `promName` converts a dotted name to its Prometheus form: replace `.` with `_`, then prefix the registry namespace and `_`. With the default namespace `nexus`, `hook.pipeline_total` becomes `nexus_hook_pipeline_total` and `requests_total` becomes `nexus_requests_total`.
- The default namespace is `nexus` (`DefaultNamespace`); `NewRegistry` pins it. The dotted name carries the subsystem when one applies (`hook.`, `traffic.`, `router.`), so the Prometheus form is already `nexus_<subsystem>_<name>` with no service segment.
- The dotted name — not the Prometheus form — is what travels on the WebSocket sample and lands in `metric_ops_raw.metric_name`, so the Prometheus namespace can change without touching rollup data.
- The AI Gateway's business metrics ride this path (`packages/ai-gateway/internal/platform/metrics/metrics.go`): `requests_total{provider,model,endpoint,status}`, `request_duration_ms{provider,model,endpoint}`, `tokens_total`, `errors_total`, `schema_mismatch_total`, `hook.pipeline_total`, `traffic.extract_total`, `router.retry_total`, and the `estimate_*` family — exposed as `nexus_requests_total`, `nexus_hook_pipeline_total`, and so on.

Labels use a pin pattern: `With(values…)` binds label values in declaration order, and the underlying Prometheus vec panics on arity mismatch when the pin is observed. On the sample side, `Collect()` renders labels into a sorted `k=v;k=v` `DimensionKey`.

### Path 2 — direct promauto (Prometheus-only)

Subsystems that do not need a Hub-bound sample register straight onto a Prometheus registerer via `promauto` (or `prometheus.New*Vec`), composing the name from the standard `Namespace` + `Subsystem` + `Name` option fields. These series appear only on `/metrics`; `Collect()` never sees them, so they are absent from the rollup pipeline.

Every such site sets `Namespace: "nexus"` and carries the subsystem in `Subsystem` (or in the name), producing `nexus_<subsystem>_<name>`. Examples:

- `nexus_cache_lookups_total` — stream cache (`packages/ai-gateway/internal/cache/stream/metrics.go`)
- `nexus_cache_l2_*` / `nexus_cache_freshness_*` — semantic + freshness caches
- `nexus_gemini_cache_*_total` — Gemini cached-content cache
- `nexus_embeddings_*` — embedding client
- `nexus_credstats_*` — credential usage buffer
- `nexus_mq_*` — MQ producer/consumer counters (`packages/shared/transport/mq/metrics.go`), shared by every service that runs MQ; `mq.Config.Namespace` is the metrics namespace, and every service's wiring passes `nexus`
- `nexus_normalize_*` — the normalize pipeline (`packages/shared/transport/normalize/core/metrics.go`)
- `nexus_credential_health_rollup_*` / `nexus_credential_circuit_flush_*` — Hub scheduler jobs

Constructors that register direct metrics take a `namespace string` parameter (or read `mq.Config.Namespace`); production callers pass `nexus`.

## 3. The canonical histogram

Latency histograms across both paths use one fixed layout, `HistogramBucketsMs = {50, 100, 200, 500, 1000}` — five explicit millisecond upper bounds plus the implicit `+Inf` bucket, six buckets total. The registry path keeps its own `[6]uint64` bucket array alongside the Prometheus instrument (the Prometheus Go client does not expose bucket counts through a public API) so the sample written to the rollup pipeline carries the same six-element shape. `bucketIndex` maps an observation in milliseconds to `[0,50) [50,100) [100,200) [200,500) [500,1000) [1000,+inf)`.

## 4. The scrape surface

All five services expose `/metrics` via `promhttp.Handler()`:

- Nexus Hub — `packages/nexus-hub/cmd/nexus-hub/wiring/routes.go`
- AI Gateway — `packages/ai-gateway/cmd/ai-gateway/wiring/routes.go`
- Control Plane — `packages/control-plane/cmd/control-plane/wiring/echosetup.go`
- Compliance Proxy — `packages/compliance-proxy/internal/health/handler.go`; the internal admin endpoint (`packages/compliance-proxy/internal/runtime/server/server.go`) requires the internal token via `tokenAuth.Require`.

Because service identity lives in the `job` label, a scrape config must assign each service a distinct `job_name` for the fleet to be queryable per service — the metric names alone do not encode it.

## References

- `packages/shared/core/metrics/registry/registry.go` — `promName`, `NewCounter` / `NewGauge` / `NewHistogram`, `HistogramBucketsMs`, `Collect`
- `packages/shared/core/metrics/platform/sampler.go` — per-tick `Collect()` into a `metrics_sample` batch
- `packages/ai-gateway/internal/platform/metrics/metrics.go` — AI Gateway business metrics (registry path)
- `packages/shared/transport/mq/metrics.go` + `packages/shared/transport/mq/config.go` — MQ metrics + namespace
- `packages/shared/transport/normalize/core/metrics.go` — normalize metrics
- `packages/ai-gateway/internal/cache/stream/metrics.go` — cache metrics (direct path)
- `packages/nexus-hub/internal/jobs/defs/rollup/credential_health_rollup.go` + `packages/nexus-hub/internal/jobs/defs/retention/credential_circuit_flush.go` — Hub job metrics
- `packages/compliance-proxy/internal/health/handler.go` — Compliance Proxy `/metrics` + shadow gauge
