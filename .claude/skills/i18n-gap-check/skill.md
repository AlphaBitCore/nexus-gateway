---
name: i18n-gap-check
description: >
  Scan all frontend i18n keys across the Control Plane UI and the Agent
  Dashboard, and report gaps + hardcoded English strings. Covers two bundles:
  control-plane-ui (ns: pages | common | nav | shared) and agent-ui
  (ns: dashboard | shared). The `shared` namespace lives in
  `packages/ui-shared/src/i18n/` and is consumed by both. Detects:
  (1) keys used in source but missing from EN — highest priority, UI renders
  raw key strings; (2) EN keys missing from ES or ZH — translation gaps;
  (3) orphan keys in ES/ZH not in EN — stale translations;
  (4) EN keys not used in source — potentially stale;
  (5) dynamic `t()` template literals — manual review list;
  (6) hardcoded English in `.tsx` (JSX text + user-facing attribute
  literals) that bypass `t()` — manual review list.
  Trigger keywords: i18n gap, missing translations, i18n check, translation
  gaps, locale scan, hardcoded strings, untranslated UI, /i18n-gap-check.
user-invocable: true
---

# i18n Gap Check

Runs `tests/scripts/i18n_gap_check.py` against the Control Plane UI **and**
the Agent Dashboard source trees plus the shared locale files, then surfaces
a structured Markdown gap report.

## Scope

Two frontend bundles are scanned:

| Bundle | Source | Namespaces | Locale dirs |
|---|---|---|---|
| `control-plane-ui` | `packages/control-plane-ui/src` | `pages`, `common`, `nav`, `shared` | `pages/common/nav` in `packages/control-plane-ui/src/i18n/locales/`; `shared` in `packages/ui-shared/src/i18n/` |
| `agent-ui` | `packages/agent/ui/frontend/src` | `dashboard`, `shared` | `dashboard` in `packages/agent/ui/frontend/src/i18n/locales/`; `shared` in `packages/ui-shared/src/i18n/` |

Bare `t('key')` calls (no `ns:` prefix) are resolved against the file's
`useTranslation('ns')` declaration; if none is present, the bundle's
default namespace is used (`common` for cp-ui, `dashboard` for agent-ui).

## When to use

- User types `/i18n-gap-check` or asks to "check i18n keys", "scan locale
  gaps", "find missing translations", "find hardcoded English in the UI".
- After adding new UI features in either app, to verify every `t('ns:key')`
  call has entries in all three locales.
- Before a release to catch raw key strings showing in the UI **and** to
  catch any English text that was committed without being routed through
  `t()`.

## Workflow

```
run script → read report → summarise findings → offer to fix Section 1 + Section 6 first
```

### Step 1: Run the gap-check script

```bash
cd /path/to/repo && python3 tests/scripts/i18n_gap_check.py
```

The repo root is the directory containing `packages/` and `tests/`. The script
auto-detects the repo root from its own location — no arguments needed for a
standard run.

```bash
# Standard run (auto-detects repo root):
python3 tests/scripts/i18n_gap_check.py

# Explicit repo root (if cwd is unusual):
python3 tests/scripts/i18n_gap_check.py --repo-root /path/to/repo
```

The script writes a Markdown report to `/tmp/i18n-gap-<UTC-timestamp>.md`
and also prints it to stdout. Capture the path from the last line.

### Step 2: Parse and present findings

Read the **Summary** table from the report and present it to the user
(one row per bundle plus a total):

| Bundle | Source keys | Missing EN | Missing ES/ZH | Orphan ES/ZH | Stale EN | Dynamic t() | Hardcoded EN |
|---|---|---|---|---|---|---|---|
| `control-plane-ui` | … | **N** | N | N | N | N | **N** |
| `agent-ui` | … | **N** | N | N | N | N | **N** |
| **total** | … | **N** | **N** | N | N | N | **N** |

Then, per bundle, present each section with its count and the first 10–20
items as a preview (full list is in the report file).

### Step 3: Offer to fix Section 1 gaps (critical path)

Section 1 — "Keys used in source but missing from EN" — are the highest-
priority items: the UI renders raw key strings like `pages:providers.type`
for these. Each entry shows the file/line where the key is first used.

After reporting, ask the user:

> **Section 1 has N missing EN keys (X in cp-ui, Y in agent-ui). Do you want
> me to add them now?**
> - A: Yes — add them to all 3 locales (en/es/zh) with sensible EN text and
>   translated ES/ZH values
> - B: Yes — add EN only, mark ES/ZH with `[TODO]` placeholders
> - C: No — I'll handle manually

If the user chooses A or B, proceed per the **Fix Protocol** below.

### Step 4: Offer to fix Section 6 — hardcoded English (critical path)

Section 6 — "Hardcoded English strings in `.tsx`" — are JSX text content
and user-facing attribute literals (`title=`, `placeholder=`, `aria-label=`,
`label=`, `description=`, `message=`, etc.) that bypass `t()` entirely. These
are *also* user-visible English; they break ZH/ES users today.

Heuristics flag a superset of real issues — review before bulk-fixing. A few
items are legitimately not-for-translation:

- Brand names (`OpenAI`, `Anthropic`, `Slack`, …)
- Acronyms used as labels (`API`, `SSO`, `mTLS`, …)
- Format examples in `placeholder` (e.g. `xoxb-…`, `0oa1abc2def3ghi4j5k6`,
  `smtp.example.com`, `e.g. Acme Corp`) — usually OK to leave verbatim
- Redaction-token examples (`[REDACTED_<RULE_ID>]`)

Ask the user:

> **Section 6 has N hardcoded English strings. Most look like real
> untranslated text (e.g. `Issuer URL`, `Client ID`, `No breakdown data
> available.`). Want me to:**
> - A: Walk file-by-file, replace each with `t('ns:...')`, add new keys to
>   all 3 locales — I'll show diffs before saving
> - B: Just generate a TODO list per file
> - C: No — I'll handle manually

If the user chooses A, follow the **Fix Protocol** below for each new key
introduced.

### Step 5: Offer to fix Section 2 gaps (translation gaps)

Section 2 — "EN keys missing from ES or ZH" — are keys that exist in EN but
lack a translation. After Section 1 and Section 6 are handled, ask:

> **Section 2 has N translation gaps. Do you want me to fill them in ES/ZH?**

If yes, follow the same **Fix Protocol** for the missing locale keys.

---

## Fix Protocol

When adding missing keys to locale files, the file paths depend on the
bundle and namespace:

| Bundle | Namespace | File to edit |
|---|---|---|
| `control-plane-ui` | `pages` / `common` / `nav` | `packages/control-plane-ui/src/i18n/locales/{en,es,zh}/<ns>.json` |
| `control-plane-ui` | `shared` | `packages/ui-shared/src/i18n/{en,es,zh}/shared.json` |
| `agent-ui` | `dashboard` | `packages/agent/ui/frontend/src/i18n/locales/{en,es,zh}/dashboard.json` |
| `agent-ui` | `shared` | `packages/ui-shared/src/i18n/{en,es,zh}/shared.json` |

Steps:

1. **Locate the correct JSON position** — keys must be nested under their
   existing parent object. For `pages:providers.type`, open
   `packages/control-plane-ui/src/i18n/locales/en/pages.json`, find the
   `"providers"` object, and add `"type": "Type"` in alphabetical order
   within that object.

2. **Add to all 3 locale files simultaneously** in the matching directory
   per the table above.

3. **After all edits, sync cp-ui's `public/locales/`** (only required for
   the Control Plane UI, which loads non-EN locales over HTTP at runtime):

   ```bash
   cp -r packages/control-plane-ui/src/i18n/locales/* \
         packages/control-plane-ui/public/locales/
   ```

   The Agent Dashboard imports JSON directly via Vite and needs **no sync**.
   The `ui-shared` `shared` namespace is also imported directly — no sync.

4. **Verify key counts match** across all 3 locales:

   ```bash
   python3 -c "
   import json, sys
   from pathlib import Path
   def count(path):
       def flat(o, p=''):
           if isinstance(o, dict):
               for k, v in o.items():
                   f = f'{p}.{k}' if p else k
                   if isinstance(v, dict): yield from flat(v, f)
                   else: yield f
       with open(path) as f:
           return sum(1 for _ in flat(json.load(f)))
   targets = [
       ('cp-ui pages',     'packages/control-plane-ui/src/i18n/locales/{loc}/pages.json'),
       ('cp-ui common',    'packages/control-plane-ui/src/i18n/locales/{loc}/common.json'),
       ('cp-ui nav',       'packages/control-plane-ui/src/i18n/locales/{loc}/nav.json'),
       ('ui-shared',       'packages/ui-shared/src/i18n/{loc}/shared.json'),
       ('agent dashboard', 'packages/agent/ui/frontend/src/i18n/locales/{loc}/dashboard.json'),
   ]
   for name, tmpl in targets:
       print(name, {loc: count(Path(tmpl.format(loc=loc))) for loc in ('en','es','zh')})
   "
   ```

   All three locales must have the **same count** per file.

5. **Re-run the gap-check** to confirm Section 1 and Section 6 counts drop:

   ```bash
   python3 tests/scripts/i18n_gap_check.py
   ```

6. **Do not commit** until the user explicitly asks.

---

## Replacing hardcoded English (Section 6) with `t()` calls

When converting a hardcoded string to a `t()` call:

1. **Pick the right namespace** — use the namespace already in use in that
   file (look for `useTranslation('ns')` at the top, or existing `t('ns:...')`
   calls). For agent-ui pages, this is usually `dashboard`. For cp-ui pages,
   usually `pages`. Common buttons/status labels shared across both apps
   belong in `shared`.

2. **Pick a clear key path** — use the page/section name as the prefix,
   then a short camelCase descriptor. Example: `pages:idp.fieldClientId`,
   `dashboard:diagnostics.noBreakdownData`.

3. **Edit the JSX**:

   ```tsx
   // before
   <FormField label="Client ID" />
   <p>No breakdown data available.</p>

   // after
   <FormField label={t('pages:idp.fieldClientId')} />
   <p>{t('pages:diagnostics.noBreakdownData')}</p>
   ```

4. **Add the new key to all 3 locale files** per the Fix Protocol above.

5. **If you keep something hardcoded on purpose** (brand name, format
   example, acronym), leave a one-line comment explaining why so the next
   scan reviewer doesn't flag it again, e.g. `// brand name -- not translated`.

---

## Translation guidelines

When generating translations, follow these conventions:

| Term | EN | ES | ZH |
|---|---|---|---|
| Provider | Provider | Proveedor | 提供商 |
| Model | Model | Modelo | 模型 |
| Credential | Credential | Credencial | 凭证 |
| Type | Type | Tipo | 类型 |
| Status | Status | Estado | 状态 |
| Enable / Disable | Enable / Disable | Habilitar / Deshabilitar | 启用 / 禁用 |
| Create / Edit / Delete | Create / Edit / Delete | Crear / Editar / Eliminar | 创建 / 编辑 / 删除 |
| Loading… | Loading… | Cargando… | 加载中… |
| No data | No data | Sin datos | 暂无数据 |

Technical terms that stay in English across all locales: API, SSO, mTLS,
Token, Hook, Agent, Device, VK, Provider, Model, SDK, HTTP, JSON, NATS,
Redis, OpenAI, Anthropic, OAuth, OIDC, SAML, MFA, PKCE.

---

## Notes

- The script statically resolves `t('ns:key')`, `t("ns:key")`, and bare
  `t('key')` calls. Dynamic template literals (`` t(`ns:${var}`) ``) appear
  in Section 5 for manual review.
- Section 4 (stale EN keys) may include keys used via dynamic patterns — do
  not delete them without cross-checking Section 5 for a matching prefix.
- Section 6 uses heuristics (multi-word JSX text, user-facing attributes)
  and intentionally flags a superset; review each hit before fixing. Pure
  format placeholders (e.g. `xoxb-…`, `e.g. Acme Corp`) and brand names are
  usually fine to leave as-is.
- The script does not modify any files; all changes are made by Claude
  following the Fix Protocol above.
- Files in `node_modules/`, `dist/`, `__tests__/`, `*.test.*`,
  `*.spec.*`, `*.stories.*`, and `*.d.ts` are skipped from both scans.
