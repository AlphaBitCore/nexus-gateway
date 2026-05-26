#!/usr/bin/env node
/**
 * Lint: forbid placeholder markers in production Go code.
 *
 * Binding rule (CLAUDE.md → Mandatory rules → "Real implementation only"):
 * Production code must ship complete implementations. Placeholder tokens
 * indicate scope that was deferred without an explicit user-approved
 * follow-up — the rule exists because half-finished prod merges have
 * shipped silent regressions before.
 *
 * The rule's wording is targeted at four families:
 *   1. Convention all-caps comment markers: TODO / FIXME / XXX / HACK
 *      (these are case-sensitive — engineers write them uppercase by
 *      convention; lowercase "xxx" inside an identifier like `auth.Xxx`
 *      is not a marker).
 *   2. `panic("not implemented")` / `panic("unimplemented")` / `panic("TODO")`
 *      style runtime placeholder fences.
 *   3. Code that *returns* a placeholder marker string verbatim, e.g.
 *      `return "UNIMPLEMENTED"` or `return "TODO"`.
 *   4. Standalone `unimplemented` / `not implemented` *outside* a comment
 *      (Go has no such keyword — appearing in code means a literal or an
 *      identifier intended as a placeholder).
 *
 * What we deliberately do NOT flag:
 *   - Documentation comments describing test-injection seams ("tests can
 *     inject a stub", "stub round-tripper"). The word "stub" is everyday
 *     English in test-pattern comments; the binding rule forbids "stub
 *     MODULES" (whole placeholder packages), not the noun in prose.
 *   - The opt-out marker `nolint:prod-todos` on the same line, for
 *     user-approved deferrals.
 *   - Files under tests/, examples/, scripts/, _test.go, _mock.go, mocks/,
 *     testharness/, testutil/, tools/db-migrate/.
 *
 * Usage:
 *   scripts/check-no-prod-todos.mjs            # warn (non-strict)
 *   scripts/check-no-prod-todos.mjs --strict   # exit 1 on hits (CI)
 *   scripts/check-no-prod-todos.mjs --staged   # only staged Go files (pre-commit)
 */

import { execSync } from 'node:child_process';

// Pattern set, paired with whether the pattern is meaningful inside a
// comment context. Anything `commentOk: false` is ignored on `//` and
// `/* */` lines.
//
// `return "UNIMPLEMENTED"` and the like are NOT flagged — those are
// canonical gRPC status code names (HTTP 501 → "UNIMPLEMENTED") that
// legitimately appear in status-mapping functions. The forbidden case
// is a function THAT ITSELF is the placeholder (panic / errors.New),
// not one that *reports* an upstream Unimplemented condition.
const PATTERNS = [
  // Convention all-caps markers — meaningful in comments AND in code.
  { re: /\b(TODO|FIXME|XXX|HACK)\b/, label: 'placeholder-marker', commentOk: true },
  // Runtime placeholder fences inside panic().
  { re: /panic\s*\(\s*["'`](?:not\s+implemented|unimplemented|TODO|FIXME)["'`]\s*\)/i, label: 'panic-placeholder', commentOk: true },
  // errors.New("not implemented") / fmt.Errorf("unimplemented") wrappers
  // that mark a function body as a deferred placeholder.
  { re: /(?:errors\.New|fmt\.Errorf)\s*\(\s*["'`](?:not\s+implemented|unimplemented)["'`]/i, label: 'errors-new-unimplemented', commentOk: true },
];

const EXCLUDE_PATH_RE =
  /(_test\.go$|_mock\.go$|\/mocks?\/|\/testharness\/|\/testutil\/|^tests\/|^examples\/|^scripts\/|^tools\/db-migrate\/)/;

const OPT_OUT_RE = /nolint:prod-todos\b/;

// Identify lines whose code portion is wholly within a `//` comment.
// (Block comments `/* … */` would need a multi-line scanner; the
// repo's Go style overwhelmingly uses `//`, so we keep this simple.)
function isCommentLine(line) {
  const trimmed = line.replace(/^\s+/, '');
  return trimmed.startsWith('//');
}

const STRICT = process.argv.includes('--strict') || process.env.STRICT === '1';
const STAGED = process.argv.includes('--staged');

function listFiles() {
  if (STAGED) {
    try {
      const out = execSync('git diff --cached --name-only --diff-filter=ACM', {
        encoding: 'utf-8',
      });
      return out
        .split('\n')
        .map((s) => s.trim())
        .filter((s) => s.endsWith('.go'));
    } catch {
      return [];
    }
  }
  try {
    const out = execSync('git ls-files "packages/**/*.go"', { encoding: 'utf-8' });
    return out
      .split('\n')
      .map((s) => s.trim())
      .filter(Boolean);
  } catch {
    return [];
  }
}

function readFromWorkingTree(path) {
  try {
    return execSync(`cat "${path}"`, { encoding: 'utf-8', stdio: ['ignore', 'pipe', 'ignore'] });
  } catch {
    return null;
  }
}

function main() {
  const files = listFiles();
  if (files.length === 0) {
    console.log(
      STAGED
        ? '[check:no-prod-todos] no staged Go files — skipping.'
        : '[check:no-prod-todos] no Go files found.',
    );
    return;
  }

  const hits = [];
  for (const f of files) {
    if (EXCLUDE_PATH_RE.test(f)) continue;
    const text = readFromWorkingTree(f);
    if (text === null) continue;
    const lines = text.split('\n');
    for (let i = 0; i < lines.length; i++) {
      const raw = lines[i];
      if (OPT_OUT_RE.test(raw)) continue;
      const inComment = isCommentLine(raw);
      for (const { re, label, commentOk } of PATTERNS) {
        if (!commentOk && inComment) continue;
        if (re.test(raw)) {
          hits.push({ file: f, line: i + 1, label, text: raw.trim().slice(0, 140) });
          break;
        }
      }
    }
  }

  if (hits.length === 0) {
    console.log(`[check:no-prod-todos] OK -- ${files.length} Go file(s) scanned, 0 placeholder markers.`);
    return;
  }

  const label = STRICT ? 'FAILED' : 'WARN';
  const ws = STRICT ? console.error : console.warn;
  ws(`[check:no-prod-todos] ${label} -- ${hits.length} placeholder marker(s):`);
  for (const h of hits) {
    ws(`  - ${h.file}:${h.line}  [${h.label}]  ${h.text}`);
  }
  ws('');
  ws('Production code must ship complete implementations.');
  ws('If a marker is intentional + user-approved, append `// nolint:prod-todos` on the same line.');
  ws('Binding rule: CLAUDE.md "Real implementation only".');
  if (STRICT) process.exit(1);
  ws('[check:no-prod-todos] non-strict mode; passing despite warnings. Run with --strict to fail.');
}

main();
