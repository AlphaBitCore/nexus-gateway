---
doc: pii-redaction-policy-architecture
area: cross-cutting
service: safety
tier: 1
updated: 2026-05-20
---

# PII Redaction Policy Architecture

> **Tier 2 architecture doc.** Read when touching PII detection code, redaction primitives, or any path where user content lands in logs / audit / SIEM forwards. Cross-references: `hook-architecture.md` §6 (PII detector hook), `audit-pipeline-architecture.md` §6 (emit-time redaction).

PII redaction is a defence-in-depth across multiple layers. The PII detector hook scrubs request / response bodies; the audit emit step scrubs known-sensitive fields; the SIEM bridge scrubs configurable fields; logs are scrubbed at write time. Defence-in-depth because a single layer failing is much more likely than all four.

---

## 1. Three primitives

| Primitive | Output | Reversibility |
|---|---|---|
| **Hash** | SHA-256 (or HMAC-SHA-256 with a per-tenant secret) | One-way; analytics can still detect duplicates (same input → same hash) |
| **Token** | Stable per-tenant opaque string (`tok_abc123`) | One-way for external readers; tenant-controlled token-to-value reversal table optional |
| **Mask** | `***` or `<PII redacted>` | One-way; value unrecoverable |

Use cases:

- **Hash** when investigators need to verify "is this the same value as another row?" without seeing the value.
- **Token** when investigators need a stable identifier for joining across systems without exposing the value.
- **Mask** when the value should disappear entirely.

## 2. Where redaction happens

| Layer | Trigger | Coded today? |
|---|---|---|
| Hook pipeline | `HookConfig.onMatch` (`inflightAction=redact`, `storageAction=redact`, etc.) | YES — `packages/shared/policy/hooks/validators/pii_detector.go` + the rulepack engine (`rulepack_engine.go`). Acts on request / response bodies pre-forward and on the stored audit payload. |
| Audit emit | `packages/shared/audit/` | PARTIAL — per-caller helpers. No central `redact.go` file. Authorization / Cookie / API-key headers and the well-known body fields below are scrubbed by the emitters that stamp them, not by a shared helper library. |
| SIEM bridge | Per-channel config | PARTIAL — there is no `packages/shared/siem/` package; SIEM forwarding lives in `packages/nexus-hub/internal/siem/` (or equivalent) and applies a fixed denylist at marshal time. Per-channel config knobs are not yet exposed in admin UI. |
| Log writers | `packages/shared/core/logging/` | PARTIAL — slog handlers do not run a separate redact pass; they rely on callers never logging raw credentials. The `pii_detector` + audit-emit layers are the actual defences; the logging layer is best-effort. |

A field redacted at the hook layer is also redacted downstream (the audit event already has the redacted value). Defence-in-depth covers the case where a hook mis-classifies and lets data through — the later layers catch known-sensitive shapes via their callers' own scrubbing.

**Status: partial.** Only the hook-pipeline layer is implemented as a coherent centralised module. The "audit-emit redact list" and "log-writer redact pass" are aspirational — see §5 for the binding contract and the current gap.

## 3. The PII detector hook (config shape)

The detector does not ship a hard-coded category catalog. `pii-detector` takes a `patternDefinitions` array on `HookConfig.Config`, each entry of the form `{ id, regex, flags, luhn, replacement }`. Compile-time regex caching is shared (`core.CompilePattern`); a per-pattern `luhn: true` additionally validates matches with the Luhn algorithm to suppress false-positive credit-card hits.

When `_rulePackInstalls` is attached to the config (`rulepack_engine.go`), the factory delegates to the unified rule-pack engine — admins curate pattern packs (Email, Phone, US-SSN, IBAN, JWTs, API keys, private-key blocks, etc.) at the policy layer and the detector consumes whatever the pack ships.

## 4. `onMatch` strategy

`HookConfig.Config.onMatch` (`OnMatchConfig`) controls behaviour per pattern:

- `inflightAction`: `block-hard | block-soft | redact | approve` (default `block-hard`).
- `storageAction`: `redact | keep | drop-content`.
- `replacement`: template string (e.g., `[REDACTED_<RULE_ID>]`); a per-pattern `replacement` overrides the template for that pattern's hits.

Combining `inflightAction=redact` + `storageAction=redact` is the canonical "let the request through with PII scrubbed and store the scrubbed copy" path. The rule pack chooses sensible defaults per category; admins override per route.

## 5. Always-on audit redactions (binding contract)

**Binding contract.** Regardless of hook config, every audit emit path MUST redact:

- HTTP `Authorization` header value → `Bearer [redacted]`.
- HTTP `Cookie` header value → `[redacted]`.
- `X-API-Key`, `X-Auth-Token`, `Api-Key` and similar → `[redacted]`.
- Request body fields named exactly: `password`, `api_key`, `client_secret`, `refresh_token`, `access_token`, `private_key`, `secret`.

**Current implementation status.** There is no central `packages/shared/audit/redact.go` library. Each caller that stamps an audit row is responsible for scrubbing the fields above before handing the row to `packages/shared/audit/event.go`. Spot reviews confirm the major call sites (admin API handlers, hub audit upload, compliance-proxy event emission) do scrub correctly — but the rule lives in conventions + reviewer attention, not in code.

**Open follow-up.** Extract the denylist + scrubber into a single helper in `packages/shared/audit/` (e.g., `scrub.go`) and route every caller through it. Until that lands, new audit-emitting code MUST be reviewed against this list. New entries to the binding list require a security-review signoff (not auto-mergeable from a contributor).

## 6. Reversibility considerations

- **HMAC-hash with tenant secret**: same input → same hash within a tenant. The tenant secret is rotated periodically; old hashes lose their join utility.
- **Token table** (optional): admin can opt-in to a token-to-value reversal table stored encrypted (AES-256-GCM). Useful for compliance investigations where the value is needed retroactively. Default: OFF.
- **Mask**: irreversible.

## 7. Field-level vs body-level

- **Field-level**: redact specific JSON fields by path (`$.messages[*].content` → mask content). Used when the request is structured.
- **Body-level**: scan the whole body for pattern matches. Used for free-form text where the position of PII is unknown.

Both run; field-level first, body-level catches anything missed.

## 8. Audit + observability of redactions

Every redaction emits observable signal:

- Hook-decision rows on `traffic_event` carry the matched hook config + outcome (`block-hard` / `block-soft` / `redact` / `approve`); investigators can correlate hook hits to which PII categories matched.
- Counter metric `pii_redactions_total{category, strategy}` for analytics.
- Alerts can fire on unusual redaction rates (sudden spike → upstream sending more PII than usual).

There is no dedicated `traffic_event.redactions_applied` JSONB column. The category-level breakdown is reconstructed from the hook-decision rows joined to the PII detector's pattern IDs.

## 9. False positives

The PII detector's regexes are tuned for low-false-negative (we'd rather mask a non-PII value than leak a real one). False positives surface as:

- Random base64-looking strings flagged as JWTs.
- Pure-numeric IDs flagged as credit cards (Luhn validator catches most).
- Email-like strings in code samples.

Admin can configure per-route exemptions (e.g., for routes that legitimately carry email-shaped IDs).

## 10. Compliance posture

Different jurisdictions / industries require different defaults:

- **HIPAA**: stricter (mask everything; no token).
- **GDPR**: token + right-to-delete on the token table.
- **PCI-DSS**: credit cards must be masked except last-4.

Admin configures per-tenant defaults via the Compliance / Hooks surface.

<!-- 💡 follow-up: extract the §5 always-on denylist into a shared `packages/shared/audit/scrub.go` helper and route every audit emitter through it. Until that lands, reviewer attention enforces the binding. -->

## 11. Sources

- `packages/shared/policy/hooks/validators/pii_detector.go` — built-in PII detector (regex + Luhn + onMatch strategy).
- `packages/shared/policy/hooks/validators/rulepack_engine.go` — unified rulepack engine that handles `_rulePackInstalls` config.
- `packages/shared/policy/hooks/builtins/builtins.go` — registry registration (`r.Register("pii-detector", ...)`).
- `packages/shared/policy/hooks/core/` — `OnMatchConfig`, `inflightAction`, `storageAction`, `replacement` shared types.
- `packages/shared/audit/event.go` + `body.go` — audit emitter (callers responsible for pre-scrubbing per §5; centralised helper is a follow-up).
- Logging / SIEM redact passes: not yet a separate package — see §2.

## 12. Cross-references

- `hook-architecture.md` §6 — built-in PII Detector hook.
- `audit-pipeline-architecture.md` §6 — emit-time scrubbing.
- `siem-bridge-architecture.md` — SIEM forwards subject to redaction.
- `credentials-architecture.md` — credential plaintext is one of the always-on redactions.
- `data-retention-purge-architecture.md` — interaction with right-to-delete.
