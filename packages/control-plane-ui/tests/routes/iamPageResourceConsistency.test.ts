/**
 * IAM page-resource consistency guard (F-0286).
 *
 * The highest-value silent-403 class is "valid-but-wrong-key": a page gates an
 * affordance on a `usePermission(...)` key that resolves to a DIFFERENT IAM
 * resource than the one the page's route is guarded on. The key is in
 * ACTION_MAP (so the ACTION_MAP coverage test passes) and the action string is
 * canonical (so the shape test passes) — yet a principal with the page's
 * resource but not the affordance's resource hits a silent 403, and the
 * converse gets an invisible button. F-0159 (quota-OVERRIDE pages gating on
 * quota-POLICY) and C9 F-0207 (node-detail tabs needing settings/observability/
 * traffic-log under a node.read route) both shipped precisely because no test
 * asserted per-page resource consistency.
 *
 * This test cross-checks, per route, the IAM resources used by in-page
 * `usePermission(...)` calls against the resource(s) the route is guarded on.
 * Legitimate cross-resource affordances (a write button whose backend action
 * differs from the page's read action) are declared in CROSS_RESOURCE_ALLOWLIST
 * with a one-line rationale — so every cross-resource gate becomes a reviewed
 * decision rather than silent drift.
 *
 * Parsing is fail-open: a route entry the regex can't recover is simply not
 * checked (weaker coverage, never a false failure). A floor assertion guards
 * against a total parse break.
 */
import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, dirname, relative } from 'node:path';
import { ACTION_MAP } from '../../src/hooks/usePermission';

const SRC = join(__dirname, '..', '..', 'src');

/** `admin:<resource>.<verb>` -> `<resource>`; null for wildcards / non-canonical. */
function resourceOf(action: string): string | null {
  const m = /^admin:([a-z][a-z0-9-]*)\.[a-z*][a-z-]*$/.exec(action);
  return m ? m[1] : null;
}

/** Map every exported Lazy* page component name to its source file (rel to src). */
function lazyNameToFile(): Record<string, string> {
  const txt = readFileSync(join(SRC, 'routes', 'lazyPages.tsx'), 'utf8');
  const re = /export const (Lazy\w+)\s*=\s*L\(\(\)\s*=>\s*import\(\s*'([^']+)'/g;
  const out: Record<string, string> = {};
  let m: RegExpExecArray | null;
  while ((m = re.exec(txt)) !== null) {
    // import paths are relative to src/routes/, e.g. '../pages/x/Y'
    out[m[1]] = join('routes', m[2]).replace(/\\/g, '/');
  }
  return out;
}

interface RouteEntry {
  path: string;
  lazy: string;
  actions: string[];
}

/**
 * Recover (path, LazyPage, page-level allowedActions) per route from the route
 * config text. The page-level `allowedActions` is the FIRST one after the
 * `LazyPage:` token (the nav's allowedActions, when present, comes later in the
 * same object literal).
 */
function routeEntries(): RouteEntry[] {
  const txt = readFileSync(join(SRC, 'routes', 'shellRouteConfig.tsx'), 'utf8');
  const re =
    /path:\s*'([^']+)'\s*,\s*LazyPage:\s*L\.(Lazy\w+)\s*,\s*allowedActions:\s*\[([^\]]*)\]/g;
  const out: RouteEntry[] = [];
  let m: RegExpExecArray | null;
  while ((m = re.exec(txt)) !== null) {
    const actions = [...m[3].matchAll(/'([^']+)'/g)].map((a) => a[1]);
    out.push({ path: m[1], lazy: m[2], actions });
  }
  return out;
}

/** Resolve a page file (rel-to-src, no extension) to its actual .tsx/.ts path. */
function resolveFile(relNoExt: string): string | null {
  for (const ext of ['.tsx', '.ts', '/index.tsx', '/index.ts']) {
    const full = join(SRC, relNoExt + ext);
    try {
      if (statSync(full).isFile()) return full;
    } catch {
      /* try next */
    }
  }
  return null;
}

/** Immediate-directory .ts/.tsx files (non-recursive, excludes tests). */
function filesInDir(dir: string): string[] {
  const out: string[] = [];
  for (const name of readdirSync(dir)) {
    const full = join(dir, name);
    if (statSync(full).isFile() && /\.(ts|tsx)$/.test(name) && !/\.test\.(ts|tsx)$/.test(name)) {
      out.push(full);
    }
  }
  return out;
}

/** Literal usePermission('key') keys found in a file. */
function permKeysInFile(file: string): string[] {
  const text = readFileSync(file, 'utf8');
  const re = /usePermission\(\s*'([^']+)'\s*\)/g;
  const keys: string[] = [];
  let m: RegExpExecArray | null;
  while ((m = re.exec(text)) !== null) keys.push(m[1]);
  return keys;
}

/**
 * Routes whose page legitimately gates an affordance on a resource other than
 * the page's own read action. Each entry: route path -> { resource: why }.
 * Adding a row is a conscious "yes, this page has a cross-resource button"
 * decision — the alternative is fixing the key (the F-0159 case).
 */
const CROSS_RESOURCE_ALLOWLIST: Record<string, Record<string, string>> = {
  'ai-gateway/providers/:id': {
    // The provider detail page hosts "add model" and "add credential"
    // affordances for the provider, each gated on its own backend resource.
    model: 'Add-model affordance (model.create)',
    credential: 'Add-credential affordance (credential.create)',
  },
  'infrastructure/overrides': {
    // Registry is reachable with settings.read (the list endpoint), but the
    // per-row Clear / Force-resync write actions carry their own node-scoped
    // backend actions (F-0065).
    node: 'Clear (node.write-override) + Force resync (node.force-resync) per-row actions',
  },
  'infrastructure/nodes/:id': {
    // Page loads via GET /nodes/:id (node.read); each tab fetches from an
    // endpoint with its own action and is hidden when the principal lacks it
    // (F-0207).
    settings: 'Configuration + Runtime tabs (applied-config / runtime)',
    observability: 'Live Metrics + Diagnostics + Usage Trends tabs',
    'traffic-log': 'Traffic tab (GET /traffic)',
  },
  'iam/organizations/:id': {
    // The organization detail page has an inline "add user" affordance that
    // gates on the user resource, distinct from the organization read guard.
    user: 'Add-user affordance (user.create)',
  },
  'fleet/devices/:id': {
    // Device detail page surfaces a metrics tab and a per-device diagnostic-mode
    // toggle, each gated on its own backend resource.
    observability: 'Device metrics tab (observability.read)',
    'diagnostic-mode': 'Per-device diagnostic-mode toggle (diagnostic-mode.update)',
  },
  'devices/:id': {
    // Same FleetDeviceDetailPage component as fleet/devices/:id.
    observability: 'Device metrics tab (observability.read)',
    'diagnostic-mode': 'Per-device diagnostic-mode toggle (diagnostic-mode.update)',
  },
};

describe('IAM page-resource consistency (F-0286)', () => {
  const lazyFiles = lazyNameToFile();
  const routes = routeEntries();

  it('parsed a representative set of guarded routes (parse-break guard)', () => {
    expect(routes.length).toBeGreaterThan(30);
  });

  // Resolved entry file for every route, so a route's scan can exclude the
  // entry files of OTHER routes that happen to share its directory (list vs
  // detail pages) — those belong to their own route's check.
  const entryFiles = new Set(
    routes
      .map((r) => (lazyFiles[r.lazy] ? resolveFile(lazyFiles[r.lazy]) : null))
      .filter((f): f is string => f !== null),
  );

  it('every in-page usePermission resource matches the route resource or is allow-listed', () => {
    const violations: string[] = [];

    for (const route of routes) {
      if (route.actions.length === 0) continue;
      const file = lazyFiles[route.lazy] ? resolveFile(lazyFiles[route.lazy]) : null;
      if (!file) continue; // unresolvable page module — fail-open

      const routeResources = new Set(
        route.actions.map(resourceOf).filter((r): r is string => r !== null),
      );
      const allowed = CROSS_RESOURCE_ALLOWLIST[route.path] ?? {};

      // Scan the route's entry file plus the non-entry helper files co-located in
      // its immediate directory (a page's row/action components). Other routes'
      // entry files in the same directory are excluded — they are checked under
      // their own route, with their own guard.
      const dir = dirname(file);
      const scanFiles = filesInDir(dir).filter((f) => f === file || !entryFiles.has(f));
      for (const f of scanFiles) {
        for (const key of permKeysInFile(f)) {
          const action = ACTION_MAP[key];
          if (!action) continue; // unmapped key is the coverage test's job
          const res = resourceOf(action);
          if (res === null) continue; // wildcard action — not resource-scoped
          if (routeResources.has(res)) continue;
          if (res in allowed) continue;
          violations.push(
            `  route '${route.path}' (guarded on {${[...routeResources].join(', ')}}) ` +
              `uses usePermission('${key}') -> ${action} [resource '${res}'] in ` +
              `${relative(SRC, f)}`,
          );
        }
      }
    }

    if (violations.length > 0) {
      throw new Error(
        `Found ${violations.length} page affordance(s) gated on an IAM resource ` +
          `that does not match the route's guard and is not in ` +
          `CROSS_RESOURCE_ALLOWLIST. Either switch the usePermission key to the ` +
          `page's resource (the F-0159 silent-403 bug), or, if the cross-resource ` +
          `affordance is intentional, add it to the allowlist with a rationale.\n` +
          violations.join('\n'),
      );
    }
  });
});
