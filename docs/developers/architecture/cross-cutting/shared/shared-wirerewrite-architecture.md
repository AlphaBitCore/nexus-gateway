# Shared wirerewrite architecture

`wirerewrite` is the byte-level rewriter that runs on the adapter-wire request
body just before two points: hashing the Nexus L1 cache key, and sending the body
upstream to the provider. It exists to make equivalent requests hash to the same
cache key, so caching actually hits.

It does NOT turn on the provider's own prompt cache. That marker is a field of one
wire's request shape, so it belongs to the codec that speaks that wire — see
[the Anthropic codec's prompt-cache marker](../../services/ai-gateway/prompt-cache-architecture.md#4-anthropic-prompt-cache-markers).
Asking the question here cost a list of adapter names at every site that had to
decide "is this an Anthropic-shaped body", and `cacheconfig.FamilyOf` already owns
that answer.

It is deliberately separate from
[normalize](../../services/ai-gateway/normalization-architecture.md): `normalize`
is the read-only canonical request/response *shape* framework used for audit, and
never mutates the bytes that go to the provider. `wirerewrite` does the opposite —
it edits the outbound bytes. The two share only the audit `TransformSpan` type;
they are otherwise different concerns.

## 1. Two entry points

The engine exposes two functions, both called with the adapter-wire body
(`PrepareBody` output) and both fail-open — any parse error, transform error, or
panic returns the original body unchanged.

- **`NormalizeKey(format, body)`** — strips key-safe volatile fields and returns
  the result *for cache-key hashing only*; the body sent upstream is untouched. It
  runs after `PrepareBody` and before the cache key is built, and it is **always
  active** — no config gates it. This is what lets two requests that differ only in
  a volatile field land on the same
  [L1 cache](../storage/cache-multi-tier-architecture.md) entry.
- **`NormalizeUpstream(format, body)`** — strips bytes from the body that *will* be
  forwarded to the provider, returning the modified body and a `Result` for audit.
  It runs after an L1 miss, before the request goes to the broker, and is
  **demand-driven**: it no-ops unless the current config actually gives it work to
  do (§4).

## 2. The engine

The `Engine` is constructed once at startup and holds its compiled rule set in an
atomic pointer. It loads the bundled rules immediately so it is operational before
any config arrives, and `Reload` rebuilds an immutable snapshot and swaps it
atomically — in-flight calls finish against the previous snapshot.

Each rule runs through a panic-recovering wrapper, and work is layered:

- **L0 key-normalise** — the key-safe subset of rules, applied by `NormalizeKey`.
- **L3 strip** — the strip rules, applied by `NormalizeUpstream`.

## 3. Rules

A rule is scoped to one adapter type and carries a transform type, default and
override enable flags, a dry-run flag, a `KeyNormalizeSafe` flag (whether it may
run during cache-key normalisation), and — for strip rules — a compiled regex plus a
LIST of `gjson` body paths. One transform type exists: `strip`.

The list is not decoration. A wire field can legitimately arrive in more than one
JSON shape, and a rule naming only one of them is silently absent on the others.
Anthropic's `system` is an ARRAY of content blocks when a native `/v1/messages`
client sends it, and a plain STRING when this gateway's own codec rebuilds the
request on the cross-format leg — and the engine runs on `PrepareBody` output, so
it sees whichever shape that leg produced. The Claude Code nonce rule declared
only `system.#.text`, whose gjson `#` requires an array; it stripped the nonce for
a `/v1/messages` caller and did nothing at all for the same conversation arriving
on `/v1/chat/completions`, on the upstream body AND on the L1 cache key. The
selectors are applied in order and are mutually exclusive — each no-ops on a shape
it does not match — so declaring both costs one failed lookup, never a double
strip.

Rules apply surgically: a strip rule removes one known volatile token from a
precise body path. Whole-body re-serialisation (e.g. JSON field-order
canonicalisation for cache-key stability) is intentionally NOT offered — re-encoding
the client's body would rewrite bytes the client chose (number formatting, escaping,
key order) and change what is forwarded upstream, an unacceptable risk for a
passthrough gateway, and it was the dominant per-request allocation when enabled.
Cache keys are therefore field-order-sensitive; canonicalisation, if ever needed,
must hash a derived form and never touch the forwarded body.

The bundled rules ship factory defaults that operator config can override:

- **Claude Code nonce strip (off by default)** — for the Anthropic and Bedrock
  wire. It removes Claude Code's `cch=<hex>` token from the system-prompt text.
  Strip rules select `gjson` paths, apply the regex to the matched string values,
  and write the result back with `sjson`.

  The nonce sits INSIDE the system prompt, which is the first segment of the
  provider's own prompt-cache prefix, so a token that rotates per Claude Code
  session defeats Anthropic's cache and not merely the gateway's L1 key. Measured
  on the live wire: with the nonce rotating, turn 2 of a conversation reported
  `cache_creation_input_tokens=11792, cache_read_input_tokens=0` — the provider
  re-created the entry and read nothing; with it stripped, the same turn reported
  `creation=0, read=11774`. At 1.25× for a write against 0.1× for a read that is
  roughly a twelvefold difference on the prefix. The rule is `KeyNormalizeSafe`
  and also in the upstream set, so enabling it fixes both caches at once.

## 4. Configuration and hot-reload

Config is projected from the `cache` config key (`configkey.Cache`): the Control
Plane assembles the cache-config blob and pushes it to the AI Gateway shadow, which
projects that blob into the wirerewrite `Config` on reload. Its zero value is a safe
all-off default. It carries exactly one thing: per-adapter per-rule overrides
(`enabled`, `dry_run_always`). A config change rebuilds the engine's snapshot
through `Reload`.

The engine holds **no per-provider state at all**. The same blob's per-provider
prompt-cache settings are read per request by the codec that writes the marker, not
projected here. An earlier version did project them, against a snapshot of the
provider list taken when the `cache` key was applied — which silently disabled
markers for any provider created after the last cache push, and for the whole
process when a cold start reached `cache` before `providers`, an order the config
loader does not guarantee (it ranges a Go map). Holding no provider snapshot removes
that class rather than patching it.

**There is no global on/off switch for the engine.** `Reload` derives an internal
`hasWork` flag from the resolved snapshot:

```
hasWork = (any adapter has ≥1 enabled upstream strip rule)
```

`NormalizeUpstream` returns the body untouched when `hasWork` is false, so a
zero-config deployment pays nothing and never rewrites a forwarded byte. Enabling a
strip rule **is itself the demand** — there is no second, operator-facing toggle to
remember. `NormalizeKey` is outside this gate entirely: the L0 cache-key
normalisation always runs, whatever `hasWork` says.

The on-the-wire identifiers — the rule IDs such as `claude-code-cch-strip` — are
stable admin/shadow/database identifiers. They are preserved verbatim, so renaming
one is a coordinated config migration, not a local refactor.

## 5. Safety

Wire rewriting edits the bytes headed to a paid provider, so the engine is
conservative:

- **Fail-open** — every transform returns the original body on any error; a broken
  rule degrades to a no-op, never a corrupted request.
- **Dry-run** — a rule can run in dry-run mode, recording in the audit `Result`
  what it *would* have stripped without changing the body, for safe rollout of a new
  rule before it is allowed to mutate traffic.
- **Per-rule circuit breaker** — each rule has a breaker that trips open after a
  burst of errors within a short window, after which that rule is skipped. A tripped
  breaker stays open for the rest of the process: a config reload preserves breaker
  state (error history is intended to survive a config change), so a rule that keeps
  failing recovers only on a process restart. Because skipping a rule is fail-open,
  this is safe — the request simply goes upstream without that rewrite.

The outcome of `NormalizeUpstream` is a `Result` with strip counts, a dry-run flag,
and byte-level `TransformSpan` records. `DryRun` is what the AI Gateway keys on to
stamp ZERO strip counts on the audit row: a dry-run rule measures and does not
edit, so the row must not report bytes as removed. Those spans are
consumed in-process (cache-key derivation and strip metrics); they are not
persisted to a database column. Masking provenance that survives to the audit
trail rides on the parent `traffic_event` (`compliance_tags`) and the redacted
markers inside the normalized payload.

## References

- `packages/shared/transport/wirerewrite/` — the wire-rewrite engine, rules, and circuit breaker
- `packages/shared/transport/wirerewrite/engine.go` — `NormalizeKey` / `NormalizeUpstream` + reload
- `packages/shared/transport/wirerewrite/bundled.go` — factory-default rule set
- `packages/shared/transport/wirerewrite/config.go` — `Config` / `Rule` types + package overview
- `packages/shared/transport/wirerewrite/circuit.go` — per-rule circuit breaker
