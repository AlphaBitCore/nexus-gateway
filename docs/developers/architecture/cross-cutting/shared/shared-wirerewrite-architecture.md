# Shared wirerewrite architecture

`wirerewrite` is the byte-level rewriter that runs on the adapter-wire request
body just before two points: hashing the Nexus L1 cache key, and sending the body
upstream to the provider. It exists to make equivalent requests hash to the same
cache key (so caching actually hits) and to inject the provider-specific markers
that turn on upstream prompt caching.

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
  active**, independent of the global enable switch. This is what lets two
  requests that differ only in a volatile field land on the same
  [L1 cache](../storage/cache-multi-tier-architecture.md) entry.
- **`NormalizeUpstream(format, providerID, body)`** — strips and/or injects bytes
  in the body that *will* be forwarded to the provider, returning the modified body
  and a `Result` for audit. It runs after an L1 miss, before the request goes to
  the broker, and is gated by the global `normaliser_enabled` switch.

## 2. The engine

The `Engine` is constructed once at startup and holds its compiled rule set in an
atomic pointer. It loads the bundled rules immediately so it is operational before
any config arrives, and `Reload` rebuilds an immutable snapshot and swaps it
atomically — in-flight calls finish against the previous snapshot.

Each rule runs through a panic-recovering wrapper, and work is layered:

- **L0 key-normalise** — the key-safe subset of rules, applied by `NormalizeKey`.
- **L3 strip** — the strip rules, applied by `NormalizeUpstream`.
- **L4 cache-control inject** — `cache_control` marker injection for the Anthropic
  and Bedrock wire (Bedrock Claude uses the Anthropic Messages format), gated
  per-provider.

## 3. Rules

A rule is scoped to one adapter type and carries a transform type, default and
override enable flags, a dry-run flag, a `KeyNormalizeSafe` flag (whether it may
run during cache-key normalisation), and — for strip rules — a `gjson` body path
plus a compiled regex. Two transform types exist: `strip` and
`cache_control_inject` (the last is driven by per-provider config rather than a
bundled rule).

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
  wire. It removes Claude Code's `cch=<hex>` billing nonce from the system-prompt
  text, so consecutive Claude Code sessions sharing an identical system prompt hash
  to the same key. Strip rules select a `gjson` path, apply the regex to the matched
  string values, and write the result back with `sjson`.

Marker injection (L4) adds `cache_control: ephemeral` markers to the Anthropic-wire
body when a provider has it enabled, optionally including the conversation-history
boundary; this is what makes the upstream provider cache the prompt.

## 4. Configuration and hot-reload

Config is projected from the `cache` config key (`configkey.Cache`): the Control
Plane assembles the cache-config blob and pushes it to the AI Gateway shadow, which
projects that blob into the wirerewrite `Config` on reload. Its zero value is a safe
all-off default. It carries the global `normaliser_enabled` gate,
per-adapter per-rule overrides (`enabled`, `dry_run_always`), and per-provider
marker-injection settings keyed by the Provider UUID. A config change rebuilds the
engine's snapshot through `Reload`.

The on-the-wire identifiers — the `normaliser_enabled` JSON tag and the rule IDs
such as `claude-code-cch-strip` — are stable admin/shadow/database identifiers.
They are preserved verbatim, so renaming one is a coordinated config migration, not
a local refactor.

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

The outcome of `NormalizeUpstream` is a `Result` with strip counts, injected-marker
count, a dry-run flag, and byte-level `TransformSpan` records. Those spans are
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
