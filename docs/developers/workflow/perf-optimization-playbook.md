# Performance Optimization Playbook

> Distilled from the AI Gateway performance programs (99 shipped `perf(...)` commits,
> 2026-04 → 2026-07). This is the **audit checklist** used when hardening any Nexus
> service. Each technique carries the gw precedent that proved it, the test for when
> it applies, and the proof required before it ships.
>
> Companion documents:
> - `docs/developers/architecture/services/ai-gateway/performance-and-switches-explained.md`
>   — measured results and the switch cost model.
> - `docs/handoffs/perf-loadtest-protocol-and-tooling.md` — the load-test protocol
>   and rig facts.
> - `docs/developers/specs/perf-ai-gateway-hotpath.md` — the original phase plan.

---

## 0. Binding rules

These govern every optimization in this repository, not just gw ones.

1. **Correctness is the precondition, not a trade.** An optimization that changes
   observable behavior is a defect, regardless of its speedup. Prove functional
   equivalence first (unit test, differential gate, or golden comparison), then
   measure. A faster wrong answer scores zero.
2. **Every optimization carries its own measurement.** A Go benchmark reporting
   `ns/op` **and** `allocs/op` on the specific path changed, or a profile diff from
   a real load run. Reasoning alone is not evidence — several plausible items in the
   gw program measured as *no win* and were dropped (see §14).
3. **No item is too small to do, and none is dropped silently.** Small wins
   accumulate. Every audited gap reaches a terminal state: shipped-with-measurement,
   or rejected with a *measured* reason.
4. **The side path must never touch the core path.** Audit, telemetry, metrics
   rollup, and spill are side paths. They may run slowly, buffer, back-pressure, or
   degrade — but a saturated side path must not add latency to the request path.
   This is the single most load-bearing architectural rule in the gw programs.
5. **Defaults must be lean; cost is opt-in.** Every switch that adds per-request
   work defaults OFF unless it is a compliance necessity. See §13.

---

## 1. Prerequisites: you cannot optimize what you cannot measure

| Item | Anchor | Applies when |
|---|---|---|
| pprof wiring | `packages/shared/core/profiling` — `profiling.Start("<service>")`, master switch `NEXUS_PPROF_ENABLED`, SIGUSR1 file dump to `NEXUS_PPROF_DIR` (Unix only — Windows has no such signal and uses `NEXUS_PPROF_ADDR` instead), optional loopback HTTP on `NEXUS_PPROF_ADDR` | **Every long-running service.** Wired in ai-gateway, compliance-proxy, nexus-hub **and the agent**. |
| Go soft memory limit | `packages/shared/core/runtimemem` — `AutoSetMemoryLimit` derives `GOMEMLIMIT` from the cgroup limit when unset | Every service that can see a load burst. |
| Package-level benchmarks | `go test -bench . -benchmem` | Every hot function you intend to change. |

**Rule:** if a service has neither pprof nor benchmarks, wiring them is task #1 of its
optimization program — not an afterthought.

**Confirm the endpoint belongs to the process you think it does.** `NEXUS_PPROF_ADDR` on a port
another process already holds used to log `pprof http listening` anyway — so the address answered,
from the wrong process, with a log line agreeing it was ours. `startHTTP` now binds before it claims
and logs an ERROR on failure, but the operator-side habit still applies: `lsof -nP -iTCP:<port>
-sTCP:LISTEN` before reading a number off a profile. Two further measurement rules learned the hard
way in the same session: read heap with `?gc=1` (without it `HeapInuse` includes uncollected
garbage, which reads as a 2–3 MB "idle leak" that is not there), and distinguish a leak from a pool
by running **several identical waves** — a connection pool plateaus, a per-request leak grows with
every wave.

**Profile at the collapse, not before it.** The gw's streaming collapse was
misdiagnosed as lock contention from a goroutine dump alone; a CPU profile captured
*during* the 98%-CPU window showed the real cause was per-frame JSON parsing, and the
lock waits were a symptom of CPU starvation. Goroutine dumps show *where goroutines
are parked*; only a CPU profile shows *what is burning the CPU*.

---

## 2. Allocation and buffer reuse

The single most productive family in the gw program.

### 2.1 Pool the per-request buffers

| Precedent | Anchor |
|---|---|
| Pool the captured request body (#1 hot-path allocator) | `ai-gateway/internal/platform/audit/record_bodypool.go` |
| Pool per-stream SSE buffers + zero-copy line scan | `ai-gateway/internal/providers/specutil/sse.go` |
| Pool the streaming-capture tee buffer with terminal reclaim | `perf(ai-gateway)` 主路 #5 |
| Pool the NDJSON audit frame buffer (−8.7 GB alloc) | `ai-gateway/internal/platform/audit/writer_batch.go` |
| Pool COPY row backing to cut per-batch `[]any` allocs | `nexus-hub` consumer |
| Pre-grow the read scratch to the real payload size | `perf(proxy)`: 128 KiB request-body scratch |

**Applicability test:** the buffer is allocated per request/per connection/per frame,
and its size is predictable. Confirm with `-benchmem` or a heap profile that it is
actually a top allocator before pooling — pooling a cold allocation adds complexity
for nothing.

**Risk:** use-after-return. Every pooled buffer needs a single, provable release
point. Where lifetime escapes the request (audit bodies outliving the response), the
gw decoupled the lifetime explicitly rather than pooling naively
(`perf(audit): decouple captured-body lifetime from publish-ack`).

### 2.2 `sync.Pool` vs a GC-stable ring — know which one

**`sync.Pool` is the wrong primitive for objects that are expensive to recreate.** The
Go runtime clears every `sync.Pool` on each GC. Under steady load the box GCs hundreds
of times, so the pool is repeatedly emptied and every miss pays full construction cost.

Two gw sites hit this and switched to a **bounded buffered channel** (a "GC-stable ring"):

- `packages/shared/policy/hooks/matcher/vectorscan.go` — Vectorscan scratch spaces.
  Recreating one is `hs_alloc_scratch`, a >20 µs cgo call that parks the M.
  Ring size 256; a spike beyond cap allocates transiently rather than blocking.
- `packages/shared/audit/compress.go` — zstd encoders. A `sync.Pool` miss meant a
  multi-MB window allocation, measured at ~5 GB/run. Ring size 64.

**Decision rule:**

| Object | Primitive |
|---|---|
| Plain `[]byte` / `bytes.Buffer` — cheap to recreate | `sync.Pool` |
| cgo handles, compressors, anything with a multi-µs or multi-MB construction cost | bounded buffered channel, sized to the concurrency fan-out |

**Corollary:** a shared single instance is also wrong when the library serializes
internally. `perf(audit): pool single-concurrency zstd encoders, not one shared
encoder` — one shared `zstd.Encoder` bottlenecked all marshal workers on its internal
state pool, measured as a drain collapse.

### 2.3 Eliminate copies, don't just make them cheaper

| Precedent | Mechanism |
|---|---|
| `Body.MarshalJSON` verbatim splice | Splice pre-encoded bytes into the output instead of decode→re-encode |
| Eliminate audit `marshalRecord` copy-out via buffer-hold | Hold the buffer instead of copying it out |
| Zero-copy pooled slim decode for alerteval | Decode into a pooled view struct, no intermediate |
| SSE bodies ride the wire as escaped text, not base64 | Avoid a +33% expansion and the encode cost |
| Store inline bodies as raw BYTEA | Same, at the storage layer |

**Applicability test:** trace the byte path end to end and count how many times the
same payload is materialized. Every re-materialization is a candidate.

---

## 3. Parse avoidance and lazy work

The highest-leverage family after buffer reuse, because the cheapest parse is the one
that never runs.

| Technique | Precedent |
|---|---|
| **Cheap-reject before the expensive parse** | `perf(hooks): fold per-hook raw-body prefilters into one union scan` — scan raw bytes with an anchor-stripped superset of every rule; on no match (the common case) skip structured extraction entirely |
| **Skip when the consumer is absent** | `skip stream PreHook normalize when no response hook` (S3); `skip no-op audit checkpoints when no response hooks bound`; `streaming model-rewrite all-skip` (S5) |
| **Skip on a null/empty discriminator** | `skip per-chunk usage normalize on null usage` (S4) |
| **Fast path with full fallback** | `slim usage extraction fast-path with full-Normalize fallback` (gw#1); `redact fast path` |
| **Lazy materialization** | `lazy canonical + lazy audit normalize` — compute at view time, not write time |
| **Typed struct instead of map/reflection** | `typed-struct stream encoder, drop per-frame map alloc`; `typed identity/details structs replace map reflection`; `narrow alert decode to compiler-enforced AlertView` |
| **Delete the gate that buys nothing** | `drop the passthrough json.Valid gate — malformed is the client's problem`; `delete field-order cache-key normalization` |
| **Cheaper primitive for the same verdict** | `replace goccy json.Valid with zero-alloc stdlib`; freshness `(?i)` regex → pre-lowercased `strings.Contains` (~1000× on large bodies) |

**Applicability test:** for each parse/scan on the hot path, ask *who consumes the
result, and is that consumer present on this request?* If the consumer is conditional,
the work must be conditional too.

**Risk:** a skip predicate that is subtly wider than the consumer's need silently
changes behavior. Every skip needs a differential test proving identical verdicts
across the skip boundary — the gw gated the freshness and rulepack rewrites behind
exactly such gates.

---

## 4. Locks and contention

| Technique | Precedent |
|---|---|
| **Shard by key** | `perf(ai-gateway/ratelimit): shard the in-process limiter so distinct keys don't contend` |
| **Async single-writer** | `perf(ai-gateway): remove HealthTracker global mutex via async single-writer` — a process-global mutex taken on *every* upstream response; replaced by a channel to one writer goroutine |
| **Lock-free precomputed table** | `perf(alerts): lock-free precomputed dispatch table` |
| **Refcount instead of mutex** | `vectorscan.go` — Scan refcounts lock-free; Close drains in-flight before freeing. Live config swap with no external synchronization |
| **Release the lock across the slow call** | `perf(nexus-hub/consumer): release batch lock during flushFn`; `perf(broker): release Registry.mu across leaderFn` |
| **Collapse the thundering herd** | `perf(ai-gateway/vertex): collapse the OAuth token-mint thundering herd (per-key mint lock)`; `perf(hooks): move hook-config TTL refresh off the request path` |
| **Bound cgo concurrency** | `perf(matcher): bound concurrent cgo scans (NEXUS_CGO_SCAN_LIMIT)` — unbounded cgo parks Ms |

**The stampede pattern is worth memorizing.** `bbb0e4d04`: a per-request TTL staleness
check meant that while one slow reload was in flight, *every* arriving request issued
its own duplicate load — 89% of 54k goroutines parked in that path, 10.5 GB of
duplicated payloads, p99 at 120 s. The fix: the resolver became a pure snapshot getter
(zero locks, zero loads), with the TTL backstop moved to a serial background ticker.

**Applicability test:** any mutex taken on a per-request basis with a process-global
scope is a candidate. Any "check staleness then maybe reload" on a request path is a
stampede waiting for a rate high enough to trigger it.

**Diagnosis caveat:** heavy `[sync]` waits in a goroutine dump may be a *symptom* of
CPU saturation elsewhere, not the cause. Confirm with a CPU profile before optimizing
a lock (§1).

---

## 5. Connection and resource pooling

| Resource | Anchor | Notes |
|---|---|---|
| Upstream HTTP | `ai-gateway/internal/providers/specutil/http.go` — `MaxIdleConns`, `MaxIdleConnsPerHost`, `MaxConnsPerHost`, `IdleConnTimeout`, all config-driven with defaults | `MaxConnsPerHost` is a **hard cap on total (active+idle)**; a silently-low value throttles streaming concurrency. The gw's default 5000 was capping streams before anyone noticed |
| Postgres | `ai-gateway/cmd/ai-gateway/wiring/db.go` — `MaxConns`/`MinConns`/`MaxConnLifetime` from config | |
| Redis | `packages/shared/storage/redisfactory` — `REDIS_POOL_SIZE`, `REDIS_MIN_IDLE_CONNS`, `REDIS_POOL_TIMEOUT` | `perf(redis)`: poolSize 10→200, minIdleConns 0→50 across four services |
| NATS | `packages/shared/transport/mq/producer.go` — a **pool of connections** (`MQ_NATS_PUBLISH_POOL_SIZE`, default 24), not one | A single NATS connection serializes publishes |

**Applicability test:** every outbound dependency has a pool; find the one that was
left at the library default. A single shared `http.Transport` also has an *internal*
mutex — at ~17k concurrent upstream connections that mutex itself becomes the
bottleneck, and the remedy is sharding the transport, not enlarging the pool.

**Risk:** enlarging a pool moves the bottleneck rather than removing it, and can
convert a fast failure into a slow one. Always pair a pool increase with the
measurement that shows where the queue actually formed.

---

## 6. Async side path and back-pressure

The gw's audit pipeline is the reference implementation of "expensive work that must
not touch the core path".

| Technique | Precedent |
|---|---|
| Enqueue raw bytes; do all encoding off-path | Request handling enqueues bytes only; zstd + marshal + normalize run on async workers |
| Bound the in-heap queue | `perf(audit): bound the in-heap queue + move overflow spill off the request path` |
| Non-blocking overflow | `perf(audit): spill-mode overflow is fully non-blocking on the request path` |
| Back-pressure instead of silent drop | `NEXUS_PERF_SPILL_BLOCK` → first-class `lossModeSpillBlock`; `perf(mq): NEXUS_EVENTS DiscardNew + configurable cap so NATS never silently drops audit` |
| Lossless spill recovery + backlog-aware drain | `perf(audit): lossless spill-recovery + backlog-aware drain` |
| Adaptive drain duty cycle | `perf(hub): real-time CPU-pressure-adaptive drain duty cycle` — the side path yields CPU to the core path under pressure |
| Adaptive memory/disk self-tuning | `perf(audit): adaptive memory/disk self-tuning + graceful back-pressure` |
| Admission gate on the core path | `ai-gateway/internal/ingress/proxy/admission_gate.go` — bounded in-flight (`AI_GATEWAY_MAX_INFLIGHT`, auto = 1024 × GOMAXPROCS); overload degrades into fast retryable 429s instead of unbounded heap growth into a GOMEMLIMIT collapse. Costs two atomic ops on the admitted path |

**Applicability test:** any unbounded in-memory queue is an OOM waiting for a rate.
Any silent drop on overflow is a correctness bug wearing a performance costume — the
gw explicitly promoted drops to either back-pressure or accounted loss modes.

---

## 7. Caching and memoization

| Technique | Precedent |
|---|---|
| Memoize parsed config structures | `perf(ai-gateway): memoize parsed routing MatchConditions` |
| Cache derived scan bounds per generation | `perf(pipeline): cache MaxPatternBound per generation + close cross-gen cache window` |
| Delta-preserve a config cache across reloads | `perf(compliance/policy): delta-preserve hook cache` |
| Per-host DNS resolve cache with single-flight | `compliance-proxy/internal/access/dns_cache.go` — 10 s TTL, 8192-host cap, in-flight dedup via a `ready` channel |
| Reuse a prepared artifact across turns | `perf(ai-gateway): reuse the prepared tool schema across turns` |

**Applicability test:** anything derived deterministically from configuration and
recomputed per request. Cache it at the config generation boundary, not per request.

**Risk:** a cross-generation window where a stale entry survives a config swap. The
gw closed exactly this window explicitly. Every config-derived cache must be keyed by
or invalidated on the config generation.

**Anti-pattern — caching that costs more than it saves.** L1/L2 response cache is
default-OFF because *every* request pays key computation + lookup while only hits pay
off; at a low hit rate this is a net loss. Measure the hit rate before enabling any
cache on a hot path.

---

## 8. Batching and coalescing

| Technique | Precedent |
|---|---|
| Batch the scan instead of per-chunk | `perf(modela): batch the union prescan in the shared engine (both substrates)`; `perf(ai-gateway): batch Model-A prescan, collapse per-chunk cgo scan count` |
| Coalesce flushes | `perf(ai-gateway): coalesce SSE flushes in the audit-only live pipeline` |
| Size-triggered flush + concurrent publish | `perf(ai-gateway): traffic_event writer — size-triggered flush + concurrent publish` |
| Record batching on the wire | `NDJSON record-batching`; `binary TLV gw↔hub audit wire` |
| Bulk insert | `perf(audit): COPY-based bulk insert for traffic_event/payload (env-gated, fallback-safe)` |
| Drain a backlog in one wake | `perf(agent): drain backlog in one wake` |
| Widen a checkpoint cadence | `perf(streaming): widen live-checkpoint cadence to keep audit rescan linear` |

**Applicability test:** any per-item syscall, cgo call, network round trip, or lock
acquisition that could serve N items instead of one.

---

## 9. Algorithmic cost

Not everything is allocation. Some hot paths are simply the wrong shape.

| Technique | Precedent |
|---|---|
| O(N²) → O(N) via windowing + amortized compaction | `perf(modela): windowed confirm + amortized scanBuf compaction` |
| Make the *rules* cheap, not just the engine | `perf(rulepack): make PII email + IPv6 rules Vectorscan-fast (+13.5% hooks-ON)`; `drop literal-less base64/hex blob rules (71× faster pack)`; `cap wide repeats in detection DB only` |
| Lint against known-expensive patterns | `perf(rulepack): lint flags wide bounded-repeat-around-literal patterns` |
| Per-rule cost profiler | `perf(matcher): opt-in per-rule scan-cost profiler` |
| Build flags for the workload | AVX-512 build flag; Vectorscan `FAT_RUNTIME=OFF` |

**Applicability test:** measure per-unit cost against input size. Anything
superlinear in a value an attacker or a large customer controls is a latent incident.

---

## 10. Logging and observability cost

Per-request observability is per-request work.

| Technique | Precedent |
|---|---|
| Demote successful-path logs to Debug | `perf(gateway): demote 2xx/3xx access logs to Debug (drop per-request hot-path file write)`; `perf(normalize): demote per-Normalize tier CLAIM logs`; `perf(agent): demote per-flow logs` |
| Drop per-request tracing instrumentation | `perf(traffic): drop per-RoundTrip httptrace; capture TTFB on first body Read` |
| Dedupe repeated warnings | `perf(compliance/policy): dedupe missing-impl warnings` |

**Applicability test:** any log statement, metric label, or trace hook on a path that
executes once per request/frame/flow. A structured log line on the success path is a
synchronous file write plus a field-marshal.

---

## 11. Storage and wire format

| Technique | Precedent |
|---|---|
| Right column type | `store inline bodies as TEXT not JSONB` (~5× faster drain); `store inline bodies as raw BYTEA` (drops base64's +33%) |
| Compress large payloads off-path | `end-to-end zstd compression of large captured bodies`; S2 codec as the default (`AI_GATEWAY_AUDIT_CODEC=s2`) |
| Binary wire instead of JSON | `binary TLV gw↔hub audit wire (default-on)` |
| Drop indexes that cost more to write than they save on read | `perf(traffic): drop 7 rarely-read traffic_event indexes to cut ingest write amplification` |
| Add the index the hot query actually needs | `perf(db): add routed_provider_id partial index` |
| Skip classification work when the outcome is fixed | `skip the json.Valid body classification when compressing` |

---

## 12. Runtime and deployment tuning

| Knob | Anchor |
|---|---|
| `GOMEMLIMIT` | `shared/core/runtimemem.AutoSetMemoryLimit` — derived from the cgroup limit when unset |
| `GOMAXPROCS`-scaled sizing | admission gate auto = 1024 × GOMAXPROCS; audit marshal workers = GOMAXPROCS; `NEXUS_CGO_SCAN_LIMIT=auto` |
| Queue storage medium | `NEXUS_EVENTS` stream defaults to MEMORY storage; `NEXUS_EVENTS_MAX_BYTES` auto-sized to RAM |
| Ship the tuned defaults everywhere | `perf(config): ship high-concurrency defaults across AMI + docker-compose + service yaml` |

**Rule:** a tuning that only exists in one deployment path is a trap. When a default
changes, it changes in the code default *and* every packaged deployment.

---

## 13. Default and switch discipline

Every switch that adds per-request work is a performance decision:

- Response cache (L1/L2), freshness rules, request rewrite, marker injection,
  provider cache, payload capture — **all default OFF**.
- Content hooks default OFF but are a compliance necessity when enabled; their cost
  (~15% non-streaming, ~3× streaming) is accepted, not eliminated.
- Audit is default ON in block mode because losing audit records is a correctness
  failure, and its cost is confined to the side path (§6).

**Rule for new work:** a feature that adds unconditional per-request cost must either
be a compliance necessity or default OFF. State which, in the plan.

---

## 14. Techniques that measured as NO WIN

Recording these matters as much as the wins — they prevent re-litigation.

- **Swapping the JSON library wholesale.** `perf-ai-gateway-hotpath.md` Phase 2
  ("internal `json` facade + benchmarked library selection") was **skipped after
  measurement**. Targeted parse *avoidance* (§3) beat library substitution.
- **`goccy` for validity checks.** Measured *slower* and allocating than the stdlib
  for the `json.Valid` use — reverted to stdlib as "the #1 allocator" fix.
- **Enlarging connection pools to fix the streaming collapse.** Raising
  `MAX_IDLE_CONNS_PER_HOST` 2000→50000 and `MAX_CONNS_PER_HOST` to 50000 raised the
  connection count as expected but did **not** improve latency — which is precisely
  what redirected the investigation to CPU (§1).

**Rule:** when a plausible optimization measures flat, record it here rather than
deleting the branch silently.

---

## 15. Audit procedure

Use this playbook as a checklist against a target service:

1. **Instrument** (§1) — pprof + `GOMEMLIMIT` + benchmarks on the hot functions.
   If any is missing, that is item #1.
2. **Map the hot path** — trace one request end to end; list every allocation, parse,
   lock, syscall, log statement, and outbound call on it.
3. **Walk every family** (§2–§13) against that map. For each technique, record
   *applies / does not apply, and why*. Non-applicability is a finding too — it
   prevents cargo-culting a gw fix into a different traffic shape.
4. **Rank** by measured or profile-estimated win against risk.
5. **Ship one at a time**, each with its benchmark or profile diff and its
   correctness proof (§0).
6. **Re-profile after the batch** — fixing the top cost reveals the next one, and the
   ranking from step 4 is stale the moment the first item lands.
