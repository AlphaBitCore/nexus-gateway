# iam-impact-review

Walk the 5-step IAM impact audit when an admin endpoint, sidebar item, or route is added / moved / renamed.

Use this skill whenever you (or a contributor) are touching any of:

- `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`
- Any `packages/control-plane/internal/**/handler/**` route registration (handlers were distributed across domain subtrees in the P9 reorg — `identity/`, `traffic/`, `settings/`, `governance/`, `observability/`, `ai/`, etc. — plus the top-level `internal/handler/admin_routes.go`).
- `packages/shared/identity/iam/catalog_data.go`
- `packages/control-plane/internal/identity/iam/managed.go`
- `tools/db-migrate/seed/seed.ts` (policy block)
- `packages/control-plane-ui/src/components/ui/Sidebar/Sidebar.tsx`

The full doc is `docs/developers/architecture/services/control-plane/iam-identity-architecture.md`. This skill is the **audit checklist**.

---

## Step 1 — UI ↔ backend symmetry

For every route touched:

1. Find the `allowedActions` value in `shellRouteConfig.tsx`.
2. Find the matching handler somewhere in `packages/control-plane/internal/**/handler/**` (the relevant domain subtree).
3. Find the `iamMW(action)` wrapper on that handler.
4. **Confirm the action strings match exactly.**

```bash
# Example: list routing-rule routes
grep -nE "allowedActions.*'admin:routing-rule" packages/control-plane-ui/src/routes/shellRouteConfig.tsx

# Find the handler (search the whole internal tree — handlers are domain-distributed)
grep -rn "iamMW.*routing-rule" packages/control-plane/internal/
```

A mismatch produces silent 403s (user sees menu item, click yields 403). The 2026-05-13 NRN-builder bug was exactly this.

## Step 2 — Resource carve-out decision

Decide: should the surface have its own resource type in `packages/shared/identity/iam/catalog_data.go`?

- **Yes** if granting this resource shouldn't imply granting unrelated settings (e.g., `prompt-cache` got its own; carved out from `settings`).
- **No** if it's a small surface that naturally bundles with an existing resource (e.g., reusing `settings`, `observability`).

Document the decision in the PR description: "_Kept on `admin:settings.read`_" or "_Carved out as `prompt-cache`_".

## Step 3 — Update fixtures + seed (if new resource)

If you carved out a new resource:

1. **`tools/db-migrate/seed/seed.ts`** — add the new action to canonical managed policies (`NexusSuperAdmin`, `NexusAdmin`, `NexusViewer`, etc.).
2. **`packages/control-plane/internal/iam/managed.go`** — add to the `NexusViewer` fixture so unit tests catch viewer-side regressions.

Missing either side means **non-super-admin users silently lose visibility**.

## Step 4 — Sidebar + breadcrumb sweep

If the route was renamed / moved:

```bash
grep -rn "<old path>" packages/control-plane-ui/src/components/ui/Sidebar/
grep -rn "<old path>" packages/control-plane-ui/src/components/ui/Breadcrumb/ 2>/dev/null
```

Remove any dead `case` arms that referenced the old path. Icon mappings live in `Sidebar.tsx`; update them so the new path has the right icon.

## Step 5 — Verification

Run two tests:

```bash
# Positive: super-admin can reach the route
cp_login                                  # tests/lib/auth.sh
cp_curl /api/admin/<new-or-renamed-path>

# Negative: a role without the action gets 403
# Switch to a viewer-level user (e.g., diana@nexus.ai / viewer123)
# Confirm 403 on the same path.
```

If either fails, **stop and fix before merging**.

## Output

Emit a one-paragraph audit summary suitable for the PR description:

```
IAM impact review:
- Action: admin:<resource>.<verb>
- Resource decision: kept on `settings` / carved out as `<new>`
- Fixtures updated: seed.ts ✓ managed.go ✓
- Sidebar / breadcrumb: swept (or "no rename, n/a")
- Positive test: PASS
- Negative test: PASS
```

This is the binding IAM impact review rule in CLAUDE.md.
