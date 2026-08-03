#!/usr/bin/env node
/**
 * Lint: forbid development-program tracking references inside CODE COMMENTS.
 *
 * Binding rule (CLAUDE.md → "No archaeology in code comments"): a comment must
 * state the constraint self-contained. Pointers into the program that produced
 * the code — pull-request numbers, Epic / Story IDs, security-finding codes,
 * commit SHAs, bug/issue numbers, task IDs — rot the moment that program
 * closes and leak internal process into a public OSS tree. A reader needs the
 * WHY, not the ticket it shipped under.
 *
 * Scope: COMMENTS ONLY (line comments, trailing comments, and block comments).
 * Two families of file are read: C-style comments in packages slash go, ts, tsx,
 * and hash comments in the build and deployment surface — shell scripts,
 * Dockerfiles, Compose files and workflow YAML. That second family was the
 * blind spot: a story reference sat in the root Compose file for two months
 * while this gate reported clean, because it only ever looked at packages.
 * String literals are deliberately NOT scanned — a test assertion that
 * documents intent by echoing a label in its message string is code, not a
 * comment, and is out of scope.
 *
 * Forbidden families (each low-false-positive by construction):
 *   - `PR #123`                       pull-request reference
 *   - `E60`, `E91-S3`                 Epic / Epic-Story id (E + 2+ digits)
 *   - `SEC-W2-03`                     security-audit finding code
 *   - `F-0198`                        finding code
 *   - `commit a1b2c3d`               commit SHA cited in prose
 *   - `bug #45` / `issue #45`         tracker reference
 *   - `Task 0.3`                      plan task id
 *
 * Deliberately NOT flagged: bare `#123` ordinals (list items, external-project
 * issues like `litellm #24339`), "Phase 1/2" (real in-code init-step labels),
 * URLs, RFC numbers. Add genuine exceptions to scripts/.comment-ref-allowlist.
 *
 * Usage:
 *   scripts/check-comment-program-refs.mjs            # warn (non-strict)
 *   scripts/check-comment-program-refs.mjs --strict   # exit 1 on hits (CI)
 *   scripts/check-comment-program-refs.mjs --staged   # only staged files (pre-commit)
 */

import { execSync } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';

const PATTERNS = [
  { re: /\bPR #\d+/, label: 'pr-ref' },
  { re: /\bE\d{2,}(?:-S\d+)?\b/, label: 'epic-ref' },
  { re: /\bSEC-[A-Z]\d+(?:-\d+)?\b/, label: 'sec-finding-code' },
  { re: /\bF-\d{3,4}\b/, label: 'finding-code' },
  { re: /\bcommit [0-9a-f]{7,40}\b/, label: 'commit-sha' },
  { re: /\b(?:bug|issue) #\d+/i, label: 'bug-issue-ref' },
  { re: /\bTask \d+\.\d+\b/, label: 'task-ref' },
];

const STRICT = process.argv.includes('--strict') || process.env.STRICT === '1';
const STAGED = process.argv.includes('--staged');
const EXT_RE = /\.(go|ts|tsx)$/;
// Hash-comment files. Dockerfiles carry no extension, so they are matched by
// name; the YAML set is scoped to the surfaces that describe how this project
// is built and deployed rather than to every yaml in the tree.
const HASH_EXT_RE = /(\.sh$|(^|\/)Dockerfile(\.[\w-]+)?$|(^|\/)docker-compose[\w.-]*\.ya?ml$|^\.github\/workflows\/.*\.ya?ml$)/;
const EXCLUDE_PATH_RE = /(node_modules\/|\/dist\/|\.min\.(js|css)$|\.pb\.go$|_pb\.ts$)/;

const ALLOWLIST = loadAllowlist();

function loadAllowlist() {
  const path = 'scripts/.comment-ref-allowlist';
  if (!existsSync(path)) return [];
  return readFileSync(path, 'utf-8')
    .split('\n')
    .map((l) => l.trim())
    .filter((l) => l && !l.startsWith('#'));
}

// Extract the comment text of a file as a list of { line, text } entries.
// Tracks block-comment state across lines and string literals within a line so
// a line-comment marker inside a string (or a "://" URL inside a string) is not
// mistaken for a comment. Comment URLs are kept — the ref patterns ignore them.
// Hash-comment files (shell, Dockerfile, YAML). A `#` only opens a comment at
// the start of a line or after whitespace, and never inside a quoted string —
// otherwise a colour code or a URL fragment in a shell string reads as a
// comment. Heredoc bodies are not tracked: a `#` line inside one is prose the
// same rule should apply to.
function extractHashComments(src) {
  const out = [];
  const lines = src.split('\n');
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    let quote = null;
    for (let j = 0; j < line.length; j++) {
      const c = line[j];
      if (quote) {
        if (c === '\\') { j++; continue; }
        if (c === quote) quote = null;
        continue;
      }
      if (c === '"' || c === "'") { quote = c; continue; }
      if (c === '#' && (j === 0 || /\s/.test(line[j - 1]))) {
        const text = line.slice(j + 1);
        if (text.trim()) out.push({ line: i + 1, text });
        break;
      }
    }
  }
  return out;
}

function commentsFor(file, src) {
  return HASH_EXT_RE.test(file) ? extractHashComments(src) : extractComments(src);
}

function extractComments(src) {
  const out = [];
  const lines = src.split('\n');
  let inBlock = false;
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    let comment = '';
    let j = 0;
    if (inBlock) {
      const end = line.indexOf('*/');
      if (end === -1) {
        if (line.trim()) out.push({ line: i + 1, text: line });
        continue;
      }
      comment += line.slice(0, end) + ' ';
      j = end + 2;
      inBlock = false;
    }
    let quote = null;
    for (; j < line.length; j++) {
      const c = line[j];
      if (quote) {
        if (c === '\\') { j++; continue; }
        if (c === quote) quote = null;
        continue;
      }
      if (c === '"' || c === "'" || c === '`') { quote = c; continue; }
      if (c === '/' && line[j + 1] === '/') { comment += ' ' + line.slice(j + 2); break; }
      if (c === '/' && line[j + 1] === '*') {
        const end = line.indexOf('*/', j + 2);
        if (end === -1) { comment += ' ' + line.slice(j + 2); inBlock = true; break; }
        comment += ' ' + line.slice(j + 2, end);
        j = end + 1;
      }
    }
    if (comment.trim()) out.push({ line: i + 1, text: comment });
  }
  return out;
}

function isAllowlisted(file, text) {
  return ALLOWLIST.some((entry) => {
    const [pathPart, ...rest] = entry.split('::');
    if (rest.length === 0) return text.includes(pathPart);
    return file.includes(pathPart) && text.includes(rest.join('::'));
  });
}

function listFiles() {
  const cmd = STAGED
    ? 'git diff --cached --name-only --diff-filter=ACM'
    : 'git ls-files "packages/**/*.go" "packages/**/*.ts" "packages/**/*.tsx" ' +
      '"*.sh" "**/*.sh" "Dockerfile*" "**/Dockerfile*" ' +
      '"docker-compose*.yml" "**/docker-compose*.yml" ".github/workflows/*.yml"';
  // A git failure (e.g. .git/index.lock held by a parallel session) must fail
  // the gate — an empty list is indistinguishable from "nothing to check" and
  // would pass vacuously.
  let out;
  try {
    out = execSync(cmd, { encoding: 'utf-8' });
  } catch (err) {
    console.error(`check-comment-program-refs: git file listing failed (${err.message.trim()}); refusing to pass on an empty list.`);
    process.exit(2);
  }
  return out
    .split('\n')
    .map((s) => s.trim())
    .filter((s) => (EXT_RE.test(s) || HASH_EXT_RE.test(s)) && !EXCLUDE_PATH_RE.test(s));
}

function readFile(path) {
  // In --staged mode read the INDEX blob — the content that would actually be
  // committed — never the working tree, which may carry unstaged edits on a
  // partially staged file.
  try {
    if (STAGED) {
      return execSync(`git show :"${path}"`, {
        encoding: 'utf-8',
        stdio: ['ignore', 'pipe', 'ignore'],
        maxBuffer: 64 * 1024 * 1024,
      });
    }
    return readFileSync(path, 'utf-8');
  } catch {
    return null;
  }
}

function main() {
  const files = listFiles();
  if (files.length === 0) {
    console.log(
      STAGED
        ? '[check:comment-program-refs] no staged Go/TS files — skipping.'
        : '[check:comment-program-refs] no Go/TS files found.',
    );
    return;
  }

  const hits = [];
  for (const f of files) {
    const text = readFile(f);
    if (text === null) continue;
    for (const { line, text: comment } of commentsFor(f, text)) {
      if (isAllowlisted(f, comment)) continue;
      for (const { re, label } of PATTERNS) {
        const m = comment.match(re);
        if (m) {
          hits.push({ file: f, line, label, match: m[0], text: comment.trim().slice(0, 140) });
          break;
        }
      }
    }
  }

  if (hits.length === 0) {
    console.log(
      `[check:comment-program-refs] OK -- ${files.length} file(s) scanned, 0 program refs in comments.`,
    );
    return;
  }

  const tag = STRICT ? 'FAILED' : 'WARN';
  const ws = STRICT ? console.error : console.warn;
  ws(`[check:comment-program-refs] ${tag} -- ${hits.length} program reference(s) in comments:`);
  for (const h of hits) {
    ws(`  - ${h.file}:${h.line}  [${h.label}: ${h.match}]  ${h.text}`);
  }
  ws('');
  ws('Comments must state the constraint self-contained — no PR/Epic/finding/commit/task refs.');
  ws('State the WHY in present tense. For a genuine exception, add a line to');
  ws('scripts/.comment-ref-allowlist (format: `path::substring` or `substring`).');
  ws('Binding rule: CLAUDE.md "No archaeology in code comments".');
  if (STRICT) process.exit(1);
  ws('[check:comment-program-refs] non-strict mode; passing despite warnings. Run with --strict to fail.');
}

main();
