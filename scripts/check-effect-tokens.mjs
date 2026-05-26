#!/usr/bin/env node
/**
 * Effect-token wiring guard.
 *
 * Every `--g-effect-*` token defined in
 * packages/ui-shared/src/styles/global.css MUST be consumed by at least
 * one `var(--g-effect-*)` reference somewhere in the codebase. A defined
 * token with no consumers is dead code that misleads theme authors into
 * thinking they can flip a visual knob that nothing reads.
 *
 * Also catches the inverse: a `var(--g-effect-*)` reference to a token
 * name that is NOT defined in global.css (typo or stale name).
 *
 * Why this is a sibling to check-design-tokens.mjs and not part of it:
 * design-tokens.mjs guards against hex/rgba literals in components.
 * Effect-tokens guards the *theme personality contract* — the small set
 * of intentionally-tunable knobs that themes use to differentiate visual
 * character without changing colours.
 *
 * Usage:
 *   node scripts/check-effect-tokens.mjs
 *   node scripts/check-effect-tokens.mjs --json
 *
 * Exit codes:
 *   0 — every defined effect token has ≥1 consumer; no stale references
 *   1 — at least one dead token or stale reference
 */

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, dirname, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = join(__dirname, '..');

const args = new Set(process.argv.slice(2));
const asJson = args.has('--json');

const GLOBAL_CSS = 'packages/ui-shared/src/styles/global.css';

const SCAN_ROOTS = [
  'packages/control-plane-ui/src',
  'packages/agent/ui/frontend/src',
  'packages/ui-shared/src',
];

const SOURCE_EXTS = new Set(['.tsx', '.ts', '.module.css', '.css']);
const SKIP_DIRS = new Set(['node_modules', 'dist', 'build', '.next']);

// Token definitions — everything declared in global.css with `--g-effect-*`.
function loadDefinedEffects() {
  const src = readFileSync(join(REPO_ROOT, GLOBAL_CSS), 'utf-8');
  const names = new Set();
  for (const match of src.matchAll(/^\s*--g-effect-([a-z][a-z0-9-]*)\s*:/gm)) {
    names.add(match[1]);
  }
  return names;
}

function walk(dir, out) {
  let entries;
  try {
    entries = readdirSync(dir);
  } catch {
    return;
  }
  for (const entry of entries) {
    const full = join(dir, entry);
    let stat;
    try {
      stat = statSync(full);
    } catch {
      continue;
    }
    if (stat.isDirectory()) {
      if (SKIP_DIRS.has(entry)) continue;
      walk(full, out);
    } else {
      const ext = entry.endsWith('.module.css') ? '.module.css'
                 : entry.slice(entry.lastIndexOf('.'));
      if (!SOURCE_EXTS.has(ext)) continue;
      // Skip the global.css definition file itself.
      if (full.endsWith(GLOBAL_CSS.split('/').pop())) {
        // Match by full suffix to avoid catching unrelated global.css files
        if (full.endsWith(GLOBAL_CSS)) continue;
      }
      out.push(full);
    }
  }
}

const defined = loadDefinedEffects();
const consumerCounts = new Map();
for (const name of defined) consumerCounts.set(name, 0);
const referencedButUndefined = new Set();

const files = [];
for (const root of SCAN_ROOTS) {
  walk(join(REPO_ROOT, root), files);
}

const USAGE_PATTERN = /var\(\s*--g-effect-([a-z][a-z0-9-]*)/g;

for (const abs of files) {
  // Skip the canonical definition file.
  if (abs.endsWith('packages/ui-shared/src/styles/global.css')) continue;
  let source;
  try {
    source = readFileSync(abs, 'utf-8');
  } catch {
    continue;
  }
  for (const match of source.matchAll(USAGE_PATTERN)) {
    const name = match[1];
    if (defined.has(name)) {
      consumerCounts.set(name, consumerCounts.get(name) + 1);
    } else {
      referencedButUndefined.add(name);
    }
  }
}

const deadTokens = [...consumerCounts.entries()]
  .filter(([, count]) => count === 0)
  .map(([name]) => name);

const violations = {
  dead: deadTokens,
  undefined: [...referencedButUndefined],
};

const violationCount = violations.dead.length + violations.undefined.length;

if (asJson) {
  console.log(JSON.stringify({
    definedCount: defined.size,
    filesScanned: files.length,
    consumerCounts: Object.fromEntries(consumerCounts),
    violations,
  }, null, 2));
} else {
  if (violationCount === 0) {
    console.log(`effect-token guard — ${defined.size} tokens defined, ${files.length} files scanned`);
    console.log('');
    console.log('✓ every defined --g-effect-* has ≥1 consumer; no undefined references');
  } else {
    console.log(`effect-token guard — ${defined.size} tokens defined, ${files.length} files scanned\n`);
    if (violations.dead.length > 0) {
      console.log(`  DEAD (${violations.dead.length}) — defined in global.css but consumed nowhere:`);
      for (const name of violations.dead) console.log(`    --g-effect-${name}`);
      console.log('');
    }
    if (violations.undefined.length > 0) {
      console.log(`  UNDEFINED (${violations.undefined.length}) — referenced via var() but not defined in global.css:`);
      for (const name of violations.undefined) console.log(`    --g-effect-${name}`);
      console.log('');
    }
    console.log('Fix: either consume the dead token in a *.module.css class, or remove the definition from global.css.');
    console.log('     For undefined references, either define the token or rename the reference to a defined one.');
  }
}

process.exit(violationCount > 0 ? 1 : 0);
