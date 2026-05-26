#!/usr/bin/env node
/**
 * i18n key parity guard (S33, M4-5).
 *
 * Walks every locale file under packages/control-plane-ui/src/i18n/locales/<lang>/
 * and confirms that the flattened key sets match across all locales.
 * Exits non-zero if any locale has missing or extra keys, with a precise
 * diff so the operator (or CI) can fix it in one pass.
 *
 * Reference locale = `en` (the canonical source — every other locale must
 * mirror it). Extra keys in non-en locales are treated as drift even
 * though they don't strictly break the UI; they almost always indicate
 * a stale translation that should be removed.
 *
 * Usage:
 *   node scripts/check-i18n-parity.mjs              # all namespaces
 *   node scripts/check-i18n-parity.mjs pages.json   # one namespace
 *
 * Wire into CI or pre-commit by adding it to npm test / a husky hook.
 */

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = join(__dirname, '..');
const REFERENCE_LOCALE = 'en';

/**
 * Locale directories scanned by this guard. Each directory contains one
 * subdirectory per language, and each language subdirectory contains
 * one JSON file per namespace. Adding a new locale source (e.g. for
 * the Wails Dashboard once it owns its own namespaces) means appending
 * a new entry here.
 */
const LOCALE_SOURCES = [
  {
    label: 'control-plane-ui',
    dir: join(REPO_ROOT, 'packages/control-plane-ui/src/i18n/locales'),
  },
  {
    label: 'ui-shared',
    dir: join(REPO_ROOT, 'packages/ui-shared/src/i18n'),
  },
];

/** Recursively flatten an object to dotted-path keys. */
function flatten(obj, prefix = '') {
  const out = [];
  for (const [k, v] of Object.entries(obj)) {
    const path = prefix ? `${prefix}.${k}` : k;
    if (v !== null && typeof v === 'object' && !Array.isArray(v)) {
      out.push(...flatten(v, path));
    } else {
      out.push(path);
    }
  }
  return out;
}

function loadLocaleFile(baseDir, lang, namespace) {
  const path = join(baseDir, lang, namespace);
  const raw = readFileSync(path, 'utf8');
  try {
    return JSON.parse(raw);
  } catch (err) {
    throw new Error(`failed to parse ${path}: ${err.message}`);
  }
}

function listLocales(baseDir) {
  return readdirSync(baseDir).filter((entry) => {
    const full = join(baseDir, entry);
    return statSync(full).isDirectory();
  });
}

function listNamespaces(baseDir, lang) {
  return readdirSync(join(baseDir, lang))
    .filter((f) => f.endsWith('.json'))
    .sort();
}

function diff(reference, candidate) {
  const refSet = new Set(reference);
  const candSet = new Set(candidate);
  const missing = reference.filter((k) => !candSet.has(k));
  const extra = candidate.filter((k) => !refSet.has(k));
  return { missing, extra };
}

function checkSource(source, requestedNs) {
  const locales = listLocales(source.dir);
  if (!locales.includes(REFERENCE_LOCALE)) {
    console.error(`✗ [${source.label}] reference locale "${REFERENCE_LOCALE}" not found in ${source.dir}`);
    return { problems: 1, totalChecks: 0 };
  }
  const referenceNamespaces = listNamespaces(source.dir, REFERENCE_LOCALE);
  const namespaces = requestedNs
    ? referenceNamespaces.filter((n) => n === requestedNs)
    : referenceNamespaces;
  if (namespaces.length === 0) {
    if (requestedNs) {
      console.log(`(skipped ${source.label}: no namespace "${requestedNs}")`);
    }
    return { problems: 0, totalChecks: 0 };
  }
  const otherLocales = locales.filter((l) => l !== REFERENCE_LOCALE).sort();
  let problems = 0;
  let totalChecks = 0;
  for (const ns of namespaces) {
    const refKeys = flatten(loadLocaleFile(source.dir, REFERENCE_LOCALE, ns)).sort();
    for (const lang of otherLocales) {
      totalChecks++;
      let candKeys;
      try {
        candKeys = flatten(loadLocaleFile(source.dir, lang, ns)).sort();
      } catch (err) {
        console.error(`✗ [${source.label}] ${lang}/${ns}: ${err.message}`);
        problems++;
        continue;
      }
      const { missing, extra } = diff(refKeys, candKeys);
      if (missing.length === 0 && extra.length === 0) {
        console.log(`✓ [${source.label}] ${lang}/${ns}: ${refKeys.length} keys aligned with ${REFERENCE_LOCALE}`);
        continue;
      }
      problems++;
      console.error(`✗ [${source.label}] ${lang}/${ns}: drift detected`);
      if (missing.length > 0) {
        console.error(`  missing in ${lang} (${missing.length}):`);
        for (const k of missing.slice(0, 20)) console.error(`    - ${k}`);
        if (missing.length > 20) console.error(`    … and ${missing.length - 20} more`);
      }
      if (extra.length > 0) {
        console.error(`  extra in ${lang} not in ${REFERENCE_LOCALE} (${extra.length}):`);
        for (const k of extra.slice(0, 20)) console.error(`    + ${k}`);
        if (extra.length > 20) console.error(`    … and ${extra.length - 20} more`);
      }
    }
  }
  return { problems, totalChecks };
}

function main() {
  const requestedNs = process.argv[2];
  let totalProblems = 0;
  let totalChecks = 0;
  for (const source of LOCALE_SOURCES) {
    const result = checkSource(source, requestedNs);
    totalProblems += result.problems;
    totalChecks += result.totalChecks;
  }
  console.log('');
  if (totalProblems === 0) {
    console.log(`✓ i18n parity OK across ${totalChecks} locale × namespace combinations`);
    process.exit(0);
  } else {
    console.error(`✗ i18n parity FAILED in ${totalProblems} of ${totalChecks} combinations`);
    process.exit(1);
  }
}

main();
