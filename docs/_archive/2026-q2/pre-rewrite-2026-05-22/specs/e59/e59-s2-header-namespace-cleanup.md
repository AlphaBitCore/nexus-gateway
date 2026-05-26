# E59-S2 — Response Header Namespace Cleanup

> Story: e59-s2
> Epic: 59 (Cache UX Honesty)
> Status: Draft
> Requirements: `docs/developers/specs/e59/e59-header-namespace-cleanup.md`
> Architecture: `docs/developers/architecture/cross-cutting/foundation/nexus-response-markers.md` (canonical inventory rewrite); `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md` § 6.4 (cache header rename reference)
> Blocked by: none (sibling to E59-S1; E59-S1 shipped 2026-05-19 commit 7d474f1e)
> Related: E60 — Agent Attestation (independent epic; this story merely reserves the `x-nexus-attestation` slot in `ExposeHeaders`)

## User Story

As a Platform Admin / SRE / app developer reading Nexus response headers, I want exactly ~22 well-named, single-writer-or-clearly-disambiguated headers in browser DevTools / curl / SDK code — not 38 with single-value constants, name duplicates, opaque hashes, and casing inconsistencies. The first reading of any response should answer the four questions that matter (was-it-cached, did-routing-redirect, did-hooks-modify, am-I-near-quota) without 17 noise headers to scroll past.

## Tasks

### T1 — Rewrite the canonical inventory in `nexus-response-markers.md`

- T1.1 Delete sub-sections for headers removed in FR-2 (mode×3, allowlist-version, routing-rule, stream, model, provider, quota-remaining, quota-period, overhead-ms, latency-ms, upstream-ttfb-ms, upstream-total-ms, agent-domain-rule).
- T1.2 Rename sub-sections per FR-3 (aigw-cache → cache, aigw-routed-* → routed-*, aigw-quota-* → quota-*, aigw-coerced → coerced, aigw-attempts → attempts, aigw-dry-run → dry-run, x-nexus-upgraded-to → x-nexus-upgraded-to, request-id unification).
- T1.3 Add a "## Server-Timing (RFC 8674)" section documenting the new HTTP-standard timing header.
- T1.4 Add a "## Reserved: x-nexus-attestation (E60)" section noting the slot is present in ExposeHeaders but not yet written.
- T1.5 Add a "## Appendix — Removed in E59-S2" section listing every deleted header with: (a) what it used to carry, (b) where the data now lives (typically `traffic_event` DB columns), (c) cross-ref to the E59-S2 commit.
- T1.6 Update the "Reading Markers from Browser JavaScript" example to use new names.
- T1.7 Update last-updated date.

### T2 — Update `shared/traffic/markers.go` ExposeHeaders

- T2.1 Replace the 30-entry `ExposeHeaders` slice with the new ~22-entry list matching FR-1.1.
- T2.2 Add a top-of-file comment cross-referencing `nexus-response-markers.md` as the canonical doc.
- T2.3 Reserve `x-nexus-attestation` in the slice with an inline comment "// E60 — writer not yet implemented; slot reserved for ExposeHeaders parity with the architecture doc."

### T3 — AI Gateway proxy writers sweep

- T3.1 `packages/ai-gateway/internal/ingress/proxy/proxy.go`:
    - Delete the 4 `x-nexus-aigw-mode` writes.
    - Delete `x-nexus-aigw-allowlist-version`, `x-nexus-aigw-routing-rule`, `x-nexus-aigw-stream`.
    - Delete the duplicate `x-nexus-aigw-model` and `x-nexus-aigw-provider` writes (logic bug — same value as routed-*).
    - Delete `x-nexus-aigw-quota-remaining`, `x-nexus-aigw-quota-period`, `x-nexus-aigw-overhead-ms`.
    - Delete `x-nexus-aigw-latency-ms`, `x-nexus-aigw-upstream-ttfb-ms`, `x-nexus-aigw-upstream-total-ms` writes — replaced by Server-Timing.
    - Delete the duplicate `x-nexus-aigw-request-id` write (line ~2000).
    - Rename: `x-nexus-routed-model` → `x-nexus-routed-model`, `x-nexus-routed-provider` → `x-nexus-routed-provider`, `x-nexus-aigw-quota-{used,limit,downgrade,original-model,warning}` → `x-nexus-quota-*`, `x-nexus-coerced` → `x-nexus-coerced`, `x-nexus-attempts` → `x-nexus-attempts`.
    - Rename: `x-nexus-upgraded-to` (proxy.go:594) → `x-nexus-upgraded-to`.
    - Consolidate the 2 `x-nexus-aigw-hook` writes (lines 997, 1582) into a single finalize-stage write that emits the combined two-stage outcome (FR-5.2).
- T3.2 `packages/ai-gateway/internal/ingress/proxy/proxy_cache.go`:
    - Rename `x-nexus-cache` → `x-nexus-cache` at all 4 sites.
    - Rename `x-nexus-coerced` → `x-nexus-coerced` at site ~466.
    - Consolidate the 2 `x-nexus-aigw-hook` writes (lines 184, 189) similarly to T3.1.
- T3.3 `packages/ai-gateway/internal/ingress/proxy/dry_run.go`:
    - Rename `x-nexus-dry-run` → `x-nexus-dry-run` (line ~154).
    - Rename `x-nexus-aigw-model` → drop (per FR-2.5 — covered by routed-*); the dry-run path stamps `x-nexus-routed-model` instead.
    - `x-nexus-estimate` stays unchanged (already correct name).

### T4 — Middleware: unified request-id

- T4.1 `packages/ai-gateway/internal/platform/middleware/middleware.go`:
    - Rename the middleware-set `x-nexus-request-id` (line 21-22) → `x-nexus-request-id` (lowercase).
    - Logger references (`w.Header().Get("x-nexus-request-id")`) updated to lowercase.
- T4.2 `packages/nexus-hub/internal/handler/middleware.go`:
    - The `nexusRequestIDHeader` constant already says `"x-nexus-request-id"` (lowercase) — no change.
- T4.3 Add `Server-Timing` emission in ai-gateway's final response stage (proxy.go ~line 2020 area):
    - Format: `gw;dur=<gateway-overhead-ms>, upstream-ttfb;dur=<ms>, upstream-total;dur=<ms>` (the last two only when upstream was called).
    - Source values: `rec.UpstreamTtfbMs`, `rec.UpstreamTotalMs`, and `(latencyMs - upstream-total)` for gw overhead.

### T5 — Compliance Proxy markers sweep

- T5.1 `packages/shared/transport/tlsbump/markerhook.go`:
    - Delete the 3 `x-nexus-cp-mode` writes (lines 37, 44, 76).
    - Delete the 2 `x-nexus-cp-request-id` writes (lines 35, 79) — middleware already stamps `x-nexus-request-id`.
    - Consolidate the 2 `x-nexus-cp-hook` writes (38, 77) into one finalize-stage write.
    - Keep `x-nexus-cp-domain-rule` (lines 40, 82) — service-prefixed because CP and agent may both write domain rules in the future.

### T6 — Agent markers sweep

- T6.1 `packages/agent/internal/network/proxy/marker.go`:
    - Delete the `x-nexus-agent-mode` write (line 41).
    - Delete the dead `x-nexus-agent-domain-rule` reference (it was in ExposeHeaders but has no writer here — confirm by grep and remove from MergeExposeHeaders slice).
    - Keep `x-nexus-agent-flow-id` (line 43) and `x-nexus-agent-hook` (line 45).
    - Ensure the `MergeExposeHeaders` call (line 47) lists only headers this file actually writes.

### T7 — Test sweep

- T7.1 Grep across all `*_test.go` for `x-nexus-request-id`, `x-nexus-aigw-*`, `x-nexus-upgraded-to`, the deleted header names. Replace with new names; delete assertions on removed headers.
- T7.2 Add a new test `TestServerTiming_Format` in proxy package asserting the `Server-Timing` header is RFC 8674-compliant and contains the three expected sub-metrics on non-cached requests.
- T7.3 Add `TestHookHeader_Consolidation` asserting that `aigw-hook` is set exactly once per response (not twice), and that the consolidated value reflects both request-stage and response-stage outcomes correctly.
- T7.4 Run `go test ./packages/... -count=1 -race`; all packages whose tests touched headers pass.
- T7.5 Coverage on `packages/ai-gateway/internal/ingress/proxy/`, `packages/shared/traffic/`, `packages/shared/transport/tlsbump/`, `packages/agent/internal/network/proxy/` stays ≥95%.

### T8 — Frontend / client-side updates

- T8.1 Grep `packages/control-plane-ui/src/` for any explicit reference to a renamed/deleted header. Update references; delete references to deleted headers.
- T8.2 If the admin Cost dashboard or Cache ROI dashboard reads any of these headers via SDK / API client, update those consumers.
- T8.3 Run `npm test` for the UI; tests pass.
- T8.4 Run `npm run check:i18n`, `check:design-tokens`, `check:workspace-replace` — all pass.

### T9 — Smoke + verification

- T9.1 Run `tests/scripts/smoke-gateway.py --all-ingress` once the parallel-session CP build resolves. Verify: every model × ingress combo sees the new canonical header set (29 × 4 = 116 requests); no deleted header present; `Server-Timing` present on every response.
- T9.2 Manual DevTools check: open the CP UI, click into a recent traffic event, verify the rendered Cache block (E59-S1) still works with the renamed `x-nexus-cache` field — but the UI doesn't read response headers, so this is a smoke-against-smoke check rather than a UI-broken test.

### T10 — 2-round audit + commit + push

- T10.1 Round 1 audit: every todo (T1-T9) completed; no `TODO`/`FIXME`/`XXX` introduced; every code change covered by a test or explicitly acknowledged untested.
- T10.2 Round 2 audit: re-verify; iterate until two consecutive clean rounds.
- T10.3 Commit with explicit pathspec per parallel-session safety. Do NOT use `--no-verify` — coverage should hold on touched packages (FR-NFR-4).
- T10.4 Push.

## Acceptance Criteria

| ID | Acceptance |
|---|---|
| AC-1 | `grep -rEn 'Header\(\).Set\("[Xx]-[Nn]exus' packages --include="*.go" \| grep -v _test.go` lists only writers from the canonical set (FR-1.1). |
| AC-2 | `shared/traffic/markers.go` `ExposeHeaders` matches FR-1.1 exactly (21 entries + reserved attestation slot = 22). |
| AC-3 | Single-value constants (`aigw-mode`, `cp-mode`, `agent-mode`) are absent from every production writer. |
| AC-4 | The 12 deleted headers (FR-2.1-FR-2.10) have no production writer. |
| AC-5 | All 7 renames (FR-3.1-FR-3.7) are applied; old names absent except in `_archive/` history. |
| AC-6 | `Server-Timing` header is emitted on every ai-gateway response (non-dry-run); format is RFC 8674-compliant. |
| AC-7 | Each hook header (`aigw-hook`/`cp-hook`/`agent-hook`) is written at exactly one finalize-stage site per service. |
| AC-8 | `nexus-response-markers.md` is rewritten; the "Removed in E59-S2" appendix lists every removal with rationale + DB-column cross-ref. |
| AC-9 | `npm run check:i18n`, `check:design-tokens`, `check:workspace-replace` pass. |
| AC-10 | `go test ./packages/... -count=1 -race` passes (modulo parallel-session WIP unrelated failures, which are surfaced in the commit message). |
| AC-11 | smoke-gateway --all-ingress passes (once CP rebuilds). |

## Testing strategy

- **Unit (Go)**: per-package coverage gate (NFR-4); table-driven tests for the new Server-Timing format helper; consolidation test for the hook header.
- **Unit (Vitest)**: snapshot the rendered traffic-event drawer in the CP UI — no header-name references should appear in the rendered DOM (cache info comes from `traffic_event.cache_status`, not from response headers).
- **Integration**: smoke `--all-ingress` (T9.1).
- **Manual UX review**: open browser DevTools on a real request; confirm only the canonical set appears.

## Rollback plan

- Each rename is mechanical and a small commit-on-revert. Per-file revert via `git revert` is trivial.
- `nexus-response-markers.md` rewrite is documentation-only; if a regression surfaces, revert the doc + the writer change for that header.
- No DB schema changes; no SDK/proto changes; no public API contract breaks except via the renamed headers (dev-phase, no installed users).

## Appendix — Header-name change map

| Before | After | Reason |
|---|---|---|
| `x-nexus-cache` | `x-nexus-cache` | Single-writer; drop service prefix |
| `x-nexus-routed-model` | `x-nexus-routed-model` | Single-writer |
| `x-nexus-routed-provider` | `x-nexus-routed-provider` | Single-writer |
| `x-nexus-quota-used` | `x-nexus-quota-used` | Single-writer |
| `x-nexus-quota-limit` | `x-nexus-quota-limit` | Single-writer |
| `x-nexus-quota-downgrade` | `x-nexus-quota-downgrade` | Single-writer |
| `x-nexus-quota-original-model` | `x-nexus-quota-original-model` | Single-writer |
| `x-nexus-quota-warning` | `x-nexus-quota-warning` | Single-writer |
| `x-nexus-attempts` | `x-nexus-attempts` | Single-writer |
| `x-nexus-coerced` | `x-nexus-coerced` | Single-writer |
| `x-nexus-dry-run` | `x-nexus-dry-run` | Single-writer |
| `x-nexus-upgraded-to` | `x-nexus-upgraded-to` | Casing + namespace |
| `x-nexus-request-id` | `x-nexus-request-id` | Casing; unify with hub middleware |
| `x-nexus-aigw-hook` | `x-nexus-aigw-hook` | Keep — multi-writer disambiguation |
| `x-nexus-cp-hook` | `x-nexus-cp-hook` | Keep — multi-writer |
| `x-nexus-agent-hook` | `x-nexus-agent-hook` | Keep — multi-writer |
| `x-nexus-via` | `x-nexus-via` | Keep — already correct |
| `x-nexus-agent-flow-id` | `x-nexus-agent-flow-id` | Keep — agent-specific identity |
| `x-nexus-cp-domain-rule` | `x-nexus-cp-domain-rule` | Keep — service prefix reserved for future agent collision |
| `x-nexus-estimate` | `x-nexus-estimate` | Keep — already prefix-less |
| (none) | `x-nexus-attestation` | Reserved slot; E60 writer |
| (none) | `Server-Timing` | New HTTP-standard timing |

## Appendix — Deletions

| Header | Reason | Data now lives at |
|---|---|---|
| `x-nexus-aigw-mode` | always `"proxied"` (single value) | N/A — was never useful |
| `x-nexus-cp-mode` | always `"mitm"` (single value) | N/A |
| `x-nexus-agent-mode` | always `"mitm"` (single value) | N/A |
| `x-nexus-aigw-allowlist-version` | opaque hash, no documented consumer | `traffic_event` (if needed by audit) |
| `x-nexus-aigw-routing-rule` | internal rule name (debug-only) | `traffic_event.routing_rule_name` + admin UI |
| `x-nexus-aigw-stream` | derivable from `Content-Type` | N/A |
| `x-nexus-aigw-model` | duplicate of routed-model | `traffic_event.routed_model_name` |
| `x-nexus-aigw-provider` | duplicate of routed-provider | `traffic_event.routed_provider_name` |
| `x-nexus-aigw-quota-remaining` | derivable from limit − used | client-computable |
| `x-nexus-aigw-quota-period` | always `"monthly"` (single value) | N/A |
| `x-nexus-aigw-overhead-ms` | derivable from latency − upstream-total | `Server-Timing` |
| `x-nexus-aigw-latency-ms` | replaced by Server-Timing `gw;dur=...` | `Server-Timing` |
| `x-nexus-aigw-upstream-ttfb-ms` | replaced by Server-Timing | `Server-Timing` |
| `x-nexus-aigw-upstream-total-ms` | replaced by Server-Timing | `Server-Timing` |
| `x-nexus-agent-domain-rule` | dead code (no writer) | N/A — never emitted |
| `x-nexus-trace-id` | dead in response-side ExposeHeaders (only used as outbound REQUEST header) | request-side only |
| `x-nexus-aigw-request-id` | duplicate of middleware request-id | `x-nexus-request-id` |
| `x-nexus-cp-request-id` | duplicate of middleware request-id | `x-nexus-request-id` |
| `X-Cache` | duplicate of `x-nexus-cache`; deleted in E59-S1 commit 7d474f1e | `x-nexus-cache` |
