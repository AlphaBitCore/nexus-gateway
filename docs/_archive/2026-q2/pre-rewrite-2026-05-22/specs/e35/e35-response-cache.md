# E35 — Response Cache (SSE + non-stream unification)

## Background

Today the AI Gateway response cache (`packages/ai-gateway/internal/cache`)
is **non-streaming only** — `classifyCachePreLookup` returns
`SKIP_STREAM` whenever `isStream=true`. SSE is the production main path
(Anthropic / Claude Code, Cursor, every modern SDK that defaults to
`stream:true`), so the biggest cost lever the gateway has is currently
turned off.

This epic adds streaming cache while upgrading the non-streaming cache
to share the same key formula and broker. The primary outcomes:

1. Streaming requests cacheable end-to-end.
2. In-flight request coalescing (`streamcache.Broker`) so N concurrent
   identical requests collapse to one upstream call.
3. Cross-ingress / cross-alias cache sharing (compute key on the
   bytes that actually leave the gateway, not the raw client body).
4. Hooks always run on every request — including HIT — so rule
   updates take effect with zero invalidation latency.

Full design: `docs/_archive/2026-q2/brainstorms/2026-05-06-sse-cache-design.md`.
Implementation plan: `docs/_archive/2026-q2/brainstorms/plans/2026-05-06-sse-cache.md`.

## Functional Requirements

- **FR-RC1**: Streaming requests on `/v1/chat/completions` and
  `/v1/messages` (and the cross-format variants enabled by E28-S7)
  must hit the cache when an equivalent prior request stored an
  entry. **(Must)**
- **FR-RC2**: N concurrent identical streaming MISS requests must
  collapse to one upstream call via a broker fan-out, with all N
  receiving the same chunk stream in real time. **(Should)**
- **FR-RC3**: Non-streaming and streaming paths must share the cache
  layer, broker layer, and cache key formula. The two paths may
  diverge only at the response-encoding step (JSON vs SSE).
  **(Must)**
- **FR-RC4**: Compliance hooks must run on every request, including
  cache HIT, so rule changes take effect with zero invalidation
  latency. **(Must)**
- **FR-RC5**: Cache key must be computed from the post-`PrepareBody`
  body so equivalent requests across client model aliases (`auto`,
  `gpt-4o`, `gpt-4o-2024-08-06`) and across ingress shapes (OpenAI
  body vs Anthropic body, same upstream model) share entries.
  **(Must)**
- **FR-RC6**: Stream cache HIT must preserve the original chunk
  granularity from the producing upstream call (no "single frame
  with full text"). Replay runs at full speed (no inter-frame
  sleep). **(Must)**
- **FR-RC7**: Non-streaming cache must adopt the same key formula
  and broker; existing G1 alias-collision bug must be fixed.
  **(Must)**

## Non-Functional Requirements

- **NFR-RC1**: Cache HIT latency dominated by Redis fetch + hook
  eval (target: sub-50ms typical, p99 < 200ms).
- **NFR-RC2**: Broker subscription join latency under contention
  must be < 5ms (sync.Map lookup + ringbuffer cursor).
- **NFR-RC3**: Stream entry size cap default 1 MiB; oversized
  streams served live without cache write.
- **NFR-RC4**: TTL unchanged from existing `defaultTTL = 1 hour`.
- **NFR-RC5**: Existing forward-header allowlist (security
  posture) must not change as part of this work.

## User Roles & Personas

- **AI gateway operators** — manage TTL, max entry size, observe
  cache stats via the new Prometheus metrics
  (`nexus_aigw_cache_*`).
- **Customer SDK clients** (OpenAI / Anthropic / Gemini) — consume
  `/v1/*` API; benefit from lower latency on cache HIT and lower
  cost on broker fan-out (transparent — no client change).
- **Compliance officers** — rely on hooks running on every request,
  including HIT, with no rule-update lag.

## Constraints & Assumptions

- Pre-GA: no installed user base; cache schema bump (v1 → v2)
  invalidates existing entries by key prefix; no data migration
  per CLAUDE.md "no backward compatibility" rule.
- Redis is the existing cache backend; no change in this epic.
- E28-S7 cross-format streaming transcoder is the substrate the
  per-subscriber layer uses on cache HIT (replay) and MISS (live).

## Glossary

- **Broker** — per-cache-key in-flight coordinator that owns one
  upstream connection and fans chunks to multiple subscribers.
- **ChunkSubscription** — common interface implemented by both
  replay (HIT) and broker (MISS) subscription, so the downstream
  pipeline is source-agnostic.
- **HIT_LIVE** — audit cache status for a request that joined an
  in-flight broker without triggering an upstream call.
- **PrepareBody** — pure-function part of `Adapter.Execute`:
  cross-format translation + model alias rewrite + parameter
  strip. Promoted to public interface in this work.

## Architecture impact

**No architecture impact.** Internal to the AI Gateway data plane;
no new services, no new MQ topics, no new DB tables, no protocol
surface changes. `docs/users/product/architecture.md` does not require an
update.

## Priority (MoSCoW)

- **Must**: FR-RC1, FR-RC3, FR-RC4, FR-RC5, FR-RC6, FR-RC7
- **Should**: FR-RC2 (single-flight broker)
- **Could**: per-VK cache analytics dashboard (deferred to a
  separate UX epic)
- **Won't (this epic)**: Bedrock streaming cache
  (T-BEDROCK-STREAM tracked under E28-S6); per-VK cache TTL
  overrides; forward-header allowlist runtime config (separate
  workstream documented at
  `docs/_archive/2026-q2/brainstorms/2026-05-06-forward-header-allowlist-context.md`).
