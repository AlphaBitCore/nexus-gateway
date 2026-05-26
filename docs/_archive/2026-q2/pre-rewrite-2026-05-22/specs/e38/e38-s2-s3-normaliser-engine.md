# E38-S2/S3 — L0 Cache Key + Body Normaliser Rule Engine

> Stories: e38-s2, e38-s3
> Epic: 38 (Prompt Cache Friendliness)
> Status: Approved

## User Story

As a Platform Admin, I want the AI Gateway to strip volatile non-semantic
bytes from request bodies before upstream dispatch (and before Nexus L1
cache key computation), so that upstream provider prompt caches hit
reliably on multi-turn conversations.

## Tasks

- T1: Create `packages/ai-gateway/internal/normaliser` package
  - `Config` struct (loaded from system_metadata)
  - `Engine` type with `atomic.Pointer[Config]` for hot-swap
  - `NormalizeKey(format, body) []byte` — L0, key-safe rules only
  - `NormalizeUpstream(format, body) ([]byte, Result)` — L3+L4
  - `Result` struct: `StripCount int`, `StripBytes int`, `MarkersInjected int`, `DryRun bool`
  - Per-rule circuit breaker (10 errors / 60 s → disable + alert)
  - Bundled rules registry
- T2: Bundled rule: `anthropic/claude-code-cch-strip`
  - regex `cch=[0-9a-f]+;` on `system[*].text` paths
  - `key_normalize_safe: true`
- T3: Bundled rule: `openai/field-order-normalize`
  - Canonical JSON key ordering for stable hash
  - `key_normalize_safe: true`, `enabled_by_default: true`
- T4: Wire `NormalizeKey` into `proxy.go` between `PrepareBody` and `Cache.BuildKey`
- T5: Wire `NormalizeUpstream` into `proxy.go` after L1 MISS, before `runViaBroker`
- T6: Set `rec.NormalizedStripCount`, `rec.NormalizedStripBytes`, `rec.CacheMarkerInjected` from Result
- T7: Hub shadow config push → normaliser `Engine.Reload(cfg)` in `thingclient.OnConfigChanged`
- T8: Unit tests: engine, cch= rule, field-order rule, key vs upstream separation, fail-open, circuit breaker

## Acceptance Criteria

- AC1: Two consecutive Claude Code requests differing only in `cch=<hex>` produce the same Nexus L1 cache key (L0 active) without modifying the bytes forwarded to Anthropic.
- AC2: When global switch is OFF, `NormalizeUpstream` returns the original body unchanged and `StripCount=0`.
- AC3: When global switch is ON and `cch=` rule is enabled, the `cch=<hex>` token is absent from the upstream request bytes and `StripCount=1`.
- AC4: Injecting a deliberate panic in a rule does not block the request; the original body is forwarded and a metric is incremented.
- AC5: After 10 rule errors in 60 s, the circuit breaker disables the rule; subsequent requests skip it.
- AC6: Config reload (simulate Hub push) takes effect on the next request with no downtime.
- AC7: `go test -race -count=1 ./packages/ai-gateway/internal/normaliser/...` passes.

## Package Design

```
packages/ai-gateway/internal/normaliser/
  engine.go       — Engine, atomic.Pointer[resolvedConfig], NormalizeKey, NormalizeUpstream
  config.go       — Config, Rule, RuleType, BundledRuleID constants
  rule_strip.go   — stripRule: regex path match + byte removal
  rule_field_order.go — fieldOrderRule: canonical JSON field ordering
  circuit.go      — per-rule circuit breaker
  bundled.go      — bundledRules() returning all bundled Rule definitions
  engine_test.go
  rule_strip_test.go
  rule_field_order_test.go
  circuit_test.go
```

### Key types

```go
type Rule struct {
    ID              string
    AdapterType     providers.Format
    Type            RuleType   // strip | field_order_normalize | cache_control_inject
    Enabled         bool
    DryRunAlways    bool
    KeyNormalizeSafe bool
    // strip-rule fields
    BodyPath        string   // gjson path pattern
    Regex           *regexp.Regexp
}

type Result struct {
    StripCount      int
    StripBytes      int
    MarkersInjected int
    DryRun          bool  // true when rules ran in dry-run mode only
}
```

## Ordering invariant

`NormalizeKey` and `NormalizeUpstream` MUST both run **after**
`adapter.PrepareBody` and **after** the hook pipeline. The hook
pipeline sees the original client body (PII, compliance). The normaliser
sees the provider-wire body (post-codec, post-hook-rewrite). A unit test
in `proxy_test.go` pins this order by asserting `NormalizeKey` is called
with the `PrepareBody` output, not the original ingress body.

## system_metadata config shape

```json
{
  "prompt_cache": {
    "normaliser_enabled": false,
    "rules": {
      "anthropic": {
        "claude-code-cch-strip": { "enabled": false },
        "anthropic-cache-marker-inject": { "enabled": false }
      },
      "openai": {
        "openai-field-order-normalize": { "enabled": true }
      }
    },
    "providers": {
      "<provider_id>": {
        "cache_marker_inject_enabled": false,
        "cache_marker_boundary3_enabled": false,
        "extended_ttl_enabled": false
      }
    },
    "global": {
      "extended_ttl_enabled": false
    }
  }
}
```
