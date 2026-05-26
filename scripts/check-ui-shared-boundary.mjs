#!/usr/bin/env node
/**
 * Lint: enforce packages/ui-shared/ boundary.
 *
 * ui-shared MUST be a leaf in the dependency graph from each consumer's
 * perspective. It can import externals (react, react-i18next, recharts, etc.)
 * but MUST NOT import from packages/control-plane-ui/, packages/agent/,
 * or any other consumer bundle.
 *
 * Heuristic regex scan over `packages/ui-shared/src/**\/*.{ts,tsx,js,jsx}`.
 */

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';

const ROOT = 'packages/ui-shared/src';
const EXT = new Set(['.ts', '.tsx', '.js', '.jsx', '.mts', '.cts']);
const STRICT = process.argv.includes('--strict') || process.env.STRICT === '1';

// Forbidden import path prefixes (heuristic).
const FORBIDDEN_PREFIXES = [
  '../control-plane-ui',
  '../../control-plane-ui',
  '../agent',
  '../../agent',
  'packages/control-plane-ui',
  'packages/agent',
  '@control-plane-ui/',  // hypothetical alias
  '@agent-ui/',
  '@cp-ui/',
];

const IMPORT_RE = /^\s*(?:import|export)\b[\s\S]*?from\s+["']([^"']+)["']/m;
const IMPORT_RE_GLOBAL = /(?:import|export)\b[\s\S]*?from\s+["']([^"']+)["']/g;

function walk(dir) {
  const out = [];
  for (const entry of readdirSync(dir)) {
    const p = join(dir, entry);
    let st;
    try { st = statSync(p); } catch { continue; }
    if (st.isDirectory()) out.push(...walk(p));
    else if (EXT.has(entry.slice(entry.lastIndexOf('.')))) out.push(p);
  }
  return out;
}

function scanFile(file) {
  const text = readFileSync(file, 'utf-8');
  const violations = [];

  let m;
  IMPORT_RE_GLOBAL.lastIndex = 0;
  while ((m = IMPORT_RE_GLOBAL.exec(text)) !== null) {
    const spec = m[1];
    for (const bad of FORBIDDEN_PREFIXES) {
      if (spec.startsWith(bad)) {
        const line = text.slice(0, m.index).split('\n').length;
        violations.push({ file, line, spec });
        break;
      }
    }
  }
  return violations;
}

function main() {
  let allViolations = [];
  try {
    const files = walk(ROOT);
    for (const f of files) {
      allViolations.push(...scanFile(f));
    }
  } catch (e) {
    if (e.code === 'ENOENT') {
      console.log(`[check:ui-shared-boundary] No ${ROOT} directory; skipping.`);
      return;
    }
    throw e;
  }

  if (allViolations.length > 0) {
    const label = STRICT ? 'FAILED' : 'WARN';
    const ws = STRICT ? console.error : console.warn;
    ws(`[check:ui-shared-boundary] ${label} -- ${allViolations.length} forbidden import(s):`);
    for (const v of allViolations) {
      ws(`  - ${relative('.', v.file)}:${v.line}  -- imports "${v.spec}"`);
    }
    ws('');
    ws('packages/ui-shared/ MUST be a leaf — no imports from consumer bundles.');
    ws('See .cursor/rules/ui-shared-boundary.mdc');
    if (STRICT) process.exit(1);
    ws('[check:ui-shared-boundary] non-strict mode; passing despite warnings. Run with --strict to fail.');
    return;
  }

  console.log('[check:ui-shared-boundary] OK -- no forbidden imports detected.');
}

main();
