# Cost estimation architecture

> Full cost-estimation architecture write-up is queued in the docs-backfill program. This page currently captures (1) the endpoint-type vocabulary the cost layer consumes and (2) the streaming mode × cost stamping interaction. Full cost-formula registry / metering / pricing pipeline follows in a later commit.
>
> Source-of-truth code: [`packages/ai-gateway/internal/cache/layer/pricing.go`](../../../../../packages/ai-gateway/internal/cache/layer/pricing.go), [`packages/ai-gateway/internal/observability/metrics/cost.go`](../../../../../packages/ai-gateway/internal/observability/metrics/cost.go), [`packages/ai-gateway/internal/execution/estimator/`](../../../../../packages/ai-gateway/internal/execution/estimator/).

## Endpoint-type vocabulary

Cost formulas are keyed by the canonical `typology.EndpointKind` string (`chat`, `embeddings`, `stt`, `tts`, `image_generation`, `batch`). The `audit.Record.EndpointType` field on a finalised traffic event carries this string verbatim, and the cost estimator looks it up against the registered formula registry by exact match.

The string is derived once per request at handler-dispatch time via `string(typology.KindFromWireShape(resolved.WireShape))` and flows unchanged into `audit.Record.EndpointType`, `TrafficEventMessage.EndpointType`, `traffic_event.endpoint_type`, and the AI Gateway Prometheus `endpoint` label — no translation hop, no per-consumer vocabulary. See [endpoint-typology-architecture.md §7](../../cross-cutting/foundation/endpoint-typology-architecture.md) for the shared path-segment helper that hooks + audit both delegate to.

**Adding a new cost formula for a new endpoint kind.** Three coordinated changes: (1) add the `EndpointKind*` constant in `packages/shared/transport/typology/endpointkind.go`; (2) register the cost formula keyed by the canonical kind string in `packages/ai-gateway/internal/execution/estimator/cost_formula_registry.go`; (3) add the rule to `packages/shared/transport/typology/defaults.go` so `ClassifyPath` recognises the request path.

## Cost stamp / per-record fields

The per-traffic-event cost is stamped onto `traffic_event` rows at audit-emit time and again on cache-hit short-circuits (cache HIT serves bypass the upstream call but still record a billable event at the cached cost). The five stamp sites in the AI Gateway hot path are documented inline in `packages/ai-gateway/internal/ingress/proxy/proxy.go` and `proxy_cache.go`.

## Streaming mode × cost stamping interaction (#115)

ai-gateway's SSE handler dispatches between two streaming modes based on the admin policy in `*streampolicy.Store`:

| Mode | Cost-stamping point |
|---|---|
| `chunked_async` (live) | Usage / cost lands on `rec` per-checkpoint as deltas arrive; final usage from upstream's terminal frame seals the row. |
| `buffer_full_block` | Whole body buffers before the single hook checkpoint; final usage from the buffered terminal frame stamps `rec` once on the path back through replay. |

Both paths go through the same usage extractor and pricing lookup (`pricing.go`); the dispatch only changes *when* the rec fields land, not *what* they contain. See [`sse-streaming-compliance-architecture.md`](../../cross-cutting/safety/sse-streaming-compliance-architecture.md) for the streaming dispatch contract.
