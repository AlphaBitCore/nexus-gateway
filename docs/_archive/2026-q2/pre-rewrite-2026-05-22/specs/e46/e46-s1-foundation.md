# E46 S1 — Foundation: schema, types, BlockSoft rename

**Epic:** E46
**Requirements:** [e46-traffic-normalization.md](../../../../docs/developers/specs/e46/e46-traffic-normalization.md)
**OpenAPI:** [e46-s1-foundation.yaml](../../../../docs/users/api/openapi/ai-gateway/e46-s1-foundation.yaml)
**Status:** Approved (auto-approved per user's "execute all" directive 2026-05-13)
**Date:** 2026-05-13

---

## Architecture summary

Phase 0 introduces the shared types, the new sidecar storage table, and the cross-cutting rename of `Decision.RejectSoft` → `Decision.BlockSoft` without changing any runtime behavior. Subsequent phases consume these foundations. The Phase 0 PR must build, test, vet, and lint clean across all modules and may not introduce any half-implemented runtime path.

The new package `packages/shared/transport/normalize/` exposes:

- `NormalizedPayload` (kind-discriminated union of AI-shape and HTTP-shape fields)
- `NormalizedRequest` / `NormalizedResponse` (specialisations)
- `ContentBlock` (text / image_ref / tool_use / tool_result / reasoning)
- `RedactionSpan` (rule_id, content_index, start, end, replacement)
- `Normalizer` interface (`Normalize(rawBytes, meta) (NormalizedPayload, error)`)
- `NormalizerRegistry` (provider+content-type → Normalizer)
- `ErrUnsupported` sentinel for "this normalizer cannot handle the input"

`packages/shared/policy/hooks/types.go` gains `OnMatchConfig`, `InflightAction`, `StorageAction` string-typed enums, plus a closed-set `ReasonCode` constant list.

`tools/db-migrate/schema.prisma` gains a new `traffic_event_normalized` model (1:1 sidecar to `traffic_event`).

The `aiguard.normalizeContent` helper is renamed to `canonicalizeForCacheKey` so the name does not collide with the shared normalize concept.

---

## Story

### S1 — Foundation: shared types, sidecar table, BlockSoft rename

**User story:** As a Nexus platform engineer, I want the shared type vocabulary and the storage table for normalized payloads in place so that subsequent phases can plug in their normalizers and hook adaptations without re-litigating schema.

**Tasks:**

- **T1.1** — Create `packages/shared/transport/normalize/` package:
  - `types.go` — `NormalizedPayload`, `NormalizedRequest`, `NormalizedResponse`, `ContentBlock`, `RedactionSpan`, `Kind` string enum, `Direction` string enum.
  - `normalizer.go` — `Normalizer` interface, `ErrUnsupported`, `NormalizeMeta` struct (provider, model, content_type, direction).
  - `registry.go` — `NormalizerRegistry` (freezable, similar pattern to `hooks.HookRegistry`).
  - `metrics.go` — Prometheus stubs (`normalize_total{adapter,kind,status}`, `normalize_latency_ms`, `normalize_payload_bytes`, `normalize_fallback_total`). Uses `promauto`, accepts namespace parameter.
  - `doc.go` — package-level documentation describing kind discrimination, three-side consistency, and the contract that production code never mutates inputs.
- **T1.2** — Add `OnMatchConfig` and related types to `packages/shared/policy/hooks/types.go`:
  - `OnMatchConfig struct { InflightAction InflightAction; StorageAction StorageAction; Replacement string }`
  - `InflightAction` enum: `InflightApprove`, `InflightBlockHard`, `InflightBlockSoft`, `InflightRedact`
  - `StorageAction` enum: `StorageKeep`, `StorageRedact`, `StorageDropContent`
  - `ReasonCode` constants: `ReasonRedactInflightUnsupported`, `ReasonRedactStorageOnlyByPolicy`, `ReasonStorageDroppedByPolicy`, `ReasonAIGuardSuggestedVsPolicy`. Delete `MODIFY_DOWNGRADED_TO_REJECT` constant references.
  - Add `RedactionSpan` import re-export from `shared/normalize` to avoid hooks taking a circular dep on normalize.
  - Add `HookConfig.ApplicableTrafficKinds []string` field with JSON/YAML tag.
- **T1.3** — Rename `Decision.RejectSoft` → `Decision.BlockSoft` across the entire repo:
  - `packages/shared/policy/hooks/types.go` — enum constant rename.
  - All Go callsites in `packages/ai-gateway`, `packages/compliance-proxy`, `packages/agent`, `packages/shared` (about 51 callsites identified in P0.1 audit).
  - Metric label literals (`"REJECT_SOFT"` → `"BLOCK_SOFT"`).
  - AI-Guard prompt templates in `packages/ai-gateway/internal/pipeline/aiguard/prompt.go` (judge output `reject_soft` → `block_soft`).
  - Existing tests updated to the new constant + new literal.
- **T1.4** — Rename `aiguard.normalizeContent` → `canonicalizeForCacheKey` in `packages/ai-gateway/internal/pipeline/aiguard/normalize.go` and update all callsites + tests.
- **T1.5** — Prisma migration `20260516000000_e46_traffic_event_normalized`:
  - New table `traffic_event_normalized` (FK PK to `traffic_event.id`, on delete cascade):
    - `traffic_event_id  TEXT PK FK`
    - `request_normalized JSONB NULL`
    - `response_normalized JSONB NULL`
    - `request_status TEXT NULL  -- "ok" | "partial" | "failed"`
    - `response_status TEXT NULL`
    - `request_error_reason TEXT NULL`
    - `response_error_reason TEXT NULL`
    - `request_redaction_spans JSONB NULL  -- []RedactionSpan`
    - `response_redaction_spans JSONB NULL`
    - `normalize_version TEXT NOT NULL  -- e.g. "1"`
    - `created_at TIMESTAMPTZ(3) NOT NULL DEFAULT NOW()`
  - Index `(request_status)`, `(response_status)` for "show me failed normalizations" admin queries.
  - Update `traffic_event` Prisma model with `normalized traffic_event_normalized?` relation.
- **T1.6** — Update `tools/db-migrate/seed/seed.ts` if any seeded hook config references the old action vocabulary or the old `reject_soft` value. Phase 0 keeps existing hook configs functional; only literal `"reject_soft"` → `"block_soft"` rewrites are in scope (action vocab unification is S4).
- **T1.7** — `go build`, `go test -race -count=1`, `go vet`, `golangci-lint run` clean per module. `npx prisma migrate dev --create-only` then apply to local DB to verify the migration is correct.

**Acceptance:**

- **AC-S1.1** — `grep -rn "RejectSoft\b" packages/ tools/` returns zero hits (Go identifier fully renamed).
- **AC-S1.2** — `grep -rn "REJECT_SOFT" packages/ tools/` returns zero hits (uppercase wire format fully renamed).
- **AC-S1.3** — `grep -rn "reject_soft" packages/ai-gateway/internal/pipeline/aiguard/ tools/db-migrate/seed/seed-aiguard.ts` returns zero hits (AI-Guard judge prompt format fully renamed). Other lowercase `reject_soft` occurrences remain in legacy hook-config parsing paths (`content_safety.go`, seed `seed-hook-configs.ts`, `admin_extras.go` JSON schema enum) — these are the legacy `action: "reject_soft"` *config* vocab that the S4 unified-onMatch refactor replaces wholesale. Phase 0 does NOT touch them.
- **AC-S1.3** — `grep -rn "aiguard\.normalizeContent\|^func normalizeContent" packages/ai-gateway/internal/pipeline/aiguard/` returns zero hits; `canonicalizeForCacheKey` is the only name in use.
- **AC-S1.4** — `go build ./...` and `go test -race -count=1 ./packages/...` pass.
- **AC-S1.5** — `npx prisma migrate dev` applies the new migration without error; `\d traffic_event_normalized` in psql shows the expected columns and FK.
- **AC-S1.6** — The new `shared/normalize` package contains only types, interfaces, registry plumbing, and metric stubs — no concrete normalizer implementations (those land in subsequent phases). `go vet` and `golangci-lint` pass.
- **AC-S1.7** — `MODIFY_DOWNGRADED_TO_REJECT` is gone from the source tree; the corresponding pipeline branch at `packages/shared/compliance/pipeline.go:269-281` is left in place for Phase 4 to delete (it is unreachable only once cp/agent set `allowModify=true`).
