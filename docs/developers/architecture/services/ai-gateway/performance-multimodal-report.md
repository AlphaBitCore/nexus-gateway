# AI Gateway multimodal surfaces — performance optimization report

> **Interim, and deliberately short.** The optimization program that produced it is still
> running; live state is in `docs/handoffs/perf-compliance-agent-program.md`. This phase
> found far less than the other two, and that is the result rather than a gap in the
> audit — the reasoning is below.

Scope: the **parallel** multimodal handlers, not `ServeProxy`. The chat hot path has its
own document, `performance-and-switches-explained.md`, which this one does not duplicate.

Companion reports: `../compliance-proxy/performance-optimization-report.md` and
`../agent/performance-optimization-report.md`.

---

## The phase premise was wrong, and that is the main finding

The audit opened on this hypothesis:

> The gw multimodal surfaces are parallel handlers, not `ServeProxy`, so they likely
> inherit **none** of the `ServeProxy` hot-path work — admission gate, body pooling, fast
> paths.

**Refuted.** Every parallel handler acquires the admission gate:

| handler | gate |
|---|---|
| `stt_handler.go:73` | `h.gate` |
| `realtime_handler.go:128` | `h.gate` |
| `video_handler.go:93` | `h.gate` |
| `guardrail_handler.go:50` | `h.gate` |
| `video_follow_handler.go:60,105` | `h.gate` |
| `video_content_handler.go:157` | `h.gate` |

So the overload protection that keeps the chat path from growing unbounded into a
`GOMEMLIMIT` collapse is present here too. Only body pooling is genuinely not inherited
(see M-1).

Recording the refutation is the point. The premise was plausible from the code
structure — parallel handlers really do bypass the `ServeProxy` stage chain — and acting
on it would have meant re-deriving protections that already exist.

## Two more sites audited and found correct as-is

Per the playbook, non-applicability is a finding: it stops a gw fix being cargo-culted
into a different traffic shape.

**`realtimeproxy` is already well designed.** `realtime_pump.go` has zero internal
queueing — one in-flight frame per direction, with back-pressure left to the transport —
forwards frames **verbatim** with no re-marshal, and runs its metering tap *after* the
bytes are on the wire. Client→provider is fully opaque. There is nothing to pool, batch,
or skip.

**`realtimeproxy/tap.go:39` is the in-repo reference implementation of cheap
classification.** `ClassifyServerEvent` uses a single-field
`gjson.GetBytes(frame, "type")` scan over the existing bytes — no copy, no full unmarshal.
It is worth naming because the same codebase's `streaming/live.go` did the opposite for
years, and this is the shape the audit kept pointing at.

**The three normalize/scan entry points on the multimodal path already have the
skip-when-no-consumer fast path** that the compliance-proxy equivalent was missing:
`stt_prompt_scan.go:39→53`, `video_submit_scan.go:37→60` and
`guardrail_handler.go:164→186` all call `BuildPipeline` first and return early on
`pl == nil` before building `Normalized`. That is what makes the `tlsbump` omission
(finding C-18, still open) an inconsistency rather than a design choice.

---

## Open

### M-1 — response bodies read via unpooled `io.ReadAll` — FIXED by sizing, not pooling (smoke owed)

**The premise below was wrong and is kept for the record.** All three sites read **responses**,
and responses on these endpoints are small JSON: an STT transcript, a video job descriptor, and
error bodies only (`video_content_handler.go`'s 2xx artifact goes to `streamVideoArtifact` and
is never buffered). The multi-megabyte payloads are the **requests**. The 32 MiB / 1 MiB caps
are DoS ceilings, not typical sizes.

**And pooling was the wrong fix.** `readResponseBounded` sizes the buffer from `Content-Length`
instead: measured `io.ReadAll` vs sized at 512 B / 2 KiB / 8 KiB / 64 KiB gives
**−58 % / −56 % / −53 % / −53 %** of allocated bytes and a flat **2 allocations at any size**
(`io.ReadAll` grows geometrically, allocating ~2.2× the body across O(log n) allocations).
Sizing has no `sync.Pool` hit-rate dependency — which is what the worry below was really
about, since a pool is cleared every GC cycle and a low-rate endpoint would miss it — needs no
ownership transfer to the audit record, and cannot pin an oversized buffer.

Two mutation-verified guards: truncate to the bytes actually read (an overstated
`Content-Length` otherwise leaves NUL padding — a 16-byte body became 4,096 bytes with 4,080
NULs), and never trust an over-cap `Content-Length` as an allocation size (otherwise 1 GiB is
allocated from a response header alone).

**A declared length is a claim, not evidence.** The header sizes the FIRST allocation only up to
`preallocCap` (64 KiB); past that, growth is earned — the buffer jumps to the declared length only
after that many bytes were really delivered — and nothing exceeds the read cap. Without that bound
a response declaring `Content-Length: 32MiB` (the STT cap) and sending a few bytes would commit
32 MiB before any payload arrived, N concurrent ones committing N x 32 MiB — an exposure
`io.ReadAll` never had, since its growth follows delivered bytes. Measured after the bound:
**−53.8 % / −50.4 % / −45.9 % / +0.8 % / −49.1 %** of allocated bytes at 512 B / 2 KiB / 8 KiB /
64 KiB / 400 KiB, with allocation count flat at **3–4** instead of 6–25. The 64 KiB row is neutral
because `declared+1` lands one byte over the bound.

**Owed before this counts as done:** an ai-gateway smoke, per `CLAUDE.md`. Scope-limited is
sufficient — the STT and video paths plus one chat model as a control — because the blast
radius is three response-read call sites with no codec, cost or cache code touched.

#### The original (incorrect) framing, retained

The gw's `readBufPool` / `requestBodyPool` / `responseBodyPool` are used by the
`ServeProxy` stage chain only, not by the parallel handlers:

- `stt_handler.go:324`
- `video_handler.go:385`
- `video_content_handler.go:230`

The trade here is the inverse of the chat path: **request rate is lower, but bodies are
much larger** — audio, video and images rather than a few KB of JSON. Whether pooling pays
depends on which side dominates, and that has not been measured. Before implementing,
price it: a pool that is missed on most requests, or that pins multi-megabyte buffers for
a low-rate endpoint, is the local-optimum trap this program's own gate exists to catch,
and the playbook already records "caching that costs more than it saves" as a real
anti-pattern in this codebase.

**The mechanism is already in this module, and using it is not optional.** A pooled body
cannot simply be returned when the handler exits: `audit.NewInlineBody` retains the
caller's slice and the zstd+base64 happens lazily in `MarshalJSON` on the async marshal
worker, off a queue that holds up to ~1000 events until flush. Returning the buffer at
handler exit would let a later request overwrite bytes that worker has not yet read — one
request's payload landing in another request's persisted audit body.

`internal/platform/audit/record_bodypool.go` already solves this by transferring
**ownership** rather than borrowing: `AcquireResponseBuffer` hands out a pooled `*[]byte`,
`AttachPooledResponseBody` records that the record's body points at it, and the **writer**
reclaims it at the record's terminal resolution — published, spilled or dropped.
`ReleaseResponseBuffer` is the arm for a body that is never attached, and
`responseBodyPoolCap` (2 MiB) stops one oversized body inflating every pooled buffer. Note
that the tee appends **through the handle**, which is what keeps the handle valid when a
body outgrows the initial capacity; that detail is easy to lose when copying the pattern.

So M-1 is adoption of an in-module mechanism, not construction — which is what makes it
tractable where the compliance-proxy equivalent (finding C-11) is not: `shared/audit.Writer`
has no handle-reclaim mechanism at all, and adding one touches a package that ships in a
released agent binary.

### Shared-surface items that reach here

**C-32** — the goccy wire-decode safety sweep — is not confined to compliance-proxy. The
same codec entry points under `packages/shared/transport/normalize/codecs/` are reachable
from gateway ingress, and 7 of 13 of them fault in a *production* build on a malformed
escape. That is a safety item, ranked ahead of everything in this report.

**C-33** — **REJECTED, and the reason generalizes.** Adopting `gjson.Valid` at
`normalize/codecs/generic_http_sniff.go:89,139` on the strength of its 2.9× was measured
against the pinned versions and refused: stdlib's scanner enforces a 10,000-frame nesting
depth limit and gjson enforces none, so gjson admits arbitrarily deep bodies stdlib
refuses — **under-protecting the very decoder the sniff exists to gate.** Measured stack
cost 16.2 MiB at depth 100k, 128 MiB at 1M, 512 MiB at 5M (a 5 MB body — an ordinary
size), then `fatal error: stack overflow` above ~10M, which `recover()` cannot contain. A
20-case differential over malformed JSON found gjson and stdlib agreeing on **all 20**, so
there was no correctness gain to weigh against it. The site's documented invariant — the
validator must be stricter than the goccy decoder it feeds — holds as written and stays.

---

## Why this report is thin

Three of the phase's four audited areas came back correct as-is, one hypothesis was
refuted, and the single genuine finding needs a measurement before it can be
implemented responsibly. The multimodal surfaces were built after the chat path's
performance program, and they inherited its conclusions — the admission gate, the
skip-when-no-consumer ordering, verbatim frame forwarding, single-field classification.

That is a good outcome for the codebase and a short report. It is recorded at this length
rather than padded, because a report that manufactures items to look thorough is worse
than one that says the phase found little.

---

## Measurement caveats

The four caveats in the compliance-proxy report apply here too, and one matters
especially for M-1: the development host cannot resolve small `ns/op` effects (an
`after`-vs-`after` null control on identical binaries drifted +14.1 % at p=0.62), so any
body-pooling decision should rest on `allocs/op` and `B/op`, which are ±0 %, or wait for
the deferred `llm-gateway-benchmark` rig run.

## M-1 follow-up — the algorithm moved, and it had a hang in it

`readResponseBounded` is now a thin `*http.Response` adapter over
`shared/transport/bodyread.Bounded`; the bumped path needed the identical read (finding C-11) and
this algorithm had already produced an OOM vector and a +42 % regression within one session, so two
copies was the wrong number. Behaviour is unchanged for the three multimodal handlers, and the
wrapper keeps two wiring tests that fail if `resp.ContentLength` stops reaching the primitive or if
the declared length and the cap are transposed.

Consolidating it surfaced a defect **in the shipped M-1 loop**: `next = declared + 1` can be at or
below the current capacity when a sender delivers more than it declared, leaving the read window
empty so `Read` returns `(0, nil)` forever — measured at 5,000,001 iterations with `len == cap ==
101` and zero progress. `net/http` frames response bodies by Content-Length, so no provider could
reach it over a real wire; it is fixed regardless, because the primitive documents the declared
length as a hint correctness never depends on.
