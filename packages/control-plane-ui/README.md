# @nexus-gateway/control-plane-ui

The admin browser app — React + TypeScript + Vite. Talks exclusively to
Control Plane (`packages/control-plane`) via the OAuth + PKCE-protected
admin API. Every visible feature in the dashboard, from IAM management
to traffic audit to live metrics, lives here.

## Where it sits

| | |
|---|---|
| **Port (dev)** | `3000` (Vite); proxies `/api/*` to Control Plane on `3001` |
| **Build output** | `dist/` — static SPA the Control Plane serves in prod |
| **Auth** | OAuth + PKCE bearer cached in `localStorage`; refresh-token flow on 401 |
| **Shares with Agent UI** | The `@nexus-gateway/ui-shared` workspace package (components, types, `shared` i18n namespace) |

## Build / dev / test

```bash
# Dev server (HMR; proxies API to localhost:3001):
npm run dev:control-plane-ui

# Type-check + Vite build:
make control-plane-ui-build         # or: npx tsc --noEmit && npx vite build

# Unit tests (Vitest):
make control-plane-ui-test

# Lint:
make control-plane-ui-lint
```

## Key directories

| Path | Purpose |
|---|---|
| `src/routes/` | Top-level routing config (`shellRouteConfig.tsx`) + per-area shell. Every admin nav item maps here. |
| `src/pages/` | Per-feature page bundles (IAM, virtual keys, providers, traffic, routing, hooks, settings, …). |
| `src/components/` | Page-scoped components. Generic primitives belong in `@nexus-gateway/ui-shared`. |
| `src/lib/` | API client (`apiFetch`, `useApi`), auth helpers, OAuth flow, query-key conventions. |
| `src/i18n/locales/{en,zh,es}/` | The canonical i18n source. `src/i18n/locales/` is the truth; `public/locales/` is a copy for the HTTP backend (kept in sync by `sync-locales.mjs`). |
| `src/components/ui/Sidebar/` | Nav structure + icon mapping. Every new route lands here. |
| `e2e/` | Playwright UI tests. |

## Binding rules (per CLAUDE.md)

- **i18n mandatory.** No hardcoded JSX strings. Always `t('namespace:key')`.
- **Design tokens strict.** No hex / rgba / hsla literals; no raw
  numeric padding/margin/gap/fontSize in `style={{}}`. Layer-2 semantic
  CSS variables only (`--color-*`, `--space-*`, `--sidebar-*`, …).
  Enforced by `npm run check:design-tokens`.
- **`useApi` queryKey contract.** Every `useApi(fetcher, queryKey)`
  call starts with at least two string-literal segments
  (`['admin', '<resource>', ...]`). Bare state arrays collide in the
  React Query cache.
- **IAM impact review** is mandatory when adding / renaming an admin
  endpoint or sidebar route — see CLAUDE.md "API / menu / route
  changes require IAM impact review".

## Architecture references

- `docs/developers/architecture/cross-cutting/ui/design-tokens-architecture.md`
- `docs/developers/architecture/cross-cutting/ui/i18n-pipeline-architecture.md`
- `docs/developers/architecture/services/control-plane/iam-identity-architecture.md`
- `docs/users/features/cp-ui/` — per-section feature docs.
