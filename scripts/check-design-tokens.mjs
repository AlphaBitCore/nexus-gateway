#!/usr/bin/env node
/**
 * Design-token compliance guard.
 *
 * Enforces the CSS framework rule: all visual values (color, spacing,
 * font-size, font-weight, border-radius, box-shadow, transition, z-index)
 * MUST be referenced as CSS variables — either Layer 1 (--g-*) raw tokens
 * or Layer 2 (--color-*, --sidebar-*, --shadow-*, --space-*, --radius-*,
 * --font-size-*, --transition-*, --z-*) semantic tokens. Tailwind v4 +
 * shadcn ship a third feed of raw tokens via `prime-shadcn-tokens.css`
 * (`@theme { … hex … }`), which the Layer 2 bridge re-exports — that file
 * is a definition file and is exempt.
 *
 * Forbidden:
 *   - hex / rgba / hsla / hsl literals in *.module.css
 *   - hex / rgba / hsla / hsl literals inside `style={{...}}` blocks in *.tsx
 *   - raw numeric values for padding/margin/gap/fontSize/fontWeight/
 *     borderRadius/boxShadow/transition/zIndex inside `style={{...}}` blocks
 *
 * Allowed escape hatches:
 *   - imports from chartColors.ts (Recharts requires JS color strings)
 *   - CSS variable bridges: style={{ '--foo': dynamic }}
 *   - runtime-computed dimensions where a template literal is required
 *     (e.g. paddingLeft: `${level * 20}px`)
 *   - theme definition files: styles/{global,light,dark,base,utilities,
 *     animations,prime-shadcn-tokens}.css
 *   - .stories.tsx and .test.tsx files
 *
 * Usage:
 *   node scripts/check-design-tokens.mjs                       # check all
 *   node scripts/check-design-tokens.mjs --json                # machine-readable
 *   node scripts/check-design-tokens.mjs --fix-suggest         # print mapping hints
 *   node scripts/check-design-tokens.mjs --files=a.tsx b.tsx   # scope to listed files
 *   echo "a.tsx\nb.tsx" | node scripts/check-design-tokens.mjs --files=-  # files from stdin
 *
 * The `--files` flag bypasses the SCAN_ROOTS walk and checks ONLY the
 * provided paths (filtered by extension + exempt rules). Used by the
 * pre-commit hook so a developer's commit isn't blocked by pre-existing
 * violations in a parallel session's unstaged work — the hook passes
 * `git diff --cached --name-only` (staged paths) so only what they're
 * actually committing is scanned. Paths may be relative to repo root
 * or absolute.
 *
 * Exit codes:
 *   0 — no violations
 *   1 — violations found
 */

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, dirname, basename, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = join(__dirname, '..');

const SCAN_ROOTS = [
  'packages/control-plane-ui/src',
  'packages/ui-shared/src',
  'packages/agent/ui/frontend/src',
];

// Theme definition files are exempt — they DEFINE the tokens.
// prime-shadcn-tokens.css holds the Tailwind v4 `@theme {}` raw palette
// and the `:root` / `.dark` / `[data-theme="dark"]` mode flip; like the
// older global.css / light.css / dark.css, raw hex inside it is legitimate.
const DEFINITION_FILE_NAMES = new Set([
  'global.css', 'light.css', 'dark.css',
  'base.css', 'utilities.css', 'animations.css',
  'prime-shadcn-tokens.css',
]);

const rawArgs = process.argv.slice(2);
const args = new Set(rawArgs);
const asJson = args.has('--json');
const fixSuggest = args.has('--fix-suggest');

// --files=<comma-or-space-separated> | --files=- (read newline-delimited
// from stdin). Both forms collect into a Set of absolute paths so the
// driver below can swap the SCAN_ROOTS walk for a direct file list.
function collectExplicitFiles() {
  const list = new Set();
  for (const a of rawArgs) {
    if (!a.startsWith('--files')) continue;
    const eq = a.indexOf('=');
    const val = eq >= 0 ? a.slice(eq + 1) : '';
    if (val === '-') {
      let stdin = '';
      try {
        stdin = readFileSync(0, 'utf8');
      } catch {
        // No stdin attached — treat as empty set.
      }
      for (const line of stdin.split(/\r?\n/)) {
        const p = line.trim();
        if (p) list.add(p);
      }
      continue;
    }
    for (const p of val.split(/[\s,]+/)) {
      const t = p.trim();
      if (t) list.add(t);
    }
  }
  if (list.size === 0) return null;
  // Resolve every path to absolute form anchored at REPO_ROOT so the
  // scanner can locate files regardless of CWD.
  const abs = new Set();
  for (const p of list) {
    abs.add(p.startsWith('/') ? p : join(REPO_ROOT, p));
  }
  return abs;
}
const explicitFiles = collectExplicitFiles();

function walk(dir) {
  const out = [];
  let entries;
  try {
    entries = readdirSync(dir);
  } catch {
    return out;
  }
  for (const name of entries) {
    if (name === 'node_modules' || name.startsWith('.')) continue;
    const p = join(dir, name);
    const st = statSync(p);
    if (st.isDirectory()) {
      out.push(...walk(p));
    } else {
      out.push(p);
    }
  }
  return out;
}

function isExempt(path) {
  const base = basename(path);
  if (DEFINITION_FILE_NAMES.has(base)) return true;
  if (base === 'chartColors.ts') return true;
  if (base.endsWith('.stories.tsx')) return true;
  if (base.endsWith('.test.tsx') || base.endsWith('.test.ts')) return true;
  return false;
}

// ─── Detectors ─────────────────────────────────────────────────────────────

const HEX_LITERAL = /(?<![\w'"\\])#[0-9a-fA-F]{3,8}\b/g;
const RGBA_LITERAL = /\b(?:rgba?|hsla?)\([^)]+\)/g;

// Returns true if `match.index` falls inside a `var(--name, ...fallback)` parens
// where the literal is the fallback (we allow that as a transitional escape).
function isInsideVarFallback(src, idx) {
  // Look back up to 200 chars
  const start = Math.max(0, idx - 200);
  const window = src.slice(start, idx);
  // Last `var(` and its matching `)` — if the var( is open at idx, we're inside.
  const lastVar = window.lastIndexOf('var(');
  if (lastVar === -1) return false;
  const after = window.slice(lastVar);
  let depth = 0;
  for (const ch of after) {
    if (ch === '(') depth++;
    else if (ch === ')') depth--;
  }
  return depth > 0;
}

// Walk a .tsx file and yield each `style={{...}}` block's [start, end, body].
function* extractStyleBlocks(src) {
  const re = /style=\{\{/g;
  let m;
  while ((m = re.exec(src))) {
    const open = m.index + 'style={{'.length;
    let depth = 2; // we've opened two {
    let i = open;
    while (i < src.length && depth > 0) {
      const ch = src[i];
      if (ch === '{') depth++;
      else if (ch === '}') depth--;
      i++;
    }
    // body is src.slice(open, i-2) (strip closing }})
    yield { start: m.index, bodyStart: open, bodyEnd: i - 2, body: src.slice(open, i - 2) };
  }
}

function lineOf(src, idx) {
  return src.slice(0, idx).split('\n').length;
}

// Hex token mapping hints (small heuristic for --fix-suggest)
const HEX_HINTS = new Map([
  ['#ffffff', 'var(--color-surface) / var(--color-surface-raised)'],
  ['#fff', 'var(--color-surface)'],
  ['#0f172a', 'var(--g-slate-900) / var(--sidebar-bg)'],
  ['#f8fafc', 'var(--g-slate-50)'],
  ['#198754', 'var(--color-success)'],
  ['#dc3545', 'var(--color-danger)'],
  ['#0d6efd', 'var(--color-info)'],
  ['#ffc107', 'var(--color-warning)'],
  ['#16a34a', 'var(--color-success)'],
  ['#10b981', 'var(--color-success) [chart phase: var(--color-success) or import chartColors phase palette]'],
  ['#3b82f6', 'var(--color-info)'],
  ['#22c55e', 'var(--color-success)'],
  ['#ef4444', 'var(--color-danger)'],
  ['#f59e0b', 'var(--color-warning)'],
  ['#8b5cf6', 'var(--color-violet)'],
  ['#6d28d9', 'var(--color-violet-dark)'],
  ['#6366f1', 'getPhaseColor(mode, "reqHooks") / var(--color-info)'],
  ['#b91c1c', 'var(--color-danger-dark)'],
  ['#15803d', 'var(--color-success-dark)'],
  ['#a16207', 'var(--color-warning-dark)'],
]);

function hint(literal) {
  const norm = literal.toLowerCase();
  if (HEX_HINTS.has(norm)) return HEX_HINTS.get(norm);
  if (norm.startsWith('rgba(0, 0, 0,') || norm.startsWith('rgba(0,0,0,')) {
    if (norm.includes('0.5')) return 'var(--color-overlay-strong) / var(--color-overlay)';
    if (norm.includes('0.4')) return 'var(--shadow-xl)';
    if (norm.includes('0.15')) return 'var(--shadow-md)';
    if (norm.includes('0.1')) return 'var(--shadow-sm)';
    if (norm.includes('0.08')) return 'var(--shadow-xs)';
  }
  if (norm.startsWith('rgba(15, 23, 42,')) {
    if (norm.includes('0.35')) return 'var(--color-overlay)';
    if (norm.includes('0.12')) return 'var(--shadow-md)';
    if (norm.includes('0.06')) return 'var(--shadow-xs)';
  }
  if (norm.match(/^rgba?\((34,\s*197,\s*94|25,\s*135,\s*84|22,\s*163,\s*74|16,\s*185,\s*129)/)) {
    return 'var(--color-success-light) / var(--color-success)';
  }
  if (norm.match(/^rgba?\((220,\s*38,\s*38|220,\s*53,\s*69|239,\s*68,\s*68|221,\s*51,\s*51)/)) {
    return 'var(--color-danger-light) / var(--color-danger)';
  }
  if (norm.match(/^rgba?\((217,\s*119,\s*6|255,\s*193,\s*7|234,\s*179,\s*8|180,\s*83,\s*9|146,\s*64,\s*14)/)) {
    return 'var(--color-warning-light) / var(--color-warning)';
  }
  if (norm.match(/^rgba?\((37,\s*99,\s*235|29,\s*78,\s*216|59,\s*130,\s*246|13,\s*110,\s*253|29,\s*111,\s*255)/)) {
    return 'var(--color-info-light) / var(--color-info)';
  }
  if (norm.match(/^rgba?\((102,\s*16,\s*242|139,\s*92,\s*246)/)) {
    return 'var(--color-violet-light) / var(--color-violet)';
  }
  return null;
}

// ─── Scans ─────────────────────────────────────────────────────────────────

function scanModuleCss(path, src) {
  const violations = [];
  function check(re) {
    let m;
    while ((m = re.exec(src))) {
      if (isInsideVarFallback(src, m.index)) continue;
      violations.push({
        kind: 'css-color-literal',
        literal: m[0],
        line: lineOf(src, m.index),
        col: m.index - src.lastIndexOf('\n', m.index - 1),
        suggest: hint(m[0]) || '',
      });
    }
  }
  check(new RegExp(HEX_LITERAL.source, 'g'));
  check(new RegExp(RGBA_LITERAL.source, 'g'));
  return violations;
}

const INLINE_RAW_DETECTORS = [
  {
    name: 'inline-color-literal',
    re: /(?:color|background(?:Color)?|borderColor|borderTopColor|borderBottomColor|borderLeftColor|borderRightColor|fill|stroke|outline|outlineColor|boxShadow)\s*:\s*(?:`[^`]*`|'[^']*'|"[^"]*")/g,
    extract: (snippet) => {
      // Only flag if snippet contains a hex or rgba literal (var(--...) form OK)
      const hex = snippet.match(HEX_LITERAL);
      const rgba = snippet.match(RGBA_LITERAL);
      const hasVar = /var\(--/.test(snippet);
      const lits = [...(hex || []), ...(rgba || [])];
      if (lits.length === 0) return null;
      if (hasVar && lits.length === 0) return null;
      // If the literal lives inside `var(--x, #fff)` fallback, the isInsideVarFallback path will skip.
      return lits;
    },
  },
  {
    name: 'inline-spacing-literal',
    re: /(?:padding|paddingTop|paddingBottom|paddingLeft|paddingRight|margin|marginTop|marginBottom|marginLeft|marginRight|gap|rowGap|columnGap)\s*:\s*(?:\d+(?:\.\d+)?|`[^`]*`|'[^']*'|"[^"]*")/g,
    extract: (snippet) => {
      if (/var\(--/.test(snippet)) return null;
      // Allow runtime-computed template-literal values with interpolation
      if (/`[^`]*\$\{/.test(snippet)) return null;
      // Allow `'100%'`, `'auto'`, `'fit-content'` etc., and composite shorthands that contain those keywords (e.g. `'0 auto'` for centering).
      if (/['"`](?:auto|inherit|initial|unset|fit-content|max-content|min-content|100%|0)['"`]/.test(snippet)) return null;
      if (/['"`][^'"`]*\b(?:auto|inherit|max-content|min-content|fit-content)\b[^'"`]*['"`]/.test(snippet)) return null;
      const m = snippet.match(/(?:\d+(?:\.\d+)?(?:px|rem|em)?|['"`][^'"`]*['"`])$/);
      return m ? [m[0]] : null;
    },
  },
  {
    name: 'inline-fontsize-literal',
    re: /fontSize\s*:\s*(?:\d+|`[^`]*`|'[^']*'|"[^"]*")/g,
    extract: (snippet) => {
      if (/var\(--/.test(snippet)) return null;
      if (/`[^`]*\$\{/.test(snippet)) return null;
      return snippet.match(/(?:\d+(?:px)?|['"`][^'"`]*['"`])$/) ?? null;
    },
  },
  {
    name: 'inline-fontweight-literal',
    re: /fontWeight\s*:\s*(?:\d+|'[^']*'|"[^"]*")/g,
    extract: (snippet) => {
      if (/var\(--/.test(snippet)) return null;
      return snippet.match(/(?:\d+|['"`][^'"`]*['"`])$/) ?? null;
    },
  },
  {
    name: 'inline-radius-literal',
    re: /borderRadius\s*:\s*(?:\d+|`[^`]*`|'[^']*'|"[^"]*")/g,
    extract: (snippet) => {
      if (/var\(--/.test(snippet)) return null;
      if (/`[^`]*\$\{/.test(snippet)) return null;
      // Allow percentage values like '50%' (circles / pills) and 0
      if (/['"`](?:\d+%|0)['"`]/.test(snippet)) return null;
      return snippet.match(/(?:\d+(?:px)?|['"`][^'"`]*['"`])$/) ?? null;
    },
  },
  {
    name: 'inline-transition-literal',
    re: /transition\s*:\s*(?:`[^`]*`|'[^']*'|"[^"]*")/g,
    extract: (snippet) => {
      if (/var\(--/.test(snippet)) return null;
      if (/`[^`]*\$\{/.test(snippet)) return null;
      const m = snippet.match(/['"`]([^'"`]*\d+ms[^'"`]*)['"`]/);
      return m ? [m[1]] : null;
    },
  },
  {
    name: 'inline-zindex-literal',
    re: /zIndex\s*:\s*\d+/g,
    extract: (snippet) => {
      if (/var\(--/.test(snippet)) return null;
      const m = snippet.match(/zIndex\s*:\s*(\d+)/);
      return m ? [m[1]] : null;
    },
  },
];

function scanTsx(path, src) {
  const violations = [];
  for (const block of extractStyleBlocks(src)) {
    for (const det of INLINE_RAW_DETECTORS) {
      const re = new RegExp(det.re.source, 'g');
      let m;
      while ((m = re.exec(block.body))) {
        const snippet = m[0];
        const lits = det.extract(snippet);
        if (!lits || lits.length === 0) continue;
        // Allow if snippet contains a CSS-variable bridge: style={{ '--foo': ... }}
        // (We're already inside style block; the detector keys filter for known props)
        const absIdx = block.bodyStart + m.index;
        violations.push({
          kind: det.name,
          literal: Array.isArray(lits) ? lits.join(',') : String(lits),
          snippet: snippet.length > 80 ? snippet.slice(0, 80) + '…' : snippet,
          line: lineOf(src, absIdx),
          col: absIdx - src.lastIndexOf('\n', absIdx - 1),
          suggest: hint(Array.isArray(lits) ? lits[0] : lits) || '',
        });
      }
    }
  }
  return violations;
}

// ─── Driver ────────────────────────────────────────────────────────────────

const allViolations = [];
let filesScanned = 0;

// Two modes:
//   - --files=... → scan exactly the supplied paths (after extension +
//     exempt filtering). Skip files outside SCAN_ROOTS so unrelated
//     changes (Go sources, docs, agent-platform Swift, etc.) don't
//     surface false alarms when the pre-commit hook passes the full
//     staged list.
//   - default → walk SCAN_ROOTS like before.
function inScanRoots(path) {
  for (const root of SCAN_ROOTS) {
    const abs = join(REPO_ROOT, root);
    if (path === abs || path.startsWith(abs + '/')) return true;
  }
  return false;
}

if (explicitFiles) {
  for (const file of explicitFiles) {
    if (!inScanRoots(file)) continue;
    if (isExempt(file)) continue;
    const isCss = file.endsWith('.module.css');
    const isTsx = file.endsWith('.tsx');
    if (!isCss && !isTsx) continue;
    let src;
    try {
      src = readFileSync(file, 'utf8');
    } catch {
      // File may have been deleted in the staged change — skip silently.
      continue;
    }
    filesScanned++;
    const found = isCss ? scanModuleCss(file, src) : scanTsx(file, src);
    if (found.length > 0) {
      const rel = relative(REPO_ROOT, file);
      for (const v of found) allViolations.push({ file: rel, ...v });
    }
  }
} else {
  for (const root of SCAN_ROOTS) {
    const abs = join(REPO_ROOT, root);
    for (const file of walk(abs)) {
      if (isExempt(file)) continue;
      const isCss = file.endsWith('.module.css');
      const isTsx = file.endsWith('.tsx');
      if (!isCss && !isTsx) continue;
      filesScanned++;
      let src;
      try {
        src = readFileSync(file, 'utf8');
      } catch {
        continue;
      }
      const found = isCss ? scanModuleCss(file, src) : scanTsx(file, src);
      if (found.length > 0) {
        const rel = relative(REPO_ROOT, file);
        for (const v of found) allViolations.push({ file: rel, ...v });
      }
    }
  }
}

// ─── Report ────────────────────────────────────────────────────────────────

if (asJson) {
  process.stdout.write(JSON.stringify({ filesScanned, count: allViolations.length, violations: allViolations }, null, 2) + '\n');
  process.exit(allViolations.length > 0 ? 1 : 0);
}

// Pretty / grouped report
const byKind = new Map();
for (const v of allViolations) {
  if (!byKind.has(v.kind)) byKind.set(v.kind, []);
  byKind.get(v.kind).push(v);
}

console.log(`design-tokens guard — scanned ${filesScanned} files`);
console.log('');

if (allViolations.length === 0) {
  console.log('✓ no violations — every visual value resolves through the token layer');
  process.exit(0);
}

console.log(`✗ ${allViolations.length} violation(s) across ${byKind.size} categor${byKind.size === 1 ? 'y' : 'ies'}`);
console.log('');

for (const [kind, list] of byKind) {
  console.log(`── ${kind} (${list.length}) ──`);
  // Group by file
  const byFile = new Map();
  for (const v of list) {
    if (!byFile.has(v.file)) byFile.set(v.file, []);
    byFile.get(v.file).push(v);
  }
  for (const [file, vs] of byFile) {
    console.log(`  ${file}`);
    for (const v of vs) {
      const where = `:${v.line}:${v.col}`;
      const lit = v.literal;
      const suggest = fixSuggest && v.suggest ? `  → ${v.suggest}` : '';
      console.log(`    ${where.padEnd(10)} ${lit}${suggest}`);
    }
  }
  console.log('');
}

console.log(`See docs in CLAUDE.md (Conventions → TypeScript / Control Plane UI → Design-token rule).`);
console.log(`Run \`node scripts/check-design-tokens.mjs --fix-suggest\` for mapping hints.`);
process.exit(1);
