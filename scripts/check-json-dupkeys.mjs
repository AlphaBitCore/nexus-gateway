#!/usr/bin/env node
/**
 * Duplicate JSON key guard (M1).
 *
 * Walks every .json file under the control-plane-ui locale directories and
 * flags any object literal that contains the same key twice. `JSON.parse`
 * silently picks the last duplicate — and, critically, does so BEFORE invoking
 * any reviver — so a reviver-based detector is a no-op. We therefore tokenize
 * the raw text with a minimal state machine that tracks OBJECT / ARRAY frames
 * and records each key the moment it's parsed, reporting duplicates with
 * line:column so CI logs are actionable.
 *
 * Usage:
 *   node scripts/check-json-dupkeys.mjs              # scan locale files
 *   node scripts/check-json-dupkeys.mjs --selftest   # verify detector on fixtures
 *
 * Exit codes:
 *   0 - no duplicates / selftest passed
 *   1 - at least one duplicate or selftest failure
 */

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, dirname, extname, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = join(__dirname, '..');
const ROOTS = [
  'packages/control-plane-ui/src/i18n/locales',
  'packages/control-plane-ui/public/locales',
];

function walk(dir) {
  const out = [];
  for (const entry of readdirSync(dir)) {
    const p = join(dir, entry);
    let s;
    try {
      s = statSync(p);
    } catch {
      continue; // broken symlink or transient; skip
    }
    if (s.isDirectory()) out.push(...walk(p));
    else if (extname(p) === '.json') out.push(p);
  }
  return out;
}

/**
 * Scan a JSON document and return an array of { line, column, key } for every
 * duplicate key within an object literal. Throws on syntax errors the scanner
 * can't recover from (caller converts to a "parse fail" report).
 */
function scanDuplicates(text) {
  const issues = [];
  const frames = []; // {type: 'object'|'array', keys?: Set<string>}
  let i = 0;
  let line = 1;
  let col = 1;
  const len = text.length;

  function advance() {
    const ch = text[i];
    i++;
    if (ch === '\n') {
      line++;
      col = 1;
    } else {
      col++;
    }
    return ch;
  }

  function skipWhitespace() {
    while (i < len) {
      const ch = text[i];
      if (ch === ' ' || ch === '\t' || ch === '\r' || ch === '\n') {
        advance();
        continue;
      }
      return;
    }
  }

  function readString() {
    // Called with text[i] === '"'. Returns [startLine, startCol, value].
    const startLine = line;
    const startCol = col;
    advance(); // consume opening quote
    let buf = '';
    while (i < len) {
      const ch = advance();
      if (ch === '"') return [startLine, startCol, buf];
      if (ch === '\\') {
        if (i >= len) break;
        const esc = advance();
        switch (esc) {
          case '"': buf += '"'; break;
          case '\\': buf += '\\'; break;
          case '/': buf += '/'; break;
          case 'b': buf += '\b'; break;
          case 'f': buf += '\f'; break;
          case 'n': buf += '\n'; break;
          case 'r': buf += '\r'; break;
          case 't': buf += '\t'; break;
          case 'u': {
            let hex = '';
            for (let k = 0; k < 4 && i < len; k++) hex += advance();
            buf += String.fromCharCode(parseInt(hex, 16));
            break;
          }
          default: buf += esc;
        }
      } else {
        buf += ch;
      }
    }
    throw new SyntaxError(`unterminated string at ${startLine}:${startCol}`);
  }

  function skipValue() {
    skipWhitespace();
    if (i >= len) return;
    const ch = text[i];
    if (ch === '{' || ch === '[') {
      // Nested container — main loop will handle it; we just return so the
      // caller falls back to the top-level walk.
      return;
    }
    if (ch === '"') {
      readString();
      return;
    }
    // Primitive: number / true / false / null. Consume until a delimiter.
    while (i < len) {
      const c = text[i];
      if (c === ',' || c === '}' || c === ']' ||
          c === ' ' || c === '\t' || c === '\r' || c === '\n') return;
      advance();
    }
  }

  while (i < len) {
    skipWhitespace();
    if (i >= len) break;
    const ch = text[i];

    if (ch === '{') {
      advance();
      frames.push({ type: 'object', keys: new Set() });
      continue;
    }
    if (ch === '[') {
      advance();
      frames.push({ type: 'array' });
      continue;
    }
    if (ch === '}' || ch === ']') {
      advance();
      frames.pop();
      continue;
    }
    if (ch === ',' || ch === ':') {
      advance();
      continue;
    }

    const top = frames[frames.length - 1];
    if (ch === '"' && top && top.type === 'object') {
      // Need to decide: key or value? A key is a string followed by ':' after
      // optional whitespace. Inside an object, after '{' or ',' we expect a
      // key. Rather than track that state explicitly, peek past the string
      // for ':'.
      const savedI = i, savedLine = line, savedCol = col;
      const [keyLine, keyCol, key] = readString();
      const afterStrI = i, afterStrLine = line, afterStrCol = col;
      skipWhitespace();
      if (text[i] === ':') {
        if (top.keys.has(key)) {
          issues.push({ line: keyLine, column: keyCol, key });
        } else {
          top.keys.add(key);
        }
        // leave ':' for main-loop to consume
        continue;
      }
      // Not a key — it was a string value. Restore position is unnecessary
      // because readString already consumed it; just continue the outer loop.
      // Reset whitespace pointer to where we were after the string so the
      // outer loop sees the next delimiter (`,` / `}` / `]`).
      void savedI; void savedLine; void savedCol;
      void afterStrI; void afterStrLine; void afterStrCol;
      continue;
    }
    if (ch === '"') {
      readString(); // string value at the top level or inside an array
      continue;
    }

    // Primitive at top / inside array.
    skipValue();
  }

  if (frames.length !== 0) {
    throw new SyntaxError('unexpected end of input: unclosed container');
  }
  return issues;
}

function runSelfTest() {
  const cases = [
    { name: 'simple dup', text: '{"a":1,"a":2}', expectDups: ['a'] },
    { name: 'no dup', text: '{"a":1,"b":2}', expectDups: [] },
    { name: 'nested dup', text: '{"outer":{"k":1,"k":2}}', expectDups: ['k'] },
    { name: 'dup with same value-key string NOT counted', text: '{"a":"a","b":"a"}', expectDups: [] },
    { name: 'array of strings', text: '{"x":["a","a","a"]}', expectDups: [] },
    { name: 'dup key inside array of objects', text: '{"xs":[{"k":1,"k":2}]}', expectDups: ['k'] },
    { name: 'escaped quote in key', text: '{"a\\"b":1,"a\\"b":2}', expectDups: ['a"b'] },
    { name: 'dup across whitespace and newlines', text: '{\n  "k": 1,\n  "k": 2\n}', expectDups: ['k'] },
    { name: 'sibling objects do not cross-contaminate', text: '{"a":{"k":1},"b":{"k":1}}', expectDups: [] },
    { name: 'triple dup', text: '{"a":1,"a":2,"a":3}', expectDups: ['a', 'a'] }, // two violations on the 2nd and 3rd occurrences
  ];
  let failed = 0;
  for (const c of cases) {
    let actual;
    try {
      actual = scanDuplicates(c.text).map((d) => d.key);
    } catch (err) {
      console.error(`selftest FAIL [${c.name}]: threw ${err.message}`);
      failed++;
      continue;
    }
    const expected = c.expectDups;
    if (actual.length !== expected.length || actual.some((k, idx) => k !== expected[idx])) {
      console.error(`selftest FAIL [${c.name}]: expected ${JSON.stringify(expected)}, got ${JSON.stringify(actual)}`);
      failed++;
    }
  }
  if (failed > 0) {
    console.error(`\nselftest: ${failed} case(s) failed`);
    process.exit(1);
  }
  console.log(`selftest: ${cases.length} case(s) passed`);
}

if (process.argv.includes('--selftest')) {
  runSelfTest();
} else {
  let total = 0;
  for (const root of ROOTS) {
    const absRoot = join(REPO_ROOT, root);
    for (const f of walk(absRoot)) {
      const rel = relative(REPO_ROOT, f);
      const txt = readFileSync(f, 'utf8');
      try {
        for (const d of scanDuplicates(txt)) {
          console.error(`dup key: ${rel}:${d.line}:${d.column}: "${d.key}"`);
          total++;
        }
      } catch (err) {
        console.error(`parse fail: ${rel}: ${err.message}`);
        total++;
      }
    }
  }
  if (total > 0) {
    console.error(`\n${total} issue(s) — duplicate or invalid JSON in locale files`);
    process.exit(1);
  }
  console.log('json duplicate key check: clean');
}
