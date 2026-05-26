#!/usr/bin/env node
/**
 * Theme completeness guard.
 *
 * Every theme pack JSON (packages/{control-plane-ui,agent/ui/frontend}/
 * public/themes/*.json) MUST define every semantic token listed in
 * REQUIRED_THEME_TOKENS (sourced from ui-shared/src/theme/completeness.ts)
 * in BOTH `lightTokens` AND `darkTokens`.
 *
 * Why: themes are merged on top of the previously loaded theme's token
 * style block. A theme that omits `--color-primary` keeps whatever the
 * previous theme set — most visibly, Nexus's default blue bleeding
 * through a customer's branded UI. Forcing complete coverage means
 * dropping in a new theme JSON gives a deterministic, fully branded
 * result on the first paint.
 *
 * Usage:
 *   node scripts/check-theme-completeness.mjs
 *   node scripts/check-theme-completeness.mjs --json
 *
 * Exit codes:
 *   0 — every theme covers every required token in both modes
 *   1 — at least one theme is missing required tokens
 */

import { readFileSync, readdirSync, existsSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = join(__dirname, '..');

const args = new Set(process.argv.slice(2));
const asJson = args.has('--json');

// Theme directories to scan. Each app keeps its own copy of the canonical
// theme packs under public/themes/ so Vite/Wails serves them at runtime.
const THEME_DIRS = [
  'packages/control-plane-ui/public/themes',
  'packages/agent/ui/frontend/public/themes',
];

/**
 * Extract REQUIRED_THEME_TOKENS by reading and parsing the source file.
 * We hand-parse rather than dynamic-import because this script runs in
 * plain Node without a TS loader, and the file is a flat readonly array
 * of string literals.
 */
function loadRequiredTokens() {
  const src = readFileSync(
    join(REPO_ROOT, 'packages/ui-shared/src/theme/completeness.ts'),
    'utf-8',
  );
  const match = src.match(/REQUIRED_THEME_TOKENS:[^=]*=\s*\[([\s\S]*?)\]/);
  if (!match) {
    throw new Error('Failed to parse REQUIRED_THEME_TOKENS from completeness.ts');
  }
  return [...match[1].matchAll(/'([a-z][a-z0-9-]*)'/g)].map((m) => m[1]);
}

const REQUIRED = loadRequiredTokens();

function findThemeFiles() {
  const files = [];
  for (const dir of THEME_DIRS) {
    const abs = join(REPO_ROOT, dir);
    if (!existsSync(abs)) continue;
    for (const name of readdirSync(abs)) {
      if (!name.endsWith('.json')) continue;
      files.push({ dir, name, abs: join(abs, name) });
    }
  }
  return files;
}

const themes = findThemeFiles();
const violations = [];

for (const { dir, name, abs } of themes) {
  let theme;
  try {
    theme = JSON.parse(readFileSync(abs, 'utf-8'));
  } catch (err) {
    violations.push({ dir, name, kind: 'parse-error', message: err.message });
    continue;
  }

  for (const modeKey of ['lightTokens', 'darkTokens']) {
    const map = theme[modeKey] ?? {};
    const missing = REQUIRED.filter((t) => !(t in map));
    if (missing.length > 0) {
      violations.push({ dir, name, kind: 'missing-tokens', mode: modeKey, missing });
    }
  }

  // Also require a brand block with productName — the lint rule that
  // backs "no hardcoded brand strings".
  if (!theme.brand?.productName) {
    violations.push({ dir, name, kind: 'missing-brand', detail: 'brand.productName' });
  }
}

if (asJson) {
  console.log(JSON.stringify({
    requiredTokenCount: REQUIRED.length,
    themesScanned: themes.length,
    violationCount: violations.length,
    violations,
  }, null, 2));
} else {
  if (violations.length === 0) {
    console.log(`theme-completeness guard — ${themes.length} themes scanned`);
    console.log('');
    console.log(`✓ every theme covers all ${REQUIRED.length} required semantic tokens (light + dark) and defines brand.productName`);
  } else {
    console.log(`theme-completeness guard — ${themes.length} themes scanned, ${violations.length} issue(s)\n`);
    for (const v of violations) {
      const path = `${v.dir}/${v.name}`;
      if (v.kind === 'parse-error') {
        console.log(`  ✗ ${path} — parse error: ${v.message}`);
      } else if (v.kind === 'missing-brand') {
        console.log(`  ✗ ${path} — missing ${v.detail}`);
      } else if (v.kind === 'missing-tokens') {
        console.log(`  ✗ ${path} — ${v.mode} missing ${v.missing.length} required token(s):`);
        for (const t of v.missing) console.log(`      --${t}`);
      }
    }
    console.log('');
    console.log('Fix: add the missing tokens to the theme JSON. The full required set is exported from packages/ui-shared/src/theme/completeness.ts.');
  }
}

process.exit(violations.length > 0 ? 1 : 0);
