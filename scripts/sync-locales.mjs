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
 *   node scripts/sync-locales.mjs
 */

import { readdirSync, statSync, copyFileSync, mkdirSync } from 'node:fs';
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
    mkdirSync(destLang, { recursive: true });
    for (const file of listJsonFiles(srcLang)) {
      copyFileSync(join(srcLang, file), join(destLang, file));
      totalCopied++;
    }
  }
}

console.log(`✓ sync-locales: ${totalCopied} locale files copied to ${DEST_ROOT.replace(REPO_ROOT + '/', '')}`);
