#!/usr/bin/env node
/**
 * Accessibility-label i18n check.
 *
 * conventions.md states, binding, that every user-visible string goes through
 * `t()`. Nothing enforced the "goes through t()" half: check-i18n-parity.mjs
 * compares the locale bundles against EACH OTHER, so a string with no key at
 * all is structurally invisible to it. A hardcoded literal is not a missing
 * key — it is a key that was never created, and parity cannot miss what was
 * never there.
 *
 * This gate takes the narrowest slice where that blindness costs the most:
 * `aria-label` / `title` / `alt`. Those are read by screen-reader users and by
 * nobody else, so an untranslated one can sit in the product indefinitely with
 * no sighted reviewer ever seeing it. Four were found this way ("unsaved
 * changes", "capture request body", "capture response body", "masked secret").
 *
 * Deliberately NOT a general hardcoded-string detector. Two measurements ruled
 * that out:
 *
 *   1. It would be blind where it matters. Product copy lives in object
 *      literals as often as in JSX — LatencyMini held five phase labels and
 *      five prose descriptions as `label:` / `desc:` values, which no
 *      JSX-text scan can see. A detector that misses the largest sites while
 *      reporting a clean sweep is worse than none: it certifies.
 *   2. It would be noisy where it is not. A `>text<` scan cannot tell JSX
 *      text from a TypeScript generic argument, so `Promise<T>` and
 *      `Record<K, V>` read as user-facing prose — roughly a third of a naive
 *      run's hits.
 *
 * `placeholder` is excluded on purpose: nearly every one in this tree is
 * example DATA ("smtp.example.com", "3128", "gpt-4o-mini", "#alerts"), which
 * is not copy and should not be translated. Including it would mean a large
 * allowlist, and an allowlist that big is where a gate goes to die.
 *
 * "Prose" = two or more alphabetic words. One word is a token ("email",
 * "groups", "openid"); two is a sentence fragment a human reads.
 */

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';

const ROOT = process.cwd();
const TREES = [
  'packages/control-plane-ui/src',
  'packages/agent/ui/frontend/src',
  'packages/ui-shared/src',
];

const PROP = /\b(aria-label|title|aria-description|alt)="([^"]+)"/g;

/** Floor: below this the scan is broken, and "no violations" means nothing. */
const MIN_FILES = 300;

function walk(dir, out) {
  let entries;
  try {
    entries = readdirSync(dir);
  } catch {
    return out; // tree absent in this checkout — reported by the floor below
  }
  for (const e of entries) {
    const full = join(dir, e);
    if (statSync(full).isDirectory()) walk(full, out);
    else out.push(full);
  }
  return out;
}

function isProse(v) {
  const words = v.trim().split(/\s+/).filter((w) => /^[A-Za-z][A-Za-z'-]*$/.test(w));
  return words.length >= 2;
}

function main() {
  const violations = [];
  let scanned = 0;

  for (const tree of TREES) {
    for (const full of walk(join(ROOT, tree), [])) {
      if (!full.endsWith('.tsx')) continue;
      if (full.endsWith('.stories.tsx') || full.endsWith('.test.tsx')) continue;
      scanned++;
      const src = readFileSync(full, 'utf-8');
      const lines = src.split('\n');
      for (let i = 0; i < lines.length; i++) {
        for (const m of lines[i].matchAll(PROP)) {
          if (isProse(m[2])) {
            violations.push({
              where: `${full.slice(ROOT.length + 1)}:${i + 1}`,
              prop: m[1],
              value: m[2],
            });
          }
        }
      }
    }
  }

  if (scanned < MIN_FILES) {
    console.error(
      `[check:a11y-label-i18n] FAIL — scanned only ${scanned} .tsx files (floor ${MIN_FILES}). ` +
        `The scan is broken, so "no violations" would mean nothing.`,
    );
    process.exit(1);
  }

  if (violations.length > 0) {
    console.error(
      `[check:a11y-label-i18n] FAIL — ${violations.length} accessibility label(s) are hardcoded ` +
        `English. Screen-reader users on es/zh read these verbatim, and no sighted reviewer ` +
        `ever sees them. Add a key to en/zh/es together and call t():`,
    );
    for (const v of violations) {
      console.error(`    ${v.prop}="${v.value}"\n        ${v.where}`);
    }
    process.exit(1);
  }

  console.log(
    `[check:a11y-label-i18n] OK — no hardcoded prose in aria-label/title/alt across ` +
      `${scanned} .tsx files.`,
  );
}

main();
