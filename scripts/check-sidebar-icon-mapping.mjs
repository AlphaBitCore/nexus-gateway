#!/usr/bin/env node
/**
 * Lint: dead Sidebar icon-mapping arms.
 *
 * Sidebar.tsx has a switch on the route path returning the icon. When a
 * route is renamed in shellRouteConfig.tsx, the matching case arm in
 * Sidebar.tsx is easy to forget — it lingers as a dead arm. This script
 * extracts both sets and reports orphans.
 *
 * Heuristic:
 *   - Sidebar.tsx contains arms like `case '/traffic':` / `case '/config/providers':`.
 *   - shellRouteConfig.tsx contains nav entries with `to: '/traffic'` AND/OR
 *     route `path: 'traffic'` (no leading slash; resolved against the shell route base).
 *   - Both forms normalised to leading-slash form for comparison.
 *
 * A Sidebar case '/X' is "dead" if no nav entry or route in
 * shellRouteConfig.tsx resolves to '/X'.
 */

import { readFileSync, existsSync } from 'node:fs';

const SHELL_ROUTE_CONFIG = 'packages/control-plane-ui/src/routes/shellRouteConfig.tsx';
const SIDEBAR = 'packages/control-plane-ui/src/components/ui/Sidebar/Sidebar.tsx';
const STRICT = process.argv.includes('--strict') || process.env.STRICT === '1';

function normaliseSlash(p) {
  if (!p.startsWith('/')) return '/' + p;
  return p;
}

function main() {
  if (!existsSync(SHELL_ROUTE_CONFIG) || !existsSync(SIDEBAR)) {
    console.log(`[check:sidebar-icon-mapping] Required files missing; skipping.`);
    return;
  }

  const configText = readFileSync(SHELL_ROUTE_CONFIG, 'utf-8');
  const sidebarText = readFileSync(SIDEBAR, 'utf-8');

  // Extract all known paths from shellRouteConfig.tsx.
  // - to: 'path'  (nav entries)
  // - path: 'path' (route definitions)
  const paths = new Set();

  let m;
  const toRE = /to:\s*['"]([^'"]+)['"]/g;
  while ((m = toRE.exec(configText)) !== null) {
    paths.add(normaliseSlash(m[1]));
  }
  const pathRE = /\bpath:\s*['"]([^'"]+)['"]/g;
  while ((m = pathRE.exec(configText)) !== null) {
    paths.add(normaliseSlash(m[1]));
  }

  // Some Sidebar arms use parent paths (e.g. '/' for overview). Allow
  // bare slash + any extracted top-level segment.
  paths.add('/');

  // Extract case arms from Sidebar.tsx — strings beginning with `/`.
  const caseRE = /case\s+['"](\/[^'"]*)['"]\s*:/g;
  const sidebarCases = new Map();    // case -> line number
  let cm;
  while ((cm = caseRE.exec(sidebarText)) !== null) {
    const line = sidebarText.slice(0, cm.index).split('\n').length;
    if (!sidebarCases.has(cm[1])) sidebarCases.set(cm[1], line);
  }

  const orphans = [];
  for (const [casePath, line] of sidebarCases) {
    // Accept exact match; or match where the case is a known prefix /
    // alias (rare; manual review on first violation).
    if (!paths.has(casePath)) {
      orphans.push({ key: casePath, line });
    }
  }

  if (orphans.length > 0) {
    const label = STRICT ? 'FAILED' : 'WARN';
    const ws = STRICT ? console.error : console.warn;
    ws(`[check:sidebar-icon-mapping] ${label} -- ${orphans.length} potentially dead case arm(s) in Sidebar.tsx:`);
    for (const o of orphans) {
      ws(`  - Sidebar.tsx:${o.line}  case '${o.key}'  -- no matching path in shellRouteConfig.tsx`);
    }
    ws('');
    ws('Sweep dead case arms when renaming routes (binding: sidebar-ia-architecture.md §4).');
    ws('Note: some Sidebar arms may legitimately be aliases / parent paths; verify before deleting.');
    if (STRICT) process.exit(1);
    ws('[check:sidebar-icon-mapping] non-strict mode; passing despite warnings. Run with --strict to fail.');
    return;
  }

  console.log(`[check:sidebar-icon-mapping] OK -- all ${sidebarCases.size} case arm(s) have matching paths.`);
}

main();
