---
doc: i18n-pipeline-architecture
area: cross-cutting
service: ui
tier: 1
updated: 2026-05-20
---

# i18n Pipeline Architecture

> **Tier 2 architecture doc.** Read this when touching `t('namespace:section.key')` usage, locale files, or the parity-check pipeline. IDE-side rule: `.cursor/rules/i18n-mandatory.mdc`. Lint: `scripts/check-i18n-parity.mjs` (`npm run check:i18n`). Existing skill: `/i18n-gap-check`.

Every user-visible string in JSX MUST go through `t('namespace:section.key')` from `react-i18next`. The pipeline keeps three locales (en / zh / es) in lockstep across two UI bundles (Control Plane UI + Agent Dashboard).

---

## 1. The two bundles + three locales

| Bundle | Local-namespace source path | Local namespaces | Runtime delivery |
|---|---|---|---|
| Control Plane UI | `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/*.json` | `pages.json`, `common.json`, `nav.json` | i18next-http-backend fetches from `packages/control-plane-ui/public/locales/{en,zh,es}/*.json` at runtime. `pages`/`common`/`nav` are copied here; `shared.json` is published from `packages/ui-shared/src/i18n/{en,zh,es}/shared.json` into the same `public/locales/` tree. |
| Agent Dashboard | `packages/agent/ui/frontend/src/i18n/locales/{en,zh,es}/dashboard.json` | `dashboard.json` only | Bundled directly via JS `import` — Wails has no http-backend; `shared.json` is also imported directly from `@nexus-gateway/ui-shared/i18n/{en,zh,es}/shared.json` (see `packages/agent/ui/frontend/src/i18n/index.ts`). |

The `shared.json` namespace lives in `packages/ui-shared/src/i18n/{en,zh,es}/shared.json` and is **consumed by both bundles**. Reusable atoms (button labels like `Save`, `Cancel`, `Delete`, status terms like `Online`, `Offline`) belong there. Note that CP-UI's `src/i18n/locales/<lang>/` does NOT carry `shared.json` — that file flows into the CP-UI build from `ui-shared`.

## 2. Namespaces

| Namespace | What goes there |
|---|---|
| `pages` | Page-level text (titles, descriptions, form labels per page) |
| `common` | Reusable atoms used by multiple pages but NOT in `shared` (because they're CP-UI-specific) |
| `nav` | Sidebar labels |
| `shared` | Atoms reusable across CP-UI **and** Agent Dashboard |
| `dashboard` | Agent-Dashboard-page-level text |

## 3. Key shape

```tsx
t('pages:settings.theme.title')
t('common:fields.email')
t('nav:aiGateway.routingRules')
t('shared:status.online')
```

Convention: `namespace:section.subsection.<leaf>`. Leaves are camelCase, sections are camelCase.

## 4. The 4 binding rules

1. **Mandatory wrap** — every user-visible JSX string uses `t()`. Hardcoded English is forbidden.
2. **Add to all 3 locales** — when adding a key, add it to `en`, `zh`, AND `es`. The parity check fails CI on missing keys.
3. **Mirror CP-UI's `pages` / `common` / `nav` to `public/locales/`** — those three namespaces live in `src/i18n/locales/` AND must be republished to `public/locales/` so i18next-http-backend can fetch them at runtime. `shared.json` is sourced from `packages/ui-shared/src/i18n/` and lands in `public/locales/` via the build; do NOT add a `shared.json` to CP-UI's `src/i18n/locales/`. Agent UI requires no mirror — it imports JSON directly.
4. **Technical terms stay English** — `API`, `SSO`, `Provider`, `Model`, `Agent`, `Device`, `Hook`, `Token`, `mTLS`, `OAuth`, `PKCE`, `JWT` etc. are kept in English across all locales.

## 5. Adding a new key

```bash
# Pick the correct namespace + edit all three locale files:
$EDITOR packages/control-plane-ui/src/i18n/locales/en/pages.json
$EDITOR packages/control-plane-ui/src/i18n/locales/zh/pages.json
$EDITOR packages/control-plane-ui/src/i18n/locales/es/pages.json

# If the key is reusable across CP-UI AND Agent, edit ui-shared/src/i18n/<lang>/shared.json instead.

# Mirror CP-UI's pages/common/nav to public/ (do NOT touch shared.json there):
for lang in en zh es; do
  cp packages/control-plane-ui/src/i18n/locales/$lang/{pages,common,nav}.json \
     packages/control-plane-ui/public/locales/$lang/
done

# Verify:
npm run check:i18n
```

The `i18n-gap-check` skill (`/i18n-gap-check`) provides a more comprehensive sweep including hardcoded-string detection in `.tsx` files.

## 6. Detection passes (in `i18n-gap-check`)

1. **Keys used in source but missing from EN** — UI would render the raw key string at runtime; highest priority.
2. **EN keys missing from ES / ZH** — translation gaps.
3. **Orphan keys in ES / ZH not in EN** — stale translations.
4. **EN keys not used in source** — potentially stale, candidates for removal.
5. **Dynamic `t()` template literals** — `t(\`pages:${section}.title\`)`; manual review list (can't statically verify).
6. **Hardcoded English in `.tsx`** — JSX text + user-facing attribute literals that bypass `t()`.

## 7. Sources

- `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/{pages,common,nav}.json` — CP-UI source-of-truth for its three local namespaces.
- `packages/control-plane-ui/public/locales/{en,zh,es}/*.json` — runtime-served mirror; consumed by i18next-http-backend on CP-UI boot. Contains `pages.json`, `common.json`, `nav.json` (mirrored from `src/i18n/locales`) PLUS `shared.json` (sourced from `ui-shared`).
- `packages/agent/ui/frontend/src/i18n/locales/{en,zh,es}/dashboard.json` — Agent UI source-of-truth (only `dashboard.json` lives here).
- `packages/agent/ui/frontend/src/i18n/index.ts` — Agent's i18n bootstrap; direct JSON `import` for `dashboard` + `shared` (no http-backend, Wails firewalls network).
- `packages/ui-shared/src/i18n/{en,zh,es}/shared.json` — `shared` namespace, consumed by both bundles.
- `scripts/check-i18n-parity.mjs` — parity check.
- `scripts/check-json-dupkeys.mjs` — duplicate-key guard inside JSON.
- `.cursor/rules/i18n-mandatory.mdc` — IDE rule.
- `.claude/skills/i18n-gap-check/` — comprehensive audit skill.

<!-- 💡 harvest: nothing new — existing rule + lint + skill set is complete. -->

## 8. Cross-references

- `design-tokens-architecture.md` — sister UI binding.
- `useapi-queryclient-architecture.md` — i18n keys often appear in API hook returns (validation messages).
- `ui-shared-architecture.md` — where the `shared` namespace lives.
- Agent UI terminology binding → `.cursor/rules/agent-ui-terminology.mdc` (which terms agent UI must avoid even after translation).
