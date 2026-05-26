# E29 — Hooks Region + Rule Pack Runtime Refactor

Status: In progress
Epic owner: Gateway / Compliance
Related: E27 (Rule Packs import/dry-run foundation), E28 (Provider adapter redesign — authoritative provider metadata).

## 1. Background

Audit of `/config/hooks` surfaced three defects:

1. `HookInput.SourceIP` is not populated on the ai-gateway path, so
   `ip-access-filter` evaluated at request stage always saw an empty
   IP.
2. `HookInput.ProviderRegion` was intended to be an authoritative
   per-provider deployment region; in practice the value was left
   blank because neither routing nor provider metadata carried it.
   Data-residency evaluation therefore degraded to a "no region =
   block" or "no region = allow" toss-up depending on the policy
   shape.
3. The Rule Pack feature (E27) delivers admin-only listing, import,
   and dry-run, but nothing about it participates in the runtime hook
   pipeline. Meanwhile `content-safety`, `keyword-filter`, and
   `pii-detector` each carry their own pattern/regex configuration
   inside a `HookConfig` blob, so operators have three overlapping
   ways to describe "block content that looks like X" with no shared
   lifecycle or telemetry. Pack creation/edit/delete is also missing
   from the UI.

This epic resolves all three in one pass. The user has explicitly
approved a full refactor (no phased compatibility path), consistent
with the project's pre-GA "no backward compatibility, no defer"
policy.

## 2. Scope

### Must
- Inject authoritative `SourceIP` into every request- and
  response-stage `HookInput` produced by ai-gateway.
- Store provider deployment region as a first-class column on the
  `Provider` table and expose it through the admin API / UI.
- Resolve `ProviderRegion` from provider metadata (routing target)
  for every request- and response-stage `HookInput` produced by
  ai-gateway. Connection-stage input continues to set
  `ProviderRegion` only when the connection already knows the target.
- Seed a `request`-stage `content-safety` hook row so operators can
  enable inbound content safety without manually inserting SQL.
- Provide a single runtime Rule Pack engine (`rulepack-engine` hook
  implementation) that evaluates installs + overrides against hook
  content and emits a standard `Match` on hit.
- Migrate `content-safety`, `keyword-filter`, and `pii-detector`
  runtime evaluation onto the Rule Pack engine. The hook rows still
  exist but delegate to the shared engine; their bespoke config
  blobs are replaced by "which Rule Packs bind to me".
- Complete the Rule Pack Admin API with update, delete, list-installs,
  and uninstall / disable operations.
- Persist `blocking_rule` attribution (`pack/version/ruleId`) on
  audit records produced by ai-gateway and compliance-proxy when a
  rule-pack match causes a reject decision.
- Wire a full Rule Pack CRUD surface in the Control Plane UI,
  including create-from-scratch, edit metadata, edit rules, and
  delete flows. Install / uninstall / override panels become
  first-class pages instead of embedded drawers.
- i18n parity across `en / zh / es` for every new UI string.

### Should
- Report `SourceIP` on the ai-gateway audit record consistently
  (single extraction helper shared across middleware and request
  handler).
- Allow an operator to clear a provider's region (send `null`) and
  have the admin API differentiate "not provided" (keep existing
  value) from "explicit null" (clear).

### Could
- Future: region inference from provider base URL when the column is
  empty. Out of scope for this epic.

### Won't
- Re-introduce IP geolocation on the request hot path.
- Add a Rule Pack "versioning graph / promotion" surface. Installs
  continue to pin `(packId, pinVersion)` like E27.

## 3. User roles & personas

- **Compliance operator** — owns which Rule Packs bind to which
  hooks, creates / edits / publishes packs, reviews pack hit audit.
- **Platform operator** — owns provider deployment region metadata.
- **Service account** — runtime caller; hits ai-gateway `/v1`.
- **Admin viewer** — reads pack catalog, installs, audit, but cannot
  mutate.

## 4. Constraints & assumptions

- All new work is greenfield (pre-GA), so there is no rollback /
  compat shim for the hook runtime path.
- Provider region is a free-form string that matches the
  `allowedRegions` sets used by `data-residency` policies. No
  validation ladder is required beyond trimming whitespace.
- Rule Pack rule matching continues to be regex-based as delivered in
  E27; semantic classifiers stay in `ai-guard`.
- Hook configuration cache invalidation still flows through
  `thingclient.OnConfigChanged` — the runtime engine must re-resolve
  active installs without restart.

## 5. Functional requirements (MoSCoW)

**Must**

- F1. ai-gateway populates `HookInput.SourceIP` on request- and
  response-stage hooks from the shared `middleware.ClientIP`
  extractor (trusted `X-Forwarded-For` first hop → `X-Real-IP` →
  `RemoteAddr` host).
- F2. ai-gateway populates `HookInput.ProviderRegion` on request-
  and response-stage hooks from the resolved routing target's
  `Provider.region`. Unknown region is the empty string; policies
  decide how to treat that.
- F3. Provider CRUD API exposes `region` as a nullable string on
  list / get / create; `update` distinguishes missing key (keep),
  string (set), and explicit null (clear).
- F4. Seed inserts two `content-safety` rows: one at `request`
  stage and one at `response` stage, both disabled by default.
- F5. A new hook implementation `rulepack-engine` evaluates the
  active `RulePackInstall` rows bound to the hook, applies rule
  overrides, matches rules against `HookInput.Content`, and returns
  the highest-severity rule as the decision (`hard` → RejectHard,
  `soft` → RejectSoft, `info` → tag-only Approve).
- F6. The `rulepack-engine` emits a `BlockingRule` (pack name,
  pack version, rule id, category, severity, labels) on its
  `HookResult`. Audit records persist this attribution in a
  `blocking_rule` JSON column.
- F7. Existing `content-safety`, `keyword-filter`, `pii-detector`
  hook rows delegate their runtime execution to the rule-pack
  engine. Their legacy config blobs (`patternDefinitions`,
  `patterns`, `categories`) are no longer interpreted at runtime;
  the UI either hides the fields or turns them into "manage packs"
  shortcuts.
- F8. The Admin API supports `PATCH /api/admin/rule-packs/{id}`
  (metadata + rules), `DELETE /api/admin/rule-packs/{id}`,
  `GET /api/admin/hooks/{hookId}/rule-packs` (list installs),
  `DELETE /api/admin/rule-pack-installs/{installId}` (uninstall),
  and a form-level `create` route that produces a pack without a
  YAML upload.
- F9. The UI exposes full CRUD for packs: list, detail, create,
  edit metadata, edit rules, delete. Bind / unbind / override flows
  are full pages (not modals-only) with deep links.

**Should**

- S1. ai-gateway audit record's `SourceIP` field matches the value
  injected into `HookInput`, so hook telemetry and analytics stay
  consistent.
- S2. Admin provider API serializes `region` as either a JSON
  string or `null`; legacy rows without a region value return
  `null` rather than an empty string.
- S3. UI provider form renders a free-form region text input with
  placeholder suggestions (`us-east-1`, `eu-west-1`, …) and stores
  `null` when cleared.

**Could**

- C1. Analytics dashboards surface `blocking_rule` aggregate
  counters per pack/version/rule so operators can spot regressions
  in a pack version quickly. (Out of scope for this epic; record as
  follow-up.)

**Won't**

- W1. Pack-level import/export UI changes beyond the existing
  import-YAML modal remain as-is.
- W2. Rule Pack signing flows are not extended.

## 6. Non-functional requirements

- **Performance** — The rule-pack engine must evaluate O(100) rules
  per request with sub-millisecond added latency on the request
  hot path (regex compilation is cached per install).
- **Reliability** — Hot-swap of installs/overrides must not
  interrupt in-flight requests; the runtime engine must read an
  atomic snapshot of effective rules.
- **Security** — No raw API keys or secrets leak into
  `blocking_rule` or audit bodies; rule text may include patterns
  but must not include runtime match payloads by default.
- **Observability** — Every matched rule emits a structured log
  event with pack / rule / severity / hookId for SIEM pipelines.
- **Internationalization** — Every new UI string has keys in
  `en / zh / es` per repository policy.

## 7. Glossary

- **Rule Pack** — versioned bundle of regex rules identified by
  `(name, version)`. Installed onto a specific hook.
- **Install** — the binding of a `(packId, pinVersion)` to a hook.
- **Effective rule set** — the install's pack rules after applying
  per-rule `RuleOverride` rows.
- **Blocking rule** — the pack/version/rule triple attributed to a
  reject decision.
- **Provider region** — the deployment region of the upstream
  provider, e.g. `us-east-1`. Free-form string compared by
  `data-residency`.

## 8. Out-of-scope / future work

- Pack-level promotion / approval workflow.
- Geo-IP based `SourceIP → region` classification.
- Per-detector semantic classifier routing (remains in `ai-guard`).
- Analytics dashboard for pack match counts (see C1).
