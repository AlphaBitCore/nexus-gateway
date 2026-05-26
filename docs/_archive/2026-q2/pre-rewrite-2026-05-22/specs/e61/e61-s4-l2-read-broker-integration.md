# E61-S4 — L2 Read Path + Broker Integration + Freshness Wire-up

> Story: e61-s4
> Epic: 61
> Status: Draft
> Requirements: `docs/developers/specs/e61/e61-smart-response-cache.md` §FR-2 (read half), §FR-1.3, §FR-1.5, §FR-6
> Architecture: `docs/developers/architecture/services/ai-gateway/response-cache-architecture.md` §3.8, §4
> Blocked by: e61-s1, e61-s2, e61-s2b, e61-s3, e61-s5
> Blocks: e61-s7 (smoke)

## User Story

As a Gateway Admin who has enabled semantic cache on a route, I want incoming requests that miss L1 but semantically match a prior request to be served from L2 with the right `GatewayCacheKind=semantic` audit stamp, and the freshness detector to skip cache entirely for time-sensitive prompts, so the cache delivers real ROI without false-hits on stale-prone content.

## Tasks

### T1 — Freshness detector wire-up in `classifyCachePreLookup`

- T1.1 `packages/ai-gateway/internal/ingress/proxy/proxy.go` `classifyCachePreLookup` signature extension:
    ```go
    func classifyCachePreLookup(
        cacheEnabled, hasNoCacheHeader, hasTargets, passthroughBypassCache bool,
        timeSensitiveDetector *freshness.Detector,
        canonicalMessages []canonical.Message,
        skipTimeSensitivePolicy bool,
    ) (audit.GatewayCacheStatus, audit.GatewayCacheSkipReason)
    ```
- T1.2 Decision tree (in order):
    1. `!cacheEnabled || !hasTargets` → `(skipped, disabled)`.
    2. `passthroughBypassCache` → `(skipped, passthrough)`.
    3. `hasNoCacheHeader` → `(skipped, no_cache)`.
    4. `skipTimeSensitivePolicy && timeSensitiveDetector.IsTimeSensitive(canonicalMessages)` → `(skipped, time_sensitive)`.
    5. `("", "")` → proceed to L1 → L2.
- T1.3 The caller (ServeProxy) now also has access to `routePolicy.SkipTimeSensitive`. Pass it through.

### T2 — L2 lookup hook in proxy.go

- T2.1 Between the L1 lookup (existing) and the broker dispatch (existing), insert the L2 phase:
    ```go
    // After L1 miss:
    if route.SemanticPolicy.Enabled {
        plan := inputstaging.Plan(canonicalMessages, embeddingModel.ContextLimit, route.SemanticPolicy.EmbedStrategy, reserveOutput)
        if plan.OverflowKind != inputstaging.OverflowNone {
            // skip L2; proceed to broker
            rec.GatewayCacheSkipReason = audit.GatewayCacheSkipReasonOversizeForEmbedding
            // ^ note: L1 wasn't skipped, only L2; the skip reason here is informational
            //   on a per-tier basis. We continue to broker either way.
        } else {
            embedding, cost, err := semanticClient.Embed(ctx, plan.Messages)
            rec.EmbeddingCostUsd = cost
            if err == nil {
                hit, err := semanticClient.Lookup(ctx, &semantic.LookupInput{
                    Embedding:       embedding,
                    EmbedProvID:     route.SemanticPolicy.EmbeddingProviderID,
                    EmbedModelID:    route.SemanticPolicy.EmbeddingModelID,
                    UpstreamProvID:  primaryTarget.ProviderID,
                    UpstreamModelID: primaryTarget.ProviderModelID,
                    VKScope:         resolveVKScope(rec, route.SemanticPolicy.VaryBy),
                    Threshold:       route.SemanticPolicy.Threshold,
                    AllowCrossModel: route.SemanticPolicy.AllowCrossModel,
                })
                if hit != nil {
                    rec.GatewayCacheStatus = audit.GatewayCacheHit
                    rec.GatewayCacheKind = audit.GatewayCacheKindSemantic
                    return handleSemanticHit(...)
                }
            }
        }
    }
    // Fall through to broker.
    ```
- T2.2 The semantic hit branch reuses `handleStreamHit` / `handleNonStreamHit` — the only difference from extract is the `GatewayCacheKind` stamp.
- T2.3 The lookup result's `ResponseBody` populates the `*cache.StreamEntry` or `*cache.ResponseEntry` exactly as if it had come from extract — so the existing hit-replay path runs unchanged.

### T3 — Lookup implementation

- T3.1 `packages/ai-gateway/internal/cache/semantic/lookup.go`:
    ```go
    func (c *Client) Lookup(ctx context.Context, in *LookupInput) (*Entry, error)
    ```
- T3.2 Build the FT.SEARCH query:
    ```
    filterParts := []string{
        fmt.Sprintf("@vk_scope:{%s}", in.VKScope),
    }
    if !in.AllowCrossModel {
        filterParts = append(filterParts, fmt.Sprintf("@upstream_provider:{%s} @upstream_model:{%s}", in.UpstreamProvID, in.UpstreamModelID))
    }
    query := fmt.Sprintf("(%s) =>[KNN 1 @vector $vec AS __vector_score]", strings.Join(filterParts, " "))
    // FT.SEARCH indexName query PARAMS 2 vec <vectorBlob> SORTBY __vector_score DIALECT 2
    ```
- T3.3 Parse the result:
    - If 0 results → return nil, nil (miss).
    - If 1 result: parse `__vector_score` (the cosine DISTANCE; similarity = 1 - distance/2 for cosine in valkey-search; verify the exact formula in module docs and add a constant).
    - If similarity ≥ threshold → return `*Entry` populated from the hash fields.
    - Else → return nil (threshold miss). Stamp `nexus_cache_l2_threshold_misses_total`.
- T3.4 Streaming entries: when `response_kind=stream`, the `Entry.StreamChunks` populates from a stored JSON blob; non-stream stores the bytes directly.

### T4 — Wire embeddingCostUsd on every path

- T4.1 Even when L2 misses (threshold or no-result), the embedding call cost is real and must be stamped on `traffic_event.embedding_cost_usd`.
- T4.2 The audit Record set in T2.1 (`rec.EmbeddingCostUsd = cost`) flows through `audit.Write`.

### T5 — Metrics

- T5.1 Add to `packages/ai-gateway/internal/cache/semantic/metrics.go`:
    - `nexus_cache_l2_lookups_total{outcome="hit|threshold_miss|miss|skip_overflow|skip_disabled"}`
    - `nexus_cache_l2_similarity_histogram` — bucketing 0.5, 0.7, 0.8, 0.85, 0.9, 0.92, 0.94, 0.96, 0.98, 1.0.
    - `nexus_cache_l2_lookup_latency_seconds` histogram.
- T5.2 Increment on every Lookup call regardless of outcome.

### T6 — Tests

- T6.1 Integration test with miniredis + valkey-search stub: write 3 entries, look up a 4th with one being a near-neighbour above threshold → returns the matching entry.
- T6.2 Threshold-miss test: similarity below threshold → returns nil + increments threshold_miss counter.
- T6.3 Cross-model filter test: `allow_cross_model=false` rejects a same-provider-different-model entry; `allow_cross_model=true` accepts it.
- T6.4 VK scope test: entries written under `vk_scope=v1` do not match a `vk_scope=v2` lookup.
- T6.5 Time-sensitive end-to-end: `classifyCachePreLookup` with a time-sensitive prompt returns `(skipped, time_sensitive)` and neither L1 nor L2 is consulted.
- T6.6 Hook ordering test: assert that the request-stage hook pipeline runs BEFORE the L2 lookup; a request that the request hook rejects never reaches semantic lookup.
- T6.7 Latency budget benchmark: L2 lookup on a 10k-entry index completes the embedding+KNN within the p95 ≤80ms budget (excluding network round-trip variability — test with a mock embedding adapter that simulates 30ms latency).
- T6.8 Coverage ≥95%.

## Acceptance Criteria

- A1: A request whose prompt is semantically similar to a previously-cached prompt (above threshold, same upstream provider+model, same VK) hits L2 and serves the cached response with `GatewayCacheStatus=hit, GatewayCacheKind=semantic`.
- A2: A request whose embedding similarity is below threshold proceeds to the broker; the threshold-miss is recorded in metrics.
- A3: A time-sensitive prompt (matching any active rule) is stamped `GatewayCacheStatus=skipped, GatewayCacheSkipReason=time_sensitive` and neither L1 nor L2 fires.
- A4: An oversize embedding input is stamped `GatewayCacheSkipReason=oversize_for_embedding` for L2 only; L1 lookup still ran.
- A5: Request-stage hooks ALWAYS run before L2 lookup. A request hook reject prevents L2 lookup from running (this is the existing behaviour for L1; assert it holds for L2 too).
- A6: `traffic_event.embedding_cost_usd` is stamped on every L1 miss where an embedding call ran, regardless of L2 outcome.
- A7: The hit-replay path is shared with extract — `handleStreamHit` / `handleNonStreamHit` run identically.
- A8: Latency budget p95 ≤80ms additional on the L1-miss path (excluding upstream call).

## Out of Scope (S4)

- New embedding adapter — S5.
- UI changes — S6.
- E2E smoke validation — S7.
