# E37-S1 — Threshold Unification (`MaxInlineBodyBytes`)

Status: draft
Epic: E37 — Payload Capture Unification
Story: S1 — Collapse `MaxBodyBytes` and `InlineThresholdBytes` into one knob

## 1. Problem

Two thresholds govern payload capture today:

- `payloadcapture.Config.MaxBodyBytes` (admin-tunable, runtime via
  shadow): historically the "audit truncation cap". Comment claims
  `0 = unlimited` but three downstream sites override that to 64 KiB
  or to "capture nothing".
- `spillstore/spillfactory.FactoryConfig.InlineThresholdBytes` (YAML,
  static, default 256 KiB): inline-vs-spill cutoff fed to
  `spillstore.EmitBody`.

Result: an admin who raises the runtime cap cannot influence where
spill kicks in; an operator who tweaks the YAML threshold has to push
to every host. The two knobs drift, and the "0 = unlimited" semantic
is nominally promised but never actually delivered.

## 2. Goal

Replace both knobs with a single admin-tunable threshold
`MaxInlineBodyBytes` whose meaning is "bodies up to this size are
stored inline in the database; larger bodies are stored as files in
the configured spill backend".

After this story lands:

1. `Config.MaxBodyBytes` is gone. `Config.MaxInlineBodyBytes` exists.
2. `FactoryConfig.InlineThresholdBytes` is gone from the Go struct
   and from every `*.dev.yaml`.
3. Every call to `spillstore.EmitBody` passes `MaxInlineBodyBytes`
   as the threshold parameter.
4. `MaxInlineBodyBytes <= 0` coerces to `DefaultMaxInlineBodyBytes`
   (256 KiB) at the single decode boundary
   (`payloadcapture.DecodeConfigJSON`). Downstream callers carry no
   fallback constants of their own.
5. The wire JSON key changes from `maxBodyBytes` to
   `maxInlineBodyBytes`. The Hub Cat B aggregator and Control Plane
   admin write path use the new key only; the old key is rejected.

## 3. Non-goals

- Pre-signed URL flow for the agent — see S2.
- Streaming-response capture coverage — see S3.
- Reader-side `ContentType` symmetry — covered alongside agent
  changes in S2 (since the resolver lives next to the
  audit-consumer plumbing).

## 4. Design

### 4.1 `shared/payloadcapture` rename + constant change

`packages/shared/policy/payloadcapture/config.go`:

```go
// DefaultMaxInlineBodyBytes is the inline-vs-spill cutoff used when the
// admin has not set an explicit value. 256 KiB matches Postgres' efficient
// JSONB inline range. Bodies at or below this size are stored inline on
// traffic_event_payload.inline_*_body; larger bodies are spilled to the
// configured SpillStore backend by the producer.
const DefaultMaxInlineBodyBytes int64 = 256 * 1024

type Config struct {
    StoreRequestBody  bool
    StoreResponseBody bool

    // MaxInlineBodyBytes is the inline-vs-spill cutoff for the captured
    // copy that hits the audit pipeline. Bodies <= this size travel as
    // JSONB on traffic_event_payload.inline_*_body; larger bodies are
    // written to the SpillStore backend by the producer and the row keeps
    // a *_spill_ref. Coerced to DefaultMaxInlineBodyBytes when <= 0.
    MaxInlineBodyBytes int64

    MaxRequestBytes  int64
    MaxResponseBytes int64
}
```

`DefaultConfig()` returns `MaxInlineBodyBytes = DefaultMaxInlineBodyBytes`
(not 0). The "0 = unlimited" comment is removed.

### 4.2 Wire format

`payloadcapture.configWire`:

```go
type configWire struct {
    StoreRequestBody   bool  `json:"storeRequestBody"`
    StoreResponseBody  bool  `json:"storeResponseBody"`
    MaxInlineBodyBytes int64 `json:"maxInlineBodyBytes"`
    MaxRequestBytes    int64 `json:"maxRequestBytes"`
    MaxResponseBytes   int64 `json:"maxResponseBytes"`
}
```

`DecodeConfigJSON` coerces:

```go
if cfg.MaxInlineBodyBytes <= 0 {
    cfg.MaxInlineBodyBytes = DefaultMaxInlineBodyBytes
}
```

No more `< 0` vs `== 0` split. The tests in `config_test.go` are
updated accordingly.

### 4.3 `spillfactory.FactoryConfig` shrink

`packages/shared/storage/spillstore/spillfactory/factory.go`:

- Delete the `InlineThresholdBytes` field.
- Delete the `InlineThreshold()` method.
- Delete the `InlineThresholdDefault` constant.
- The factory still owns backend selection (`Backend`, `Localfs`,
  `S3`) and storage-policy fields (`PerObjectCap`, retention). These
  are unaffected.

### 4.4 Caller migration

Every `EmitBody` site passes the runtime `MaxInlineBodyBytes` value:

| File | Old | New |
|---|---|---|
| `ai-gateway/internal/audit/audit.go:577-583` | `cfg.Spill.InlineThreshold()` (via `WithSpillStore`) | `pcCfg.MaxInlineBodyBytes` |
| `compliance-proxy/internal/compliance/emitter.go:189-203` | `cfg.Spill.InlineThreshold()` | `pcCfg.MaxInlineBodyBytes` |
| `ai-gateway/cmd/ai-gateway/main.go` | wires `cfg.Spill.InlineThreshold()` to `WithSpillStore` | wires `payloadcapture.Store` and pulls `MaxInlineBodyBytes` per request |
| `compliance-proxy/cmd/compliance-proxy/init.go` | same | same |

Hub's `nexus-hub/internal/handler/agent_audit.go` no longer calls
`EmitBody` (S2 covers that) so it has no threshold dependency.

### 4.5 Removed `clipForAudit`

`ai-gateway/internal/handler/proxy.go::clipForAudit` and its three
call sites (`proxy.go:267`, `proxy.go:1569`, `proxy_cache.go:354`,
`proxy_cache.go:774`) are deleted. With the unified threshold, the
captured bytes flow directly into `EmitBody`, which decides
inline / spill in one place.

### 4.6 `cappedTeeWriter` rename + semantic

`cappedTeeWriter` is renamed to `streamCaptureTee`. It buffers up to
`spill.perObjectCap` (a hard ceiling, default 256 MiB) instead of
`MaxInlineBodyBytes`. The end-of-stream `EmitBody` decides inline
vs spill. `Truncated=true` flips when the cap is exceeded.

(Detailed wiring covered in S3; the rename is in S1 because the
type lives in the same file as `clipForAudit` removal.)

### 4.7 Hub Cat B loader + Control Plane admin write

`nexus-hub/internal/store/catb_agent_payload_capture.go` updates the
struct field and JSON key. Default fallback flips to
`DefaultMaxInlineBodyBytes`.

`packages/control-plane/internal/handler/admin_extras.go` (or
wherever payload_capture admin writes happen) accepts
`maxInlineBodyBytes` only. A request with the old `maxBodyBytes`
key returns `400 BAD_REQUEST` with a hint about the rename.

## 5. Tasks

- T1 — Rename `Config` field + constant + `configWire` field +
  decode coercion. Update `payloadcapture` unit tests.
- T2 — Delete `FactoryConfig.InlineThresholdBytes` /
  `InlineThreshold()` / `InlineThresholdDefault`. Update factory
  tests.
- T3 — Migrate every `EmitBody` caller to use
  `pcCfg.MaxInlineBodyBytes`. Delete `clipForAudit`. Rename
  `cappedTeeWriter` to `streamCaptureTee` and make it
  `perObjectCap`-bounded.
- T4 — Hub Cat B + Control Plane admin write update. Reject the old
  wire key with a clear error.
- T5 — Reseed `system_metadata['payload_capture.config']` with the
  new key. The seed lives in `tools/db-migrate/seed/`.
- T6 — Delete `spill.inlineThresholdBytes` from every `*.dev.yaml`.

## 6. Acceptance criteria

- AC1 — `git grep MaxBodyBytes` returns zero hits in production code
  (test files referencing the OLD wire shape may exist only as
  malformed-input fixtures).
- AC2 — `git grep InlineThresholdBytes` returns zero hits.
- AC3 — `clipForAudit` is gone; `git grep clipForAudit` returns zero.
- AC4 — A unit test feeds a 100 KiB body and a 1 MiB body through
  `EmitBody` with `MaxInlineBodyBytes = 256 KiB`; the first emits
  inline, the second emits spill, both with the correct
  `SizeBytes` and `Truncated=false`.
- AC5 — `DecodeConfigJSON([]byte("{\"maxInlineBodyBytes\":0}"))`
  returns `Config{MaxInlineBodyBytes: 256*1024, ...}`.
- AC6 — `DecodeConfigJSON([]byte("{\"maxBodyBytes\":1024}"))`
  returns the default config (the unknown key is ignored; the old
  key is not honored).
- AC7 — All four data-plane test suites (ai-gateway, compliance-
  proxy, agent, shared) pass under `go test -race -count=1`.

## 7. Risks

- **R1** — Reseed of `system_metadata` row drops in-flight admin
  edits made during the upgrade window. Pre-GA mitigates: a single
  flag-day reseed is acceptable.
- **R2** — Older test fixtures using `maxBodyBytes` keys must be
  updated; missed fixtures surface as decode errors at test time
  (loud failure, easy fix).
- **R3** — Stream `perObjectCap` fallback is now 256 MiB instead of
  the prior implicit 64 KiB. A misconfigured runtime can grow
  per-stream RSS by 256 MiB in worst case. Mitigated by the existing
  `spill.perObjectCap` YAML cap (operators set the real ceiling)
  and by F3.4's truncation flag.
