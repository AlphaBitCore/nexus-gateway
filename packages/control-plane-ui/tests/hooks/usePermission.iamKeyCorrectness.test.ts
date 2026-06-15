/**
 * IAM key correctness regression — explicit, named fixtures (F-0286).
 *
 * Context: F-0159 shipped undetected because quota-override pages used
 * `usePermission('quotaPolicy:...')` keys (→ admin:quota-POLICY.*) instead of
 * `usePermission('quota:...')` keys (→ admin:quota-OVERRIDE.*). The existing
 * ACTION_MAP coverage test (usePermission.coverage.test.ts) and the broad
 * sweep in iamPageResourceConsistency.test.ts both catch this TODAY — but only
 * after the fix. These tests are the explicit, named regression fixtures that
 * would have caught F-0159 at PR time and make the discrimination unmistakable
 * to the next engineer maintaining ACTION_MAP or quota pages.
 *
 * What this file tests that the other tests do NOT:
 *   1. Named fixture: exact expected action string per key, by resource.
 *      The sweep test asserts "resource matches route guard"; this asserts
 *      the precise target action value, so a rename/remap is caught even if
 *      a new route guard is also changed to match.
 *   2. Explicit anti-regression: asserts that quota:* and quotaPolicy:*
 *      resolve to DIFFERENT resources, with the F-0159 scenario spelled out.
 *   3. Route-level fixture: asserts that infrastructure/overrides allowedActions
 *      contains admin:settings.read, matching thing_overrides.go list endpoint.
 *
 * Non-goals: does not re-test the behavioral grant/deny logic (that is in
 * usePermission.test.ts) or the call-site sweep (usePermission.coverage.test.ts).
 */
import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { ACTION_MAP } from '../../src/hooks/usePermission';

// ---------------------------------------------------------------------------
// 1. Exact action mapping for quota-override keys
//    These are the keys the quota-override pages MUST use. If someone
//    accidentally swaps these for quotaPolicy:* keys (the F-0159 bug), this
//    test fails immediately with a clear message.
// ---------------------------------------------------------------------------
describe('ACTION_MAP — quota-override resource correctness (F-0159 regression)', () => {
  it('quota:create maps to admin:quota-override.create (not quota-policy)', () => {
    expect(ACTION_MAP['quota:create']).toBe('admin:quota-override.create');
  });

  it('quota:update maps to admin:quota-override.update (not quota-policy)', () => {
    expect(ACTION_MAP['quota:update']).toBe('admin:quota-override.update');
  });

  it('quota:delete maps to admin:quota-override.delete (not quota-policy)', () => {
    expect(ACTION_MAP['quota:delete']).toBe('admin:quota-override.delete');
  });

  it('quota:reset maps to admin:quota-override.update (reset is an update)', () => {
    // Reset is modeled as an update action because it mutates existing override
    // state; it does not create or delete.
    expect(ACTION_MAP['quota:reset']).toBe('admin:quota-override.update');
  });

  // The "wrong key" that F-0159 used — assert it resolves to the POLICY
  // resource, NOT override. This makes the discrimination explicit: if you
  // use quotaPolicy:create on an override page, the backend will 403 any
  // principal whose policy only covers quota-override.create.
  it('quotaPolicy:create maps to admin:quota-POLICY.create (distinct from quota-override)', () => {
    expect(ACTION_MAP['quotaPolicy:create']).toBe('admin:quota-policy.create');
  });

  it('quotaPolicy:update maps to admin:quota-POLICY.update (distinct from quota-override)', () => {
    expect(ACTION_MAP['quotaPolicy:update']).toBe('admin:quota-policy.update');
  });

  it('quotaPolicy:delete maps to admin:quota-POLICY.delete (distinct from quota-override)', () => {
    expect(ACTION_MAP['quotaPolicy:delete']).toBe('admin:quota-policy.delete');
  });

  // Cross-discrimination: the two resources must be different strings so no
  // IAM policy can accidentally grant both via an exact-match on the wrong one.
  it('quota:create and quotaPolicy:create resolve to different actions', () => {
    expect(ACTION_MAP['quota:create']).not.toBe(ACTION_MAP['quotaPolicy:create']);
  });

  it('quota:update and quotaPolicy:update resolve to different actions', () => {
    expect(ACTION_MAP['quota:update']).not.toBe(ACTION_MAP['quotaPolicy:update']);
  });

  it('quota:delete and quotaPolicy:delete resolve to different actions', () => {
    expect(ACTION_MAP['quota:delete']).not.toBe(ACTION_MAP['quotaPolicy:delete']);
  });
});

// ---------------------------------------------------------------------------
// 2. Quota-override pages use quota:* keys, NOT quotaPolicy:* keys.
//    Scans the actual page source files and asserts:
//      - At least one quota:* call is present (pages are not accidentally empty).
//      - No quotaPolicy:* call is present (F-0159 regression).
// ---------------------------------------------------------------------------
const SRC = join(__dirname, '..', '..', 'src');

function permKeysInFile(filePath: string): string[] {
  const text = readFileSync(filePath, 'utf8');
  const re = /usePermission\(\s*'([^']+)'\s*\)/g;
  const keys: string[] = [];
  let m: RegExpExecArray | null;
  while ((m = re.exec(text)) !== null) keys.push(m[1]);
  return keys;
}

// Page files that form the quota-override feature (list + detail; create and
// edit inherit mutations from the list/detail actions via the route guard).
const QUOTA_OVERRIDE_PAGE_FILES = [
  join(SRC, 'pages/ai-gateway/quota-overrides/QuotaOverrideList.tsx'),
  join(SRC, 'pages/ai-gateway/quota-overrides/QuotaOverrideDetail.tsx'),
  join(SRC, 'pages/ai-gateway/quota-overrides/QuotaOverrideEdit.tsx'),
  join(SRC, 'pages/ai-gateway/quota-overrides/QuotaOverrideCreate.tsx'),
];

describe('quota-override page files — key source scan (F-0159 regression)', () => {
  // Collect all keys actually used across override pages.
  const overridePageKeys: Array<{ file: string; key: string }> = [];
  for (const file of QUOTA_OVERRIDE_PAGE_FILES) {
    try {
      const keys = permKeysInFile(file);
      for (const key of keys) overridePageKeys.push({ file, key });
    } catch {
      // File may not exist in this worktree snapshot; test is fail-open for
      // missing files but will fail if the page exists but uses wrong keys.
    }
  }

  it('quota-override pages use at least one quota:* permission key', () => {
    // Sanity guard: if no page file uses any permission keys at all, the scan
    // is broken or the feature was removed — flag it explicitly.
    const quotaKeys = overridePageKeys.filter((e) => e.key.startsWith('quota:'));
    expect(quotaKeys.length).toBeGreaterThan(0);
  });

  it('quota-override pages do NOT use quotaPolicy:* keys (F-0159 was this bug)', () => {
    // F-0159: quota-override pages had usePermission('quotaPolicy:create') etc.,
    // which routes to admin:quota-policy.create — the WRONG resource. Any
    // principal whose IAM only covers quota-override would get silent 403s on
    // every mutation affordance (Create/Edit/Delete buttons hidden).
    const wrongKeys = overridePageKeys.filter((e) => e.key.startsWith('quotaPolicy:'));
    if (wrongKeys.length > 0) {
      const detail = wrongKeys.map((e) => `  ${e.file}: usePermission('${e.key}')`).join('\n');
      throw new Error(
        `quota-override pages are using quotaPolicy:* keys (the F-0159 bug). ` +
          `Switch to quota:* keys so the correct admin:quota-override.* ` +
          `actions are checked.\n${detail}`,
      );
    }
    expect(wrongKeys).toHaveLength(0);
  });
});

// ---------------------------------------------------------------------------
// 3. Route-level fixture: infrastructure/overrides uses admin:settings.read.
//    Parses shellRouteConfig.tsx directly so any future rekey of that route
//    is caught immediately, with a message explaining WHY the key matters
//    (thing_overrides.go GET handler is gated on admin:settings.read).
// ---------------------------------------------------------------------------
describe('shellRouteConfig — infrastructure/overrides route guard (F-0286)', () => {
  const routeConfigText = readFileSync(
    join(SRC, 'routes/shellRouteConfig.tsx'),
    'utf8',
  );

  it("infrastructure/overrides route is guarded on 'admin:settings.read'", () => {
    // The backend GET /api/admin/overrides handler (thing_overrides.go) requires
    // admin:settings.read. The route guard must match exactly — any other action
    // would make the nav item invisible to settings.read principals even though
    // the backend accepts their token.
    //
    // Parse: find the infrastructure/overrides route entry and assert it has
    // allowedActions: ['admin:settings.read'].
    const overridesRouteRe =
      /path:\s*'infrastructure\/overrides'[^}]*allowedActions:\s*\[([^\]]+)\]/;
    const m = overridesRouteRe.exec(routeConfigText);
    expect(m, "Could not find path: 'infrastructure/overrides' with allowedActions in shellRouteConfig.tsx").not.toBeNull();

    const actionsBlock = m![1];
    // Extract the action strings from the allowedActions array literal.
    const actions = [...actionsBlock.matchAll(/'([^']+)'/g)].map((a) => a[1]);
    expect(actions).toContain('admin:settings.read');
  });
});
