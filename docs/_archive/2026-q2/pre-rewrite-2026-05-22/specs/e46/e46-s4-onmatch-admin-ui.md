# E46 S4 — Unified onMatch hook config + admin UI

**Epic:** E46
**Requirements:** [e46-traffic-normalization.md](../../../../docs/developers/specs/e46/e46-traffic-normalization.md)
**OpenAPI:** [e46-s4-hook-config.yaml](../../../../docs/users/api/openapi/ai-gateway/e46-s4-hook-config.yaml)
**Status:** Approved
**Date:** 2026-05-13

---

## Architecture summary

Phase 3 collapses five different hook action vocabularies into one. Every content-touching hook reads `cfg.Config.onMatch` exactly. Storage and inflight actions are independent, and `applicableTrafficKinds` (default `["ai"]`) controls per-kind dispatch in the pipeline.

The admin UI's hook editor presents a single action matrix (inflight × storage) and a `replacement` template field for redact modes. Each hook implementation retains its hook-specific configuration (patterns, severity thresholds, rule-pack bindings) — `onMatch` only replaces the *decision* portion of the legacy config.

---

## Story

### S4 — onMatch unified config + admin surface

**User story:** As a security admin, I configure all content-touching hooks via one consistent action shape — inflight action, storage action, replacement template — instead of learning four legacy schemas.

**Tasks:**

- **T4.1** — Rewrite hook factories to read `cfg.Config["onMatch"]` and validate against `hooks.OnMatchConfig`:
  - `pii_detector.go` — drop the legacy `action: "block"|"warn"|"redact"` field, drop the inline `replacement` argument (now under `onMatch`).
  - `keyword_filter.go` — drop the per-pattern `severity` field; one `onMatch` applies to all matched patterns. Per-pattern severity overrides go into the rule-pack engine, not keyword-filter.
  - `content_safety.go` — drop the legacy `action: "reject_hard"|"reject_soft"`.
  - `rulepack_engine.go` — rule severity (`hard|soft|info`) is now a *hint*; the effective Decision is computed by combining rule severity with `onMatch.inflightAction`. info-severity rules still emit tags only.
  - `quality_checker.go` — drop `blockOnAnomaly: bool`; use `onMatch.inflightAction` (`approve` = log-only; `block-soft` = today's `blockOnAnomaly=true`).
  - `webhook_forward.go` — gains `payloadMode: "full"|"redacted"|"metadata-only"` next to `onMatch`. Default `redacted`.
- **T4.2** — `HookConfig.ApplicableTrafficKinds` flows through the loader; pipeline filters hooks before execution per `NormalizedPayload.kind`.
- **T4.3** — Admin API `POST/PUT /api/admin/hooks` validates `onMatch` server-side. OpenAPI spec at `docs/users/api/openapi/ai-gateway/e46-s4-hook-config.yaml`. IAM gating unchanged (`iam.ResourceHook` actions).
- **T4.4** — Generic dry-run endpoint `POST /api/admin/hooks/{id}/dry-run` accepts a `NormalizedPayload` body and returns the `HookResult` without side effects (no traffic_event written). Replaces the per-hook test scaffolds currently in `packages/control-plane/internal/handler/`.
- **T4.5** — Admin UI hook editor:
  - One "Action" panel rendering the inflight × storage matrix as labelled radio groups with inline help.
  - `replacement` text input shown only when `redact` is selected on either axis.
  - `applicableTrafficKinds` multi-select (defaults to "AI traffic only").
  - i18n keys added to `en / zh / es` `nav.json` / `pages.json`; verify count parity.
- **T4.6** — Update `tools/db-migrate/seed/seed.ts` to use the new onMatch shape for every seeded hook. Old `action:` / `severity:` literals are removed from the seed.
- **T4.7** — IAM impact audit per CLAUDE.md (no new admin resources; the existing `iam.ResourceHook` covers; the `/hooks/{id}/dry-run` route gets `VerbProbe`).

**Acceptance:**

- **AC-S4.1** — A `POST /api/admin/hooks` with `config.onMatch = {inflightAction:"block-hard", storageAction:"redact", replacement:"[REDACTED_<id>]"}` succeeds and the runtime hook honors both axes independently (verified by integration test: upstream provider sees the original body, `traffic_event_normalized.request_normalized` has the matched span replaced).
- **AC-S4.2** — `onMatch.inflightAction = "approve"` + `storageAction = "redact"` results in: upstream call proceeds unchanged, `traffic_event_normalized` row has redacted content, `traffic_event.request_hook_decision = "approve"`, `traffic_event.request_hook_reason_code = "REDACT_STORAGE_ONLY_BY_POLICY"`.
- **AC-S4.3** — `onMatch.storageAction = "drop-content"` results in `traffic_event_normalized.request_normalized = {"redacted":true, "kind":"ai-chat", "rule_ids":[…]}` with no message text persisted. Decision/metadata still recorded.
- **AC-S4.4** — `applicableTrafficKinds=["ai"]` (default) skips hook execution for `http-*` kinds; the hook does not appear in `traffic_event.request_hooks_pipeline` for non-AI traffic.
- **AC-S4.5** — `POST /api/admin/hooks/{id}/dry-run` returns the hook result for a synthetic `NormalizedPayload` without writing any audit row.
- **AC-S4.6** — Admin UI hook editor renders the new action matrix in `en / zh / es` with no untranslated keys (i18n parity check passes).
