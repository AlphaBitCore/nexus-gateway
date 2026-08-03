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
import { dirname, basename, resolve, relative } from 'node:path';

const REPO_ROOT = execSync('git rev-parse --show-toplevel', { encoding: 'utf-8' }).trim();

const SIBLING_PATH_RE = /github\.com\/AlphaBitCore\/nexus-gateway\/packages\/([a-z][a-z0-9-]*)/;
const REQUIRE_LINE_RE = /^\s*github\.com\/AlphaBitCore\/nexus-gateway\/packages\/([a-z][a-z0-9-]*)\s+(\S+)/;
const REPLACE_LINE_RE = /^replace\s+github\.com\/AlphaBitCore\/nexus-gateway\/packages\/([a-z][a-z0-9-]*)\s*=>\s*(\S+)/;

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

// checkModText runs the require/replace rules (Rules 1-2) against go.mod text
// directly, without touching the filesystem, so the self-test fixture can drive
// the exact regexes that silently rotted in F-0317. `ownModule` is passed in so
// the self-test does not need a real `module` line resolved from disk.
// expectedReplaceTarget computes the relative path a go.mod's replace directive
// must point at, from the requiring module's directory to the sibling's
// directory. Both live under packages/ and are named by their module-path last
// segment, so the sibling dir is `packages/<name>`. A FLAT sibling
// (packages/ai-gateway -> packages/shared) yields `../shared`; a NESTED module
// (packages/agent/ui -> packages/agent) yields `..` — the reason a hardcoded
// `../<name>` is wrong for nested modules and breaks their workspace-OFF build.
// Pure string math (no filesystem) so the self-test fixtures drive it directly.
function expectedReplaceTarget(modFile, name) {
  const rel = relative(dirname(modFile), `packages/${name}`).split('\\').join('/');
  return rel === '' ? '.' : rel;
}

function checkModText(modFile, text, ownModule) {
  const violations = [];
  const lines = text.split('\n');

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
          `Fix: change to \`v0.0.0\` and ensure the matching \`replace => ${expectedReplaceTarget(modFile, r.name)}\` directive is present.`,
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
          `Add: \`replace github.com/AlphaBitCore/nexus-gateway/packages/${r.name} => ${expectedReplaceTarget(modFile, r.name)}\`.`,
      });
      continue;
    }
    const expectedTarget = expectedReplaceTarget(modFile, r.name);
    if (replace.target !== expectedTarget) {
      violations.push({
        file: modFile,
        line: replace.lineNo,
        msg:
          `replace for sibling \`packages/${r.name}\` points at \`${replace.target}\` — must be \`${expectedTarget}\`.`,
      });
    }
  }

  return violations;
}

function checkOne(modFile) {
  const text = readFileSync(resolve(REPO_ROOT, modFile), 'utf-8');
  const violations = checkModText(modFile, text, ownModulePath(modFile));

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

// --selftest drives the require/replace regexes against in-memory fixtures so a
// future regex drift (the exact F-0317 failure: the patterns hardcoded the wrong
// org and silently matched nothing) can never ship green. A known-bad go.mod MUST
// produce violations and a known-good one MUST produce none; if the regexes stop
// matching the real module path, the bad fixture yields zero violations and this
// self-test fails loudly. Run by CI alongside the live scan.
if (process.argv.includes('--selftest')) {
  const ownModule = 'github.com/AlphaBitCore/nexus-gateway/packages/example';
  const badMod = [
    'module github.com/AlphaBitCore/nexus-gateway/packages/example',
    '',
    'require (',
    // sibling at a real pseudo-version (Rule 1 violation) AND with no replace (Rule 2):
    '\tgithub.com/AlphaBitCore/nexus-gateway/packages/shared v0.0.0-20260101000000-abcdef123456',
    ')',
  ].join('\n');
  const goodMod = [
    'module github.com/AlphaBitCore/nexus-gateway/packages/example',
    '',
    'require github.com/AlphaBitCore/nexus-gateway/packages/shared v0.0.0',
    '',
    'replace github.com/AlphaBitCore/nexus-gateway/packages/shared => ../shared',
  ].join('\n');

  // A NESTED module (packages/agent/ui depends on the parent packages/agent):
  // the correct replace target is `..`, NOT `../agent`. The good fixture uses
  // `..` and must pass; the bad fixture uses the flat-convention `../agent` and
  // must be rejected (that path resolves to packages/agent/agent and breaks the
  // workspace-OFF build).
  // Block-form require (the indented `\tgithub...` inside `require ( … )`) — the
  // real agent/ui/go.mod form and the only shape REQUIRE_LINE_RE matches.
  const nestedOwn = 'github.com/AlphaBitCore/nexus-gateway/packages/agent/ui';
  const nestedGoodMod = [
    'module github.com/AlphaBitCore/nexus-gateway/packages/agent/ui',
    '',
    'require (',
    '\tgithub.com/AlphaBitCore/nexus-gateway/packages/agent v0.0.0',
    ')',
    '',
    'replace github.com/AlphaBitCore/nexus-gateway/packages/agent => ..',
  ].join('\n');
  const nestedBadMod = nestedGoodMod.replace('=> ..', '=> ../agent');
  const nestedGoodViolations = checkModText('packages/agent/ui/go.mod', nestedGoodMod, nestedOwn);
  const nestedBadViolations = checkModText('packages/agent/ui/go.mod', nestedBadMod, nestedOwn);

  const badViolations = checkModText('packages/example/go.mod', badMod, ownModule);
  const goodViolations = checkModText('packages/example/go.mod', goodMod, ownModule);
  const failures = [];
  if (nestedGoodViolations.length !== 0) {
    failures.push(
      `self-test FAILED: the nested-module good fixture (replace => ..) produced ` +
        `${nestedGoodViolations.length} violation(s); expected 0 — the expected-path math must ` +
        `compute the real relative path for nested modules, not a hardcoded ../<name>.`,
    );
  }
  if (nestedBadViolations.length === 0) {
    failures.push(
      'self-test FAILED: the nested-module bad fixture (replace => ../agent) produced ZERO ' +
        'violations — the flat-convention path that breaks the nested workspace-OFF build was accepted.',
    );
  }
  if (badViolations.length === 0) {
    failures.push(
      'self-test FAILED: the known-bad fixture produced ZERO violations — the require/replace ' +
        'regexes match nothing (the F-0317 silent-no-op regression). Check SIBLING/REQUIRE/REPLACE_LINE_RE.',
    );
  }
  if (goodViolations.length !== 0) {
    failures.push(
      `self-test FAILED: the known-good fixture produced ${goodViolations.length} violation(s); expected 0.`,
    );
  }
  if (failures.length > 0) {
    for (const f of failures) console.error(`[check:workspace-replace] ${f}`);
    process.exit(1);
  }
  console.log('[check:workspace-replace] ✓ self-test passed (bad fixture rejected, good fixture accepted)');
  process.exit(0);
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
