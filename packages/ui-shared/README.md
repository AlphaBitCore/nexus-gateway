# @nexus-gateway/ui-shared

Pure presentational React + types + i18n shared between
**@nexus-gateway/control-plane-ui** (admin browser app) and
**@nexus-gateway/agent-dashboard** (Wails desktop app).

The two apps fetch from *different backends* — CP UI hits CP's
authenticated HTTP API, Dashboard talks to the local agent's
`statusapi` over a Unix socket / named pipe. This package exists so
the **presentation layer** is unified even though the **data layer**
is not.

## What lives here

| Folder | Contents | Why it's shareable |
|--------|----------|--------------------|
| `src/components/` | Stateless React components (Button, Card, …). Take data + callbacks as props. Render output only — no fetching, no global state, no router awareness. | Both apps render the same visual primitives. |
| `src/types/` | TypeScript shapes both backends produce (`Device`, `AuditEvent`, `PolicyRule`, …). Admin-only fields are optional so the agent path can omit them. | Pages compose components with these types as the contract. |
| `src/i18n/{en,zh,es}/shared.json` | The `shared` i18n namespace: strings every Nexus surface uses (actions, status, trust-level labels). | Translations are written once, consumed everywhere. |
| `src/styles/*.css` | CSS variable tokens, animations, utility classes. | Identical look-and-feel across web admin and desktop Dashboard. |
| `src/styles/prime-shadcn-tokens.css` | shadcn/Tailwind semantic tokens (aligned with prime-console). | Imported by each app's `tailwind-app.css` after `@import tailwindcss`. |
| `src/shadcn/` | shadcn-style primitives (`ShadcnButton`) + `src/lib/cn.ts`. | **New** code should prefer these; legacy `components/Button` is migrated incrementally. |

## What does NOT live here (binding rule)

- **API clients / HTTP fetchers / Wails bindings.** Each app owns its
  own data layer. ui-shared is transport-agnostic.
- **React Router or any opinionated routing.** Pages decide their own
  navigation primitives.
- **App-level context providers** (auth, theme switcher, query client).
  Apps wire these up themselves; ui-shared components must work
  without depending on a specific provider tree.
- **Anything that imports from `@nexus-gateway/control-plane-ui` or
  `@nexus-gateway/agent-dashboard`.** Inversion of control: ui-shared
  is a leaf in the dependency graph.

If something *feels* like it should be shared but requires a CP API
call or an `i18next-http-backend` setup, it belongs in the consumer
app, not here.

## How consumers use it

```ts
// control-plane-ui or agent-dashboard
import { Button, type Device } from '@nexus-gateway/ui-shared';
import '@nexus-gateway/ui-shared/styles/base.css';
import sharedEn from '@nexus-gateway/ui-shared/i18n/en/shared.json';
```

CP UI's `src/i18n/index.ts` and the Dashboard's equivalent both load
`sharedEn` into the `shared` namespace at i18n init time so
`t('shared:actions.cancel')` works in either app.

## Adding a component

1. Drop the `.tsx` + `.module.css` (+ test) under `src/components/<Name>/`.
2. Re-export from `src/index.ts`.
3. If the component supersedes an existing CP UI component, leave a
   thin re-export shim in `packages/control-plane-ui/src/components/ui/<Name>/index.ts`:
   ```ts
   export { Name } from '@nexus-gateway/ui-shared';
   export type { NameProps } from '@nexus-gateway/ui-shared';
   ```
   No imports need to change in CP UI's 200+ call sites.
4. Run `npm test --workspace=@nexus-gateway/ui-shared` and the CP UI
   test suite — both must stay green.

## Adding an i18n key

1. Edit `src/i18n/en/shared.json` (canonical).
2. Add the matching key to `zh/shared.json` and `es/shared.json` (the
   key set must match exactly — `node scripts/check-i18n-parity.mjs`
   enforces this in CI).
3. CP UI's build's `sync-locales` step copies the JSONs into its
   `public/locales/<lang>/` so the HTTP backend can load them.

## No build step (yet)

This package exports source TypeScript directly (`./src/index.ts`).
Vite in both consumer apps transpiles it. There's no `dist/` to
publish because we're a workspace package, not a registry-published
library. If we ever publish externally we'll add a `tsup` build; for
now keeping the source path means hot-reload works across the
boundary during dev.
