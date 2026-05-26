#!/usr/bin/env node
/**
 * Lint: enforce the workspace-replace contract for sibling Go modules.
 *
 * Background
 *   Every packages/<svc>/go.mod that depends on a sibling package under
 *   github.com/AlphaBitCore/nexus-gateway/packages/<sibling> must:
 *
 *     1. require the sibling at version `v0.0.0` (inert placeholder), NOT a
 *        real pseudo-version like `v0.0.0-YYYYMMDDHHMMSS-COMMITHASH`.
 *        Reason: Go 1.25 validates real pseudo-versions against the upstream
 *        Git remote even under `go.work`. Workspace mode is then disabled at
 *        build time and Go silently fetches a stale snapshot from GitHub.
 *        Using `v0.0.0` + `replace` keeps the version inert and forces local
 *        resolution.
 *
 *     2. carry a matching `replace github.com/.../packages/<sibling> => ../<sibling>`
 *        directive so workspace-OFF builds also resolve to the local copy
 *        (and fail loudly if the sibling directory is missing — instead of
 *        silently pulling a remote snapshot).
 *
 *     3. NOT have any `github.com/AlphaBitCore/nexus-gateway/packages/`
 *        line in its go.sum (workspace + replace means sums for siblings are
 *        irrelevant noise, and stale entries here are how the pre-fix state
 *        let `go mod tidy` quietly regress the require version).
 *
 * Failure modes this catches
 *   - Someone ran `go mod tidy` outside the workspace and Go rewrote
 *     `require <sibling> v0.0.0` back to a real pseudo-version.
 *   - Someone added a new sibling-package import but forgot the replace.
 *   - Someone renamed/moved a sibling and the replace path went stale.
 *
 * Scope: every `packages/<svc>/go.mod` and `packages/<svc>/go.sum`. Modules
 * outside `packages/` (e.g. `tests/integration-go/`) are intentionally
 * skipped — they are standalone modules not part of the workspace.
 *
 * Exit: 0 = clean; 1 = at least one violation.
 *
 * Wired into: package.json `check:workspace-replace`, `check:all`, and the
 * `.githooks/pre-commit` hook (HARD guard #12, scoped to staged go.mod /
 * go.sum changes).
 */

import { execSync } from 'node:child_process';
import { readFileSync, existsSync } from 'node:fs';
import { dirname, basename, resolve } from 'node:path';

const REPO_ROOT = execSync('git rev-parse --show-toplevel', { encoding: 'utf-8' }).trim();

const SIBLING_PATH_RE = /github\.com\/ai-nexus-platform\/nexus-gateway\/packages\/([a-z][a-z0-9-]*)/;
const REQUIRE_LINE_RE = /^\s*github\.com\/ai-nexus-platform\/nexus-gateway\/packages\/([a-z][a-z0-9-]*)\s+(\S+)/;
const REPLACE_LINE_RE = /^replace\s+github\.com\/ai-nexus-platform\/nexus-gateway\/packages\/([a-z][a-z0-9-]*)\s*=>\s*(\S+)/;

function listGoMods() {
  const out = execSync('git ls-files "packages/*/go.mod"', { encoding: 'utf-8', cwd: REPO_ROOT });
  return out
    .split('\n')
    .map((s) => s.trim())
    .filter(Boolean);
}

function ownModulePath(modFile) {
  const text = readFileSync(resolve(REPO_ROOT, modFile), 'utf-8');
  const m = text.match(/^module\s+(\S+)/m);
  return m ? m[1] : null;
}

function checkOne(modFile) {
  const violations = [];
  const text = readFileSync(resolve(REPO_ROOT, modFile), 'utf-8');
  const lines = text.split('\n');
  const ownModule = ownModulePath(modFile);

  // Collect sibling requires (excluding self-references — a sibling-shaped
  // path that IS this module's own path is normal).
  const requires = []; // { name, version, lineNo }
  const replaces = new Map(); // name -> { target, lineNo }

  lines.forEach((line, idx) => {
    const lineNo = idx + 1;

    const reqMatch = line.match(REQUIRE_LINE_RE);
    if (reqMatch) {
      const fullPath = `github.com/AlphaBitCore/nexus-gateway/packages/${reqMatch[1]}`;
      if (fullPath !== ownModule) {
        requires.push({ name: reqMatch[1], version: reqMatch[2], lineNo });
      }
    }

    const repMatch = line.match(REPLACE_LINE_RE);
    if (repMatch) {
      replaces.set(repMatch[1], { target: repMatch[2], lineNo });
    }
  });

  // Rule 1: every sibling require must be at exactly v0.0.0.
  for (const r of requires) {
    if (r.version !== 'v0.0.0') {
      violations.push({
        file: modFile,
        line: r.lineNo,
        msg:
          `require for sibling \`packages/${r.name}\` is at \`${r.version}\` — must be exactly \`v0.0.0\`. ` +
          `Real pseudo-versions trigger Go 1.25 to re-validate against GitHub and silently pull stale snapshots. ` +
          `Fix: change to \`v0.0.0\` and ensure the matching \`replace => ../${r.name}\` directive is present.`,
      });
    }
  }

  // Rule 2: every sibling require must have a matching replace pointing at
  // the expected relative path.
  for (const r of requires) {
    const replace = replaces.get(r.name);
    if (!replace) {
      violations.push({
        file: modFile,
        line: r.lineNo,
        msg:
          `require for sibling \`packages/${r.name}\` has no matching \`replace\` directive. ` +
          `Add: \`replace github.com/AlphaBitCore/nexus-gateway/packages/${r.name} => ../${r.name}\`.`,
      });
      continue;
    }
    const expectedTarget = `../${r.name}`;
    if (replace.target !== expectedTarget) {
      violations.push({
        file: modFile,
        line: replace.lineNo,
        msg:
          `replace for sibling \`packages/${r.name}\` points at \`${replace.target}\` — must be \`${expectedTarget}\`.`,
      });
    }
  }

  // Rule 3: go.sum (next to go.mod) must not have any sibling-package lines.
  const sumFile = modFile.replace(/go\.mod$/, 'go.sum');
  const sumAbs = resolve(REPO_ROOT, sumFile);
  if (existsSync(sumAbs)) {
    const sumText = readFileSync(sumAbs, 'utf-8');
    sumText.split('\n').forEach((line, idx) => {
      if (line.startsWith('github.com/AlphaBitCore/nexus-gateway/packages/')) {
        violations.push({
          file: sumFile,
          line: idx + 1,
          msg:
            `stale sibling-package hash line in go.sum. ` +
            `Sibling resolution is workspace+replace; no sum verification is needed. ` +
            `Delete the line.`,
        });
      }
    });
  }

  return violations;
}

const allViolations = [];
for (const modFile of listGoMods()) {
  allViolations.push(...checkOne(modFile));
}

if (allViolations.length === 0) {
  console.log('[check:workspace-replace] ✓ all packages/*/go.mod satisfy the sibling-replace contract');
  process.exit(0);
}

console.error('[check:workspace-replace] FAILED — sibling-replace contract violated:\n');
for (const v of allViolations) {
  console.error(`  ${v.file}:${v.line}`);
  console.error(`    ${v.msg}\n`);
}
console.error(
  'See CLAUDE.md → "replace directives in go.mod" for the binding and ' +
    'scripts/check-workspace-replace.mjs for the contract rules.',
);
process.exit(1);
