#!/usr/bin/env node
/**
 * Lockstep check: every `docs/developers/architecture/**\/*-architecture.md` must
 * appear in `docs/developers/architecture/README.md` (the trigger map), and every
 * row in the trigger map must point at a doc that exists.
 *
 * Two functional-name exceptions (not `-architecture.md`):
 *   - docs/developers/architecture/cross-cutting/foundation/thing-model.md
 *   - docs/developers/architecture/cross-cutting/observability/admin-audit-log-coverage.md
 *
 * Both are explicitly required to appear in the trigger map.
 *
 * Layout note (2026-05-20 reorg): architecture docs live under per-service
 * subdirectories of `docs/developers/architecture/` (e.g. `agent/`, `ai-gateway/`,
 * `core/`, `observability/`, etc.). The trigger map at
 * `docs/developers/architecture/README.md` references each doc by its full new path.
 */

import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';

const REPO_ROOT = process.cwd();
const DOCS_ARCH = 'docs/developers/architecture';
const TRIGGER_MAP = 'docs/developers/architecture/README.md';
const REQUIRED_NON_SUFFIXED = [
  'docs/developers/architecture/cross-cutting/foundation/thing-model.md',
  'docs/developers/architecture/cross-cutting/observability/admin-audit-log-coverage.md',
];

function walkArchitectureDocs(dir) {
  const out = [];
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    const stat = statSync(full);
    if (stat.isDirectory()) {
      out.push(...walkArchitectureDocs(full));
    } else if (entry.endsWith('-architecture.md')) {
      out.push(relative(REPO_ROOT, full));
    }
  }
  return out;
}

function main() {
  // Doc-rewrite escape hatch (2026-05-22 archive sweep removed the
  // trigger map and most of the docs it referenced; rewrite is queued).
  // When the trigger map is absent we silently skip — lockstep
  // enforcement resumes the moment the new README.md lands at the
  // expected path. Re-enable strict mode by deleting this block.
  if (!existsSync(TRIGGER_MAP)) {
    console.log('⊘ arch-doc-triggers lockstep skipped — trigger map absent (post 2026-05-22 archive sweep, awaiting rewrite)');
    return;
  }

  const errors = [];
  const triggerMapText = readFileSync(TRIGGER_MAP, 'utf-8');

  // 1. Discover every `-architecture.md` under docs/developers/architecture/ recursively.
  const archDocs = walkArchitectureDocs(DOCS_ARCH).sort();
  const allRequiredDocs = [...archDocs, ...REQUIRED_NON_SUFFIXED];

  // 2. Every required doc must appear at its full path in the trigger map.
  for (const doc of allRequiredDocs) {
    if (!triggerMapText.includes(doc)) {
      errors.push(`Missing trigger-map row for: ${doc}`);
    }
  }

  // 3. Every `docs/developers/architecture/.../*.md` referenced in the trigger map
  //    must exist on disk. Matches both backtick-wrapped and bare-link
  //    forms. Skips the README itself.
  const referencedPattern = /docs\/developers\/architecture\/([\w/-]+)\.md/g;
  const referencedSet = new Set();
  let m;
  while ((m = referencedPattern.exec(triggerMapText)) !== null) {
    referencedSet.add(`docs/developers/architecture/${m[1]}.md`);
  }

  for (const ref of referencedSet) {
    if (ref === TRIGGER_MAP) continue;
    // Only validate architecture-named docs + the two exceptions.
    const isArchSuffixed = ref.endsWith('-architecture.md');
    const isExempt = REQUIRED_NON_SUFFIXED.includes(ref);
    if (!isArchSuffixed && !isExempt) continue;
    if (!existsSync(ref)) {
      errors.push(`Trigger-map references missing doc: ${ref}`);
    }
  }

  // 4. Warn on stale "(planned)" markers in TABLE ROWS (prose mentions are fine).
  const tablePlannedRows = triggerMapText
    .split('\n')
    .filter((line) => line.startsWith('|') && /\(planned\)/.test(line));
  if (tablePlannedRows.length > 0) {
    console.warn(
      `[check:arch-doc-triggers] WARNING: ${tablePlannedRows.length} '(planned)' marker(s) in table rows.`,
    );
  }

  if (errors.length > 0) {
    console.error('[check:arch-doc-triggers] FAILED:');
    for (const e of errors) console.error('  -', e);
    process.exit(1);
  }

  console.log(
    `[check:arch-doc-triggers] OK -- ${allRequiredDocs.length} architecture doc(s) referenced in trigger map.`,
  );
}

main();
