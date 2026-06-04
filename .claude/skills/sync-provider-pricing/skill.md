# sync-provider-pricing

Scrape each provider's **official** model + pricing page and reconcile our model
catalog — `provider-templates/*.json` (UI preset), `seed-baseline.sql` (fresh-seed
dataset), `seed.ts` cache multipliers, and the **prod `Model` table** — so prices,
model lists, and deprecation flags match the vendor's published page.

Use this skill when:
- A provider ships/renames/retires a model or changes prices.
- An audit finds price drift between the template JSON, the seed, and prod.
- The user says "update provider pricing / models", "sync prices from the official site".

**Hard rule — facts only, always re-verify LIVE, never guess.** Every price and every
model name comes from a fresh WebFetch of the vendor's official page **on this run**. No
price is typed from memory, and **nothing is trusted from cache**: provider pricing and
model line-ups change fast, so a `verified_date` in `provider-sources.json` means "URL was
canonical then", NOT "the numbers are still current". Any specific number written in this
skill's files (e.g. a `notes` field saying "prod has 1.25" or "Anthropic read = 0.1×
input") is a **diagnostic hint to confirm, not a value to apply** — re-read it off the live
page every time. If a page can't be fetched, WebSearch for the canonical vendor page, update
`provider-sources.json`, and refetch. A run that can't reach a provider's official page does
not edit that provider — it reports the gap.

**Division of labor (why this is reliable):** the LLM does the part that genuinely needs
judgment and freshness — fetch the live page, read the table, map vendor columns to our
fields, decide deprecations — and emits a normalized desired-state JSON. The deterministic
`sync_pricing.py` does every mechanical step from that JSON (diff, edit template JSON, edit
seed-baseline.sql, emit prod SQL) so the edits never drift. The script invents no facts; the
LLM verifies every fact live. Neither shortcut is allowed: no applying without a live fetch,
no hand-editing the JSON/SQL that the script should write.

The mutable URL knob is **`provider-sources.json`** (next to this file): `models_urls` (model
catalog/spec page) + `pricing_urls` ($/MTok) per provider. Edit it freely as vendor URLs
drift; this procedure stays fixed.

---

## Where prices live (the 4 surfaces — keep them consistent)

1. **`packages/control-plane-ui/public/provider-templates/<name>.json`** — UI "add provider"
   preset. Explicit per-model object: `code`, `name`, `description`, `providerModelId`,
   `type` (chat|embedding|image|audio), `features[]`, `inputPricePerMillion`,
   `outputPricePerMillion`, `cachedInputReadPricePerMillion`, `cachedInputWritePricePerMillion`,
   `maxContextTokens`, `maxOutputTokens`. Also bump `modelCount` for this provider in
   `provider-templates/index.json`. The build copies `public/` → `dist/`; after editing
   `public/`, either rebuild the UI (`npm run build -w packages/control-plane-ui`) or mirror
   the same edit into `dist/provider-templates/<name>.json` so a no-rebuild deploy is current.
2. **`tools/db-migrate/seed/data/seed-baseline.sql`** — `INSERT INTO public."Model"` rows.
   Carries `inputPricePerMillion` + `outputPricePerMillion` only — **NO cache columns**,
   plus `status`, `deprecationDate`, `replacedBy`, `code`, `providerModelId`, `name`, etc.
3. **`tools/db-migrate/seed/seed.ts`** — the cache-price backfill `VALUES (adapter, read, write)`
   block (search `cachePriceBackfill`). It fills `cachedInput{Read,Write}PricePerMillion`
   = `input × mult` **only when NULL** (COALESCE). It is a FALLBACK, not the truth — prefer
   explicit official values in the template JSON. Update a multiplier only if the provider's
   whole-family ratio changed.
4. **prod `Model` table** — the live runtime source the gateway's cost-estimation reads
   (`docs/developers/architecture/.../cost-estimation-architecture.md`). 4 price fields +
   `status`/`lifecycle`/`deprecationDate`/`replacedBy`. **OUT OF SCOPE for this skill** —
   this skill only edits surfaces 1–3 (repo). Prod is changed separately, with explicit
   per-run approval (see Step 5). Reading prod for the diff in Step 2 is fine; writing it is not.

### Cache-field mapping (one write slot, one read slot)
- `cachedInputWritePricePerMillion` = the provider's **standard short cache-write** price.
  Anthropic = **5-minute** cache write (1.25× input); the 1-hour write (2×) is NOT stored.
  OpenAI/Gemini/DeepSeek/most = **0** (automatic/implicit caching, no write surcharge).
- `cachedInputReadPricePerMillion` = cache **hit/read** price. Per provider, per model —
  read the official number. Anthropic 0.1× input; OpenAI per-model (~0.25–0.5×, NOT a flat
  1.25); Gemini ~0.25× (Pro is context-tiered); DeepSeek = published cache-hit input price.
- Non-chat models (embedding/image/audio) have **no prompt cache** → cache fields NULL.
- Per-provider specifics live in `provider-sources.json` → `cache_convention`.
- ⚠ **NULL-price trap (chat models):** `internal/cache/layer/pricing.go` defaults
  `CacheReadUSDPerM` → **full input price** when `cachedInputReadPricePerMillion` is NULL. So a
  **chat** model left with NULL cache_read silently bills cache hits at full input (overcharge).
  Always set an explicit per-model cache_read for chat models; NULL is correct ONLY for non-chat.

---

## Procedure (per provider)

Run for one provider, or loop `active_in_prod` then the rest. Do providers **one at a time**;
each ends with its files re-grepped and (if applied) prod verified.

### Step 1 — Fetch the official page
Read `provider-sources.json` for `pricing_urls` + `deprecations_url`. WebFetch each with a
prompt that demands an exact table: every model's display name, the API model id if shown,
base input $/MTok, cache-write $/MTok (note 5m vs 1h for Anthropic), cache-read/hit $/MTok,
output $/MTok, and any **deprecated / retired** marker. If a URL fails or looks stale,
WebSearch the vendor's pricing page, **update `provider-sources.json`** (url + `verified_date`),
refetch. Capture the extracted table verbatim before editing anything.

### Step 2 — Read our current 3 surfaces
- `cat packages/control-plane-ui/public/provider-templates/<name>.json`
- `grep -n "INSERT INTO public.\"Model\"" tools/db-migrate/seed/data/seed-baseline.sql` then
  the rows for this provider's `code`s.
- prod: `SELECT code,name,status,lifecycle,"inputPricePerMillion" in_p,
  "cachedInputWritePricePerMillion" cw,"cachedInputReadPricePerMillion" cr,
  "outputPricePerMillion" out_p,"deprecationDate","replacedBy" FROM "Model" m JOIN "Provider" p
  ON p.id=m."providerId" WHERE p.name='<name>' ORDER BY code;` via the prod psql-over-ssh
  helper (see prod-deploy skill / tests/.env.prod).

### Step 3 — Diff and decide, per model
For every model the vendor lists AND every model we carry:
- **price drift** → new official value wins. Map cache write→5m, read→hit per the mapping above.
- **vendor marks deprecated** → set `status=deprecated`, `lifecycle=deprecated`, a
  `deprecationDate`, and `replacedBy` (the successor). **"retired"** → `status=disabled` (keep
  the row for historical cost lookups; do not delete — cost-estimation must still resolve old
  traffic_event rows). Mark in ALL surfaces.
- **vendor has a model we lack** → adding is a product call; list it, don't auto-add unless the
  user asked to expand the catalog.
- **we carry a model the vendor dropped** → flag for deprecation, don't silently delete.
- Non-chat model with a bogus cache price (e.g. embeddings cr=0.31) → NULL it.
Write the decision table to chat before editing.

### Step 3b — Verify adapter CODE support (thinking + cache), not just price values
A price refresh is necessary but not sufficient: the gateway must also **classify and cost** the
provider's tokens correctly. A model can be priced right yet leak its reasoning text into the
answer, or bill cache hits at full input. Each run, for the provider's models, confirm the
adapter code still handles its THINKING/REASONING and PROMPT-CACHE behavior (grep + the anchors
below; cite file:line in the report; this is verification, not a code change — fixing a found
gap is a separate, smoke-gated PR per the AI-gateway binding).

**Thinking / reasoning** (only for models that expose it):
- REQUEST enable reaches the wire: canonical `reasoning_effort` (OpenAI / compat identity),
  Gemini `generationConfig.thinkingConfig` / `thinking_budget`, Anthropic
  `nexus.ext.anthropic.thinking`. Anchors: `internal/execution/estimator/reasoning.go`,
  `specs/gemini/codec/codec.go`, `specs/openai/responses/codec_responses.go`.
- RESPONSE decode keeps reasoning SEPARATE from the answer: non-stream
  `choices[].message.reasoning_content` + streaming `delta.reasoning_content`→`ReasoningDelta`
  (never appended to the answer `Delta`). Anchors: `transport/normalize/codecs/openai_chat.go`;
  per-adapter tests e.g. `specs/compat/deepseek/coverage_test.go`.
- UNSUPPORTED-PARAM strip for thinking models (temperature/top_p/penalties) — ONLY where the
  provider returns an **observed 400** (binding §3a: no speculative rewrite). Pattern:
  `specs/compat/moonshot/rewrites.go`; Anthropic guards in `specs/anthropic/codec/codec.go`.
  Known gaps to recheck: DeepSeek has NO strip (confirm whether deepseek thinking 400s on
  temperature before adding one) and is absent from `estimator/output_budget.go`; Minimax/Xai
  have no thinking implementation.

**Prompt cache** (cost correctness):
- Token classification maps the provider's cache fields into `Usage.CacheReadTokens` /
  `CacheCreationTokens`. Anchors: `shared/traffic/detect.go`,
  `shared/traffic/adapters/api/<provider>/detect.go` (Anthropic `cache_read_input_tokens` +
  `cache_creation_input_tokens`; OpenAI `prompt_tokens_details.cached_tokens`; DeepSeek
  `prompt_cache_hit_tokens`; Gemini `cachedContentTokenCount`; Moonshot `prompt_cache_tokens`).
- Cost formula does not double-bill (anchor `internal/ingress/proxy/proxy_cachecost.go`):
  `regularInput = prompt − cacheRead − cacheCreation`, then
  `regularInput×input + cacheRead×cacheReadPrice + cacheCreation×cacheWritePrice + completion×output`.
- Only Anthropic has a real cache-WRITE surcharge (`cache_creation_input_tokens`); OpenAI /
  Gemini / DeepSeek / Moonshot caching is implicit (no write) → cache_write 0/NULL. Embeddings/
  image/audio fabricate no cache tokens (coerced to 0 at the audit boundary).
Record the thinking + cache code-verification result (✓ / gap + file:line) in the report.

### Step 3.5 — Approval gate (BINDING — no change without it)
**Every price and every model change requires explicit user approval before it is
applied to ANY surface (template JSON, seed-baseline.sql, or prod).** Run
`sync_pricing.py diff <desired.json>` and present its output to the user as the approval
request — it pairs each change with the **source URL + fetch date** at the top so the user
can confirm against the live page. For each drifted model show: field, old→new value, and
the reference link. Apply (Step 4) and prod-sql (Step 5) run ONLY after the user approves;
if the user approves a subset, build a reduced `desired.json` with only the approved models
and re-run. Never apply silently, never batch-approve across providers — one provider's diff,
one approval.

### Step 4 — Edit the repo surfaces (safe, reversible)
- Template JSON: update each model's price fields; add/flag models per Step 3; keep field order.
  Bump `index.json` `modelCount`. Mirror into `dist/` or rebuild the UI.
- `seed-baseline.sql`: update `inputPricePerMillion` / `outputPricePerMillion` and
  `status`/`deprecationDate`/`replacedBy` in the matching `Model` INSERT rows. (Cache prices are
  NOT in seed-baseline; they come from the seed.ts multiplier or an admin edit.)
- `seed.ts`: only if the provider's whole-family read/write ratio changed.
- English only. Decimal literals, not "$5 / MTok".

### Step 5 — prod is OUT OF SCOPE for this skill
This skill updates ONLY the repo surfaces — the template JSON and seed-baseline.sql. It
**never writes the prod `Model` table.** Changing prod prices is a SEPARATE operation that
requires explicit, per-run user approval every time (there is no standing authority), done
outside this skill under the prod-deploy backup discipline (pg_dump first, transactional
`UPDATE ... WHERE code=... AND "providerId"=(...)`, re-SELECT to verify). The deterministic
`sync_pricing.py prod-sql` subcommand may EMIT the UPDATE SQL for a human to review, but
this skill's flow stops at the repo. Do not run anything against prod as part of running
this skill.

### Step 6 — Verify
- Re-grep the edited template JSON + seed rows; confirm the numbers match the captured official table.
- Cost-estimation lockstep: a price change touches `cost-estimation-architecture.md`'s domain —
  if behavior/fields changed, update the doc (`scripts/check-doc-lockstep.mjs`). A pure value
  refresh usually needs no doc edit; note that in the summary.
- Optional strongest check: `Skill('smoke-gateway')` cross-checks the `traffic_event` cost stamp
  against the new prices for affected models.

---

## Output
Per provider, report: official table (with source URL + fetch date), the decision table
(drift / deprecations / adds-flagged), files edited, prod rows updated (before→after), the
Step 3b thinking + cache code-verification result (✓ / gap + file:line), and any model needing
a product decision. End with the commit reminder (never auto-commit):
`chore(pricing): sync <provider> models+prices from official page (<date>)`.

## Bindings
- Facts-only (above). `Model` = data model → configuration-architecture applies; new keys go
  through `configkey` + §7 catalog (not needed for value refresh).
- Dev-phase: deprecate/disable, never delete a Model row that historical `traffic_event` rows
  reference. English-only. pg_dump before prod writes. Ask-before-commit.
