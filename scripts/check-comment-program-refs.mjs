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
 * The forbidden families are the PATTERNS table below, each carrying its own
 * example. They live there rather than in a prose list here because this file
 * is scanned like any other: a catalogue of the shapes being banned, written
 * as a comment, is nine violations of its own rule. As data they are string
 * literals, which are deliberately out of scope.
 *
 * Deliberately NOT flagged: bare `#123` ordinals (list items, external-project
 * issues like `litellm #24339`), "Phase 1/2" (real in-code init-step labels),
 * URLs, RFC numbers.
 *
 * There is no allowlist file, and that is the design: the one thing that ever
 * looked like an exception here — this header's own catalogue of the shapes it
 * bans — was the same list written twice, and it now rides the PATTERNS table
 * as data. A genuine exception should change a pattern, not accumulate beside
 * it. (`loadAllowlist` still reads the path if someone creates it, so the
 * escape hatch exists; it is empty on purpose.)
 *
 * Usage:
 *   scripts/check-comment-program-refs.mjs            # warn (non-strict)
 *   scripts/check-comment-program-refs.mjs --strict   # exit 1 on hits (CI)
 *   scripts/check-comment-program-refs.mjs --staged   # only staged files (pre-commit)
 */

import { execSync } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';

const PATTERNS = [
  { re: /\bPR #\d+/, label: 'pr-ref', eg: 'PR #' + '123' },
  { re: /\bE\d{2,}(?:-S\d+)?\b/, label: 'epic-ref', eg: 'E' + '60, E' + '91-S3' },
  { re: /\bSEC-[A-Z]\d+(?:-\d+)?\b/, label: 'sec-finding-code', eg: 'SEC-' + 'W2-03' },
  { re: /\bF-\d{3,4}\b/, label: 'finding-code', eg: 'F-' + '0198' },
  { re: /\bcommit [0-9a-f]{7,40}\b/, label: 'commit-sha', eg: 'commit ' + 'a1b2c3d' },
  { re: /\b(?:bug|issue) #\d+/i, label: 'bug-issue-ref', eg: 'bug #' + '45' },
  // The fractional part is optional: a plan numbers its tasks 0.3 or 16 with
  // equal ease, and a pattern that demands the dot reaches neither.
  { re: /\bTask \d+(?:\.\d+)?\b/, label: 'task-ref', eg: 'Task ' + '0.3, Task ' + '16' },
  // Comment-initial only: a review label opens the sentence. An algorithm
  // that genuinely has rounds says so mid-sentence, and the allowlist takes
  // the rest.
  { re: /^\s*Round \d+[.,]/, label: 'review-round', eg: 'Round ' + '3, HIGH.' },
  // The letter is the review's own axis, not a fixed one: this tree alone
  // carries A-4, C-30, L-7 and S-4. Pinning a single letter is how 56 of these
  // sat green in a gate whose stated purpose is to forbid them.
  { re: /\bfindings? [A-Z]-\d+/i, label: 'review-finding-code', eg: 'findings ' + 'C-18' },
  // A bare `#123` stays unflagged — list ordinals and external-project issues
  // (`litellm #24339`) both look like that. These two shapes do not: a finding
  // number carried as a noun phrase, and one qualifying a fix.
  { re: /\b(?:the|pre-|post-|pins|audit)\s?#\d+/i, label: 'finding-number', eg: 'the #' + '13, pre-#' + '88' },
  { re: /#\d+[a-z]?\s+(?:fix|finding|leak|flag|gate|invariant|regression|policy|defen[cs]e)\b/i,
    label: 'finding-number', eg: '#' + '13 fix' },
];

const STRICT = process.argv.includes('--strict') || process.env.STRICT === '1';
const STAGED = process.argv.includes('--staged');
// .mjs/.js were in the file listing from the start and dropped here, so no
// JavaScript was ever scanned — including every gate under scripts/, which
// is where the patterns being banned are written down and quoted.
const EXT_RE = /\.(go|ts|tsx|mjs|cjs|js|jsx)$/;
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
    // The same trees --staged already reaches. Globbing packages/** alone let
    // the same reference pass the sweep and fail a commit, depending on which
    // tree it lived in.
    : 'git ls-files "packages/**/*.go" "packages/**/*.ts" "packages/**/*.tsx" ' +
      '"tests/**/*.go" "tests/**/*.ts" "tools/**/*.ts" "tools/**/*.mjs" ' +
      // NOT scripts/**/*.mjs: git's `**` needs a directory to match, so that
      // pathspec listed ZERO files and this gate had never read its own
      // directory — including its own documentation of the patterns it bans.
      '"scripts/*.mjs" ' +
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

  // A clean run and a run that reached nothing print the same OK, so the sweep
  // asserts its reach rather than reporting it. ~5500 files today; a glob that
  // stops matching one of the trees drops it by hundreds at a time.
  //
  // Full sweep only. --staged is scoped to a commit, where a handful of files
  // is the normal case and a floor would block every small commit.
  const MIN_FILES = 3000;
  if (!STAGED && files.length < MIN_FILES) {
    console.error(
      `[check:comment-program-refs] FAILED -- only ${files.length} file(s) reached, ` +
        `expected at least ${MIN_FILES}. The sweep is not seeing the tree, so a clean ` +
        `result here would mean nothing.`,
    );
    process.exitCode = 1;
    return;
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
