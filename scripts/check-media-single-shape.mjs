#!/usr/bin/env node
/**
 * Lint: one media representation, defined once.
 *
 * `MediaRef` and the card that renders it are the single shape every surface
 * uses to talk about an image, an audio clip, a video or a file attached to a
 * request. They live in packages/ui-shared/ and consumers re-export them.
 *
 * That is true today because it was made true, not because anything stops it
 * from coming apart. Before the convergence there were two BinaryRef families,
 * six divergent per-codec behaviours, and parallel types in the Control Plane
 * UI and the Agent UI — and none of those were accidents. Each was, at the
 * moment it was written, the fastest correct-looking way to add one case. A
 * seventh will look the same way. This check is the thing that says no.
 *
 * What it forbids, in every consumer bundle:
 *   - declaring MediaRef / MediaModality / MediaSource locally
 *   - defining a second MediaCard component
 *   - hardcoding the custody vocabulary (captured / external / provider-ref /
 *     fingerprint / aged-out / absent) instead of importing the constants
 *
 * What it allows: re-exporting the shared types, which is how a consumer
 * surfaces them to its own modules without owning them.
 *
 * Usage: node scripts/check-media-single-shape.mjs
 */

import { readdirSync, statSync, readFileSync } from 'node:fs';
import { join, dirname, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const REPO_ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');

// The one place the shape may be defined.
const OWNER = 'packages/ui-shared/src';

// Every bundle that consumes it.
const CONSUMERS = [
  'packages/control-plane-ui/src',
  'packages/agent/ui/frontend/src',
];

const EXT = new Set(['.ts', '.tsx']);

const RULES = [
  {
    // `interface MediaRef {` / `type MediaRef = {` — a local declaration.
    // A re-export (`export type { MediaRef } from '…'`) does not match,
    // because the type name is inside braces there, not after the keyword.
    re: /^\s*(?:export\s+)?(?:interface|type)\s+(MediaRef|MediaModality|MediaSource)\b/,
    msg: (m) => `declares ${m[1]} locally — re-export it from @nexus-gateway/ui-shared instead`,
  },
  {
    re: /^\s*(?:export\s+)?(?:function|const)\s+MediaCard\b/,
    msg: () => 'defines a second MediaCard — extend the shared one',
  },
  {
    // The custody vocabulary as a bare string literal. Reading a value off the
    // wire and comparing it is exactly how the six divergent behaviours got
    // their footholds; the constants exist so the set has one owner.
    re: /['"](provider-ref|aged-out)['"]/,
    msg: (m) => `hardcodes the custody value ${m[0]} — use the shared constant`,
  },
];

function walk(dir, out = []) {
  let entries;
  try {
    entries = readdirSync(dir);
  } catch {
    return out; // consumer bundle absent in this checkout
  }
  for (const name of entries) {
    if (name === 'node_modules' || name === 'dist') continue;
    const full = join(dir, name);
    if (statSync(full).isDirectory()) walk(full, out);
    else if (EXT.has(name.slice(name.lastIndexOf('.')))) out.push(full);
  }
  return out;
}

const violations = [];
for (const consumer of CONSUMERS) {
  for (const file of walk(join(REPO_ROOT, consumer))) {
    const rel = relative(REPO_ROOT, file);
    // Tests may construct fixtures that mention the vocabulary.
    if (/\.(test|spec)\.tsx?$/.test(rel) || rel.includes('/test/')) continue;
    const lines = readFileSync(file, 'utf8').split('\n');
    lines.forEach((line, i) => {
      for (const rule of RULES) {
        const m = line.match(rule.re);
        if (m) violations.push(`${rel}:${i + 1}: ${rule.msg(m)}`);
      }
    });
  }
}

// The owner must actually own it — a check that passes because the shared
// definition was deleted would be worse than no check.
const ownerFiles = walk(join(REPO_ROOT, OWNER));
const ownerDefines = ownerFiles.some((f) =>
  /^\s*export\s+interface\s+MediaRef\b/m.test(readFileSync(f, 'utf8')));
if (!ownerDefines) {
  violations.push(`${OWNER}: no exported MediaRef — the shared definition is gone, so "one shape" is vacuous`);
}

if (violations.length) {
  console.error(`[check:media-single-shape] FAILED — ${violations.length} violation(s):`);
  for (const v of violations) console.error(`  - ${v}`);
  console.error('\nMedia has one representation. Import it; do not restate it.');
  console.error('Owner: packages/ui-shared/src/types/media.ts');
  process.exit(1);
}

console.log(`[check:media-single-shape] OK -- MediaRef defined once in ${OWNER}; ${CONSUMERS.length} consumer bundle(s) clean.`);
