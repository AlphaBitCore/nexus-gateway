# Why the streaming prefilter scans in block mode, and what that costs

This records a measurement, not a decision that has been taken. The change it
argues for is not implemented; what is implemented is the block-mode prefilter
described below, plus the tail window that exists to compensate for it.

## The chain

`packages/shared/policy/hooks/matcher/vectorscan.go` compiles the rule set with
`HS_MODE_BLOCK`. Block mode keeps no memory between calls, so a prescan must
re-scan **everything accumulated so far**, every time it runs. Four consequences
follow, each forced by the one above it:

1. Re-scanning is O(bytes accumulated), so it cannot run per token.
2. So it is batched — `PrescanBatchBytes`, 1024 bytes of new content.
3. So up to 1024 bytes may arrive between scans; delivering them as they arrive
   would let a value complete and leave before the next scan sees it.
4. So a tail must be **held undelivered**: `MaxPatternBytes + PrescanBatchBytes`,
   which for the shipped rule pack is 7362 + 1024, giving a 9410-byte window.

A typical chat answer is smaller than that window, so nothing is released until
the stream ends. Measured: a 5 KB redactable response delivers **200 of 200
frames after EOF** — time-to-first-token equals total generation time.

## Where block mode came from

The commit that introduced the Vectorscan matcher (`f82014548`, 2026-06-22)
states:

> Scope: block-mode detection (one `hs_compile_multi` DB, no start-of-match).

That is a scope declaration for one PR, not a finding that streaming mode is
unsuitable. Nothing in `docs/` or `packages/shared/policy/` evaluates
`HS_MODE_STREAM`. The tail-window architecture was then built on top of it, and
the constraint stopped looking like a choice.

## What the two modes actually cost

Measured locally against the shipped 423-rule pack (`libhs` via pkg-config; the
probe lives beside this program's other one-off harnesses and is reproducible
from the numbers below).

Both modes compile the whole pack. The streaming database is slightly smaller,
and streaming needs a small per-stream state:

| | compiles | database | per-stream state |
|---|---|---|---|
| `HS_MODE_BLOCK` | 423/423 | 2,916,280 B | — |
| `HS_MODE_STREAM` | 423/423 | 2,687,992 B | **1,162 B** |

Wall time and bytes scanned, 25-byte frames, 1024-byte prescan batch (the
production values):

| response | conc | block | stream | | block B/resp | stream B/resp |
|---|---|---|---|---|---|---|
| 5 KB | 256 | 67 ms | 94 ms | block 1.4× | 15,250 | 5,000 |
| 32 KB | 256 | 329 ms | 189 ms | stream 1.7× | 540,400 | 32,000 |
| 200 KB | 256 | 3.157 s | 368 ms | **stream 8.6×** | 19,787,750 | 200,000 |

Block mode scans O(N²/2B); streaming scans each byte once. Memory held per
concurrent stream diverges the same way — block holds the accumulated buffer it
must re-scan, streaming holds 1,162 bytes of automaton state and one
undelivered frame:

| 1000 concurrent streams | 5 KB responses | 200 KB responses |
|---|---|---|
| block | 13.7 MB | **199.7 MB** |
| stream | 1.1 MB | **1.1 MB** |

## What real traffic looks like

Read-only query against a customer staging deployment, 50,062 `traffic_event`
rows with a non-zero completion:

| percentile | tokens | ≈ bytes | bytes block would scan | ratio |
|---|---|---|---|---|
| p50 | 235 | 0.9 KB | 0.4 KB | 0.5× |
| p90 | 1,497 | 5.8 KB | 17.1 KB | 2.9× |
| p99 | 6,995 | 27.3 KB | 373.3 KB | 13.7× |
| p99.9 | 16,829 | 65.7 KB | 2,160.8 KB | 32.9× |
| max | 65,521 | 255.9 KB | 32,753.0 KB | **128×** |

Half of all responses are around a kilobyte, where block mode is genuinely the
faster of the two. One in a hundred is 27 KB, one in a thousand is 66 KB, and
the largest observed is 256 KB — and those are where p99 latency and per-box
memory are decided.

## Why streaming mode does not weaken the guarantee

The tail window exists so that **a complete sensitive value is never delivered**.
Streaming mode reaches the same guarantee by a shorter route: `hs_scan_stream`
carries automaton state across chunks, so a pattern spanning frame boundaries is
matched by construction — no lookahead, no held window. Scanning each frame
before deciding whether to deliver it means the frame completing a card number is
the frame withheld; the client holds a fragment, which is the property that
matters.

One fact makes this feasible. The prefilter answers a boolean "could any rule
match", and needs no start-of-match — which is streaming mode's most expensive
and most constrained feature. The confirm step does need match positions, and it
runs on accumulated text, rarely. So the two modes are not alternatives: block
belongs to the confirm, streaming to the prefilter.

## The streaming database's lifetime

A scan stream outlives the window in which a config swap can close the matcher, so
the streaming database is reference-counted rather than protected by the inflight
counter that guards block-mode scans: the matcher holds one reference, each live
stream holds one, and whoever drops the last one frees the database. That lets
`Close` return immediately instead of busy-waiting out a 200 KB response.

The counter must refuse to move off zero. Acquiring is a compare-and-swap, not an
increment with a closed-check around it, because zero means "the last reference is
gone or going" and an increment can lift it back to one. When it does, two
goroutines each observe a legitimate 1→0 transition and each free the same handle
— reproduced as a `SIGABRT` inside `hs_free_database`, and no amount of rechecking
the closed flag afterwards can separate the two. For the same reason the handle
itself is read under a mutex: the last release nils the field, and the acquire
path's pre-check reads it while holding no reference at all.

## What is not settled

- The hit sets of the two modes must be proven identical over the corpus before
  either is trusted to replace the other.
- `TailWindowBytes`, `MaxPatternBytes` and `PrescanBatchBytes` exist to
  compensate for block mode. Whichever survive need their own justification
  rather than inheriting one.
- Per-stream state is small but not free; 1,162 B × concurrent streams needs a
  ceiling and a metric.
