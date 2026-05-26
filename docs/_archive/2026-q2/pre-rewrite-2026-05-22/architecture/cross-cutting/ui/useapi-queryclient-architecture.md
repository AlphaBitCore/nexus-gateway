---
doc: useapi-queryclient-architecture
area: cross-cutting
service: ui
tier: 1
updated: 2026-05-20
---

# `useApi` + QueryClient Architecture

> **Tier 2 architecture doc.** Read this when touching `useApi` / `useMutation` hooks, React Query setup, or any `queryKey` shape. IDE-side rule: `.cursor/rules/useapi-querykey.mdc`. Lint: `scripts/check-useapi-querykey.mjs` (`npm run check:useapi-querykey`).

`useApi` is the standard pattern for fetching admin API data in the Control Plane UI. It is a thin wrapper over TanStack React Query's `useQuery` that enforces a `['api', ...]` queryKey namespace, applies admin-dashboard-appropriate defaults (always-fresh on mount, `keepPreviousData` for stable filter inputs), and normalises the return shape. It does NOT touch auth, error normalisation, or routing — those concerns live in the fetcher functions and `AuthContext`.

---

## 1. The hook

```tsx
const { data, loading, error, refetch } = useApi<RoutingRule[]>(
  fetcher,                                                              // () => Promise<T>
  ['admin', 'routing-rules', 'list', search, enabled, offset, limit],   // queryKey
  { skip?, refetchInterval?, staleMs? }?,                               // narrow options
);
```

Signature (`packages/control-plane-ui/src/hooks/useApi.ts`):

```ts
export function useApi<T>(
  fetcher: () => Promise<T>,
  queryKey: readonly unknown[],
  options?: {
    skip?: boolean;            // disables the query (returns data=null, loading=false)
    refetchInterval?: number;  // ms; >0 enables auto-refetch (live views like reliability panel)
    staleMs?: number;          // overrides staleTime (default 0 = always refetch on mount)
  },
): { data: T | null; loading: boolean; error: Error | null; refetch: () => void };
```

Internally `useApi` calls `useQuery` with:

- `queryKey: ['api', ...queryKey]` so every useApi entry sits under the `api` namespace.
- `queryFn: fetcher` — passed through unchanged; the fetcher owns its own auth + URL.
- `refetchOnMount: 'always'` when `staleMs` is 0 (the default — always-fresh on nav-back); `true` when the caller opts into a positive `staleMs`.
- `refetchOnWindowFocus: true`, `retry: 1`.
- `placeholderData: keepPreviousData` so filter/search changes don't unmount the input mid-keystroke.

The return shape uses `loading` (not `isLoading`); `loading` is `false` while `skip` is set even when the underlying query is technically pending.

## 2. The binding `queryKey` shape

The required shape is:

```
['admin' | 'my' | 'user' | 'proxy', '<resource>', '<variant?>', ...stateVars]
```

The **first** element is the domain (allowed: `admin`, `my`, `user`, `proxy`). The **second** is the resource (string literal). Subsequent elements are state vars that affect the fetch.

### Examples (correct)

```tsx
useApi(fetcher, ['admin', 'routing-rules', 'list', search, enabled, offset, limit])
useApi(fetcher, ['admin', 'policies', 'detail', id])
useApi(fetcher, ['admin', 'providers', 'list', 'model-list-picker'])   // usage-suffix
useApi(fetcher, ['my', 'profile'])
useApi(fetcher, ['proxy', 'health', instanceId])
```

### Examples (forbidden — cause cache collisions)

```tsx
useApi(fetcher, [])                                          // ❌ empty
useApi(fetcher, [debouncedSearch, offset, pageLimit])        // ❌ no domain prefix
useApi(fetcher, [id])                                        // ❌ single var
```

## 3. Why this matters

React Query stores entries under `['api', ...queryKey]`. Two pages with the same state shape produce the same key:

```
Page A queryKey = ['', '', 0, 20]
Page B queryKey = ['', '', 0, 20]   ← same!
```

Navigating from Page A → Page B shows A's `data` with B's `columns` for an instant before refetch — confusing UI bug class.

The two-string-literal prefix (domain + resource) prevents this. `['admin', 'routing-rules', 0, 20]` and `['admin', 'policies', 0, 20]` are distinct.

## 4. Disambiguating duplicate fetchers

When the same API is fetched from multiple call sites intentionally (e.g., providers list used by both `ModelList` and `CredentialList`):

```tsx
// ModelList:
useApi(fetcher, ['admin', 'providers', 'list', 'model-list-picker'])

// CredentialList:
useApi(fetcher, ['admin', 'providers', 'list', 'credential-list-picker'])
```

The usage-site suffix makes the two callers dedupe **only within themselves**. They don't leak stale data into unrelated pages.

## 5. QueryClient configuration

The shared `QueryClient` is instantiated inline in `packages/control-plane-ui/src/main.tsx` (no separate `lib/queryClient.ts`). Defaults:

- `staleTime: 30_000` (30 seconds; data considered fresh)
- `retry: 1` (one retry on failure)
- `refetchOnWindowFocus: true` (window-focus refetches enabled)

Per-query overrides flow through `useApi`'s `staleMs` / `refetchInterval` / `skip` options. `useApi` itself sets a per-call `refetchOnWindowFocus: true` and `placeholderData: keepPreviousData` regardless of the QueryClient default, so admin pages can rely on identical behaviour at every call site.

## 6. Mutations

`useMutation` (file `packages/control-plane-ui/src/hooks/useMutation.ts`, function `useMutation`) is the sibling for write operations. It wraps TanStack's `useMutation` with auto-toast + auto-namespaced invalidation:

```tsx
const { mutate, loading, error } = useMutation<CreateInput, RoutingRule>(
  (input) => apiCreate(input),
  {
    onSuccess: (rule) => navigate(`/ai-gateway/routing/${rule.id}`),
    successMessage: t('pages:routing.createSuccess'),
    errorMessage: t('common:errors.generic'),
    silentError: false,                    // default: a toast fires on error
    invalidateQueries: [['admin', 'routing-rules']], // omit the 'api' prefix — useMutation prepends it
  },
);
```

Behaviour:

- A success toast fires automatically using `successMessage` (or `'Operation completed successfully'`).
- An error toast fires automatically using `errorMessage` (or `err.message`) unless `silentError: true` is set.
- Every entry in `invalidateQueries` is auto-prefixed with `'api'` if not already present, so callers can write `['admin', '<resource>']` and the hook normalises to `['api', 'admin', '<resource>']` (matching the keys `useApi` registered under).
- The return shape is `{ mutate, loading, error }`. `mutate` is the async `mutateAsync` — `await mutate(input)` resolves with the mutation result.

## 7. Auth integration

`useApi` itself does NOT touch the bearer token. The fetcher passed in owns its own auth: every CP-UI fetcher composed via `lib/api/*` reads the token from `AuthContext` and stamps `Authorization: Bearer <token>` before issuing the request. `useApi` is auth-blind on purpose — it only forwards whatever the fetcher returns or throws.

Token lifecycle, OAuth + PKCE redirect, and `/login` recovery on expiry live in `packages/control-plane-ui/src/auth/context/AuthContext.tsx` (and the OAuth callback page). Failures from a fetcher (`401 / 403`) surface as thrown Errors and propagate through `useApi` to the consumer, which is responsible for either showing an inline error or delegating to the AuthContext's session-recovery flow.

## 8. Enforcement

| Tool | Catches |
|---|---|
| `npm run check:useapi-querykey` | Forbidden shapes (empty, no-domain, single-element) in `*.tsx` / `*.ts` |
| `npm run check:useapi-querykey:strict` | Same, fails CI |
| `.cursor/rules/useapi-querykey.mdc` | IDE-time rule with examples |

## 9. Sources

- `packages/control-plane-ui/src/hooks/useApi.ts` — main hook implementation.
- `packages/control-plane-ui/src/hooks/useMutation.ts` — mutation sibling (function name: `useMutation`).
- `packages/control-plane-ui/src/main.tsx` — inline `QueryClient` instantiation + `QueryClientProvider` mount (lines 24-32).
- `packages/control-plane-ui/src/auth/context/AuthContext.tsx` — bearer token source consumed by fetchers.
- `scripts/check-useapi-querykey.mjs` — heuristic regex scanner.
- `.cursor/rules/useapi-querykey.mdc` — IDE rule.

## 10. Cross-references

- `sidebar-ia-architecture.md` — sister UI binding (route + IAM).
- `idp-sso-architecture.md` — where the bearer comes from.
- `iam-identity-architecture.md` — `allowedActions` gates which pages even mount.
