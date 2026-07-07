# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed — overload now degrades into retryable 429s (in-flight admission gate)

- The AI Gateway bounds concurrent in-flight proxy requests (default
  `1024 × GOMAXPROCS`; `AI_GATEWAY_MAX_INFLIGHT` overrides, `0` disables). At
  arrival rates beyond the box's capacity, excess requests are rejected fast
  with **429 + `Retry-After: 1`** in the caller's ingress error shape (OpenAI /
  Anthropic / Gemini envelopes) instead of queueing in-heap until the Go memory
  limit collapses throughput (measured pre-fix: 15.9s p99 at 1.5× capacity;
  the pre-GOMEMLIMIT failure mode was an OOM kill). 429 was already part of the
  data-plane contract (per-key rate limits and quota denials); SDK retry logic
  engages unchanged. Health, metrics, and admin endpoints are never gated. Shed
  requests are counted on `nexus_ai_gateway_admission_shed_total`.

### Fixed — hook-config reload stampede at high load

- Hook configuration freshness is now push-driven with a background TTL-backstop
  ticker; the request path never loads configuration. Previously a TTL-stale
  check on the request path could fan out one full rule-pack database load per
  in-flight request while a slow load was running, collapsing the gateway at
  high request rates (measured: p99 120s at 16k req/s with content hooks on;
  fixed: p99 27ms at the same rate). Rule-pack install ordering also gained a
  deterministic tiebreaker so no-change config reloads can no longer churn the
  compiled matchers.

### Performance — content-hook path allocation and CPU

- Bodies-off deployments no longer allocate a fresh request-body buffer per
  request (the pooled buffer is returned at request end; previously measured at
  52% of all gateway allocation under content-scan load).
- Redact-action rule packs skip re-localization entirely on benign traffic
  (zero matches on a complete scan).
- Config snapshot loads expose `nexus_configcache_load_failures_total` and
  `nexus_configcache_last_success_timestamp_seconds` for alerting on a frozen
  config plane.

### Fixed — Request/Response hook timing

- **Streamed responses now record response-hook timing, exactly once per hook.**
  The streaming response pipeline runs the response stage at every checkpoint, so
  the live audit-only path previously recorded nothing (`response_hooks_ms` NULL)
  while the chunked_async path recorded the same hook once per checkpoint (N
  duplicate rows, an N×-inflated aggregate — observed as a "RESPONSE PIPELINE (63)"
  list of identical rows). The trace is now folded to one record per hook (summed
  latency, latest decision) across the ai-gateway live + Model A paths and the
  shared compliance-proxy/agent path. The audit drawer also collapses any residual
  duplicates (historical rows) into a single `×N` card.

### Added — microsecond-precision hook timing (additive, backward compatible)

- Per-hook latency is now measured in **microseconds** (`latencyUs`) alongside the
  existing truncated-millisecond `latencyMs`, with new aggregate columns
  `request_hooks_us` / `response_hooks_us` beside the unchanged `_ms` columns.
  Hooks run at microsecond scale, so the millisecond aggregates floored a
  sub-millisecond hook to `0`; the µs fields carry the real value, surfaced
  precisely per hook in the control-plane audit drawer. The `_ms` columns / wire
  ids / values are unchanged. The new binwire field ids are forward-incompatible,
  so the deploy order is **schema → Hub → producers**.

### Changed (BREAKING — major version bump)
- **Hook `onMatch` collapses to a single `action` (approve | redact | block).**
  The orthogonal `onMatch.inflightAction` (approve / block-hard / block-soft /
  redact) × `onMatch.storageAction` (keep / redact / drop-content) pair is
  replaced by one `action` field across the AI Gateway, Compliance Proxy, and
  Agent. `redact` rewrites the payload (the same masked body is forwarded,
  returned, and stored); `block` rejects and stores the policy attribution
  (matched rule, reason, compliance tags) — not a content body, since a blocked
  request never produces a masked wire copy; `approve` forwards and stores as-is.
  A redact whose adapter cannot reverse-encode the masked content onto the wire
  (`ErrRewriteUnsupported`) fails **closed** (the request/response is rejected,
  not forwarded unredacted). Soft-block (HTTP 246) is removed — block-soft folds
  into block (HTTP 403). The canonical normalized projection is **no longer
  persisted** for audit; the control plane recomputes it at view time from the
  (already-redacted) raw body, so `request_normalized` / `response_normalized`
  and `request_redaction_spans` / `response_redaction_spans` are no longer
  emitted.
  **Migration:** the config reader maps the legacy keys for a deprecation window
  (one-shot warning); a one-off data migration
  (`tools/db-migrate/manual-scripts/migrate_hook_onmatch_action_2026_06_22.sql`)
  rewrites stored `HookConfig.config.onMatch` rows:
  `block-hard|block-soft → block`, `redact → redact`,
  `approve + keep → approve`, `approve + redact|drop-content → redact`.
  Runtime enforcement is unchanged by the mapping: `block-soft` already **rejected**
  the request — it returned an error response (previously with the non-standard
  status 246, now 403) and never forwarded the traffic, so this is a status-code
  change, not an allow→deny change. The only data-level behavior change is
  `approve + redact|drop-content → redact`, which upgrades a storage-only redact to
  a full redact (the compliance-safe direction, never less masked than before) and
  occurs in no current row, so the live migration is lossless. Client note: any SDK
  that branched on the soft-block status 246 must now treat such a rule's response
  as a 403 reject. The Agent signals a block by dropping the
  connection (no rich error body); the proxies return an attributed 403 whose
  response-stage reason carries rule-ID labels only, never the upstream value.
### Fixed — co-firing redact + soft-block no longer drops the redaction (security)

- **A redact hook co-firing with a soft-block hook now masks-and-delivers instead of
  leaking or failing closed.** When a redact hook (`Modify` + masked content/spans) and
  a soft-block hook fired on the same request or response, the pipeline aggregator
  promoted the reported `Decision` to `BlockSoft` (the strictest) but DROPPED the redact
  hook's replacement content, leaving spans without content. Downstream this produced a
  no-op rewrite that, depending on the path, either failed closed (canonical response)
  or replayed/forwarded the ORIGINAL unredacted body — a PII leak on the shared buffer
  pipeline (compliance-proxy appliance included), the agent Model A wire, and both
  request stages. `mergeResults` now carries the redact's `ModifiedContent`
  unconditionally, and every redaction consumer gates on the new
  `decision.CompliancePipelineResult.CarriesRedaction()` predicate (Modify OR a
  BlockSoft masking a co-firing redact) rather than `Decision==Modify`, so the masked
  body is applied and delivered on all paths. The audit row stamps the disposition
  `action=redact` even when the (soft-block) `Decision` ceiling is `BlockSoft`. No
  config or schema change; behavior is compliance-safe (a hard `block`/`RejectHard`
  still rejects; a standalone soft-block still delivers-with-warning). The no-redactor
  buffer degrade is now posture-aware (appliance fail-closed, agent fail-open).

### Changed — three-end streaming-compliance parity via a shared Model A engine

- **The Model A streaming-compliance algorithm is now a single shared engine driving
  three ends.** The prescan-gated real-time streaming path (bounded tail-hold +
  union prescan + confirm + escalate-to-buffer redaction) for a redact-scope
  `chunked_async` stream is extracted into a substrate-agnostic engine
  (`shared/transport/streaming/modela`). The AI Gateway drives it with a canonical
  substrate (fail-closed) and the transparent proxy used by the Agent +
  Compliance Proxy drives it with a raw-SSE-wire substrate (fail-open, NE
  host-packet safety) — so hooks/compliance behave identically across all three
  ends while each keeps its own ingress and delivery. The transparent-proxy live
  path becomes **audit-only** (real-time write-through, observe-only checkpoints,
  never blocks/rewrites): scope-derived routing sends a `block` scope to buffer and
  a `redact` scope to Model A (or buffer), so only non-enforcing traffic reaches
  live. The adoption also closed two latent PII-leak paths in the shipped AI Gateway
  Model A (a redact masked behind a co-firing soft-block; a memory-pressure eviction
  of an incomplete content unit). No config or contract change; behavior is
  compliance-safe (a sub-window value is never delivered raw; storage never persists
  a raw prefix on an enforcing outcome).

### Changed — normalized projection is now fully view-time (no migration required)

- **The normalized traffic projection is no longer written on the hot path; it is
  recomputed at view time.** Building on 1.1.0 (where the producers stopped
  stamping it), this completes the move end-to-end: the Hub no longer
  self-derives the projection from agent uploads, and the periodic
  **normalize-backfill job is retired**. The Control Plane (and the Agent
  dashboard) recompute the normalized request/response on demand — when an
  operator opens a Traffic detail drawer — from the stored, already-redacted
  body, so the rendered projection always reflects the current decoder version
  with no scheduled job and no stored copy to drift.
  - **`traffic_event_normalized` and `traffic_event_normalize_skip` are retained,
    write-frozen.** No schema change and **no migration is required.** The
    `traffic_event_normalized` sidecar still receives a row only when an older
    shipped agent uploads its own governed normalized copy — for a block/redact
    row whose raw body was dropped, that uploaded copy is the sole forensic
    record. The `traffic_event_normalize_skip` ledger is now inert (the job that
    wrote it is gone). Dropping both tables is a planned deprecation-window
    follow-up, not part of this change.
  - **`GET /api/admin/traffic/{id}/normalized`** now returns the recompute and no
    longer includes redaction spans (the recompute reads an already-redacted
    body). It returns `404` when the projection is unavailable — no stored body
    to recompute from (payload capture was off, or a spilled body has aged out of
    retention) and no stored sidecar fallback.
  - **Operators:** the `nexus_normalize_backfill_*` counters are no longer
    emitted. A missing/NULL `traffic_event_normalized` sidecar is now the normal
    state for current traffic, not a gap to heal.

### Changed — streaming-compliance enforcement (config-compatible, no migration)

- **Streaming response compliance is scope-routed, and the real-time path is
  audit-only.** A response hook's enforcement scope decides how a streamed (SSE)
  response is handled, overriding the admin streaming-mode default wherever that
  default cannot enforce:
  - A **block** scope buffers the full response before any byte is delivered
    (zero-leak hard block).
  - A **redact** scope under `chunked_async` streams in real time behind a prescan
    gate that holds a bounded trailing window and escalates to buffered redaction on
    a confirmed match — best-effort on the wire: a complete sensitive value is never
    delivered, but a leading fragment of a value longer than the window may reach the
    client before redaction engages, while the persisted audit copy stays fully
    masked within that window. A redact scope under `passthrough` falls back to
    buffering rather than forwarding raw.
  - A **non-enforcing** pipeline streams in real time, audit-only: it scans and tags
    every checkpoint but never blocks or rewrites the wire.
  - An **unbuildable fail-closed** response hook forces buffering, which fails closed
    with an in-band error frame — never a silent fail-open on the real-time path.
- **The streamed `finish_reason` is preserved** across the canonical re-encode
  instead of collapsing to `stop`.
- The `streaming_compliance.config` mode enum (`passthrough` / `buffer_full_block` /
  `chunked_async`) is unchanged; no migration. The Control Plane UI shows an
  always-visible per-mode disclosure of exactly what each mode enforces.

## [1.1.0] — 2026-06-28

The first release after the 1.0 GA. It is a **performance and audit-storage**
release: the captured-traffic pipeline was reworked to push far higher no-loss
throughput on a single box, several shipped defaults flip toward that
throughput, the **Windows desktop agent reaches GA**, and the AWS Marketplace
AMI / single-instance appliance form factor is now a first-class deployment
target.

> **Upgrade note.** Two changes are breaking **for direct database / config
> consumers** and require a one-time migration on deployments that retain
> traffic history (see **BREAKING (migration required)**, below). Fresh
> installs — the AMI appliance, or `prisma db push` against an empty database —
> need no manual step. The supported appliance upgrade path applies the schema
> change automatically, which is why this ships as a minor rather than a major;
> the data re-encode is the only manual action, and only when old rows must
> remain readable.

### Changed — BREAKING (migration required for existing deployments)

- **Captured body storage is now raw `BYTEA`.**
  `traffic_event_payload.inline_request_body` / `inline_response_body` hold the
  captured body's **raw bytes** (text verbatim, arbitrary binary, or a raw
  `zstd` / `s2` compressed frame), discriminated by the
  `inline_request_encoding` / `inline_response_encoding` columns
  (`text` | `binary` | `zstd` | `s2`, with `base64` accepted as a read tag).
  Raw bytes let PostgreSQL store the body as-is — no per-insert parse / validate
  / tree-store, and no +33% base64 size inflation.
  - **Direct `traffic_event_payload` consumers:** read the `inline_*_body`
    column together with its `inline_*_encoding` discriminator and decompress
    accordingly, instead of parsing the old JSONB envelope.
  - **Migration:** `prisma db push` applies the `TEXT` → `BYTEA` column change.
    Rows captured before the upgrade whose encoding is `zstd` / `s2` were stored
    as base64 text; their bytes survive the type swap as base64 ASCII and must
    be decoded once to the raw frame, or they read as absent:
    `UPDATE traffic_event_payload
       SET inline_request_body = decode(convert_from(inline_request_body,'UTF8'),'base64')
     WHERE inline_request_encoding IN ('zstd','s2');`
    (and the same for `inline_response_body` / `inline_response_encoding`).
    Old `base64`-tagged rows decode transparently on the read path. The
    authoritative note lives in `tools/db-migrate/schema/traffic.prisma`
    (model `traffic_event_payload`).

- **Hook `onMatch` collapses to a single `action` (`approve` | `redact` |
  `block`).** The orthogonal `inflightAction` × `storageAction` pair is replaced
  by one field across the AI Gateway, Compliance Proxy, and Agent: `approve`
  forwards and stores as-is; `redact` rewrites the payload (the same masked body
  is forwarded, returned, and stored); `block` rejects and stores the masked
  copy. The soft-block path folds into `block`. The canonical normalized
  projection is **not persisted** for audit — the control plane recomputes
  it at view time from the (already-redacted) raw body — so
  `request_normalized` / `response_normalized` and the
  `request_redaction_spans` / `response_redaction_spans` columns are not
  emitted.
  - **Migration:** the config reader maps the legacy
    `inflightAction` / `storageAction` keys for a deprecation window (one-shot
    warning), and the one-off data migration
    `tools/db-migrate/manual-scripts/migrate_hook_onmatch_action_2026_06_22.sql`
    rewrites stored `HookConfig.config.onMatch` rows
    (`block-hard|block-soft → block`, `approve + keep → approve`,
    `approve + redact|drop-content → redact`). The proxies return an attributed
    `403` whose response-stage reason carries rule-ID labels only, never the
    upstream value; the Agent signals a block by dropping the connection.

### Changed — defaults (overridable, no migration required)

### Changed (defaults — overridable, no migration required)
These flip shipped behavior toward higher throughput; each is overridable by env
or yaml and an upgrade silently inherits the new default. Operators relying on the
prior strictness should set the opt-out shown.
- **Quota enforcement is soft by default (`NEXUS_QUOTA_WRITE_BEHIND` ON).** Per-
  request quota cost is accumulated in-process and flushed to Redis on a 250ms
  interval behind a 1s read cache, instead of a synchronous per-request Redis
  round-trip. Overshoot per instance ≤ ~1.25s of spend; across an N-instance fleet
  the blind-spend window is that × N, and a hard kill loses un-flushed increments
  (graceful shutdown drains). Opt out: `NEXUS_QUOTA_WRITE_BEHIND=0` (strict
  synchronous per-request accounting).
- **Credential-stats write-behind ON by default (`NEXUS_CREDSTATS_WRITE_BEHIND`).**
  Credential usage counters defer off the request path; circuit-breaker
  transitions stay synchronous. Opt out: `NEXUS_CREDSTATS_WRITE_BEHIND=0`.
- **Audit overflow default `AI_GATEWAY_AUDIT_LOSS_MODE=spill`.** The request path no
  longer back-pressures on a full audit pipeline; overflow spills to a durable
  on-disk spool replayed to Postgres. No loss until the spill channel + disk
  saturate; sustained overload past that drops records, counted on `dropped_total`.
  Opt out for strict no-drop back-pressure: `AI_GATEWAY_AUDIT_LOSS_MODE=block`.
- **`NEXUS_EVENTS` audit stream is in-memory by default (`NEXUS_EVENTS_STORAGE=memory`,
  `DiscardNew`, cap `NEXUS_EVENTS_MAX_BYTES=auto` = 15% RAM).** Keeps the
  delay-tolerant burst buffer off the data disk. A NATS broker restart/crash drops
  published-but-undrained events (the overflow→disk no-loss path covers only the
  stream-full case). Opt out for a durable file-backed stream:
  `NEXUS_EVENTS_STORAGE=file`.
- **`GOMEMLIMIT` auto-set from the cgroup limit when unset.** Each service, if
  `GOMEMLIMIT` is not provided, reads the cgroup memory limit at boot and sets the
  Go soft limit to ~70% of it (logging a WARN with the value), leaving it unset
  when no cgroup limit is detectable. Pin explicitly to override.
- **Cache freshness protection defaults ON (`extract_cache_config.apply_freshness_rules`
  default `false → true`).** Freshness protection is intrinsic to caching: enabling a
  cache tier should not silently replay a stale time-sensitive answer (today's date,
  "latest" prices, live status). The freshness detector only runs when a cache tier is
  active, so a cache-off gateway still pays nothing and stays a lean passthrough. The
  flip applies to fresh installs and the no-row default; an existing deployment that
  already saved an `extract_cache_config` row keeps its stored value, so **no migration
  runs and no admin choice is overwritten**. Operators who already enabled L1/L2 and
  want freshness should re-save the extract-cache config (or toggle the Freshness rules
  card) once; operators who want maximum hit-rate can leave it off explicitly.
Each default below flips shipped behavior toward higher throughput. An upgrade
silently inherits the new value; the opt-out to restore prior behavior is shown.

- **One same-target retry by default** (`maxAttemptsPerTarget` 1 → 2). A single
  transient upstream fault (network / timeout / 429 / 5xx) now retries once in
  place before failover, so flaky provider endpoints self-heal instead of
  surfacing a hard error. Bounded to one retry so a non-idempotent generation is
  re-sent at most once. Opt out: set `maxAttemptsPerTarget: 1` on the routing
  rule / retry policy.
- **Audit overflow defaults to `spillblock` (zero-loss).** The request path does
  not back-pressure on a full audit pipeline; overflow spills to a durable
  on-disk spool, and when the spool channel itself saturates the writer
  back-pressures rather than dropping. Opt out:
  `AI_GATEWAY_AUDIT_LOSS_MODE=spill` (drop on saturation) or `=block` (strict
  synchronous back-pressure on the request path).
- **Quota enforcement is soft by default** (`NEXUS_QUOTA_WRITE_BEHIND=1`).
  Per-request quota cost accumulates in-process and flushes to Redis on a ~250ms
  interval behind a 1s read cache. Overshoot per instance ≤ ~1.25s of spend; a
  hard kill loses un-flushed increments (graceful shutdown drains). Opt out:
  `NEXUS_QUOTA_WRITE_BEHIND=0`.
- **Credential-stats write-behind by default**
  (`NEXUS_CREDSTATS_WRITE_BEHIND=1`). Credential usage counters defer off the
  request path; circuit-breaker transitions stay synchronous. Opt out:
  `NEXUS_CREDSTATS_WRITE_BEHIND=0`.
- **`NEXUS_EVENTS` audit stream is in-memory by default**
  (`NEXUS_EVENTS_STORAGE=memory`, `DiscardNew`, cap `NEXUS_EVENTS_MAX_BYTES=auto`
  ≈ 15% RAM). Keeps the delay-tolerant burst buffer off the data disk; a NATS
  restart/crash drops published-but-undrained events. Opt out for a durable
  file-backed stream: `NEXUS_EVENTS_STORAGE=file`.
- **Response cache is opt-in per route, with substring freshness matching.**
  Caching is enabled per route rather than globally; turn it on for the routes
  that benefit. The Control Plane UI surfaces the staleness risk tip.
- **`GOMEMLIMIT` auto-set from the cgroup limit when unset.** Each service reads
  the cgroup memory limit at boot and sets the Go soft limit to ~70% of it
  (WARN-logged), leaving it unset when no cgroup limit is detectable. Pin
  explicitly to override.
- **Seed defaults:** content hooks ship **OFF**, and the application virtual key
  carries a default **$50k/month** quota policy.
- **Inline-body audit codec defaults to `s2`** (`AI_GATEWAY_AUDIT_CODEC`, `zstd`
  available); the CGO matcher scan limit auto-sizes (`NEXUS_CGO_SCAN_LIMIT=auto`).

### Changed — audit transport (internal, no shipped-contract break)

- **gw→Hub audit wire defaults to a binary TLV frame**
  (`NEXUS_AUDIT_WIRE=binary`). The Hub peeks the frame magic and dual-reads, so
  the legacy JSON wire still decodes; `NEXUS_AUDIT_WIRE=json` reverts. No
  persisted-contract or external API change.

### Added

- **Windows desktop agent is now GA.** Windows interception runs on a signed
  `NexusWFP` kernel driver (Windows Filtering Platform, transparent TCP
  connect-redirect, with QUIC fallback and IPv6). macOS, Linux, and Windows
  desktop agents are all GA.
- **AWS Marketplace AMI / single-instance appliance.** `nexus-ami/` bakes the
  binaries, UI, Prisma, nginx, PostgreSQL, Valkey, and NATS into one AL2023
  image via Packer, with Vectorscan compiled on-instance and the rig-validated
  audit-write defaults shipped in. See `nexus-ami/README.md` and
  `docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md`.
- **Vectorscan-backed hook pattern matching** with an edit-time pattern
  performance test in the Control Plane (governance) so admins see a rule's scan
  cost before saving.
- **Semantic vector cache tiering** — the L1 exact-match extract and L2 semantic
  lookup are now independent tiers.
- **On-demand profiling** — a `NEXUS_PPROF_ADDR` pprof endpoint on all four
  services plus SIGUSR1 file dumps that include Go `MemStats`.
- **Typed error banner** for non-200 rows in the audit drawer.

### Performance

- **COPY-based bulk insert** for `traffic_event` / `traffic_event_payload`,
  with a row-backing pool to cut per-batch allocations.
- **Adaptive memory/disk self-tuning** of the audit pipeline: lossless
  spill-recovery, backlog-aware drain, batched spill with geometric growth, and
  a lazy-canonical default.
- **Hook scan** folds each hook's raw-body prefilters into one union scan, caps
  wide repeats in the detection database, and ships an AVX-512 build flag.
- **Lower allocation on the audit/alert hot paths** — lock-free precomputed
  alert dispatch, zero-copy pooled slim decodes, and typed identity/detail
  structs replacing map reflection.
- **Dropped 7 rarely-read `traffic_event` indexes** to cut ingest
  write-amplification.

### Fixed

- View-time normalization uses the **ingress** wire format rather than the
  upstream adapter format, so the audit drawer renders the request as the client
  sent it.
- Routing-strategy filter lists all canonical strategies with labels.
- Dashboard number formatting — token B/T tiers and cost separators.
- Governance pattern-performance endpoint returns `[]` rather than `null`.

### Removed

- The in-tree load generator (`tools/loadtest`) was extracted to the standalone
  `nexus-loadtest` repository.

### Fixed (gateway response cache correctness)
- **Emergency cache master kill switch is now wired into the data plane.**
  `cache_master_kill_switch` (the Tier-1 global cache config) was parsed but never
  consulted by the AI Gateway, so flipping it did nothing. It now gates both gateway
  response cache tiers — L1 exact-match and L2 semantic — at the cache stage
  (`cacheEnabled = (l1||l2) && !cache_master_kill_switch`). It does not disable
  provider-side prompt caching (Anthropic markers / Gemini context cache), which only
  makes the upstream cache and never serves a stored gateway response.
- **L1 exact-match cache fills regardless of the `cache.broker` flag.** With
  `cache.broker=false` (the default) the broker registry was never constructed and the
  broker pump is the cache's sole writer, so an admin-enabled L1 tier silently never
  filled (0% hit rate). The registry is now always constructed; `cache.broker` controls
  only same-key in-flight dedup (coalesce concurrent same-key MISSes onto one upstream
  call vs. independent calls) — either way the cache fills.
- **L1 cache no longer serves cross-VK entries during the boot window or on
  Sentinel/Cluster Redis.** L1 folds the fleet `vary_by` isolation scope into its cache
  key, but that scope arrives on the semantic-cache config push. Before the first push
  the scope was unset (fleet-wide), so an entry written in that window could be read by
  a different virtual key; and on Sentinel/Cluster Redis the semantic config was never
  delivered to the gateway at all. L1 now fails closed (no lookup/store) until the fleet
  config has loaded, and the config snapshot (including `vary_by`) is delivered on every
  Redis topology — decoupled from the `*redis.Client`-only index lifecycle.

## [1.0.0] — 2026-06-14

First general-availability release. All three intercept planes (AI Gateway,
Compliance Proxy, Desktop Agent) and the full architecture — Hub Thing/shadow
model, control plane + UI, compliance/audit pipeline, provider-adapter
framework — are production-complete. macOS + Linux desktop agents are **GA**
(Windows experimental).

### Added

- **Desktop Agent AI-chat capture (macOS + Linux GA).** End-to-end interception
  and structured normalization of AI-chat traffic — codex (OpenAI Responses on
  chatgpt.com), Cursor (app + `cursor-agent` CLI via
  `/agent.v1.AgentService/Run`), and browser web-chat — into the audit /
  `traffic_event` pipeline without breaking the tools. macOS uses the
  `NETransparentProxyProvider` system extension as the sole intercept path.
- Cursor connect-RPC decoder: per-frame gzip-decompressed agent-service frames
  decode embedded OpenAI-compat / Lexical JSON into structured conversation +
  model + readable tool calls.
- AI vibe-coding documentation surface (`docs/developers/workflow/ai-workflow.md`,
  `docs/developers/workflow/ai-skill-catalog.md`).
- Two binding lints with HARD pre-commit + strict CI gates:
  `check-no-prod-todos.mjs` and `check-no-yaml-secrets.mjs`; reverse-grep
  detection in `check-no-redis-pubsub.mjs`.
- `.github/ISSUE_TEMPLATE/` and `.github/CODEOWNERS`.

### Changed

- `useapi-querykey` and `no-redis-pubsub` lints ratcheted from warn-only to HARD
  pre-commit + strict CI.
- Streaming-policy three-service alignment: all three data planes load the
  streaming-policy snapshot from the Hub-pushed `streaming_compliance.config`
  shadow; an unreadable snapshot at boot resolves to `passthrough`
  (`DefaultPolicy()`) rather than a hard-coded YAML value.
- `MQBatchWriter.Flush()` coordinates with the writer loop so all pending events
  are drained, including those moved into the loop's private buffer.

### Fixed

- **`traffic_event` requested-vs-routed semantics.** REQUESTED columns
  (`model_id` / `provider_id` / `provider_name`) mean what the client asked for
  and are NULL when the request did not pin a single catalog model; the
  `routed_*` columns carry what actually served, and all usage / cost /
  analytics attribute by the routed side. Direct consumers reading `provider_id`
  / `model_id` as "what served" should switch to `routed_*`.
- Connect-RPC envelope flags (`0x01` per-message gzip vs `0x02` end-of-stream)
  are decoded distinctly, fixing Cursor `/agent.v1.AgentService/Run` capture.
- Cursor host interception is chat-only — `*.cursor.sh` passes through by
  default and captures only chat-bearing paths.
- `docker-compose.yml` Postgres credentials honor `${POSTGRES_*}` overrides.

---

## How releases work

Shipped work accumulates under `Unreleased`; at each release cut the section is
renamed to `[X.Y.Z] — YYYY-MM-DD` and a fresh `Unreleased` opens above it. Each
release mirrors the structure above
(`Added` / `Changed` / `Performance` / `Fixed` / `Removed` / `Deprecated` /
`Security`).

Versioning policy:

- **Major** — a breaking change to a shipped contract (public/admin API,
  routing-rule schema, `traffic_event_*` tables, agent↔Hub wire) with **no
  in-place migration path**: a re-architecture an existing deployment cannot
  follow without rework.
- **Minor** — new features, performance work, and schema changes that ship with
  an automated migration, **even when direct database consumers must adapt** —
  those adaptations are called out per entry under "BREAKING (migration
  required)".
- **Patch** — bug fixes, docs, and lint changes.
