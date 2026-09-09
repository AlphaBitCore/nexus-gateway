#!/usr/bin/env node
/**
 * Undefined design-token reference check.
 *
 * `var(--token)` naming a custom property that NO source defines is invalid at
 * computed-value time. The browser does not warn and does not fall back to
 * anything sensible: it drops the whole declaration, and the property inherits
 * or takes its initial value. `border: var(--g-border-width) solid
 * var(--color-border)` renders no border at all; `padding: var(--g-space-sm)`
 * computes to 0. Nothing in the tree could see this before:
 * check-design-tokens.mjs matches hex/rgba LITERALS, never indexes token
 * definitions, and deliberately whitelists a literal sitting in var() fallback
 * position — none of which is the same question.
 *
 * Found on its first run: eleven tokens across 36 sites. Most were
 * misspellings of tokens that already existed (--g-color-text-muted for
 * --color-text-muted, --color-error-fg for --color-error-text), and the rest
 * were genuine gaps in a scale (--g-space-sm missing between an xs and an md
 * that both existed, so six sites computed padding 0).
 *
 * WHY THIS IS A SEPARATE SCRIPT and not another detector inside
 * check-design-tokens.mjs: that one supports `--files=` so pre-commit can scan
 * only what is staged. This check cannot work that way. The definition index
 * must cover the WHOLE tree on every run regardless of what is staged, or a
 * reference whose definition lives in an unstaged file reads as undefined.
 * Bolting it on would have meant either breaking that mode or silently giving
 * wrong answers inside it.
 *
 * SCOPE — this guard reads the SOURCE tree and deliberately skips `dist`.
 * A token can be defined here and still be absent from the shipped stylesheet:
 * Tailwind v4 tree-shakes `@theme` variables no utility class references, so a
 * token reached only through hand-written `var()` is dropped at build time.
 * That failure is invisible from source and is covered by its counterpart,
 * `check-css-var-resolution.mjs`, which asks the same question of the built
 * bundle. Neither subsumes the other — keep both.
 *
 * Definition sources, all three of them:
 *   1. `--name:` declarations in any .css in the scanned trees.
 *   2. `'--name':` keys in a TS/TSX inline style object — a component defining
 *      its own scoped property and reading it back from its stylesheet. Not a
 *      design token, but a real definition.
 *   3. lightTokens / darkTokens keys in packages/control-plane-ui/public/themes/*.json.
 *
 * Two things are deliberately NOT flagged:
 *   - `var(--x, fallback)`. A fallback means the declaration still produces a
 *     value, so it is not this defect class. global.css uses that pattern
 *     intentionally (`var(--color-bg-pressed, rgba(0,0,0,0.08))`).
 *   - `var(--color-${cond}-text)`. The name is computed; a static probe cannot
 *     resolve it, and guessing would produce noise.
 */

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';

const ROOT = process.cwd();
const TREES = [
  'packages/control-plane-ui',
  'packages/ui-shared',
  'packages/agent/ui/frontend',
];
const THEME_DIR = 'packages/control-plane-ui/public/themes';

const CSS_DEF = /(?<![\w-])(--[A-Za-z0-9_-]+)\s*:/g;
const TS_DEF = /['"](--[A-Za-z0-9_-]+)['"]\s*:/g;
const USE = /var\(\s*(--[A-Za-z0-9_-]*)([^)]*)/g;

/** Floors: below these the scan is broken and "no violations" means nothing. */
const MIN_DEFS = 200;
const MIN_CSS_FILES = 100;

function walk(dir, out) {
  let entries;
  try {
    entries = readdirSync(dir);
  } catch {
    return out;
  }
  for (const e of entries) {
    if (e === 'node_modules' || e === 'dist' || e === 'build') continue;
    const full = join(dir, e);
    if (statSync(full).isDirectory()) walk(full, out);
    else out.push(full);
  }
  return out;
}

function main() {
  const defs = new Set();
  const uses = new Map(); // name -> [{where, guarded}]
  let cssFiles = 0;

  // Theme overrides (they re-point existing semantic tokens, but a name that
  // only ever appears there is still defined at runtime).
  for (const f of walk(join(ROOT, THEME_DIR), [])) {
    if (!f.endsWith('.json')) continue;
    let theme;
    try {
      theme = JSON.parse(readFileSync(f, 'utf-8'));
    } catch {
      console.error(`[check:design-token-defined] FAIL — cannot parse ${f.slice(ROOT.length + 1)}`);
      process.exit(1);
    }
    for (const key of ['lightTokens', 'darkTokens']) {
      for (const name of Object.keys(theme[key] ?? {})) {
        defs.add(name.startsWith('--') ? name : `--${name}`);
      }
    }
  }

  const sourceFiles = [];
  for (const tree of TREES) sourceFiles.push(...walk(join(ROOT, tree), []));

  for (const full of sourceFiles) {
    const isCss = full.endsWith('.css');
    const isTs = (full.endsWith('.tsx') || full.endsWith('.ts')) &&
      !full.endsWith('.test.tsx') && !full.endsWith('.test.ts');
    if (!isCss && !isTs) continue;

    const src = readFileSync(full, 'utf-8');
    if (isCss) {
      cssFiles++;
      for (const m of src.matchAll(CSS_DEF)) defs.add(m[1]);
    } else {
      for (const m of src.matchAll(TS_DEF)) defs.add(m[1]);
    }

    const lines = src.split('\n');
    for (let i = 0; i < lines.length; i++) {
      for (const m of lines[i].matchAll(USE)) {
        const [, name, rest] = m;
        if (rest.startsWith('${') || (name === '--color-' && lines[i].includes('${'))) continue;
        const list = uses.get(name) ?? [];
        list.push({ where: `${full.slice(ROOT.length + 1)}:${i + 1}`, guarded: rest.includes(',') });
        uses.set(name, list);
      }
    }
  }

  if (defs.size < MIN_DEFS || cssFiles < MIN_CSS_FILES) {
    console.error(
      `[check:design-token-defined] FAIL — indexed ${defs.size} definitions from ${cssFiles} ` +
        `css file(s) (floors ${MIN_DEFS} / ${MIN_CSS_FILES}). The scan is broken, so ` +
        `"no undefined tokens" would mean nothing.`,
    );
    process.exit(1);
  }

  const violations = [];
  for (const [name, sites] of uses) {
    if (defs.has(name)) continue;
    const bare = sites.filter((s) => !s.guarded);
    if (bare.length > 0) violations.push({ name, bare, guarded: sites.length - bare.length });
  }

  if (violations.length > 0) {
    violations.sort((a, b) => b.bare.length - a.bare.length);
    const total = violations.reduce((n, v) => n + v.bare.length, 0);
    console.error(
      `[check:design-token-defined] FAIL — ${violations.length} token(s) referenced with no ` +
        `definition anywhere, at ${total} site(s). Each of these declarations is DROPPED at ` +
        `computed-value time: the property silently inherits or takes its initial value. Point ` +
        `at the token that already exists, or define the missing one beside its siblings:`,
    );
    for (const v of violations) {
      const extra = v.guarded ? `  (+${v.guarded} more carrying a fallback, harmless)` : '';
      console.error(`    ${v.name}  — ${v.bare.length} site(s)${extra}`);
      for (const s of v.bare) console.error(`        ${s.where}`);
    }
    process.exit(1);
  }

  console.log(
    `[check:design-token-defined] OK — every var(--token) resolves to one of ${defs.size} ` +
      `definitions (${cssFiles} css files scanned).`,
  );
}

main();
