# E48 — Emergency Passthrough 3-Tier Config

## 1. Background

The post-E47 AI Gateway request pipeline funnels every customer request through L3 ingress normalization → L4 routing → L4 hooks → executor. Three of those layers can independently break a provider's traffic when their implementation regresses:

- **Normalize** — a new model variant ships a body field our parser doesn't recognise; `normalize.Registry.Normalize` panics or returns an `unsupported` payload, smart routing fails to ground decisions, and cache key computation degrades.
- **Hooks** — a rule-pack pattern update produces false positives, content-safety classifier hits a panic, AI-Guard webhook timing exceeds SLA; legitimate customer requests are blocked or hard-rejected.
- **Cache** — a key-derivation refactor introduces a collision; clients see somebody else's response, or an Anthropic prompt-caching marker boundary breaks and explicit cache writes diverge from reads.

Today the operator's emergency response menu is short and bad:

1. **Roll back the binary** — heavy (full cycle, ~10-15 min) and may revert unrelated good changes.
2. **`UPDATE hook_config SET enabled = false WHERE …`** — silent, undocumented, leaves no audit trail, and is reversible only by remembering to flip it back.
3. **Wait and hope** — customers see errors while we debug.

E48 adds a fourth option: a typed, audited, IAM-gated, **time-bounded** "passthrough" mode that turns the gateway into a dumb pipe for a specific provider (or all providers of an adapter type, or globally). The compliance / smart-routing / cache layers are bypassed for matched traffic; the routing layer still picks a target, the executor still injects upstream credentials, the bytes still flow.

Three failure modes → three independent toggles (`bypassHooks`, `bypassCache`, `bypassNormalize`). 3-tier config (global / adapter / provider) mirrors the E38-S13 prompt-cache pattern so operators reuse the mental model. Mandatory `expiresAt ≤ 8 hours` prevents the silent-compliance-bypass failure mode.

## 2. Scope

### Must

**M1 — Three independent bypass toggles, JSONB merge across 3 tiers.**

- `bypassHooks`: skips Phase 5 request hooks + Phase 7 response hooks + SSE live compliance pipeline for matched traffic.
- `bypassCache`: skips cache lookup + cache write for matched traffic.
- `bypassNormalize`: skips Phase 3.5 request `normalize.Registry.Normalize` call AND response-side normalize emission to `traffic_event_normalized`. (Single toggle covers both directions; the same engine fails both sides in real incidents.)
- Resolution order: provider > adapter > global. Each tier carries a partial JSONB; effective config = jsonb `||` merge from top to bottom.

**M2 — Cross-toggle constraint (server + UX coupled).**

- Admin API rejects (HTTP 400, code `passthrough_normalize_requires_cache_bypass`) any request whose effective `bypassNormalize=true` does not also have `bypassCache=true`. This is true at every tier.
- The admin UI couples the toggles: flipping `bypassNormalize` ON auto-flips `bypassCache` ON in the form state AND disables the `bypassCache` toggle (greyed-out, tooltip "Auto-enabled — cache key derives from normalized payload"). An inline info banner above the toggles explains the rule in plain language. Flipping `bypassNormalize` OFF re-enables (does NOT auto-flip OFF) `bypassCache` so the operator can independently choose to keep cache-bypass on.
- The constraint applies to PUT/PATCH of any tier (global/adapter/provider). Server-side validation is defense-in-depth: scripted clients hitting the API directly receive the same 400.

**M3 — Mandatory `expiresAt`, max 8 hours.**

- DB CHECK constraints: `enabled = true` requires `expires_at IS NOT NULL` and `expires_at <= now() + interval '8 hours'`.
- Default value when admin UI fills the form: `expires_at = now() + 1 hour`.
- Reason: SRE P0-incident windows are industry-standard 4-8 h. Need a longer window → operator must re-enable (fresh audit row + fresh AlertRule fire). No "extend" UX; no silent renewal.

**M4 — Fail-closed cold-start.**

- `passthrough.NewCache()` returns an empty snapshot. Effective lookup on an empty cache returns the zero-value `PassthroughConfig{}` — all bypass = false.
- The cache is populated only after the Hub WebSocket handshake delivers the `gateway_passthrough_config` shadow. Until then, even rows with `enabled=true` in the DB do not take effect.
- The brief over-restriction window (sub-second) is acceptable; the brief silent bypass would be a compliance failure.

**M5 — Mandatory `reason` field on enabled rows.**

- DB CHECK: `enabled = true` requires `reason` length ≥ 20 characters.
- The admin UI shows the field as required on the enable-form; the double-confirm modal cannot be dismissed without it.
- Reason is recorded on the row + propagates to every `traffic_event.passthrough_reason` that fires under this rule (correlation evidence in audit log).

**M6 — `ResolvedRequest` L4 view.**

- New type in `packages/ai-gateway/internal/pipeline/requestcontext/resolved.go` wraps an immutable `*RequestContext`, the post-routing `*router.RouteResult`, and the merged `*PassthroughConfig`.
- Constructed at Phase 4.5 (after routing resolves the primary target) via `requestcontext.Resolve(rc, routeResult, passthroughCfg)`. The original `*RequestContext` is unchanged (E47-S1's immutability invariant is preserved).
- Downstream L4 consumers (hooks pipeline, audit Writer, executor) take `*ResolvedRequest`. Pre-routing layers (auth, rate-limit, routing engine itself) continue to take `*RequestContext`.

**M7 — Audit fan-out per traffic_event.**

- `ALTER TABLE traffic_event ADD COLUMN passthrough_flags TEXT[];`
- `ALTER TABLE traffic_event ADD COLUMN passthrough_reason TEXT;`
- The audit Writer populates both when the resolved passthrough fired any bypass. Empty array (= no bypass) is preserved as `NULL` (so a SQL `WHERE passthrough_flags IS NOT NULL` filter isolates affected rows efficiently).
- The control-plane Traffic Detail page renders a red `BYPASS` badge when the column is non-empty, with the flag list + reason expanded inline.

**M8 — Hub auto-revert on expiry.**

- A 60-second reconcile loop on Nexus Hub scans the three passthrough tables for rows whose `expires_at < now()` AND `enabled = true`; flips `enabled = false`, clears the bypass flags to `false`, logs the auto-revert with the original enabledBy + reason for traceability.
- The same loop emits a `gateway_passthrough_config` config-changed event so ai-gateway picks up the cleared snapshot within the next config push cycle.

**M9 — AlertRule template `passthrough-active`.**

- 5-minute poll on `gateway_passthrough_config_{global,adapter,provider}` WHERE `enabled = true AND expires_at > now()`.
- Fires page-once-per-hour until the count drops to zero.
- Seeded by the standard AlertRule template machinery (see `tools/db-migrate/seed/seed.ts`).
- Operators cannot disable this AlertRule via admin UI (template-level mandate) — same pattern as existing compliance-critical alerts.

**M10 — IAM `passthrough` resource.**

- New resource `passthrough` in `packages/shared/security/iam/catalog_data.go`.
- Verbs: `passthrough.read`, `passthrough.write`, `passthrough.emergency-enable`.
- `passthrough.read` granted to any admin role that can view AI Gateway config (Provider Admin, Compliance Admin, Viewer).
- `passthrough.write` granted to Provider Admin and Compliance Admin (covers GLOBAL + ADAPTER tier non-enabling edits and the disable path).
- `passthrough.emergency-enable` granted ONLY to `NexusIncidentResponse` and super-admin; this is the gate for `enabled=true` writes at any tier.
- IamMW guards each endpoint with the appropriate verb; the API enforces the `enabled=true` ↔ `emergency-enable` mapping at the handler level (you cannot smuggle an `enabled:true` field through a request that only authorises `passthrough.write`).

### Should

**S1 — UI countdown-to-expire.**

- Active Overrides table column "Expires In" shows live countdown (`mm:ss` if < 1 h, else `1h 23m`). Updates client-side every 10 s via React state + timer. When a row expires within the polling window, the table row visually highlights as auto-reverted.

**S2 — Admin UI prefill on re-enable.**

- When operator clicks Enable on a tier that has a previous (now-expired) row, the form prefills `bypassHooks/Cache/Normalize` from the prior row's values + sets `expires_at = now() + 1 hour`. Reason field stays empty (forces explicit acknowledgement that the original reason may not apply to the re-enable).

### Could

**C1 — Per-VirtualKey passthrough.**

- An enterprise customer with their own compliance pipeline might want their traffic to skip ours regardless of provider. Not in V1; tracked as a future story when product demand justifies it. The 3-tier table structure does NOT need to grow to accommodate this — a separate `gateway_passthrough_config_vk` table can plug into the same resolution chain.

**C2 — Audit-only mode (run hooks, ignore decisions).**

- A different axis from passthrough: "I want to see what hooks WOULD have decided without actually enforcing". Out of scope; separate epic if product wants it.

### Won't

**W1 — Per-pipeline-stage bypass.** All-or-nothing for hooks (request stage + response stage skipped together) in V1. Adds complexity; rarely needed.

**W2 — Synthetic canary on disable.** When passthrough is turned off, fire a probe to verify the underlying issue is actually fixed before going live again. Over-engineering for V1 — operators routinely verify out-of-band.

**W3 — Auto-passthrough on metrics anomaly.** Heuristic auto-enable based on error rate / latency. Too easy to false-trigger; manual operator control is the V1 contract.

## 3. Non-Functional Requirements

**NFR1 — Performance: passthrough resolution adds ≤ 50 µs per request.**

`PassthroughCache.Effective(providerID)` is a hash-table lookup against a pre-computed merged map (refreshed on Hub config push, not per-request). Per-request cost is one map lookup; no DB hit, no JSONB merge in the hot path.

**NFR2 — Audit completeness: every bypassed request is traceable.**

`traffic_event.passthrough_flags` populated when ANY bypass fires; never silently drops the marker. Compliance officers can run a single SQL query to find every request that skipped our hooks in any time window.

**NFR3 — Observability: Prometheus gauges expose live state.**

- `nexus_aigw_passthrough_active{tier,bypass_kind}` — gauge, 1 per active (enabled, not expired) row × bypass kind. Total in dashboards = "how much of our fleet is currently in degraded mode".
- `nexus_aigw_passthrough_requests_total{bypass_kind,provider_id}` — counter, increments per request that hit each bypass. Lets operators see traffic impact.

**NFR4 — Schema stability.**

Once shipped, the JSONB shape is additive-only. New bypass kinds get added as new keys with `false` default for old configs.

**NFR5 — English-only artefacts.**

All E48 source / SDD / requirements / runbook / admin API error strings / OpenAPI examples are English. UI strings go through i18n with translations in en/zh/es.

## 4. User Roles & Personas

**Incident responder.** On-call engineer who flips passthrough during an active incident. Needs: fast UI to enable + auto-disable so they don't have to remember to clean up + audit trail for postmortem. IAM: holds NexusIncidentResponse policy.

**Compliance officer.** Wants to know every time the compliance pipeline was bypassed, when, by whom, why, for which traffic. Needs: SQL filter on `traffic_event.passthrough_flags` + AlertRule notifications. IAM: holds `passthrough.read` to see config state.

**Provider admin.** Wants to disable / clean up an existing passthrough after the underlying issue is fixed. Needs: UI to disable + the per-tier resolution view so they know what they're affecting. IAM: holds `passthrough.write` (but not `emergency-enable` for tier-up changes).

**Super-admin.** Holds all verbs via `admin:*`; can do anything. Useful for break-glass.

## 5. Constraints & Assumptions

**C1 — E47 has shipped.** L3 RequestContext + L4 RoutingContext canonical-payload path is live. The `ResolvedRequest` wrapper depends on this foundation. If a future change rolls back E47, E48 needs a re-plumb.

**C2 — Hub config push pattern is reliable.** E38-S13 (prompt cache 3-tier) uses the same shadow-key + atomic snapshot pattern. The Hub-side `cp_config_drift_total` metric proves the path holds under load.

**C3 — Cross-format translation is NOT a passthrough scope.** Smart routing may route OpenAI ingress → Anthropic upstream via the canonical bridge codec. When `bypassNormalize=true`, the canonical payload is unavailable so cross-format translation cannot run. The handler MUST detect this combination and reject the request with HTTP 502 + body `{"error":"passthrough mode requires ingress format == target format"}`. Operators enabling passthrough on a provider that requires translation will see this error on the first matched request and know to disable the rule. (We do NOT silently fall through to a raw-byte forward because that would send OpenAI-shaped bytes to an Anthropic upstream → upstream rejects.)

**C4 — No partial bypass for streaming.** SSE / streaming responses run the live compliance pipeline as a hook stage; `bypassHooks` covers it. `bypassResponseNormalize` semantics on streaming: the chunked normalizer fails-open in the existing E46 path anyway, so the toggle has no observable effect for streaming endpoints. The UI surfaces this with a greyed-out tooltip on the toggle when the matched providers are streaming-only.

**C5 — Sequential PR cadence.** S1 → S7 ship as 7 separate PRs (mirroring the E47 cadence). Each must pass `go test -race -count=1 ./...` and a focused smoke check before the next starts.

## 6. Glossary

- **Bypass** — runtime skip of a policy-plane layer for matched traffic.
- **Tier** — one of three configuration scopes: global / adapter (e.g. "openai") / provider (specific Provider row by UUID).
- **Effective config** — JSONB `||` merge of all three tiers for a given provider, computed lazily by the in-memory cache.
- **ResolvedRequest** — L4 immutable view that bundles `*RequestContext` + `*router.RouteResult` + `*PassthroughConfig`. Constructed at Phase 4.5; consumed by hooks/audit/executor.
- **Cross-toggle constraint** — invariant that `bypassNormalize=true` requires `bypassCache=true`, enforced at admin API AND surfaced in the form UX.
- **Cold-start deny-all** — empty cache snapshot at boot returns zero-value config, ensuring no bypass fires before Hub config push.
- **Auto-revert** — Hub-side 60-second reconcile loop that flips `enabled=false` on rows past their `expires_at`.

## 7. Out-of-Scope Cleanups (recorded so they are not lost)

- Per-VirtualKey passthrough — see §2 Could C1.
- Audit-only hook mode — see §2 Could C2.
- Per-pipeline-stage bypass — see §2 Won't W1.
- Synthetic canary on disable — see §2 Won't W2.
- Auto-passthrough on metrics anomaly — see §2 Won't W3.
- A unified `gateway_runtime_overrides` table that hosts passthrough + future modes (e.g. strict-mode) under a single schema. Tempting but YAGNI for V1; track for when a second runtime-override mode is actually scoped.

## 8. Phasing (informative — full breakdown in SDDs)

Sequential, one PR per Story:

- **S1** Schema + Prisma migration + thing_config_template + shadow key (`gateway_passthrough_config`). DB CHECK constraints for expires_at + reason.
- **S2** `requestcontext.ResolvedRequest` type + `Resolve()` + signature changes through hooks/audit/executor.
- **S3** AI Gateway runtime — `passthrough.Config` / `passthrough.Cache` / atomic.Pointer snapshot + Hub config-applier wiring + Phase 4.5 attach.
- **S4** Bypass branches: Phase 5 hooks skip + cache layer skip + Phase 3.5 normalize skip. Inline comments per branch.
- **S5** `traffic_event.passthrough_flags` + `passthrough_reason` columns + audit Writer + admin UI Traffic Detail page red BYPASS badge.
- **S6** Admin API (7 endpoints) + 3-tier editor UI page + double-confirm modal + cross-toggle UX (auto-flip + disable + banner).
- **S7** IAM `passthrough` resource + `NexusIncidentResponse` policy update + AlertRule template + Hub reconcile loop.
