#!/usr/bin/env node
// Runs every `check:*` gate and fails if any of them fails.
//
// The list is derived from package.json rather than hand-maintained here: a
// gate added to the scripts block joins this run automatically, which is the
// property a hand-copied list loses the first time someone forgets.
//
// EXCLUDED are the `:strict` and `:staged` variants — the same check under a
// different scope, already represented by their base name — and this script
// itself.
//
// Why this is a script and not `npm-run-all -p ... || echo`: that form exits 0
// when the runner is missing, so the aggregate reported success while running
// nothing at all. A gate that cannot fail is worse than no gate, because it is
// quoted as evidence.

import { execFile } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { promisify } from 'node:util';
import { cpus } from 'node:os';

const run = promisify(execFile);

const SELF = 'check:all';
const SKIP_SUFFIXES = [':strict', ':staged', ':hints'];

// Gates that run a whole test suite. They are given the machine to themselves,
// one at a time, after the cheap gates finish.
//
// Not a preference: running them alongside each other made timing-sensitive
// tests in unrelated packages fail under the contention, and a suite that goes
// red because of how the runner scheduled it teaches the reader to distrust
// red. Widening each test's timeout would have spread this runner's defect
// across the codebase as budget nobody could account for.
const HEAVY = new Set(['check:coverage', 'check:coverage:core', 'check:coverage:ui']);

const pkg = JSON.parse(readFileSync(new URL('../package.json', import.meta.url), 'utf8'));
const gates = Object.keys(pkg.scripts)
  .filter((n) => n.startsWith('check:'))
  .filter((n) => n !== SELF)
  .filter((n) => !SKIP_SUFFIXES.some((s) => n.endsWith(s)))
  .sort();

if (gates.length === 0) {
  console.error('check:all: no check:* scripts found in package.json — refusing to report success');
  process.exit(1);
}

const light = gates.filter((n) => !HEAVY.has(n));
const heavy = gates.filter((n) => HEAVY.has(n));

const limit = Math.max(2, Math.min(8, cpus().length - 1));
const queue = [...light];
const failed = [];
const passed = [];

async function worker() {
  for (;;) {
    const name = queue.shift();
    if (!name) return;
    try {
      await run('npm', ['run', '--silent', name], { maxBuffer: 64 * 1024 * 1024 });
      passed.push(name);
      process.stdout.write(`  ✓ ${name}\n`);
    } catch (err) {
      const out = `${err.stdout ?? ''}${err.stderr ?? ''}`.trimEnd();
      failed.push({ name, out });
      process.stdout.write(`  ✗ ${name}\n`);
    }
  }
}

console.log(
  `check:all — ${light.length} gates (${limit} at a time), then ${heavy.length} suite gates one at a time\n`,
);
await Promise.all(Array.from({ length: limit }, worker));

queue.push(...heavy);
await worker();

if (failed.length > 0) {
  for (const f of failed) {
    console.error(`\n─── ${f.name} ───\n${f.out}`);
  }
  console.error(`\ncheck:all: ${failed.length} of ${gates.length} gates FAILED — ${failed.map((f) => f.name).join(', ')}`);
  process.exit(1);
}

console.log(`\ncheck:all: all ${passed.length} gates passed`);
