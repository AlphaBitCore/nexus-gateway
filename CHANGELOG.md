# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

_Nothing yet._

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
