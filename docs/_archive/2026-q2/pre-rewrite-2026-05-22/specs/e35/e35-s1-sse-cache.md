# E35-S1 — SSE Response Cache + Broker Fan-out

## 0. References

- Requirements: `docs/developers/specs/e35/e35-response-cache.md`
- Design (full): `docs/_archive/2026-q2/brainstorms/2026-05-06-sse-cache-design.md`
- Implementation plan: `docs/_archive/2026-q2/brainstorms/plans/2026-05-06-sse-cache.md`

The full architecture, decision matrix (D1–D10), key formula,
broker semantics, and security analysis are documented in the
design spec. This SDD is the SDD-pipeline-canonical entry; it
points back rather than duplicates so the spec stays the single
source of truth as design evolves.

## 1. User story

> *"As a gateway operator, when N customers send identical
> streaming prompts, I want one upstream call instead of N, and I
> want repeat requests to skip the upstream entirely while still
> passing through compliance hooks."*

## 2. Tasks (mirrored from plan, for traceability)

- **T-PREPAREBODY** — Promote `Adapter.PrepareBody` to public
  interface; split `Execute` into `PrepareBody` +
  `ExecuteWithBody`.
- **T-CACHEKEY-V2** — Cache key v2 formula
  (`SHA256(provider + ProviderModelID + canonicalize(prepareBody output))`,
  retain `stream` field).
- **T-ENTRY-TYPES** — `StreamEntry` (chunk timeline) +
  `ResponseEntry` (canonical response) with schema discriminator.
- **T-CHUNKSUB** — `ChunkSubscription` interface + replay impl.
- **T-RINGBUFFER** — Append-only chunk ringbuffer with
  notify-on-append.
- **T-BROKER** — Per-key broker; owns upstream session;
  ref-counted subscribers; no leader concept (D6); cache.Store on
  terminal chunk.
- **T-REGISTRY** — `*streamcache.Registry` global per-key broker
  map; `Subscribe` returns `isFirstSubscriber`.
- **T-AUDIT** — `audit.CacheStatusHitLive` enum;
  `CacheStatusSkipStream` removed (streaming now cacheable).
- **T-PROXY-INTEG** — Phase 5.5 rewrite; both stream and
  non-stream paths consume `ChunkSubscription` via
  `handleStreamWithSubscription` and
  `handleNonStreamWithSubscription`.
- **T-METRICS** — Prometheus counters for cache lookups, writes,
  broker active/subscribers, replay chunks; entry size histogram.
- **T-SMOKE** — VK-driven end-to-end verification on the running
  ai-gateway (`:3050`).

## 3. Acceptance criteria

- All seven functional requirements (FR-RC1..7) implemented and
  covered by unit tests.
- All non-functional requirements (NFR-RC1..5) met or budgeted.
- Existing E28-S6 round-trip golden tests stay green (canonical
  response shape unchanged on the per-subscriber pipeline).
- New unit tests in each of `cache`, `streamcache`, `providers`,
  `audit`, and `handler/proxy` packages.
- VK smoke test passes:
  - First streaming request → `x-nexus-cache: MISS`
  - Repeat streaming request → `x-nexus-cache: HIT`
  - 5 concurrent identical streaming requests → exactly 1 MISS +
    ≥1 HIT_LIVE
  - Non-streaming MISS then HIT
  - `nexus_aigw_cache_*` metrics populated

## 4. Out of scope (this story)

- Bedrock streaming cache — tracked under T-BEDROCK-STREAM in
  E28-S6.
- Streaming embeddings — no streaming wire to cache.
- Per-VK cache TTL overrides — potential follow-up; not needed
  for v1.
- Forward-header allowlist runtime configurability — separate
  workstream; context brief at
  `docs/_archive/2026-q2/brainstorms/2026-05-06-forward-header-allowlist-context.md`.
