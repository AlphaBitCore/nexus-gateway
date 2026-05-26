# E59 — Response Header Namespace Cleanup (Tier 1 + Tier 2 keepers + attestation slot)

> Epic: 59
> Story: S2 (sibling to E59-S1 Cache UX Honesty)
> Status: Draft
> Date: 2026-05-19
> Architecture impact: `docs/developers/architecture/cross-cutting/foundation/nexus-response-markers.md` (canonical header inventory rewrite); `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md` § 6.4 (cache header rename)
> SDD: `docs/developers/specs/e59/e59-s2-header-namespace-cleanup.md`
> Relationship: Standalone refactor; follows E59-S1 (state model) shipped 2026-05-19 (commit 7d474f1e). Independent of E60 (attestation) — this story merely reserves the `x-nexus-attestation` slot.

---

## 1. Background

A strict audit (2026-05-19) of every `x-nexus-*` response header across `ai-gateway`, `compliance-proxy`, `agent`, and `nexus-hub` surfaced 38 distinct headers with several structural problems:

- **Single-value constants** carry zero information yet add bytes to every response (`x-nexus-aigw-mode: "proxied"`, `x-nexus-cp-mode: "mitm"`, `x-nexus-agent-mode: "mitm"`).
- **Duplicates by name**: `X-Cache` + `x-nexus-cache` (same value, dropped in E59-S1); `x-nexus-request-id` (TitleCase) + `x-nexus-request-id` (lowercase) + `x-nexus-aigw-request-id` (proxy.go) + `x-nexus-cp-request-id` (CP marker) all carry the same correlation ID.
- **Service-prefix without disambiguation value**: 17 ai-gateway-only-writer headers carry the `aigw-` prefix although no other service writes the same concept (e.g., `aigw-cache`, `aigw-routed-model`, `aigw-quota-*`).
- **Logic bugs**: `aigw-model` and `aigw-routed-model` are stamped from the same `target.ModelCode` — the pre-routing model is lost. Same for `aigw-provider` / `aigw-routed-provider`.
- **Dead entries**: `x-nexus-trace-id` and `x-nexus-agent-domain-rule` appear in `ExposeHeaders` but have no response-side writer.
- **Casing inconsistency**: `x-nexus-request-id` and `x-nexus-upgraded-to` are the only two in TitleCase; everything else is lowercase.

After E59-S2 the canonical set drops from 38 → ~22 headers. The Tier 1 keep-set + Tier 2 "valuable, keep after review" set are documented in `docs/developers/architecture/cross-cutting/foundation/nexus-response-markers.md`. The `x-nexus-attestation` slot is reserved schema-only for the future E60 epic.

---

## 2. Functional Requirements

### FR-1: Final canonical set of x-nexus-* response headers

| ID | Requirement | Priority |
|---|---|---|
| FR-1.1 | The full set of `x-nexus-*` response headers that any Nexus service writes is: `x-nexus-request-id`, `x-nexus-via`, `x-nexus-cache`, `x-nexus-routed-model`, `x-nexus-routed-provider`, `x-nexus-quota-used`, `x-nexus-quota-limit`, `x-nexus-quota-downgrade`, `x-nexus-quota-original-model`, `x-nexus-quota-warning`, `x-nexus-attempts`, `x-nexus-coerced`, `x-nexus-aigw-hook`, `x-nexus-cp-hook`, `x-nexus-agent-hook`, `x-nexus-agent-flow-id`, `x-nexus-cp-domain-rule`, `x-nexus-dry-run` (conditional), `x-nexus-estimate` (conditional), `x-nexus-upgraded-to` (conditional), `Server-Timing` (HTTP-standard, replaces the latency-ms / upstream-ttfb / upstream-total trio). No other `x-nexus-*` response header is set by any production code path. | Must |
| FR-1.2 | The `x-nexus-attestation` header is reserved by `shared/traffic/markers.go` ExposeHeaders but has no writer in E59-S2 (E60 will add). | Should |

### FR-2: Deletions

| ID | Requirement | Priority |
|---|---|---|
| FR-2.1 | Delete writers + ExposeHeaders entries for the following constant single-value headers: `x-nexus-aigw-mode`, `x-nexus-cp-mode`, `x-nexus-agent-mode`. | Must |
| FR-2.2 | Delete `x-nexus-aigw-allowlist-version` (opaque hash, no documented consumer). | Must |
| FR-2.3 | Delete `x-nexus-aigw-routing-rule` (internal rule name; debug-only; admin UI is the right place to expose routing decisions). | Must |
| FR-2.4 | Delete `x-nexus-aigw-stream` (visible from `Content-Type: text/event-stream`). | Must |
| FR-2.5 | Delete `x-nexus-aigw-model` and `x-nexus-aigw-provider` — they were always equal to `routed-model` / `routed-provider` due to a logic bug; the "pre-routing model" intent is preserved via request-body inspection by clients who care. | Must |
| FR-2.6 | Delete `x-nexus-aigw-quota-remaining` (derivable from limit-used) and `x-nexus-aigw-quota-period` (single value `monthly`). | Must |
| FR-2.7 | Delete `x-nexus-aigw-overhead-ms` (derivable from latency-ms − upstream-total-ms). | Must |
| FR-2.8 | Delete `x-nexus-aigw-latency-ms`, `x-nexus-aigw-upstream-ttfb-ms`, `x-nexus-aigw-upstream-total-ms` — replaced by HTTP-standard `Server-Timing` header (FR-3.4). | Must |
| FR-2.9 | Delete the stale `x-nexus-agent-domain-rule` from ExposeHeaders (no writer in production). | Must |
| FR-2.10 | Delete the stale `x-nexus-trace-id` from the response-side ExposeHeaders (it is a request-side header only; agent / CP set it on outbound upstream requests, never as a response marker). | Must |
| FR-2.11 | Delete the duplicate `x-nexus-aigw-request-id` writer in `proxy.go` (the value is already emitted as `x-nexus-request-id` by the middleware). | Must |
| FR-2.12 | Delete the duplicate `x-nexus-cp-request-id` writer in `tlsbump/markerhook.go` (same as FR-2.11 rationale for the CP path). | Must |

### FR-3: Renames

| ID | Requirement | Priority |
|---|---|---|
| FR-3.1 | Rename `x-nexus-cache` → `x-nexus-cache` (only ai-gateway writes the cache header; no disambiguation needed). | Must |
| FR-3.2 | Rename `x-nexus-routed-model` → `x-nexus-routed-model` and `x-nexus-routed-provider` → `x-nexus-routed-provider` (only ai-gateway writes routing decisions). | Must |
| FR-3.3 | Rename the five `x-nexus-aigw-quota-*` headers to `x-nexus-quota-*` (only ai-gateway writes quota). | Must |
| FR-3.4 | Rename `x-nexus-coerced` → `x-nexus-coerced` and `x-nexus-attempts` → `x-nexus-attempts` (single-writer). | Must |
| FR-3.5 | Rename `x-nexus-dry-run` → `x-nexus-dry-run`. | Must |
| FR-3.6 | Rename `x-nexus-upgraded-to` → `x-nexus-upgraded-to` (casing fix). | Must |
| FR-3.7 | Unify `x-nexus-request-id` (ai-gateway middleware TitleCase) and `x-nexus-request-id` (hub middleware lowercase) under a single canonical name `x-nexus-request-id` (lowercase). Both middleware sites use the same name. The first middleware that sees the request stamps; downstream middleware echoes if unset. | Must |
| FR-3.8 | Service-prefixed headers that stay prefixed because of multi-writer disambiguation: `x-nexus-aigw-hook`, `x-nexus-cp-hook`, `x-nexus-agent-hook` (three independent hook pipelines stamp their own result). | Must |

### FR-4: New header — Server-Timing

| ID | Requirement | Priority |
|---|---|---|
| FR-4.1 | Add HTTP-standard `Server-Timing` header to ai-gateway responses, replacing the deleted latency/ttfb/total trio. Format per RFC 8674: `Server-Timing: gw;dur=X.X, upstream-ttfb;dur=Y.Y, upstream-total;dur=Z.Z`. | Must |
| FR-4.2 | Browser DevTools renders `Server-Timing` natively in the Network → Timing tab — no custom parsing needed. | Must |

### FR-5: Logic-bug fixes

| ID | Requirement | Priority |
|---|---|---|
| FR-5.1 | `aigw-model` and `aigw-routed-model` were stamped from the same `target.ModelCode` (proxy.go:2002, 2012) — the pre-routing model intent never reached the response. After deleting both `aigw-model` and `aigw-provider` (FR-2.5), the response carries only the actual routed target. Clients that need the requested model read it from the request body. | Must |
| FR-5.2 | `aigw-hook` (and `cp-hook`, `agent-hook`) currently have multiple set sites — risk of inconsistent overwrites if request-hook and response-hook stages both write. Consolidate to a single finalize-stage write per service, emitting the combined two-stage outcome (e.g., `request:pii-detector:rejected` or `request:passed,response:transformed:redact`). | Must |

### FR-6: Architecture documentation

| ID | Requirement | Priority |
|---|---|---|
| FR-6.1 | `docs/developers/architecture/cross-cutting/foundation/nexus-response-markers.md` is rewritten to reflect the new canonical set. Per-header sub-sections retained only for the ~22 keepers; deleted-header sections removed. A "Removed in E59-S2" appendix lists what went away and where the data moved (typically: `traffic_event` table + admin UI). | Must |
| FR-6.2 | `shared/traffic/markers.go` `ExposeHeaders` list is rewritten to match the new canonical set exactly. | Must |
| FR-6.3 | `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md` § 6.4 references are updated to use the new `x-nexus-cache` name. | Must |

---

## 3. Non-Functional Requirements

| ID | Requirement |
|---|---|
| NFR-1 | All UI changes are i18n-complete in en/zh/es; `npm run check:i18n` passes. |
| NFR-2 | `npm run check:design-tokens` passes. |
| NFR-3 | `go test -race -count=1` passes for every package whose tests touched the renamed/deleted headers. |
| NFR-4 | Per-package Go coverage gate (CLAUDE.md → ≥95%) holds on `packages/ai-gateway/internal/ingress/proxy/`, `packages/shared/traffic/`, `packages/compliance-proxy/internal/proxy/`, `packages/agent/internal/network/`. |
| NFR-5 | `tests/scripts/smoke-gateway.py --all-ingress` passes once the parallel-session CP build resolves; before that, AI Gateway smoke targets pass standalone. |
| NFR-6 | No `--no-verify` commits in E59-S2 unless explicitly authorized (S1's bypass was authorized for pre-existing coverage gaps; S2 should not need the bypass). |

---

## 4. User Roles & Personas

| Role | Touchpoint |
|---|---|
| **App developer** | Reads fewer headers; SDK code that referenced `x-nexus-aigw-*` will need a version bump. Net win: cleaner namespace, less to memorize. |
| **SRE / DevOps** | DevTools / curl output drops 17 noisy / constant headers. `Server-Timing` is now native in browser tooling. Net win: less noise, standard tooling. |
| **FinOps / Auditor** | `x-nexus-cache`, `x-nexus-quota-*` carry the same information under shorter names. Audit log (`traffic_event` DB) is unchanged. |
| **OSS contributor** | The canonical header set is documented in one place (`nexus-response-markers.md`); no more hunting through 4 services for what's emitted. |

---

## 5. Constraints & Assumptions

- **C1.** Dev-phase, no installed external users — header renames are a clean break per CLAUDE.md no-backward-compatibility rule.
- **C2.** The `Server-Timing` header is HTTP standard; browser DevTools already supports it (Chrome 65+, Firefox 67+, Safari 13+). No client-side polyfill needed.
- **C3.** E60 (attestation) is independent of S2. S2 reserves the `x-nexus-attestation` slot in `ExposeHeaders` but does not write it.
- **C4.** Some Tier 2 headers (e.g., `aigw-hook`, `coerced`) are escalated to "keep" because compliance and SRE rely on them. The strictest reading of "Tier 1 only" would drop these; the pragmatic decision (2026-05-19 user direction) is to retain them.

---

## 6. Glossary (additions to E59 glossary)

| Term | Meaning |
|---|---|
| **Canonical header set** | The exhaustive list of `x-nexus-*` response headers any Nexus service writes. Defined in `nexus-response-markers.md`; mirrored by `shared/traffic/markers.go` `ExposeHeaders`. After E59-S2 the set is ~22 headers + reserved `x-nexus-attestation` slot. |
| **Server-Timing** | RFC 8674 HTTP standard header for emitting per-request timing breakdowns. Replaces the deleted `aigw-latency-ms` / `aigw-upstream-ttfb-ms` / `aigw-upstream-total-ms` trio. |
| **Tier 1 / Tier 2** | Internal triage labels from the 2026-05-19 strict audit. Tier 1 = must-keep (10 headers); Tier 2 = valuable, keep after review (escalated keepers). |

---

## 7. MoSCoW Priority

| Story | Priority | Rationale |
|---|---|---|
| S2 — Header namespace cleanup (this story) | **Must** | Cleanup is coherent only as one PR — splitting risks half-renamed / half-deleted state where ExposeHeaders / arch doc / writers drift. |

---

## 8. Out of Scope

- E60 attestation implementation (the `x-nexus-attestation` slot is reserved here, not implemented).
- Header-level cryptographic signing / verification (E60 owns this).
- Migration of existing customer SDKs to new header names (downstream task; this story ships the rename, customers update their consumers).
- Request-side header cleanup (this story is response-side only; `x-nexus-virtual-key`, `X-Nexus-Trace-ID` request-side, `X-Nexus-Bedrock-*` request-side, etc. stay).
- Hook outcome format spec changes (the multi-write consolidation in FR-5.2 is mechanical; the format defined in `traffic.FormatHookOutcome` stays).

---

## 9. Acceptance Criteria

| ID | Acceptance |
|---|---|
| AC-1 | `grep -rEn 'Header\(\).Set\("[Xx]-[Nn]exus' packages --include="*.go"` (excluding tests) returns only writers from the canonical set in FR-1.1. |
| AC-2 | `shared/traffic/markers.go` `ExposeHeaders` matches FR-1.1 exactly. |
| AC-3 | The 12 single-value / opaque / derivable / dead headers from FR-2 are absent from every production writer. |
| AC-4 | The renamed headers from FR-3 are emitted under the new names; old names are absent. |
| AC-5 | `Server-Timing` header is emitted on every ai-gateway response with `gw;dur=...` plus `upstream-ttfb;dur=...` + `upstream-total;dur=...` when upstream was called. |
| AC-6 | `x-nexus-aigw-model` and `x-nexus-aigw-provider` are deleted; the logic bug (FR-5.1) is gone by removal of the affected writers. |
| AC-7 | Each of `aigw-hook` / `cp-hook` / `agent-hook` is written at exactly one finalize-stage site per service; tests cover the dual-stage merged value. |
| AC-8 | `docs/developers/architecture/cross-cutting/foundation/nexus-response-markers.md` is rewritten to the new set with a "Removed in E59-S2" appendix. |
| AC-9 | `npm run check:i18n`, `check:design-tokens`, `check:workspace-replace` all pass. |
| AC-10 | `tests/scripts/smoke-gateway.py --all-ingress` passes (once CP build resolves); ai-gateway-only smoke targets pass standalone. |
