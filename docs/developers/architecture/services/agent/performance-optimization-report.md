# Agent — performance optimization report

> **Current as of the third session.** The optimization program that produced these numbers
> is still running;
> its live state, open findings and next slice are in
> `docs/handoffs/perf-compliance-agent-program.md`, which is the source of truth. This
> document is the per-service summary that program owes, written from measured data only.

Companion reports: `../compliance-proxy/performance-optimization-report.md` (shared hot
path — read it too) and `../ai-gateway/performance-multimodal-report.md`.

---

## What makes the agent a different optimization target

Two things separate it from every other service in the repo, and both change which
optimizations are worth doing.

**1. It shares its hot path with compliance-proxy.** The agent's intercepted traffic runs
through `packages/shared/transport/tlsbump` and `packages/shared/transport/streaming`, so
**every shipped `C-*` item in the compliance-proxy report applies here verbatim** — the
SSE diagnostic storm fix, the pooled relay buffers, the pooled scan buffer, the
allocation-free serializer, the Anthropic accumulator hoist, and both functional fixes
(C-17's missing response hooks, C-31's memory-unsafe decode). That shared surface is the
program's main leverage: one change, two services.

**2. It runs on user laptops with no admission control in front of it.** The ai-gateway
has a bounded in-flight gate that degrades into retryable 429s; the agent has nothing of
the kind, because there is no operator to shed load to. That makes two cost dimensions
first-class here which the gateway program never had to model:

- **Battery and wakeups.** A resident process that wakes on a timer costs the user
  battery whether or not traffic is flowing.
- **Per-request CPU and disk on hardware the user is holding.** A busy browsing session
  is thousands of intercepted requests; anything per-request is felt directly.

A third constraint bounds *how* anything may be optimized: the macOS
`NETransparentProxyProvider` sits in the host's outbound packet path, and
`CLAUDE.md`'s NE rule makes fail-open non-negotiable. An optimization that could hang,
panic, or claim a flow without relaying it takes down the entire machine's networking —
DNS, DHCP, mDNS, NTP, Apple Push, VPN — with manual `launchctl unload` as the recovery.
Every fast path in this program therefore ships with an explicit unknown-shape test
proving the general path still fires.

---

## Shipped, with measurements

### A-1 — the agent could not be profiled at all

Before this, `packages/agent/cmd/agent` wired **neither** `profiling.Start` nor
`runtimemem.AutoSetMemoryLimit`. It was the only long-running service in the repo with
neither — ai-gateway, compliance-proxy and nexus-hub all have both — which meant a field
report of *"the agent is eating CPU/RAM"* had no way to be answered with data.

Both are now wired inside `cmdRun` rather than `main()`, deliberately: `run` is the only
long-lived process, so the one-shot commands (`version`, `enroll`, `install-ca`, …) keep
their current behaviour and startup cost. Both are **no-ops unless explicitly enabled** —
`AutoSetMemoryLimit` returns immediately when `GOMEMLIMIT` is set or no cgroup limit is
readable, and `profiling.Start` returns immediately unless `NEXUS_PPROF_ENABLED` is
truthy. That default-off property matters more here than on a server: the agent must not
open a port, spawn a goroutine, or write a profile file nobody asked for on a user's
machine.

This is the playbook's prerequisite item, and it is what makes every later agent
measurement answerable from a live host.

### A-2 — the per-request audit log carried the whole audit row

`loggingQueueWriter.Enqueue` logged **14 attributes at INFO on every intercepted
request**, and every one of them — `target_host`, `method`, `path`, `hook_decision`,
`bump_status`, `domain_rule_id`, `path_action`, `source_process`, `source_bundle`,
`provider`, `model`, `latency_ms` — is a **column on the audit row the line announces**.
They were duplicated into the log. It now carries `event_id` + `trace_id` only.

The property the anchor exists for is preserved: exactly one greppable INFO line per
request, joinable to the row that holds everything else.

| | before (14 attrs) | after (2 attrs) | delta |
|---|---:|---:|---|
| ns/op | 1,995 | **741.8** | **2.7×** |
| B/op | 600 | **32** | **−95 %** |
| allocs/op | 15 | **2** | **−87 %** |

This matters more than the raw numbers suggest, for the reason in the section above: a
busy browsing session was thousands of 14-field structured writes — CPU wakeups, disk
I/O and battery, on hardware the user is holding.

Two tests pin it. One pins the diagnostic contract — exactly one anchor line, both ids
present, and the audit event still forwarded to the real queue writer, because a
diagnostic change must never drop the row. The other pins the **cost decision** by
asserting the row-duplicated attributes are *absent*, so re-adding any of them fails the
build and forces the trade-off to be made deliberately rather than by drift.

### Inherited from the shared surface

See the compliance-proxy report for measurements and its *Cross-dimensional summary* for
the aggregate. In brief: SSE diagnostics no longer storm (6.9× on the affected arm) and
never echo wire content; the passthrough relay buffers are pooled (16.0× / 7.4× / 1.8×);
the 64 KiB scan buffer is pooled (bytes −83.8 % geomean); `WriteSSEEvent` no longer
allocates (allocs −28.7 % geomean); the pre-hook snapshot destination is reused
(**−44.9 %** of allocated bytes on a 1000-frame stream, C-22); the SSE response pipeline is
built once per request instead of twice (C-19); normalize no longer runs when no hook will
read it (C-18); two per-normalize INFO lines are demoted (C-9); and both functional fixes
landed here as well.

On the shared streaming arms that is **B/op −37.1 % / allocs −24.1 %** (OpenAI 150-frame)
and **B/op −42.1 % / allocs −36.6 %** (Anthropic 150-frame) end to end.

**Why the per-request items matter more here than the raw percentages suggest.** The
appliance amortizes a tunnel across many exchanges; a browser on a monitored host opens one
CONNECT per exchange for a large share of traffic, so per-request cost lands closer to
per-exchange. And C-19's non-strict arm is the agent's: `strictFailClosed=false` is the
host-packet-path posture, which is the 96 → 92 allocation column.

---

## Rejected, with a measured reason

### A-3 — stack-array TLS record header in `PeekSNI`

`agent/internal/network/proxy/proxy.go` allocates twice per intercepted flow:
`make([]byte, 5)` for the TLS record header, then `make([]byte, 5+recordLen)` for the
record, then copies. The attempt replaced the header with `var header [5]byte` and used
`bytes.Clone` on the two reject paths so it would not escape.

| arm | before | after |
|---|---:|---:|
| `ClientHello` allocs/op | 18 | 18 — **no change** |
| `NonTLS` allocs/op | 16 | **17 — worse** |

**Why it failed:** on the reject path the original `make([]byte, 5)` *was* the buffer
being returned, so it was never waste — `bytes.Clone` just adds a second allocation. On
the TLS path any saving sits below the harness noise floor (`net.Pipe` plus goroutine
cost is ~16 allocs and swamps a 1-alloc delta). Reverted.

Pooling had already been ruled out at finding time for an independent reason: the
returned buffer escapes and is replayed by `ReplayConn` for the connection's lifetime, so
there is no safe release point.

**What would actually be needed:** a fake `net.Conn` over a `bytes.Reader` to isolate
`PeekSNI` from the harness. Worth doing only if real agent flow rates justify it — and
A-1's profiling wiring now makes that answerable from a live host rather than guessed.

---

## Open

**A-4 — the resident wakeup surface**, which is the agent-only dimension and the largest
remaining agent-specific item. A 2 s backpressure poll, a 5 s Linux reconcile, a 30 s
darwin ticker, plus queue, exemption and updater tickers:

- `agent/internal/observability/backpressure/store.go:75,160`
- `agent/platform/linux/reconciler_linux.go:23,214`
- `agent/platform/darwin/platform_darwin.go:324`
- `agent/internal/observability/audit/queue/*`
- `agent/internal/policy/exemption/store.go:294`
- `agent/internal/host/updater/updater.go:197`

The gw precedent to apply is *drain the backlog in one wake* rather than polling on a
fixed cadence. A-1 is the prerequisite that makes the current cost measurable on a real
host instead of estimated from the code shape — which, in this program, has been wrong
often enough to be a rule.

Everything else open for the agent is a shared-surface `C-*` item; see the
compliance-proxy report's ranking, and note that **C-32** (the goccy wire-decode sweep)
reaches the agent too, since the same normalize codecs run on intercepted bodies here.

---

## Measurement caveats

The same four caveats in the compliance-proxy report apply verbatim: `ns/op` is not
trustworthy on the development host, `-count=N` does not interleave sub-benchmarks,
null results need a power check, and rig dimensions are deferred to a single
`llm-gateway-benchmark` run. One addition specific to the agent: **`net.Pipe`-based
harnesses cost ~16 allocations of their own**, which is enough to hide a single-allocation
change — the A-3 rejection is the worked example.

## C-11 — the agent shares both reads

`readBody` and `readResponseBodyBounded` live in `shared/transport/tlsbump`, so the agent's MITM
relay pays and now saves exactly what the compliance-proxy does: **−18 allocations and −12.9 %
bytes per bumped request**. The measurement and the harness caveat are in the compliance-proxy
report; only the allocation figures transfer, since the agent runs on a laptop where the relevant
consequence is battery rather than throughput.

The same change fixed a busy-loop hang reachable through the shared primitive's contract (a sender
delivering more than it declared spun 5 M iterations with no progress). `net/http` framing means no
real wire could trigger it, but on the agent a pinned core is a user-visible battery event, so it
is worth naming here.

## A-4 — the resident idle wake-up surface

The agent is the only one of the three services where idle cost is a first-class concern: it runs on
a user's laptop, so a timer that never sleeps is battery rather than throughput. The metric here is a
**count**, not a duration — idle wakes per minute is a function of the configured intervals, so it is
exact and needs no benchmark.

Enumerated across every resident timer, one of them was **93 % of the whole budget**: the audit
`QueueWriter` flush loop ran a standing 100 ms ticker, waking **600 times a minute — 36,000 an
hour** — and on an idle host every wake found an empty batch and returned immediately.

That ticker is now a timer armed only while a batch is pending, so the loop's idle wake rate is
**exactly zero**. Worst-case flush latency is unchanged at the flush interval, now measured from the
first event of a batch instead of from an arbitrary tick boundary; average latency for a batch's
first event rises by ~50 ms on a local encrypted-queue insert that is followed by a multi-second
upload drain.

The second-largest item was then **measured and rejected**. The backpressure poll wakes 30 times a
minute and runs `SELECT COUNT(*) FROM audit_events WHERE synced = 0` each time — which sounds like
43,000 encrypted-queue queries a day, and is in fact **0.18 seconds of CPU per day**: `idx_audit_
synced_created` leaves the `synced = 0` index prefix empty once everything has uploaded, so the idle
query costs 4.2 µs. That does not justify a new correctness invariant on the audit-delivery path,
where being wrong means a missed throttle (events dropped while backpressure never engages) or a
phantom one.

The wake itself is a separate axis, and the reason 30/min is acceptable where 600/min was not is
that **100 ms is short enough to defeat package idle states and 2 s is not** — the interval's
relation to idle-state residency is what matters, not the raw count. `BenchmarkUnsyncedCount_*` is
committed beside the queue so the decision is reproducible; re-open it only if that idle number
moves.

