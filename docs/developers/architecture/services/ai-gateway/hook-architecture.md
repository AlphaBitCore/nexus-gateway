# Hook architecture

Compliance hooks are the policy layer that inspects each request and response and decides whether to approve, block, or rewrite it. They run as a priority-ordered pipeline shared by all three data-plane services — the AI Gateway, the Compliance Proxy, and the Agent — so a policy authored once enforces identically wherever traffic enters. The framework and all hook implementations live in `packages/shared/policy/hooks`, with the runner in `packages/shared/policy/pipeline`; the AI Gateway's `packages/ai-gateway/internal/policy/hooks` is only a contract-test mount point, not production code.

## 1. HookConfig — the declarative unit

Each hook instance is a `HookConfig` (`packages/shared/policy/hooks/core`): an `implementationId` selecting the code, a `priority` (lower runs first), an `enabled` flag, a `stage` (`request` / `response` / `connection`), a `failBehavior` (`fail-open` or `fail-closed`), an optional `timeoutMs`, an `applicableIngress` list (`ALL` / `AI_GATEWAY` / `COMPLIANCE_PROXY` / `AGENT`), an `applicableTrafficKinds` filter (default `["ai"]`), a `scope`, and a free-form `config` map. Operators author these rows on the Control Plane; the gateway loads them and compiles them into a pipeline.

The framework ships eleven hook implementations, registered by `implementationId`: `keyword-filter`, `pii-detector`, `content-safety`, `rate-limiter`, `request-size-validator`, `ip-access-filter`, `data-residency`, `rulepack-engine`, `noop`, `webhook-forward`, and `quality-checker`.

## 2. The Hook interface and applicability

A `Hook` implements `Execute(ctx, *HookInput) (*HookResult, error)` plus `SupportsEndpoint` and `SupportsModality`, which are queried at build time so the pipeline is filtered before any request runs. An empty endpoint or modality always matches, so a request that has not yet been classified still passes through every hook. Three embeddable helpers cover the common cases:

- **`ChatOnly`** — applies only to chat text traffic.
- **`AnyEndpointAnyModality`** — runs on everything (rate limiter, IP filter, data-residency, request-size, webhook-forward, noop).
- **`TextOnlyContentScanning`** — text scanners (PII, keyword, content-safety, quality, rulepack). It supports chat, embeddings, STT, TTS, image-generation, and video-generation inputs, but not batch or job endpoints. It carries a marker interface so the builder can skip it on the embedding **response** stage, where the payload is float vectors with no scannable text.

Connection-stage hooks must additionally implement `ConnectionStageCompatible` — that stage has no body and forbids MODIFY-capable hooks.

Hooks never receive raw provider JSON. The `HookInput` carries the canonical `NormalizedPayload` produced by the normalize layer (see [normalization-architecture.md](normalization-architecture.md)), along with request metadata, detected provider/model and API-key class/fingerprint, network context, accumulated upstream tags, the provider region, and endpoint/modality classification. Content scanners read text via `TextSegments()` (the payload's text projection).

## 3. Decision vocabulary and onMatch

Internally a hook returns one of five decisions — `Approve`, `RejectHard`, `BlockSoft`, `Modify`, `Abstain`. Content-touching hooks do not hardcode that decision; they read it from an `onMatch` block in their config, which carries a **single `action` axis**:

- **`action`** — `approve` / `redact` / `block`. `DecisionForAction` maps it onto the internal decision enum (`approve → Approve`, `redact → Modify`, `block → RejectHard`); `ActionFromDecision` is the inverse used at the pipeline boundary so the aggregated result carries one action that drives both the in-flight disposition and what the audit record persists.

The single action is the one axis an operator configures; its three values mean:

- **`approve`** — forward / return the body unchanged, and store it as-is.
- **`redact`** — rewrite the payload (`Modify`); the *same* masked body is forwarded upstream, returned to the client, and persisted to the audit store.
- **`block`** — reject the transaction (`RejectHard`); persist the redacted copy so an auditor can still review what tripped the policy.

`redact` and `block` share the same payload-redaction operation (mask matched spans inline with `[REDACTED_<RULE_ID>]`) and differ only in disposition (rewrite-and-forward vs reject). Storage therefore has only two states — as-is (`approve`) or redacted (`redact` / `block`) — driven entirely by the action; there is no separate storage choice and no "store a redacted copy while forwarding the original" combination.

When the `onMatch` block is absent the defaults are `action = block` and a `[REDACTED_<RULE_ID>]` replacement template — a match rejects the request and the persisted copy is redacted. The `webhook-forward` hook re-derives its action default to `approve`, because the webhook's reply is itself the decision rather than a fixed block. Where multiple hooks disagree, the framework aggregates by strictness: `StrictestAction` ranks `block > redact > approve` for the operator-facing action, and the internal decision merge ranks `RejectHard > BlockSoft > Modify > Approve > Abstain`.

For backward compatibility, the prior two-key shape (`inflightAction` + `storageAction`) is also accepted: when `action` is absent but a prior key is present, `ActionFromLegacy` folds the pair to a single action (`block-hard`/`block-soft → block`, `redact → redact`, `approve + keep → approve`, `approve + redact`/`drop-content → redact`) and logs a one-shot warning pointing the operator at the single `action`. A one-time data migration rewrites stored rows; the schema drops the two-key form once the compatibility window closes.

## 4. Resolving the pipeline

`PolicyResolver` holds the current `HookConfig` snapshot behind an atomic pointer, so a config swap never blocks an in-flight resolve. `Swap` takes a defensive copy and reuses cached hook instances for rows whose content is unchanged, so a reload reconstructs only the hooks that actually changed. `resolve(stage, ingress, strictFailClosed)` filters the snapshot to enabled rows matching the stage and ingress, instantiates each via the registry (an unknown `implementationId` is logged once and skipped), rejects a connection-stage row that is not connection-compatible, and returns the survivors in priority order.

**The snapshot is already sorted.** Configs enter the resolver in exactly two places — `NewPolicyResolver` and `Swap`, both on the config-load path — and each stores the slice ordered by priority. `resolve` then filters by stage, ingress and traffic kind with an append loop, and a filtered subsequence of an ordered slice is ordered, so no per-request sort is needed. That is the difference between making the sort cheaper and removing it: the cost is paid once per reload rather than once per request.

The sort is **stable**, because which of two equally-prioritised hooks runs first is observable — it decides whose `Modify` the merge sees last. Equal priorities keep the order the configuration declares them in.

### What happens to the hooks a swap evicts

`Swap` closes every evicted hook immediately, and the prefilter matcher's own
in-flight-scan drain makes that safe for scans already running on it. One
window is NOT covered by the drain: a request that resolved the old hook and
has not yet called `Scan`.

That window is left **safe rather than closed**, deliberately. A scan on a
closed matcher reports the scan as INCOMPLETE, and every consumer treats
INCOMPLETE as fail-unsafe rather than as "scanned, nothing found" — so the
request pays a slower RE2 re-confirmation instead of losing its masking. A
rule-pack swap can therefore never silently approve a payload no hook actually
looked at.

A grace-period deferred close is **not** the way to close it, and the reason is
a property of how streaming callers use the pipeline. `BuildPipeline` is
stage-scoped and the SSE response stage calls it ONCE per response, while
`streaming.LivePipeline` calls `Execute` at every checkpoint. The
resolve-to-`Scan` window is therefore not microseconds — it is the whole
duration of the stream, and no fixed grace period bounds that. What would close
it is reference counting the matcher for the lifetime of every pipeline holding
it; that is not built, because the window is already safe and the only thing
bought would be latency on the rare event of a config reload, in exchange for
cross-goroutine lifetime management on a safety-critical path.

### Build-time fail-closed enforcement

An *unbuildable* hook — unknown `implementationId` (no factory), a factory that returns an error, or a connection-stage row bound to a MODIFY-capable (non-connection-compatible) impl — is by default skipped with a one-time warning. That availability-first degradation ("one bad rule degrades to that rule off, not all compliance off") is correct for resilience, but it would also let a **mandatory `fail-closed` enforcer silently become a no-op** if it can't be built. The `strictFailClosed` parameter closes that gap:

- `strictFailClosed=true` — an unbuildable row whose `failBehavior` is `fail-closed` makes `resolve`/`BuildPipeline` return an error instead of skipping it. The caller then refuses the traffic. This is set by **every caller that can safely refuse**: the AI Gateway reverse proxy ("refuse" = a safe HTTP 500 to an API client) AND the **Compliance Proxy appliance** — a dedicated forward proxy that already 403s disallowed CONNECTs, which wires `tlsbump.WithStrictFailClosed` so all five of its bump build sites refuse rather than forward uninspected (connection stage 403, request/response stages 502 + reject audit, SSE live/buffer abort the relay). Fail-OPEN rows are still skipped under strict, preserving resilience for advisory hooks.
- `strictFailClosed=false` — every unbuildable row is skipped with a warning regardless of `failBehavior`. This is required ONLY for the genuine host-network in-path caller: the **agent NE proxy** (AGENT ingress through the shared `tlsbump` forwarder), which sits in the host's outbound packet path. A build error there must never refuse/close, which would take down the host's networking (the binding NE fail-open rule). The agent leaves the tlsbump option unset, so strictness is threaded per-caller — never a global default.

`BuildPipeline` runs that resolution (forwarding `strictFailClosed`) and then applies the endpoint and modality gates — dropping hooks that do not support the request's endpoint type or any of its modalities — plus the embedding-response gate that removes text scanners when the response stage carries embedding vectors. Each exclusion increments a skip metric. When nothing applies, it returns a nil pipeline and the caller skips the hook phase. On the AI Gateway's streaming response path the response headers are already sent when the pipeline is built, so a `strictFailClosed` build error cannot become an HTTP 500: the stream-entry routing probe forces the request into buffered execution, where the canonical-buffer rewrite re-runs the build, hits the same error, and fails closed with an in-band error frame before any content reaches the client. The real-time streaming path itself is audit-only — it scans and tags but never blocks or rewrites the wire (see [sse-streaming-compliance-architecture.md](../../cross-cutting/safety/sse-streaming-compliance-architecture.md)).

**A skipped fail-open hook is still a compliance gap, and it now says so per request.** The degradation above is correct for availability and was invisible: the warning is deduplicated per reload epoch, so a hook broken since Tuesday logs one line on Tuesday and nothing afterwards. Traffic flows, the decision is an ordinary APPROVE, and the audit row is indistinguishable from one where no such hook was ever configured — which is the shape of "the PII detector has not run for six days and no one noticed".

Two signals close it. `PipelineSkippedTotal` now counts build-time skips (`unknown_implementation`, `factory_error`, `connection_incompatible`) alongside the endpoint and modality gates it already counted, so the condition is alertable. And the resolved pipeline carries the implementation ids it could not build onto **every result** as a `hook-unbuildable:<implementationId>` tag, which flows into `ComplianceTags` on the audit row. That makes "which requests went out without the PII detector" an audit query rather than a log-archaeology exercise.

The tag is deliberately not a decision change. Whether an unbuildable fail-open hook should refuse traffic is the `strictFailClosed` question above, already answered per caller; what was missing was not enforcement but evidence.

A resolved pipeline also answers `HasContentScanningHook` (`packages/shared/policy/pipeline/pipeline.go`): body-independent, true when at least one bound hook actually inspects content (a hook that does not implement `RawContentPrescanner` counts as content-scanning — conservative, we cannot prove it is metadata-only). The AI Gateway uses it to stamp an honest multimodal `compliance_coverage` value — a pipeline of only metadata hooks (rate limit / IP / size / webhook) scanned no content and must not be reported as prompt-scanned.

The same stream-entry probe drives the streaming-mode routing. The resolved response pipeline exposes two content-independent, scope-derived predicates — `MayBlock` and `MayRedact` (`packages/shared/policy/pipeline/enforcement.go`), each reading the bound hooks' `onMatch` action rather than any response body. A pipeline that may block routes the stream to buffered execution; one that may redact arms the prescan-gated real-time path that escalates to buffering on a confirmed match; a non-enforcing pipeline keeps the audit-only real-time path. Enforcement therefore never depends on the admin streaming-mode knob alone — an enforcing scope always overrides it onto a path that can enforce.

### The two shapes of "configured but not guarding", and the evidence for each

`BuildPipeline` returns `(*Pipeline, []string, error)`. The middle value is the
unbuildable list, and it travels **even when the pipeline is nil**. That is the
case the tag above could not reach: when EVERY configured hook fails to build
there is no pipeline for the merge to stamp, so the caller sees the same nil it
sees for a tenant who configured nothing at all. The AI Gateway's request and
response stages stamp `hook-unbuildable:<implementationId>` directly from the
returned list in that branch; the other call sites discard it, because they
would have to invent an audit row that does not otherwise exist. `UnbuildableTags`
is the one helper both producers go through, so the string cannot drift between
them.

The second shape is a hook that builds and runs and then FAILS on most of the
traffic it sees. Fail-open is the correct per-execution posture, and it is also
what makes this invisible: each request is approved, individually correctly,
while the rule the operator configured has in practice stopped running.

A per-implementation sliding window (`degradation.go`) watches for it:

| parameter | value | why |
|---|---|---|
| window | 3s | deliberately finer than any scrape interval — which is the other reason this cannot be a Prometheus rule |
| minimum samples | 20 | the first request after a deploy failing is a 100% rate over one sample; a floor, not a tuning knob |
| trip | failure rate ≥ 50% | the natural line between "intermittent" (per-execution fail posture handles it) and "not working" |
| clear | rate < 25%, held a full window | hysteresis; one threshold in both directions flaps at the boundary and turns the alert into noise |

Only an ERROR, TIMEOUT or PANIC counts. A REJECT or a BLOCK is the hook
**working**, and counting a firing policy as a broken one would light the signal
exactly when compliance is doing its job.

It is a DETECTOR, not a circuit breaker, and that is the design rather than an
omission. A breaker's action is to stop calling the failing dependency; here that
means either refusing all traffic (a fail-closed hook taking the service down on
its own) or silently skipping the scan (a fail-open hook doing precisely what
this exists to catch). So it changes no request-level behaviour and produces
three pieces of evidence instead: the `compliance_hook_degraded` gauge, one WARN
per state FLIP (never per request — at a thousand requests a second that log is
itself an outage), and a `hook-degraded:<implementationId>` audit tag on the
requests served while it was tripped. The gauge says "right now"; only the tag
answers "which requests went out while the scanner was down", and no metric can.

The window lives on the `PolicyResolver`, not on a `Pipeline`: pipelines are
built per request, and a window that starts empty every request never reaches the
sample floor.

### The abandoned chain owns the input, so the abandon path may not read it

`executeSequentialAbandonable` runs the hook chain on its own goroutine and
answers on the caller's. When a hook overruns its budget the caller stops
waiting, calls `abandonedResults`, and returns — while the chain goroutine is
still running, and still owns `input`. A hook that then returns MODIFY writes
`input.Normalized`.

So any read of `input` from the abandon path is a data race, and `Kind` — the
field the applicability filter needs — is a two-word string header: a torn read
is a wrong comparison at best and a segfault at worst. The race detector reports
it, and `hookAppliesToKind` therefore takes the kind as a VALUE. The snapshot is
taken once, before the chain starts (`payloadKindOf`), and nothing is lost by it:
every in-chain rewrite of the payload copies it and preserves `Kind`, so the
value is invariant for one `Execute`.

`abandon_race_test.go` drives the exact interleaving — a hook whose runtime
equals its budget, so the timer and the return land together — and is meaningful
only under `-race`.

## 5. Executing the pipeline

A pipeline runs its hooks under a total timeout with a per-hook timeout — the AI Gateway sets these to 15 and 5 seconds (the per-hook value overridable per config via `timeoutMs`), and the framework falls back to 30 and 5 seconds when a caller leaves them unset. Every `Execute` call is wrapped so a panicking hook becomes an error rather than crashing the data plane. On an error or timeout the hook's `failBehavior` decides the outcome — `fail-closed` yields `RejectHard`, the default `fail-open` yields `Approve`. A nil result is treated as `Abstain`.

Each hook is self-timed from a single captured `elapsed` and stamped in **both** units: `LatencyMs` (the truncated integer-millisecond floor, kept for backward compatibility and **never clamped** because it is summed downstream) and `LatencyUs` (the precise microsecond value). Hooks run at microsecond scale, so `LatencyMs` floors a sub-millisecond hook to `0`; `LatencyUs` carries the real value into the per-hook trace and the additive `request_hooks_us` / `response_hooks_us` aggregates. See `audit-pipeline-architecture.md` for the aggregation and the streaming one-row-per-hook fold.

### A per-hook timeout only holds if someone is watching the clock

The per-hook deadline rides in the hook's context, and a hook is free to ignore it. Every built-in scanning hook does: `pii-detector`, `keyword-filter`, `content-safety` and `rulepack-engine` never read `ctx.Done()`; only `webhook-forward` honours it, through `http.NewRequestWithContext`. Since `safeHookExecute` calls `Execute` synchronously, a hook that never returns held the whole request until the TOTAL timeout — the per-hook value described a limit nothing enforced.

The sequential runner therefore runs the chain on **one** goroutine and waits for each hook's result under that hook's own deadline. Waiting has to happen on the other side of a goroutine boundary or it does not happen at all.

Three properties follow, and they are the reason this is not per-hook parallelism:

- **One goroutine, not one per hook.** The chain is sequential and stateful — each hook sees the previous hook's redactions and tags — so it cannot be split. Abandoning it abandons the rest of the chain, which is the honest outcome: after a hook times out, what the remaining hooks would have said is unknown.
- **An abandoned chain contributes no modifications.** Half-applied redaction is worse than none.
- **The goroutine keeps owning `input`.** It rewrites `input.Normalized` and appends to `input.UpstreamTags` between hooks, and after an abandonment it goes on doing so. Callers must not read `input` after `Execute` returns; everything they need comes back through the results. Within the chain, a hook's effects are applied BEFORE its result is handed over, since the handoff is now a goroutine boundary — ordering those the other way round is a data race, and `go test -race` catches it.

The parallel runner is left as it is: it already gives each hook its own goroutine, so one hook ignoring its context delays only the final wait, not its siblings. The sequential path is the one the gateway uses and the one a hung hook holds.

The runner has two modes:

- **Sequential** (the AI Gateway) — hooks run in priority order, short-circuiting on the first `RejectHard`. When a hook returns `Modify`, its transform spans are applied to the normalized payload before the next hook runs, so later hooks see the redacted content; emitted tags accumulate across hooks.
- **Parallel** (the Compliance Proxy) — hooks run concurrently and cancel the rest on a `RejectHard`. Because parallel hooks cannot share evolving state, they neither apply MODIFY between hooks nor accumulate tags.

`mergeResults` aggregates by priority order: the first `RejectHard` wins outright; otherwise a `BlockSoft` (e.g. a soft AI-Guard verdict folded into the merge); otherwise a `Modify`; otherwise `Approve`. Tags are unioned, and the strictest action across hooks (`StrictestAction`) is carried onto the result. A merged `BlockSoft` carries no distinct client-facing response — `ActionFromDecision` folds it to the `block` action, so dispatch treats it identically to `RejectHard` (reject; see §3). The AI Gateway enables two flags on its pipeline: `allowModify` (MODIFY passes through instead of being downgraded to APPROVE) and `clearSoftOnApprove` (a later APPROVE clears a pending soft block).

When a redacting hook (`Modify`) co-fires with a soft-block hook the aggregate is promoted to `BlockSoft` while the redact's payload still rides along, so consumers must apply the mask rather than forward raw. They gate on `CompliancePipelineResult.CarriesRedaction()`, not on `Decision == Modify`. For a `BlockSoft` aggregate that predicate consults `RedactionApplicable`, a flag `mergeResults` stamps when an **enforcing** per-hook decision (`Modify` or `BlockSoft`) contributed an **applicable** artifact — `ModifiedContent` (carried only from `Modify` hooks) or a span whose `ContentAddress` is not an audit-only sentinel (`normalize.IsAuditOnlySentinelAddress`, currently the `webhook.flat` address a webhook-forward hook emits for advisory `redactions[]` it cannot apply in-flight). Keying on raw span/content presence instead would let an approve-webhook's (or an approve-ceiling-reconciled `Modify`-webhook's) audit-only spans masked behind a soft-block report a redaction the aggregate cannot apply, over-blocking a stream that should soft-deliver; keying on `Decision == Modify` alone would drop a real co-firing redaction. The sentinel check is a deliberate denylist (an unknown address counts as applicable) so the failure direction is over-block, never leak. Advisory spans remain unioned into the result for the audit record regardless.

## 6. Config flow

`HookConfigCache` is the bridge from stored config to the resolver: a loader reads the `HookConfig` rows and `Swap`s them into the `PolicyResolver`. On the server-side data planes it reloads when the Hub pushes a config change (via the thing-client `OnConfigChanged` callback), with a background TTL-backstop ticker covering a degraded push channel; the request path never loads — `Resolver()` is a pure snapshot getter, so a slow database cannot stall or stampede request goroutines. The Agent has no direct database access, so it is push-only. Before the swap, `rulepack.Enrich` binds each installed rule pack into the relevant hook's config under `_rulePackInstalls`, so the `rulepack-engine` hook evaluates packs without holding a database handle inside `Execute`.

The AI Gateway invokes the pipeline at both the request and response stages: it builds a sequential pipeline for the stage, ingress, endpoint, and modality, enables `allowModify` and `clearSoftOnApprove`, and executes it against the `HookInput`.

## 7. Relationship to AI-Guard

A policy can defer a decision to the judge-model AI-Guard pipeline through the `webhook-forward` hook pointed at the AI Gateway's AI-Guard webhook endpoint. The webhook reply (`decision` / `reason` / `redactions`) is the suggestion, which `webhook-forward` reconciles against the hook's `onMatch.Action` policy ceiling by strictness — an admin `block` ceiling cannot be undercut by a permissive judge, and a judge reject cannot be undercut by a permissive ceiling; a mismatch stamps `ReasonAIGuardSuggestedVsPolicy`. Because this is an ordinary HTTP call from a hook, it works on every data plane, including the Agent. The AI-Guard classifier itself — its endpoints, backends, cache, and cost accounting — is covered in [aiguard-architecture.md](aiguard-architecture.md).

## References

- `packages/shared/policy/hooks/core/types.go` — `HookConfig`, `Hook` interface, applicability helpers, `HookInput`, decision aliases
- `packages/shared/policy/hooks/core/onmatch.go` — `onMatch` parsing, decision mapping, strictness aggregation
- `packages/shared/policy/hooks/builtins/builtins.go` — built-in hook registry
- `packages/shared/policy/decision/types.go` — decision and action vocabulary
- `packages/shared/policy/pipeline/policy.go` — `PolicyResolver`, `BuildPipeline`, ingress/endpoint/modality gates
- `packages/shared/policy/pipeline/pipeline.go` — sequential/parallel execution, fail behavior, result merge
- `packages/shared/policy/pipeline/config_cache.go` — `HookConfigCache` load and swap
- `packages/shared/policy/rulepack/` — rule-pack store and config enrichment
- `packages/ai-gateway/internal/ingress/proxy/proxy.go` — request- and response-stage pipeline invocation
- `packages/ai-gateway/cmd/ai-gateway/wiring/hooks.go` — hook registry, config cache, and rule-pack wiring
