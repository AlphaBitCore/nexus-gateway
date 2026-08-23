#!/usr/bin/env node
/**
 * Cross-platform locale sync for the Control Plane UI build.
 *
 * Copies every JSON namespace from every locale directory under
 *   packages/control-plane-ui/src/i18n/locales/<lang>/
 *   packages/ui-shared/src/i18n/<lang>/
 * into
 *   packages/control-plane-ui/public/locales/<lang>/
 *
 * The HTTP i18next backend serves them from there for non-English
 * languages; English is bundled at build time directly via JSON
 * imports in src/i18n/index.ts.
 *
 * Replaces the previous bash for-loop that used POSIX `cp` so the
 * build works on Windows and CI runners that ship a minimal shell
 * (no bash, no POSIX cp).
 *
 * Usage:
 *   node scripts/sync-locales.mjs            copy
 *   node scripts/sync-locales.mjs --check    verify, copy nothing, exit 1 on drift
 *
 * --check exists because the copy is the only thing keeping the served
 * locale files in step with the source of truth, and nothing verified it.
 * check:i18n compares key sets ACROSS LANGUAGES; it never compares src
 * against public. So a commit that added keys to src/i18n/locales without
 * re-running this script shipped a UI that rendered raw i18n keys for the
 * new strings, and every gate stayed green. That is exactly what happened
 * to the media-card copy: the strings were committed, public/ was not
 * refreshed, and the deployed drawer showed key paths instead of English.
 */

import { readdirSync, statSync, copyFileSync, mkdirSync, readFileSync } from 'node:fs';
import { join, basename, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = join(__dirname, '..');

const SOURCES = [
  join(REPO_ROOT, 'packages/control-plane-ui/src/i18n/locales'),
  join(REPO_ROOT, 'packages/ui-shared/src/i18n'),
];
const DEST_ROOT = join(REPO_ROOT, 'packages/control-plane-ui/public/locales');

function listDirs(parent) {
  return readdirSync(parent).filter((name) => {
    const full = join(parent, name);
    try {
      return statSync(full).isDirectory();
    } catch {
      return false;
    }
  });
}

function listJsonFiles(parent) {
  return readdirSync(parent).filter((name) => name.endsWith('.json'));
}

const CHECK_ONLY = process.argv.includes('--check');
const drift = [];

let totalCopied = 0;
for (const src of SOURCES) {
  let languages;
  try {
    languages = listDirs(src);
  } catch {
    // Source missing — skip silently. Lets `ui-shared` be removed
    // later without breaking CP UI's build.
    continue;
  }
  for (const lang of languages) {
    const srcLang = join(src, lang);
    const destLang = join(DEST_ROOT, lang);
    if (!CHECK_ONLY) mkdirSync(destLang, { recursive: true });
    for (const file of listJsonFiles(srcLang)) {
      const from = join(srcLang, file);
      const to = join(destLang, file);
      if (CHECK_ONLY) {
        let served;
        try {
          served = readFileSync(to, 'utf8');
        } catch {
          drift.push(`${lang}/${file}: not served at all — public/locales is missing this namespace`);
          continue;
        }
        if (served !== readFileSync(from, 'utf8')) {
          drift.push(`${lang}/${file}: served copy differs from the source of truth`);
        }
        continue;
      }
      copyFileSync(from, to);
      totalCopied++;
    }
  }
}

if (CHECK_ONLY) {
  const rel = DEST_ROOT.replace(REPO_ROOT + '/', '');
  if (drift.length) {
    console.error(`✗ sync-locales --check: ${drift.length} served locale file(s) are stale in ${rel}`);
    for (const d of drift) console.error(`  - ${d}`);
    console.error('\nFix: node scripts/sync-locales.mjs, then commit the result.');
    console.error('The served copy is what users read; a stale one renders raw i18n keys.');
    process.exit(1);
  }
  console.log(`✓ sync-locales --check: ${rel} matches the source locales`);
} else {
  console.log(`✓ sync-locales: ${totalCopied} locale files copied to ${DEST_ROOT.replace(REPO_ROOT + '/', '')}`);
}
