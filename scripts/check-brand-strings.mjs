#!/usr/bin/env node
/**
 * Hardcoded brand-string guard.
 *
 * The product name + tagline reach UI components through the active
 * ThemeConfig.brand object — themes can rebrand the entire surface by
 * shipping a single JSON file. A literal "Nexus Gateway" string anywhere
 * other than a theme pack JSON (where it is the brand value) defeats
 * that mechanism.
 *
 * Scans:
 *   - packages/control-plane-ui/src/**\/*.{tsx,ts}
 *   - packages/agent/ui/frontend/src/**\/*.{tsx,ts}
 *   - packages/ui-shared/src/**\/*.{tsx,ts}
 *   - packages/control-plane-ui/src/i18n/locales/**\/*.json
 *   - packages/agent/ui/frontend/src/i18n/locales/**\/*.json
 *
 * Allows:
 *   - packages/{control-plane-ui,agent/ui/frontend}/public/themes/*.json
 *     (these DEFINE the brand and may legitimately contain the literal)
 *   - DEFAULT_THEME constant in packages/ui-shared/src/theme/themeLoader.ts
 *     (in-memory fallback when even default.json fails to load)
 *   - *.test.{ts,tsx} and *.stories.{ts,tsx}
 *   - comments (// or /* ... *\/) inside source files
 *
 * Trigger phrases caught: 'Nexus Gateway', "Nexus Gateway", `Nexus Gateway`.
 *
 * Usage:
 *   node scripts/check-brand-strings.mjs
 *   node scripts/check-brand-strings.mjs --json
 *
 * Exit codes:
 *   0 — no hardcoded brand strings in source
 *   1 — at least one offender
 */

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, dirname, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = join(__dirname, '..');

const args = new Set(process.argv.slice(2));
const asJson = args.has('--json');

const SCAN_ROOTS = [
  'packages/control-plane-ui/src',
  'packages/agent/ui/frontend/src',
  'packages/ui-shared/src',
];

const SOURCE_EXTS = new Set(['.ts', '.tsx', '.json']);
const TEST_EXT_PATTERN = /\.(test|stories)\.(ts|tsx)$/;
const SKIP_DIRS = new Set(['node_modules', 'dist', 'build', '.next']);

// Files allowed to contain the brand literal (they define the brand).
const ALLOWLIST_SUFFIXES = [
  'packages/ui-shared/src/theme/themeLoader.ts',
];

// Theme JSON files are scanned separately under public/themes/ via the
// theme-completeness check; here we just skip them.
const SKIP_PATH_FRAGMENTS = [
  '/public/themes/',
];

const BRAND_PATTERN = /(['"`])Nexus Gateway\1/g;

function walk(dir, out) {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    const stat = statSync(full);
    if (stat.isDirectory()) {
      if (SKIP_DIRS.has(entry)) continue;
      walk(full, out);
    } else {
      const ext = entry.slice(entry.lastIndexOf('.'));
      if (!SOURCE_EXTS.has(ext)) continue;
      if (TEST_EXT_PATTERN.test(entry)) continue;
      const rel = relative(REPO_ROOT, full);
      if (SKIP_PATH_FRAGMENTS.some((frag) => full.includes(frag))) continue;
      if (ALLOWLIST_SUFFIXES.some((s) => rel.endsWith(s))) continue;
      out.push({ abs: full, rel });
    }
  }
}

function stripCommentsAndStringsInsideComments(source) {
  // Naïve comment stripping — adequate for catching brand literals outside
  // comments. We don't try to handle string-literal-inside-comment edge
  // cases beyond removing comment text.
  return source
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .replace(/(^|[^:])\/\/[^\n]*/g, '$1');
}

const files = [];
for (const root of SCAN_ROOTS) {
  const abs = join(REPO_ROOT, root);
  try {
    walk(abs, files);
  } catch {
    // root may not exist in some layouts; skip
  }
}

const violations = [];
for (const { abs, rel } of files) {
  let source;
  try {
    source = readFileSync(abs, 'utf-8');
  } catch {
    continue;
  }
  const stripped = stripCommentsAndStringsInsideComments(source);
  const lines = source.split('\n');
  const strippedLines = stripped.split('\n');
  for (let i = 0; i < strippedLines.length; i++) {
    if (BRAND_PATTERN.test(strippedLines[i])) {
      violations.push({ file: rel, line: i + 1, text: lines[i].trim() });
    }
    BRAND_PATTERN.lastIndex = 0; // reset stateful global regex
  }
}

if (asJson) {
  console.log(JSON.stringify({
    filesScanned: files.length,
    count: violations.length,
    violations,
  }, null, 2));
} else {
  if (violations.length === 0) {
    console.log(`brand-string guard — scanned ${files.length} source files`);
    console.log('');
    console.log('✓ no hardcoded "Nexus Gateway" literals outside theme packs');
  } else {
    console.log(`brand-string guard — scanned ${files.length} files, found ${violations.length} hardcoded literal(s)\n`);
    for (const v of violations) {
      console.log(`  ${v.file}:${v.line}  ${v.text}`);
    }
    console.log('');
    console.log('Fix: route product name through brand.productName from useTheme()');
    console.log('  - In TSX:    const { brand } = useTheme(); <span>{brand.productName}</span>');
    console.log('  - In i18n:   "key": "Welcome to {{productName}}." + t(key, { productName: brand.productName })');
  }
}

process.exit(violations.length > 0 ? 1 : 0);
