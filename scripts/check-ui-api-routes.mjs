#!/usr/bin/env node
/**
 * UI-to-route reachability check.
 *
 * Every path the Control Plane UI's service layer calls must be a route the Go
 * registrars actually register. Nothing enforced that, and the drift is silent
 * in both directions: TypeScript type-checks a call to a URL that 404s, and the
 * UI test suite mocks `api.get` at the module boundary, so a dead client is
 * indistinguishable from a live one until a user clicks the button.
 *
 * What this caught on its first run: ten service methods calling endpoints that
 * no registrar registers — a stale SSO family left behind when identity
 * providers moved to /api/admin/identity-providers, a rollup-jobs pair, an
 * agent-events service, and a passthrough effective-config read — plus two that
 * were LIVE and user-visible (the device and device-group "rotate certs"
 * affordances, whose backend never existed on any of the three services).
 *
 * Matching is by (prefix-group-stripped) path suffix, not by full URL:
 *
 *   UI    '/api/admin/agent-devices/${id}/events'  ->  '/agent-devices/:/events'
 *   Go    g.GET("/agent-devices/:id/events", ...)  ->  '/agent-devices/:/events'
 *
 * Consequence, stated plainly: a path under /api/admin that matches a route
 * registered on the /api/my group is accepted. Recovering the mount prefix per
 * registrar means tracing the call graph from wiring/routes.go through every
 * RegisterXRoutes closure, which is more machinery than the defect class needs
 * — and the failure direction is a MISSED defect, never a false alarm. The two
 * groups the UI calls share no suffixes today.
 *
 * Parsing is fail-open per call site (a path the regex cannot recover is not
 * checked) but NOT fail-open in aggregate: a floor on the recovered route count
 * and on the match rate turns a parse break into a failure rather than into a
 * silent all-clear. "Nothing found" and "the scan is broken" look identical
 * without those.
 */

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';

const ROOT = process.cwd();
const GO_DIRS = [
  'packages/control-plane/internal',
  'packages/control-plane/cmd/control-plane/wiring',
];
const UI_DIR = 'packages/control-plane-ui/src';

/** Prefixes the UI service layer targets, longest first. */
const UI_PREFIXES = ['/api/admin', '/api/my'];

/** Minimum recovered Go routes / UI call sites before a result is believable. */
const MIN_GO_ROUTES = 250;
const MIN_UI_CALLS = 200;
const MIN_MATCH_RATE = 0.9;

function walk(dir, out) {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) walk(full, out);
    else out.push(full);
  }
  return out;
}

/**
 * Canonical form: every path parameter collapses to ':' so ':id' and '${id}'
 * compare equal, and a trailing slash never decides a match.
 */
function canonical(path) {
  const segs = path.split('/').map((s) => {
    if (s.startsWith(':') || s === '*') return ':';
    return s;
  });
  let out = segs.join('/');
  if (out.length > 1 && out.endsWith('/')) out = out.slice(0, -1);
  return out;
}

/**
 * A UI path literal as written in a template string. Whole-segment `${...}`
 * is a path parameter; a `${...}` glued to text (a `${qs(...)}` query builder)
 * ends the path.
 */
function canonicalUI(raw) {
  let p = raw;
  // Whole-segment interpolation -> parameter.
  p = p.replace(/(?<=^|\/)\$\{[^}]*\}(?=\/|$)/g, ':');
  // Anything still interpolated is a query/suffix builder: the path ends there.
  const glued = p.indexOf('${');
  if (glued >= 0) p = p.slice(0, glued);
  const q = p.indexOf('?');
  if (q >= 0) p = p.slice(0, q);
  return canonical(p);
}

/** Every route registered in the Go tree, canonicalised, suffix form. */
function goRoutes() {
  const files = [];
  for (const d of GO_DIRS) walk(join(ROOT, d), files);

  const routes = new Set();
  for (const full of files) {
    if (!full.endsWith('.go') || full.endsWith('_test.go')) continue;
    const src = readFileSync(full, 'utf-8');

    // Sub-groups: `x := g.Group("/prefix", ...)` then `x.GET("/rest", ...)`.
    const groupPrefix = new Map();
    for (const m of src.matchAll(/(\w+)\s*:?=\s*\w+\.Group\(\s*"([^"]*)"/g)) {
      groupPrefix.set(m[1], m[2]);
    }

    for (const m of src.matchAll(/(\w+)\.(GET|POST|PUT|PATCH|DELETE)\(\s*"([^"]*)"/g)) {
      const [, recv, , path] = m;
      const prefix = groupPrefix.get(recv) ?? '';
      routes.add(canonical(prefix + path));
    }
  }
  return routes;
}

/** Every literal path the UI service layer calls, with its source location. */
function uiCalls() {
  const files = walk(join(ROOT, UI_DIR), []).filter(
    (f) => (f.endsWith('.ts') || f.endsWith('.tsx')) && !f.endsWith('.test.ts') && !f.endsWith('.test.tsx'),
  );

  const calls = [];
  for (const full of files) {
    const src = readFileSync(full, 'utf-8');
    const re = /api\.(get|post|put|patch|delete)(?:<[\s\S]*?>)?\(\s*[`'"](\/[^`'"]*)/g;
    for (const m of src.matchAll(re)) {
      const verb = m[1].toUpperCase();
      const raw = m[2];
      const prefix = UI_PREFIXES.find((p) => raw === p || raw.startsWith(p + '/'));
      if (!prefix) continue; // not a CP admin/my call — out of scope
      const line = src.slice(0, m.index).split('\n').length;
      calls.push({
        verb,
        raw,
        suffix: canonicalUI(raw.slice(prefix.length)),
        where: `${full.slice(ROOT.length + 1)}:${line}`,
      });
    }
  }
  return calls;
}

function main() {
  const routes = goRoutes();
  const calls = uiCalls();

  if (routes.size < MIN_GO_ROUTES) {
    console.error(
      `[check:ui-api-routes] FAIL — recovered only ${routes.size} Go routes ` +
        `(floor ${MIN_GO_ROUTES}). The route scan is broken, not the UI. ` +
        `Every result below this floor is meaningless.`,
    );
    process.exit(1);
  }
  if (calls.length < MIN_UI_CALLS) {
    console.error(
      `[check:ui-api-routes] FAIL — recovered only ${calls.length} UI call sites ` +
        `(floor ${MIN_UI_CALLS}). The UI scan is broken.`,
    );
    process.exit(1);
  }

  const unmatched = calls.filter((c) => !routes.has(c.suffix));
  const rate = 1 - unmatched.length / calls.length;
  if (rate < MIN_MATCH_RATE) {
    console.error(
      `[check:ui-api-routes] FAIL — only ${(rate * 100).toFixed(1)}% of ${calls.length} ` +
        `UI calls matched a route (floor ${MIN_MATCH_RATE * 100}%). That is a matching bug, ` +
        `not ${unmatched.length} dead endpoints. Fix the canonicaliser before reading the list.`,
    );
    for (const c of unmatched.slice(0, 15)) {
      console.error(`    ${c.verb} ${c.raw}  ->  '${c.suffix}'  (${c.where})`);
    }
    process.exit(1);
  }

  if (unmatched.length > 0) {
    console.error(
      `[check:ui-api-routes] FAIL — ${unmatched.length} UI call(s) target a path no Go ` +
        `registrar registers. Either the endpoint was never built (delete the client and ` +
        `every affordance that reaches it) or it moved (repoint the client):`,
    );
    for (const c of unmatched) {
      console.error(`    ${c.verb} ${c.raw}\n        ${c.where}`);
    }
    process.exit(1);
  }

  console.log(
    `[check:ui-api-routes] OK — all ${calls.length} UI service-layer calls resolve to one of ` +
      `${routes.size} registered routes.`,
  );
}

main();
