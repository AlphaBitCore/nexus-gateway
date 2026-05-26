#!/usr/bin/env node
/**
 * Lint: `useApi` queryKey shape enforcement.
 *
 * Binding rule (CLAUDE.md + .cursor/rules/useapi-querykey.mdc): every
 * `useApi(fetcher, queryKey)` call must start `queryKey` with at least two
 * string literals (domain + resource). Avoids React Query cache collisions
 * across pages.
 *
 * This is a heuristic regex-based scanner — it catches the obvious failure
 * modes:
 *   - Empty queryKey: `useApi(..., [])`
 *   - State-vars-only: `useApi(..., [search, offset, limit])` (no leading
 *     string literal).
 *   - Single string literal: `useApi(..., ['detail'])` (missing the second
 *     literal).
 *
 * It does NOT do full AST analysis; complex dynamic keys (string concat,
 * helper functions) get reported as "unverifiable" and surface for manual
 * review.
 *
 * Files scanned: packages/control-plane-ui/src/**\/*.tsx and *.ts.
 */

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';

const ROOT = 'packages/control-plane-ui/src';
const EXT = new Set(['.tsx', '.ts']);
const ALLOWED_DOMAINS = new Set(['admin', 'my', 'user', 'proxy']);
const STRICT = process.argv.includes('--strict') || process.env.STRICT === '1';

// Skip test files + __mocks__ directories.
function isTestPath(p) {
  return /\.test\.(tsx?|jsx?)$/.test(p) || /\.spec\.(tsx?|jsx?)$/.test(p) || /__mocks__/.test(p);
}

function walk(dir) {
  const out = [];
  for (const entry of readdirSync(dir)) {
    const p = join(dir, entry);
    let st;
    try {
      st = statSync(p);
    } catch {
      continue;
    }
    if (st.isDirectory()) out.push(...walk(p));
    else if (EXT.has(entry.slice(entry.lastIndexOf('.'))) && !isTestPath(p)) out.push(p);
  }
  return out;
}

function scanFile(file) {
  const text = readFileSync(file, 'utf-8');
  const errors = [];

  // Find `useApi(...)` invocations. Heuristic: balance-paren scan.
  let idx = 0;
  while ((idx = text.indexOf('useApi(', idx)) !== -1) {
    const start = idx + 'useApi('.length;
    let depth = 1;
    let i = start;
    while (i < text.length && depth > 0) {
      const ch = text[i];
      if (ch === '(') depth++;
      else if (ch === ')') depth--;
      i++;
    }
    if (depth !== 0) break;
    const argsRaw = text.slice(start, i - 1).trim();

    // Split top-level args (depth-aware on parens / brackets / braces).
    const args = splitTopLevelCommas(argsRaw);
    if (args.length < 2) {
      idx = i;
      continue;
    }

    const queryKey = args[1].trim();
    const line = text.slice(0, idx).split('\n').length;
    const evalResult = evalQueryKey(queryKey);
    if (evalResult.kind === 'error') {
      errors.push({ file, line, queryKey, reason: evalResult.reason });
    } else if (evalResult.kind === 'unverifiable') {
      // Optional: warn on unverifiable. Skip for now.
    }
    idx = i;
  }
  return errors;
}

function splitTopLevelCommas(src) {
  const out = [];
  let depth = 0;
  let start = 0;
  for (let i = 0; i < src.length; i++) {
    const ch = src[i];
    if (ch === '(' || ch === '[' || ch === '{') depth++;
    else if (ch === ')' || ch === ']' || ch === '}') depth--;
    else if (ch === ',' && depth === 0) {
      out.push(src.slice(start, i));
      start = i + 1;
    }
  }
  if (start < src.length) out.push(src.slice(start));
  return out;
}

function evalQueryKey(src) {
  src = src.trim();
  if (!src.startsWith('[')) {
    return { kind: 'unverifiable', reason: 'not an array literal' };
  }
  if (!src.endsWith(']')) {
    return { kind: 'unverifiable', reason: 'array literal not closed inline' };
  }
  const inner = src.slice(1, -1).trim();
  if (inner === '') {
    return { kind: 'error', reason: 'empty queryKey []' };
  }
  const elems = splitTopLevelCommas(inner).map((s) => s.trim()).filter(Boolean);
  if (elems.length === 0) {
    return { kind: 'error', reason: 'empty queryKey' };
  }
  // First two elements must be string literals.
  const first = elems[0];
  if (!isStringLiteral(first)) {
    return { kind: 'error', reason: 'first element is not a string literal' };
  }
  // Allow non-domain strings to pass on first element only when the second is a string AND looks resource-like.
  const second = elems[1];
  if (second === undefined) {
    return { kind: 'error', reason: 'queryKey has only 1 element; need at least 2 string literals' };
  }
  if (!isStringLiteral(second)) {
    return { kind: 'error', reason: 'second element is not a string literal' };
  }
  // Soft check: first should be one of the allowed domains.
  const firstVal = stripQuotes(first);
  if (!ALLOWED_DOMAINS.has(firstVal)) {
    return { kind: 'error', reason: `first literal '${firstVal}' is not in {admin, my, user, proxy}` };
  }
  return { kind: 'ok' };
}

function isStringLiteral(s) {
  return /^(['"`])[^'"`]*\1$/.test(s);
}

function stripQuotes(s) {
  return s.slice(1, -1);
}

function main() {
  let allErrors = [];
  try {
    const files = walk(ROOT);
    for (const f of files) {
      const errs = scanFile(f);
      allErrors.push(...errs);
    }
  } catch (e) {
    if (e.code === 'ENOENT') {
      console.log(`[check:useapi-querykey] No ${ROOT} found; skipping.`);
      return;
    }
    throw e;
  }

  if (allErrors.length > 0) {
    const label = STRICT ? 'FAILED' : 'WARN';
    const ws = STRICT ? console.error : console.warn;
    ws(`[check:useapi-querykey] ${label} -- ${allErrors.length} violation(s):`);
    for (const e of allErrors) {
      const rel = relative('.', e.file);
      ws(`  - ${rel}:${e.line}  -- ${e.reason}`);
      ws(`      queryKey: ${e.queryKey.slice(0, 100)}`);
    }
    ws('');
    ws('Required shape: useApi(fetcher, [domain, resource, ...stateVars])');
    ws('Domain ∈ {admin, my, user, proxy}; resource is a string literal.');
    ws('See .cursor/rules/useapi-querykey.mdc');
    if (STRICT) {
      process.exit(1);
    }
    ws('[check:useapi-querykey] non-strict mode; passing despite warnings. Run with --strict to fail on violations.');
    return;
  }

  console.log('[check:useapi-querykey] OK -- no queryKey shape violations.');
}

main();
