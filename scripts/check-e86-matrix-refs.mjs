#!/usr/bin/env node
/**
 * Lockstep check: every `S-NNN` reference in `docs/developers/specs/e86-e2e-coverage-matrix.md`
 * must correspond to a real `func TestS<NNN>_*` in `tests/scenarios/*_test.go`, and vice versa
 * (orphan test functions are reported as warnings).
 *
 * Why:
 *   The matrix is the "shipped capability ↔ test arm" map. If a row claims `S-033 auto-revert`
 *   but no `TestS033_*` exists, the row is fiction and CI cannot defend the claim. Conversely
 *   a `TestS999_FooBar` that no row mentions is either a stale test or a missing matrix row.
 *
 * Behavior:
 *   - Missing test function for matrix-referenced ID  -> FAIL (exit 1)
 *   - Test function without matrix reference          -> WARN (exit 0)
 *   - Otherwise                                       -> OK   (exit 0)
 *
 * Regex notes:
 *   - Matrix IDs use `\bS-(\d{3})\b` so embedded numerics like "AES-256-GCM" don't match.
 *   - Function names use `^func TestS(\d{3,})_` so `TestSeed_*` etc. don't match.
 *     We zero-pad shorter digit captures defensively, though the current convention is 3 digits.
 */

import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';

const REPO_ROOT = process.cwd();
const MATRIX_DOC = 'docs/developers/specs/e86-e2e-coverage-matrix.md';
const SCENARIOS_DIR = 'tests/scenarios';

function extractMatrixIds(text) {
  const ids = new Set();
  const re = /\bS-(\d{3})\b/g;
  let m;
  while ((m = re.exec(text)) !== null) {
    ids.add(m[1]);
  }
  return ids;
}

function extractScenarioFns(dir) {
  // Map id -> { file, fullName }
  const found = new Map();
  for (const entry of readdirSync(dir)) {
    if (!entry.endsWith('_test.go')) continue;
    const full = join(dir, entry);
    const src = readFileSync(full, 'utf-8');
    const re = /^func (TestS(\d+)_\w+)\s*\(/gm;
    let m;
    while ((m = re.exec(src)) !== null) {
      const fullName = m[1];
      const raw = m[2];
      // Normalize to 3-digit ID for comparison with matrix IDs.
      const id = raw.padStart(3, '0');
      if (id.length !== 3) continue; // anything > 3 digits is not a matrix-shaped ID; skip
      if (!found.has(id)) {
        found.set(id, { file: full, fullName });
      }
    }
  }
  return found;
}

function main() {
  const matrixText = readFileSync(join(REPO_ROOT, MATRIX_DOC), 'utf-8');
  const matrixIds = extractMatrixIds(matrixText);
  const scenarioFns = extractScenarioFns(join(REPO_ROOT, SCENARIOS_DIR));

  const failures = [];
  const warnings = [];

  // Forward: every matrix-referenced ID must have a TestSNNN_ function somewhere.
  const sortedMatrixIds = [...matrixIds].sort();
  for (const id of sortedMatrixIds) {
    if (!scenarioFns.has(id)) {
      failures.push(
        `S-${id} referenced in ${MATRIX_DOC} but no \`func TestS${id}_*\` exists in ${SCENARIOS_DIR}/*_test.go`,
      );
    }
  }

  // Reverse: every TestSNNN_ function should be mentioned in the matrix.
  // Warn-only because some scenarios (e.g. catalog-internal regression coverage)
  // may legitimately not roll up to a customer-facing capability row.
  const sortedFnIds = [...scenarioFns.keys()].sort();
  for (const id of sortedFnIds) {
    if (!matrixIds.has(id)) {
      const { file, fullName } = scenarioFns.get(id);
      warnings.push(
        `S-${id} (\`${fullName}\` in ${file}) is not referenced in ${MATRIX_DOC}`,
      );
    }
  }

  // Reporting
  if (warnings.length > 0) {
    console.warn(`[check:e86-matrix] WARN: ${warnings.length} orphan scenario function(s) without matrix reference:`);
    for (const w of warnings) console.warn('  - ' + w);
    console.warn(
      '  Fix: add a row (or extend an existing row) in ' +
        MATRIX_DOC +
        ' mentioning each S-NNN, or remove the orphan TestSNNN_ function.',
    );
  }

  if (failures.length > 0) {
    console.error(`[check:e86-matrix] FAILED: ${failures.length} matrix reference(s) without backing test function:`);
    for (const f of failures) console.error('  - ' + f);
    console.error(
      '  Fix: either add `func TestS<NNN>_<Name>(t *testing.T)` in ' +
        SCENARIOS_DIR +
        '/<topic>_test.go, or remove the unbacked `S-<NNN>` mention from the matrix.',
    );
    process.exit(1);
  }

  console.log(
    `[check:e86-matrix] OK -- ${matrixIds.size} matrix S-NNN reference(s) all backed by a TestSNNN_ function ` +
      `(${scenarioFns.size} total TestSNNN_ functions in ${SCENARIOS_DIR}; ${warnings.length} orphan-warn).`,
  );
}

main();
