#!/usr/bin/env node
/**
 * Built-CSS variable resolution guard.
 *
 * `check-design-tokens.mjs` is the source-tree guard: it enforces that visual
 * values are written as CSS variables rather than literals. It scans the three
 * `src` trees, so every token it sees is one that IS defined in source.
 *
 * This guard covers the failure that one structurally cannot see: a token that
 * is defined in source and absent from the SHIPPED stylesheet. Tailwind v4
 * tree-shakes `@theme` variables that no utility class references — a token
 * reached only through hand-written `var(--x)` in a CSS module or an inline
 * style is dropped from the bundle even though the source declares it.
 *
 * The result renders, so nothing fails loudly. `var()` on an undefined custom
 * property yields the guaranteed-invalid value, and for a color that computes
 * to `rgba(0, 0, 0, 0)` — fully transparent. This was live on prod: a dropped
 * `--color-gray-50` emptied `--color-surface-2`, which emptied both
 * `--color-surface-alt` and `--color-bg-secondary`, and every surface painted
 * with them rendered transparent.
 *
 * The rule, checked against the build output rather than the source:
 *
 *   every `var(--x)` used WITHOUT a fallback must have `--x` defined
 *   somewhere in the same bundle.
 *
 * `var(--x, something)` is exempt by construction — the author supplied the
 * value to use when `--x` is absent, which is what a runtime-assigned variable
 * (a component setting `--shimmer-width` while animating) should look like.
 * That is why this needs no allowlist: "may legitimately be unset" and "has a
 * fallback" are the same statement.
 *
 * Requires a build first — it reads `dist/`, not `src/`. Run after
 * `npm run build -w packages/control-plane-ui`.
 *
 * Usage:
 *   node scripts/check-css-var-resolution.mjs
 *   node scripts/check-css-var-resolution.mjs --json
 *
 * Exit codes:
 *   0 — every variable resolves
 *   1 — at least one variable is used with no fallback and never defined
 *   3 — no built CSS found (unmeasured; NOT a pass)
 */

import { readFileSync, readdirSync, existsSync } from 'node:fs';
import { join, dirname, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = join(__dirname, '..');

const BUNDLES = [
  'packages/control-plane-ui/dist/assets',
  'packages/agent/ui/frontend/dist/assets',
];

const jsonOut = process.argv.includes('--json');

/** Custom-property declarations: `--x: value`. */
const DECL_RE = /(--[a-zA-Z0-9_-]+)\s*:/g;
/** `@property --x { … }` registers a variable with an initial value. */
const AT_PROPERTY_RE = /@property\s+(--[a-zA-Z0-9_-]+)/g;
/**
 * `var(--x)` with no fallback. A comma before the closing paren means the
 * author supplied one, so the reference cannot resolve to nothing.
 */
const USE_NO_FALLBACK_RE = /var\(\s*(--[a-zA-Z0-9_-]+)\s*\)/g;

function matchAll(text, re) {
  const out = new Set();
  for (const m of text.matchAll(re)) out.add(m[1]);
  return out;
}

/** Report the first place a variable is used, so the failure is navigable. */
function firstUseSite(files, name) {
  const needle = new RegExp(`var\\(\\s*${name}\\s*\\)`);
  for (const { path, text } of files) {
    const lines = text.split('\n');
    for (let i = 0; i < lines.length; i++) {
      if (needle.test(lines[i])) {
        const col = lines[i].search(needle);
        return `${relative(REPO_ROOT, path)}:${i + 1} …${lines[i].slice(Math.max(0, col - 60), col + 60).trim()}…`;
      }
    }
  }
  return '(use site not located)';
}

const results = [];
let checkedAny = false;

for (const dir of BUNDLES) {
  const abs = join(REPO_ROOT, dir);
  if (!existsSync(abs)) continue;

  const files = readdirSync(abs)
    .filter((f) => f.endsWith('.css'))
    .map((f) => ({ path: join(abs, f), text: readFileSync(join(abs, f), 'utf8') }));
  if (files.length === 0) continue;

  checkedAny = true;
  const all = files.map((f) => f.text).join('\n');
  const defined = new Set([...matchAll(all, DECL_RE), ...matchAll(all, AT_PROPERTY_RE)]);
  const used = matchAll(all, USE_NO_FALLBACK_RE);
  const dangling = [...used].filter((v) => !defined.has(v)).sort();

  results.push({
    bundle: dir,
    files: files.length,
    defined: defined.size,
    used: used.size,
    dangling: dangling.map((name) => ({ name, site: firstUseSite(files, name) })),
  });
}

if (!checkedAny) {
  const msg = 'no built CSS found — run `npm run build -w packages/control-plane-ui` first';
  if (jsonOut) console.log(JSON.stringify({ status: 'skipped', reason: msg }, null, 2));
  else console.error(`⊘ SKIPPED — did NOT run: ${msg}`);
  process.exit(3);
}

if (jsonOut) {
  const failed = results.some((r) => r.dangling.length > 0);
  console.log(JSON.stringify({ status: failed ? 'fail' : 'pass', results }, null, 2));
  process.exit(failed ? 1 : 0);
}

let failures = 0;
for (const r of results) {
  console.log(`${r.bundle} — ${r.files} css file(s), ${r.defined} vars defined, ${r.used} used without a fallback`);
  for (const d of r.dangling) {
    failures++;
    console.log(`  ✗ ${d.name} is used with no fallback and is never defined in the bundle`);
    console.log(`      ${d.site}`);
  }
  if (r.dangling.length === 0) console.log('  ✓ every variable resolves');
}

if (failures > 0) {
  console.log('');
  console.log(`${failures} unresolvable CSS variable(s). These render as the guaranteed-invalid`);
  console.log('value — for a color, fully transparent — so the page looks broken without');
  console.log('failing. If a variable is meant to be assigned at runtime, give the `var()`');
  console.log('a fallback; otherwise the token is being tree-shaken out of the bundle and');
  console.log('its `@theme` block needs the `static` option.');
  process.exit(1);
}
process.exit(0);
