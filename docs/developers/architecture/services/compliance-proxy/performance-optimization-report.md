# Compliance Proxy — performance optimization report

> **Interim.** The optimization program that produced these numbers is still running;
> its live state, open findings and next slice are in
> `docs/handoffs/perf-compliance-agent-program.md`, which is the source of truth. This
> document is the per-service summary that program owes, written from measured data
> only. Where a number was retracted, it says so rather than quietly dropping it.

Companion reports: `../agent/performance-optimization-report.md` (same hot path, see
below) and `../ai-gateway/performance-multimodal-report.md`.

---

## The load-bearing structural fact

**The compliance-proxy hot path is not in `packages/compliance-proxy`.** It is
`packages/shared/transport/tlsbump` and `packages/shared/transport/streaming`, and the
**agent shares both**. `compliance-proxy/internal/proxy/forward/forward.go` only
assembles `BumpOptions` and delegates to `tlsbump.BumpConnection`.

Every consequence follows from that: one fix lands on two services, a regression lands
on two services, and the agent's fail-open safety posture
(`CLAUDE.md` → macOS NE rule) constrains what may be done to code that looks like it
belongs to a server-side appliance.

The service's own package does carry per-**CONNECT** cost — uuid mint, `logger.With`,
`WithContext`, `BuildPipeline` at `internal/proxy/server/server.go` — which is
effectively per-request for clients that open one tunnel per exchange. That is finding
C-24, still open.

---

## Shipped, with measurements

Numbers are as measured at the time each item shipped; the harness they were taken on is
named because it changed mid-program (see *Measurement caveats*).

### Functional and safety fixes found by the perf work

These are not optimizations. They are recorded here because a performance audit is how
they surfaced, and because two of them changed what the service does.

| Item | What was wrong |
|---|---|
| **C-17** | In `live`/chunked_async mode the response hook pipeline **never executed** for non-OpenAI-chat SSE — Anthropic, Gemini, the OpenAI Responses API, Cohere, and any OpenAI stream carrying only tool-call deltas. `extractDeltaText` returned `""`, so `pendingLen` stayed 0, so every checkpoint gate failed and `Execute` was never reached; the flow was audited as an approve with zero hook executions. Enforcement was never bypassed (an enforcing scope diverts to buffer/modela) — what was silently lost was every observe-only response-stage hook. Fixed by driving the mandatory final checkpoint from raw wire bytes. |
| **C-31** | `extractDeltaText` handed raw upstream SSE bytes to a decoder that is not memory-safe on them. goccy v0.10.6 walks past its own buffer on a malformed escape; `{"\` panics ~4 % of the time and reads adjacent heap silently the rest, and under `-race` it is an unrecoverable `fatal error`. It runs on the delivery goroutine, so one malformed frame from a hostile or buggy upstream tore down the client connection. Fixed with a validity gate, narrowed to backslash-bearing frames by a mechanistic precondition. |
| **C-23** | A colon-less SSE line logged **the entire line, up to 1 MiB, at WARN** — remotely influenceable on a MITM path, i.e. a log-amplification shape, and raw intercepted payload in default-level logs on a DLP product. |

### Optimizations

| Item | Change | Measured |
|---|---|---|
| **C-14 + C-23** | Per-line `Warn` on unrecognized SSE fields demoted to Debug, deduped to one report per stream, and never echoing line content. The SSE spec *requires* ignoring unknown fields, so this was never an error condition. | `UnknownFieldWarnStorm` **120,817 → 17,490 ns/op (6.9×)**, allocs 754 → 455 (−40 %) |
| **C-15** | Both 32 KiB passthrough relay buffers pooled. This relay carries every non-inspected SSE response — kill-switch, pinning exemption, `PASSTHROUGH` path action, attested traffic, non-enforcing streaming modes — so it is the highest-volume relay on both services. | `ShortStream` **2,103 → 131.2 ns (16.0×)**, B/op −98 %; `Parallel` **4,169 → 560.3 ns (7.4×)**, B/op −89 %; `TypicalStream` 1.8× |
| **C-26** | Exemption lookup stopped re-deriving constants per entry: an empty-store early-out, plus `net.ParseIP(sourceIP)` and `strings.ToLower(targetHost)` hoisted out of the scan loop. | `EmptyStore` (**the production-common state**) **42.58 → 5.206 ns (8.2×)**; `NoMatch_8` 1.33×; `NoMatch_64` 1.48×; zero allocations throughout |
| **C-1** | Deleted `conn.BufferPool` — defined, unit-tested, **zero production callers**, and the wrong tool for its stated job (a TCP→TCP relay already reaches `splice(2)` via `TCPConn.ReadFrom`, which handing `io.CopyBuffer` a buffer does not change). | dead code removed |
| **C-12** | The 64 KiB `bufio` SSE scan buffer pooled, with the two invariants the ai-gateway's equivalent already documents: keep the **original** handle so an oversize frame cannot poison the pool, and rely on `Next` copying via `Text()` so nothing emitted aliases the recycled buffer. | `SSEParser_ShortReply` **66,384 → 1,011 B (−98.5 %)** and 6,148 → 971 ns; `OpenAIShape` −64.3 %; `LongReply` −21.3 %; geomean bytes **−83.8 %** |
| **C-21** | `WriteSSEEvent` no longer allocates: each line is formatted into a pooled scratch, and `IndexByte` walks data lines instead of `strings.Split`. Write granularity deliberately unchanged. | allocs/op **−24.1 %** (OpenAI 150-frame), **−36.5 %** (Anthropic), **−24.6 %** (OpenAI 1000-frame); geomean **−28.7 %**, all p=0.000 |
| **C-19** | The SSE path built its response pipeline three times per request on the appliance and twice on the agent — the strict entry guard built one only to check that it *could* be built and discarded it, then the scope routing built it again, then the mode branch built it a third time. Now built once at entry and threaded to the guard, the mode override, and whichever mode runs. Both per-mode "build failed, fall back to passthrough" branches were unreachable dead code and were deleted: every error return in `BuildPipeline` is gated on strict AND fail-closed, so a non-strict caller cannot produce one, and a strict caller is refused with 451 before any header is written. | strict: allocs 100 → **92**, B/op **−7.2 %** (n=5); non-strict: allocs 96 → **92**, B/op **−2.3 %** (n=9) |
| **C-18** | `runtimeNormalize` ran the full Tier 1+2+3 decode on every request and response body before the code knew whether any hook existed to consume it. Its only consumer is `HookInput.Normalized`, and this path never stamps `AuditInfo.RequestNormalized` / `ResponseNormalized`, so with no hooks bound the result was computed and thrown away. Now gated on a bound pipeline. `DetectRequestMeta` and `DetectResponseUsage` stay unconditional — they do reach the audit row. | allocs 113 → **100** (−11.5 %), B/op **−6.2 %** on the no-hooks shape (n=7) |
| **C-22** | `LockedByteBuffer.Snapshot` allocated a fresh copy of the entire accumulated transcript at every checkpoint, so total allocation was quadratic in stream length. `SnapshotInto` reuses one destination. Safe because no `NormalizedPayload` can alias the bytes it was built from — the type tree has no `[]byte` or `json.RawMessage` field, the normalize tree uses no `unsafe`, and both JSON libraries copy out of the input. The exported `Snapshot` is untouched: its promise that callers may retain and mutate the result is relied on by the ai-gateway. | `OpenAI1000` B/op 2,677,705 → **1,474,210 (−44.9 %)**; `OpenAI150` **−16.6 %** (n=5) |
| **C-9** | The two per-normalize `INFO` lines (`Registry.Normalize CLAIM`, eight attributes; `FELL-THROUGH`, four) demoted to `Debug` behind `logger.Enabled`. Originally rejected as a deliberate post-incident diagnostic; reopened when the owner ruled that logs must carry only important information. Which tier claimed a body is a debugging aid — the outcome already reaches the audit row — and a fall-through is the designed outcome for any wire no tier models. | allocs 184 → **176** (−4.3 %), B/op −0.6 % (n=5) |
| **C-3** (partial) | The request phase's audit context and the `CPMarker` were two `WithContext` stamps, and therefore two `http.Request` clones, though `stampCPMarker` runs immediately after the phase with nothing reading the context between. Merged into one clone, both values still immutable — a mutable holder was rejected because the SSE goroutines read both and B13 forbids fixing that with a lock. | allocs 100 → **99**, B/op **−2.2 %** (no-hooks), **−1.5 %** (with-hook) |
| **C-4** (remainder) | The six-attribute `Debug("SSE handler entry")` deleted rather than guarded: three attributes duplicated the `Info` line above it, the other three were nil checks on internal invariants every path already handles, and nothing in the repo greps the line. Deleting removes lines, so the file-size cap that blocked guarding stopped being an obstacle. | allocs 92 → **90**, B/op **−2.9 %** (n=5) |
| **C-20** (remainder) | `adapterWireCodec.ChunkText` converted every frame's `Data` string to `[]byte` for `ExtractStreamChunk` — one allocation plus the frame's length, per frame, on the enforcement path. Now filled into a reused per-request scratch. Safe because no adapter returns a segment aliasing its input: verified across 49 built-in adapters, and structurally guaranteed in safe Go where `string(buf)` always copies. The scratch is a `*[]byte` because the codec is stored in an interface with value-receiver methods, so a slice field would be copied per call and never grow. | B/op 280 → **157 (−43.9 %)**, allocs 4 → **3**; identical across all 5 runs of each arm |
| **C-26** (re-examined) | The exemption store's read path is now **lock-free**. `IsExempt` runs per bumped request while its writers (config push, admin grant/revoke, periodic purge) are rare, so the store became copy-on-write behind an `atomic.Pointer` — the pattern `policy/domain.Engine` and `streaming/policy.Store` already use. Reducing the earlier double `RLock` to one was not enough: `sync.RWMutex` blocks new readers once a writer is queued, so a config push could stall in-flight interceptions. A writer-only mutex serialises the read-modify-write swaps; readers never touch it. | `EmptyStore` (production-common) 11.84 → **1.42 ns**, **8.3×, non-overlapping**; the non-empty arms are directionally strong but bimodal so their magnitude is not claimed. allocs/B **0 → 0** on all arms. Most of the non-empty win is map→slice iteration, not the lock. |
| **C-11** | Both bumped-path body reads (`readBody`, `readResponseBodyBounded`) size their buffer from Content-Length instead of `io.ReadAll`'s geometric growth, via the new shared `transport/bodyread`. The declared length is never trusted as an allocation primitive — a 64 KiB ceiling bounds the first allocation, and growth past it must be earned by delivered bytes. | Whole bumped request: **−18 allocs**, B/op **−12.9 %** (`ForwardHandler_RealisticBodies`). Isolated: request read −10 allocs / **−41.0 %** B/op, response read −8 allocs / **−50.4 %** B/op. A B6 review found the first version committed the WHOLE cap once 64 KiB had been delivered — 160x memory amplification at the default cap against io.ReadAll's 1.1x — so growth is now bounded to a doubling of what actually arrived; the win re-measured bit-identical, because production bodies sit under the 64 KiB first allocation. The existing handler arms could not show this — a 72-byte body and an `http.NoBody` response leave both reads unexercised — so a production-sized arm was added. |
| **C-3** (partial) | The per-request PhaseSink stamp merged into the context clone `stampCPMarker` already makes, taking a bumped request from three `http.Request` clones to two. Nothing between the two points read the sink off a context — the phases hold it directly, and the tracing RoundTripper runs later. | **−1 allocation and ~340 bytes on every bumped request**: `NoHooks` 97 → 96 allocs / −2.6 % B/op, `WithHook` 173 → 172 / −1.8 %, `RealisticBodies` 186 → 185 / −0.4 %. The absolute saving is constant, so the percentage varies with the arm. Deleting the stamp had left the whole repository green; the assertion that now covers it is mutation-verified. |
| **C-30** | The `LivePipeline` reader goroutine and its per-frame channel deleted; parsing runs inline in the delivery loop. Also removes a goroutine leak: a panic in the delivery loop unwound through `defer cancel()` but not through `CloseUpstreamOnExit`, and a reader parked in `upstream.Read` never observed ctx. | allocs −8 (150-frame) / −19 (1000-frame), B/op **−7.1 % / −2.7 %**. **`ns/op` is NOT claimed**: +7.2 % median with overlapping distributions, inside the host's ±14 % floor — and there is a mechanistic reason it could be genuinely slower in production, since two goroutines let a parse overlap a socket write. Flagged for the rig run. |
| **C-20** (partial) | The Anthropic usage accumulator switches on the SSE `event:` name *before* spending a validity scan. Sound because the event name comes from outside the JSON, so nothing in the payload can disguise it. | `Feed_AnthropicIgnoredEvent` **22.87 → 1.93 ns (−91.6 %)**; `Feed_OpenAIContent` −8.19 %; net **−4.2 %** per stream |
| **Audit marshal buffer** | `MQBatchWriter.flushBatch` encoded each `TrafficEventMessage` with `json.Marshal`, which finishes by copying its result into a fresh exact-size slice — one ~message-sized allocation per audit event, i.e. per bumped request. It now encodes into a pooled `bytes.Buffer` and hands the aliased bytes to `producer.Enqueue`, reclaiming the buffer the moment Enqueue returns. This is the ai-gateway's `msgBufPool` shape (`platform/audit/writer_batch.go`) applied to the service that had no pooling of its own on this path. `sync.Pool` rather than a writer field because `Flush` can call `flushBatch` off the loop goroutine once the loop has exited. `mq.Producer.Enqueue`'s doc now states the no-retain requirement the aliasing depends on; the sole production implementation satisfies it because `jetstream.Publish` is a synchronous request/reply and nats.go copies the payload into the connection write buffer inside it. | Per audit event (n=5 per side, `BenchmarkFlushBatch_*`): envelope-only **4,311 → 3,157 B/op (−26.8 %)**, 18 → 17 allocs; 8 KiB captured bodies **41,222 → 22,433 B/op (−45.6 %)**, 20 → 19 allocs; 64 KiB bodies **324,112 → 167,737 B/op (−48.2 %)**, 23 → 21 allocs. Every run of each arm reported the same allocs/op. `ns/op` is **not** claimed — the arms sit inside the host's noise floor (caveat 1). The buffer is deliberately **not** pre-grown the way the ai-gateway's is: `Encode` issues one Write, so an empty buffer sizes itself exactly, and a 64 KiB pre-grow measured strictly worse on a pool miss (68,310 B vs 3,707 B envelope-only; 87,939 B vs 40,402 B at 8 KiB) for identical steady-state numbers. |

### Cross-dimensional summary

Two harnesses, kept separate on purpose. Summing across a harness change is the specific
error this program has already been burned by, so the streaming arms and the per-request
arms are reported apart and the earlier items (C-14, C-15, C-26) are quoted per-item above
rather than folded in at all.

**Dimension 1 — the streaming relay** (`transport/streaming`, shared with the agent). One
harness throughout, one process per arm, arm order rotated; `allocs/op` and `B/op` at ±0 %.

| Arm | before C-12 | after C-12 + C-21 | after C-22 | total |
|---|---:|---:|---:|---|
| `OpenAI150_Usage` B/op | 233,677 | 174,490 | **146,963** | **−37.1 %** |
| `OpenAI150_Usage` allocs | 1,251 | 950 | **949** | **−24.1 %** |
| `Anthropic150_Usage` B/op | 161,587 | 94,075 | **93,633** | **−42.1 %** |
| `Anthropic150_Usage` allocs | 1,247 | 792 | **791** | **−36.6 %** |

C-22 moved the OpenAI arm (−15.8 % from the C-21 state) and left the Anthropic arm flat —
its 150 frames carry shorter payloads, so the accumulated transcript each checkpoint copies
is smaller and there is less copying to remove. The effect scales with transcript length,
which is why the 1000-frame arm is where it shows: **−44.9 %** there.

**Dimension 2 — per-request cost on the bumped path** (`transport/tlsbump`). This harness
did not exist before R-13, so there is no pre-program baseline to sum toward; each item's
own before/after is in the table above and these are the current absolute values.

| Arm | B/op | allocs/op | moved by |
|---|---:|---:|---|
| `ForwardHandler_NoHooks` | 12,683 | 96 | C-18 (−913 B, −13), C-3 (−1) |
| `ForwardHandler_WithHook` | 18,415 | 172 | C-9 (−8), C-3 (−1) |
| `ForwardHandler_RealisticBodies` | 92,276 | 185 | C-11 (−18), C-3 (−1) |
| `HandleSSEResponse_Live_NonStrict` | 6,896 | 92 | C-19 (−160 B, −4) |
| `HandleSSEResponse_Live_Strict` | 7,046 | 92 | C-19 (−534 B, −8) |

`ForwardHandler_RealisticBodies` was added for C-11 because the other two arms carry a 72-byte body
and an empty upstream response, where neither of that finding's two reads is measurable at all.

The two SSE arms converging on 92 allocations is C-19's point: the appliance used to pay
three pipeline builds per streaming request and the agent two, and both now pay one, so the
postures cost the same.

**Dimension 3 — logging volume**, which is a cost dimension here rather than a side note,
because the agent runs on a user's laptop. C-14 (6.9× on the affected arm), C-23 (a 1 MiB
line at WARN, deleted), C-4 (`logger.Enabled` guards), C-9 (two INFO lines demoted) and
A-2 (14 attributes → 2, 2.7×) all cut it. No log line was added anywhere in this program.

**Dimension 4 — correctness**, which is not a performance result but is what the audit
actually produced most of: C-17 (response hooks never ran on non-OpenAI SSE), C-31 (a
remotely-triggerable memory-unsafe decode), C-23, and, from the adversarial reviews, two
defects in this program's own shipped work (R-1, R-7) plus two pre-existing normalize bugs
(R-14, R-15).

**Not measured here.** RPS, p50/p95/p99, CPU %, RSS, goroutine count and GC are rig
dimensions, deferred to a single `llm-gateway-benchmark` run; protocol in
`docs/handoffs/perf-loadtest-protocol-and-tooling.md`.

---

## Rejected, with measured reasons

Recorded so they are not re-proposed. Full write-ups in the program register's
*Measured no-wins*.

| Item | Why it was rejected |
|---|---|
| **C-16** cheap-reject fast path | +25.1 % end-to-end **despite zeroing allocations** on the target shapes. Note the register's original *reasoning* for the rejection was later found to be wrong (it compared a validator on a 170-byte frame against a decoder on an 85-byte frame), so the item is **reopened** — but this measurement stands. |
| **C-13** per-line copy | **No headroom under the shipped API.** The per-frame cost is exactly two allocations and both are contractually required: one string per data line, because `SSEEvent.Data` is a `string` and the field is additive-only; and one `&SSEEvent`, which callers own. Two of the finding's three claims did not hold at all — blank lines are already free, and the `dataLines` slice never escapes so it is stack-allocated. |
| **C-9** demote the per-response INFO logs | Not unconditional (early-returns on PASSTHROUGH), the two are mutually exclusive on SSE, and the INFO level is a deliberate post-incident diagnostic whose whole point is being answerable from `agent.log` alone. Demotion destroys that; lazy-building saves nothing while INFO is on; sampling breaks the guarantee. |
| **C-33** `gjson.Valid` at the sniff sites | Over-permissive where the sniff is a safety gate: stdlib enforces a 10,000-frame depth limit, gjson enforces none, so it admits arbitrarily deep bodies stdlib refuses — under-protecting the decoder it exists to gate. Measured stack cost 16.2 MiB at depth 100k, 512 MiB at 5M (a 5 MB body), then `fatal error: stack overflow` above ~10M — a `fatal error`, so `recover()` cannot contain it. A 20-case differential found gjson and stdlib agreeing on **all 20**, so the only thing on offer was the 2.9×. |
| **C-8** splice(2) on the passthrough relay | Three independent reasons, none of them a benchmark. The finding blamed `bufconn`, which wraps only when the client pipelines past the CONNECT line and is required for correctness there; the wrapper on **every** tunnel is `IdleConn` (`idleTimeout: "300s"` ships in both configs). Both embed `net.Conn` as an interface, so Go does not promote `TCPConn.ReadFrom`/`WriteTo` and `io.Copy` cannot reach the fast path. But delegating them is **not** the fix: `IdleConn`'s guard resets a `time.AfterFunc` from `Read`/`Write`, and `poll.Splice` does not return between syscalls, so one `ReadFrom` spanning a long transfer would let the 300 s timer close a healthy tunnel mid-download. An idle guard that watches userspace copies and splice, which removes them, cannot both hold. Third: `splice` is Linux-only in the Go runtime, so the effect is absent on the development host entirely. Guarded by `TestIdleConn_DoesNotExposeSpliceFastPath`. **A resolution exists and is verified**: `net/splice_linux.go` unwraps `*io.LimitedReader` before requiring a `*TCPConn`, so an `io.CopyN` loop splices in-kernel while returning to userspace every N bytes, where the idle timer can be reset — both ends must be raw `*net.TCPConn` (any wrapper on the source hits `spliceFrom`'s `default` arm), so it is a `PassThrough` signature change rather than a method on the wrapper. Linux-only, so unmeasurable on the development host. |
| **C-16b** unsafe string view for the decoder | Measured **no change at all** in B/op, and it put `unsafe` on a safety-critical MITM path for zero gain. |
| **C-11's pooled variant**, now with numbers | The body read is the largest poolable buffer left on the bumped path — a heap profile of `ForwardHandler_RealisticBodies` puts 15.4 % of the request's allocated bytes in `bodyread.Bounded`. The pool still loses. Its win is entirely hit-rate dependent, and a `sync.Pool` is cleared on every GC cycle, so the miss regime is the one that decides it: with `runtime.GC()` between iterations the pooled variant costs **4 allocs and 41–43 KB** for a 512 B body against sizing's **2 allocs and ~630 B**, because a miss allocates the pool's 64 KiB class rather than the body's size — roughly 65× the bytes, for the same read. At 8 KiB it is ~42 KB against ~9.5 KB. Only the never-miss regime favours it (1 alloc / 48 B against 2 / 624 B at 512 B). Reproducible in `transport/bodyread/pool_rejected_bench_test.go`, whose `_Hot` and `_AfterGC` arms are exactly these two bounds. This is *in addition* to the ownership transfer the pooled form would need (below), not instead of it. |
| **Passthrough relay `io.Copy` buffers** | Cost unit is **per connection**, not per request: 32 KiB per direction, 64 KiB per passthrough tunnel (measured 32,784 B/op on the generic `io.Copy` path). Pooling a per-connection allocation buys a count, not a rate. And on Linux the number is zero on the intended path — `TCPConn.ReadFrom` splices — so a pool would only pad the degraded path C-8 already describes. |
| **C-31a / C-31b / C-31c** | `recover()` cannot catch a checkptr throw; a trailing-backslash guard is provably sound but insufficient (fuzz found `{"\00\.`); decoding with stdlib is correct but +337…545 %. |

---

## Open, ranked

Live ranking with its evidence is in the program register. In brief: establish a
trustworthy measurement first (**R-9** did that for the shipped gate), then
**C-16**
reopened, then **C-8**.

**C-3's remainder is blocked by a real regression risk.** Its measured ceiling is 2,041 B /
11 allocations per request (14.8 % of bytes on the no-hooks shape), of which one clone has
been recovered. The obvious next merge — consolidating the `clientHelloKey` and `PhaseSink`
stamps — would silently break TLS fingerprint replay on the attested-traffic path:
`attestationPeek` runs between them, and when it short-circuits, `attestationPassthrough`
forwards through `UpstreamTransport.DialTLSContext`, which reads the ClientHello out of the
request context. No existing test asserts the outbound fingerprint, so the regression would
be invisible.

**C-11 needs a prerequisite, and the naive form is unsafe.** Pooling the buffered bodies
means returning them at handler exit, but `audit.NewInlineBody` retains the caller's slice
and is serialized later by an async marshal worker off a queue holding up to ~1000 events
— so a returned buffer could carry one request's payload into another request's persisted
audit body. The ai-gateway already solves this by transferring buffer **ownership** to the
audit record (`AcquireResponseBuffer` / `AttachPooledResponseBody`, reclaimed by the writer
at terminal resolution). `shared/audit.Writer` has no such mechanism, so C-11 first needs
that ported — additively, since `packages/shared/audit` ships in a released agent binary.

**Ahead of all of it, and not a performance item:** **C-32**. The goccy defect C-31 fixed
at one call site is live at **7 of 13 codec entry points** under
`packages/shared/transport/normalize/codecs/`, where it panics in a *production* build,
and `runtimeNormalize` — which carries no `recover` of its own — is called with the raw
**client request body**. A 4-byte body from any application on a monitored host reaches
it, with only `net/http`'s per-connection recover in between.

---

## Measurement caveats that apply to every number here

1. **`ns/op` on the streaming arms is not trustworthy on the development host**, which
   idles at load 15-25. An `after`-vs-`after` null control on identical binaries drifted
   **+14.1 % at p=0.62**. `allocs/op` and `B/op` are ±0 % and are what the recent items
   are measured on.
2. **`go test -bench -count=N` does not interleave sub-benchmarks** — it runs all N of
   the first, then all N of the second. Paired `impl=before`/`impl=after` sub-benchmarks
   are therefore not a controlled comparison. Use one process per arm with the arm order
   rotated.
3. **Check statistical power before believing a null.** The parallel arm has a
   coefficient of variation of 11-26 %, so its minimum detectable effect is 14-33 %:
   `p=1.000` there means *cannot tell*. The serial arms (CV 1.8-5.8 %) are the ones with
   power.
4. **Rig dimensions — RPS, p50/p95/p99, CPU %, RSS, goroutine count, GC — are not in
   this report.** They are deferred to a single `llm-gateway-benchmark` run after the
   optimization work lands; protocol in `docs/handoffs/perf-loadtest-protocol-and-tooling.md`.
