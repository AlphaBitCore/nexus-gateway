# E56-S11 — Routing-rule `preferResponsesAPI` flag (auto-upgrade)

**Epic:** E56 OpenAI Responses-API ingress
**Type:** Routing-rule feature + admin API/UI extension
**Owner:** nexus
**Depends on:** S2, S3, S4, S5.

## User story

> As an operator with legacy application code calling
> `/v1/chat/completions` against OpenAI's reasoning models (gpt-5.x,
> o-series), I want to set a routing-rule flag that makes Nexus translate
> my chat-completions ingress to OpenAI's `/v1/responses` upstream on my
> behalf — so I get reasoning, structured tool flows, and the latest
> OpenAI features without changing my application's SDK calls.

## Tasks

### T11.1 — Schema migration

**File:** `tools/db-migrate/prisma/schema.prisma` + new migration under `tools/db-migrate/prisma/migrations/<TS>_e56_prefer_responses_api/migration.sql`

```sql
ALTER TABLE "routing_rule"
  ADD COLUMN "prefer_responses_api" BOOLEAN NOT NULL DEFAULT false;
```

`<TS>` follows the unique-timestamp rule (per `memory/feedback_migration_timestamp_unique.md`). Pre-allocate at SDD time: `20260530000000` (verified unique at coding time via `ls migrations/ | cut -c1-14 | sort | uniq -d`). Confirm uniqueness immediately before commit.

`ALTER ADD COLUMN NOT NULL DEFAULT false` on PG11+ is O(1) metadata-only.

Prisma schema mirror: `RoutingRule { preferResponsesAPI Boolean @default(false) @map("prefer_responses_api") }`.

### T11.2 — Empirical model-support list

**File:** `packages/ai-gateway/internal/providers/spec_openai/responses_model_support.go` (new)

```go
// ResponsesAPISupportedModelPrefixes lists OpenAI model IDs (by prefix
// match) empirically confirmed to accept POST /v1/responses bodies as
// of 2026-05-16. Per provider-adapter-architecture.md §3a Rule 7,
// every entry below was verified by a captured 200 from the real
// endpoint with a minimal `{"model":"<id>","input":"ping"}` body.
//
// Extending this list: add a captured 200 (trace_id + date) in the
// comment. Adding without empirical evidence is forbidden.
var ResponsesAPISupportedModelPrefixes = []string{
    "gpt-5",            // gpt-5.0, gpt-5.1, gpt-5.2, gpt-5-codex; verified 2026-05-16 trace=… (record at S11 coding time)
    "gpt-4o",           // gpt-4o, gpt-4o-mini, gpt-4o-2024-08-06; verified 2026-05-16 trace=…
    "gpt-4.1",          // verified 2026-05-16 trace=…
    "o1", "o3", "o4",   // o-series reasoning models; verified 2026-05-16 trace=…
}

func IsModelSupportedOnResponses(modelID string) bool {
    for _, p := range ResponsesAPISupportedModelPrefixes {
        if strings.HasPrefix(modelID, p) { return true }
    }
    return false
}
```

Comments MUST be filled with real trace_id at coding time, not at SDD time. SDD time records the intent; coding time records the evidence.

### T11.3 — Executor branch

**File:** `packages/ai-gateway/internal/handler/proxy.go`

After routing resolution, before adapter dispatch:

```go
if ingress.BodyFormat == providers.FormatOpenAI &&
   resolved.TargetAdapter.Manifest().ProviderID == "openai" &&
   resolved.RoutingRule.PreferResponsesAPI &&
   spec_openai.IsModelSupportedOnResponses(resolved.ModelID) {

    // Auto-upgrade path:
    canonical, err := bridge.IngressChatToCanonical(providers.FormatOpenAI, body)
    if err != nil { /* fall through to classic path */ }
    upgradedBody, err := spec_openai.EncodeResponsesRequest(canonical)
    if err != nil { /* fall through */ }

    body = upgradedBody
    upstreamPath = "/v1/responses"
    streamMode = streamModeResponses  // S4's session
    egressEncoder = bridge.ResponsesToChatCompletionsEncoder  // see T11.4
    rw.Header().Set("x-nexus-upgraded-to", "responses-api")
}
```

The fall-through-on-codec-error path means a bug in S2/S3 cannot break customer traffic: worst case the flag silently no-ops and the classic path runs. Log `slog.Warn` so we see it in prod.

### T11.4 — Egress encoder: Responses → chat-completions

**File:** `packages/ai-gateway/internal/execution/canonicalbridge/bridge.go`

Add a one-liner helper because the client expects chat-completions response shape:

```go
func ResponsesToChatCompletionsEncoder(c CanonicalResponse, opts EncodeOpts) ([]byte, error) {
    // identity codec on the canonical side: chat-completions IS the canonical
    return spec_openai.EncodeChatCompletionsResponse(c, opts)
}
```

For the stream path: the S4 forward decoder converts upstream Responses SSE → canonical chunks, and the existing OpenAI chat-completions stream encoder converts canonical chunks → chat-completions SSE on egress. No new encoder needed.

### T11.5 — Admin API extension

**Files:**
- `packages/control-plane/internal/handler/routingrules.go` — accept `preferResponsesAPI` in POST/PUT body, return in GET response.
- `packages/control-plane/internal/routingrules/repo.go` (or equivalent) — Prisma model already updated by T11.1.

IAM impact (per CLAUDE.md API/menu/route binding):
- No new admin endpoint. The flag is a field on existing `/api/admin/routing-rules` resource. **Kept on `admin:routing-rules.write`** — no new resource carved out.
- No new sidebar nav item.
- No new IAM resource needed in `packages/shared/security/iam/catalog_data.go`.
- Record the decision verbatim in the S11 commit message: "kept on `admin:routing-rules.write` — additive field, no scope expansion".

### T11.6 — Admin UI extension

**File:** `packages/control-plane-ui/src/routes/admin/routing-rules/RoutingRuleEditor.tsx` (or equivalent path — confirm at coding time)

Add a checkbox below the existing target-model selector:

```tsx
<Checkbox
  checked={rule.preferResponsesAPI ?? false}
  onChange={(v) => setRule({ ...rule, preferResponsesAPI: v })}
  label={t('pages:routingRules.preferResponsesAPI.label')}
  help={t('pages:routingRules.preferResponsesAPI.help')}
/>
```

The checkbox is **disabled** when the resolved target provider is not OpenAI (computed client-side from the selected provider's manifest, surfaced via existing `useApi` call). Disabled state has an explanatory tooltip from `pages:routingRules.preferResponsesAPI.disabledReason`.

i18n: add 3 keys × 3 locales = 9 entries in `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/pages.json`:

```
pages.routingRules.preferResponsesAPI.label           # English: "Auto-upgrade to OpenAI Responses API"
                                                     # ES:      "Actualizar automáticamente a OpenAI Responses API"
                                                     # zh translation lives in the zh locale file
pages.routingRules.preferResponsesAPI.help            # English: "When this rule routes to OpenAI and the target model supports Responses API, Nexus translates /v1/chat/completions traffic to OpenAI's /v1/responses endpoint to unlock reasoning + tool features. Response shape returned to the client remains chat-completions."
                                                     # zh + es translations in their locale files
pages.routingRules.preferResponsesAPI.disabledReason  # English: "Only available when the target provider is OpenAI."
                                                     # zh + es translations in their locale files
```

After adding to `src/i18n/locales/`, copy to `public/locales/` per CLAUDE.md i18n binding; run `/i18n-gap-check` to verify all 3 bundles match.

Design tokens: the new checkbox uses the existing `Checkbox` component from `packages/ui-shared`, so no new tokens needed. Verified by `npm run check:design-tokens`.

### T11.7 — Response marker

**File:** `packages/shared/nexusmarkers/` (or wherever `X-Nexus-*` headers are emitted; see `nexus-response-markers.md`)

Register `x-nexus-upgraded-to` as a new well-known marker. Document in `docs/developers/architecture/cross-cutting/foundation/nexus-response-markers.md`.

### T11.8 — Tests

**Files:**
- `packages/ai-gateway/internal/handler/proxy_test.go` (extend) — 4 cases:
  - U1: flag=true, OpenAI target, gpt-5.2 → upgrade fires (upstream path = `/v1/responses`, response marker present)
  - U2: flag=true, OpenAI target, gpt-3.5-turbo (not in supported list) → no upgrade (path = `/v1/chat/completions`)
  - U3: flag=true, Anthropic target → no upgrade (flag silently ignored)
  - U4: flag=false → no upgrade
- `packages/control-plane/internal/handler/routingrules_test.go` (extend) — CRUD round-trip with the new field.
- `packages/control-plane-ui/src/routes/admin/routing-rules/RoutingRuleEditor.test.tsx` (extend) — checkbox renders, disabled state, i18n keys resolve.

## Acceptance criteria

- AC-11.1: All 4 executor cases pass (U1-U4).
- AC-11.2: Admin CRUD test passes.
- AC-11.3: i18n key counts match across en/zh/es; `/i18n-gap-check` clean.
- AC-11.4: `npm run check:design-tokens` clean.
- AC-11.5: After deploy: live `/v1/chat/completions` call against a routing rule with the flag → response carries `x-nexus-upgraded-to: responses-api` and `traffic_event` row shows `upstream_path` ≈ `/v1/responses` (or a similar observable signal).
- AC-11.6: Cache: when the flag is on, cached responses still ROUND-TRIP correctly — second call returns the same chat-completions-shape body (cache key includes the upgrade decision so we don't accidentally serve a Responses-shape cache row to a chat-completions client).

## Verification

```
go test ./packages/ai-gateway/internal/handler/ -run TestPreferResponses -race -count=1
go test ./packages/control-plane/internal/handler/ -run TestRoutingRules -race -count=1
npm test --workspace=control-plane-ui -- RoutingRuleEditor
npm run check:design-tokens
/i18n-gap-check
```

## Risks

- **R-11.1:** The cache key for the upgrade path MUST encode the upgrade decision; otherwise a flag-off and flag-on client share a cache row and one of them gets the wrong shape. Concrete fix: include `prefer_responses_api` (after resolution) in the cache key alongside `model`, `body_hash`, `endpoint_type`. Test pins this in AC-11.6.
- **R-11.2:** `IsModelSupportedOnResponses` uses prefix match. A future OpenAI model named `gpt-5-experimental-no-responses` would be falsely matched. Mitigation: if such a model ships, add a negative-suffix exclusion list. For now the prefix match is the documented behavior.
- **R-11.3:** Adding a routing-rule field is a low-risk schema change, but Prisma client regeneration is required: `cd tools/db-migrate && npx prisma generate` after the migration lands. Document this in the S11 commit message.
- **R-11.4:** Admin UI loads from `useApi('routing-rules', 'detail', id)` — the queryKey must remain `['admin','routing-rules','detail',id]` per CLAUDE.md `useApi` binding. Adding the checkbox does not affect the queryKey shape.
