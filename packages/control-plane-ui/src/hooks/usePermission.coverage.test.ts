/**
 * Regression coverage for usePermission ACTION_MAP.
 *
 * Walks src/ for literal `usePermission('key')` call sites and asserts every
 * key exists in ACTION_MAP. Without this, a new UI button that passes a typo
 * or a key the maintainer forgot to register silently fails closed — the
 * button is hidden from every user, including super-admin, with no warning.
 *
 * Dynamic call sites — `usePermission(varName)` or template literals — are
 * collected separately and surfaced as a console.log; they can't be checked
 * statically and rely on the dev-only runtime warning in usePermission.ts.
 */
import { describe, it, expect } from 'vitest';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { ACTION_MAP } from './usePermission';

const SRC_ROOT = join(__dirname, '..');
const SELF = 'hooks/usePermission.ts';

function walk(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    const full = join(dir, name);
    const st = statSync(full);
    if (st.isDirectory()) {
      if (name === 'node_modules' || name === 'dist') continue;
      walk(full, out);
    } else if (/\.(ts|tsx)$/.test(name) && !/\.test\.(ts|tsx)$/.test(name)) {
      out.push(full);
    }
  }
  return out;
}

interface CallSite {
  file: string;
  line: number;
  key: string;
}

function extractCalls(): { literal: CallSite[]; dynamic: CallSite[] } {
  const literal: CallSite[] = [];
  const dynamic: CallSite[] = [];
  // Match usePermission( ... ) — capture the inside of the parens, single-line.
  const callRe = /usePermission\(\s*([^)]*?)\s*\)/g;
  const litRe = /^['"]([^'"]+)['"]$/;

  for (const file of walk(SRC_ROOT)) {
    const rel = relative(SRC_ROOT, file).replace(/\\/g, '/');
    if (rel === SELF) continue;
    const text = readFileSync(file, 'utf8');
    const lines = text.split('\n');
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      let m: RegExpExecArray | null;
      callRe.lastIndex = 0;
      while ((m = callRe.exec(line)) !== null) {
        const arg = m[1].trim();
        if (!arg) continue;
        const lit = litRe.exec(arg);
        if (lit) {
          literal.push({ file: rel, line: i + 1, key: lit[1] });
        } else {
          dynamic.push({ file: rel, line: i + 1, key: arg });
        }
      }
    }
  }
  return { literal, dynamic };
}

describe('usePermission ACTION_MAP coverage', () => {
  it('every literal usePermission(...) key is present in ACTION_MAP', () => {
    const { literal, dynamic } = extractCalls();
    expect(literal.length).toBeGreaterThan(0); // sanity: we found call sites

    const orphans = literal.filter((c) => !(c.key in ACTION_MAP));

    if (dynamic.length > 0) {
      // Surface dynamic call sites; not a failure, just info.
      // eslint-disable-next-line no-console
      console.log(
        `[ACTION_MAP coverage] ${dynamic.length} dynamic call site(s) ` +
        `(cannot be checked statically — relies on dev-time runtime warn):\n` +
        dynamic.map((d) => `  ${d.file}:${d.line}  usePermission(${d.key})`).join('\n'),
      );
    }

    if (orphans.length > 0) {
      const msg = orphans
        .map((o) => `  ${o.file}:${o.line}  usePermission('${o.key}')  <-- missing from ACTION_MAP`)
        .join('\n');
      throw new Error(
        `Found ${orphans.length} usePermission(...) call site(s) whose key is ` +
        `not registered in ACTION_MAP (src/hooks/usePermission.ts). The ` +
        `corresponding UI affordance will be hidden from every user.\n${msg}`,
      );
    }
  });

  // Every ACTION_MAP target must match the canonical admin:<resource>.<verb>
  // shape. The same regex is enforced server-side by
  // TestAllAdminActionStringsAreCanonical (Go) so any mismatch fails CI
  // on either layer — a CamelCase or phantom action would compile clean
  // but then deny silently at runtime.
  it('every ACTION_MAP target matches the canonical admin:<resource>.<verb> shape', () => {
    // Allow admin:* wildcards (admin:*, admin:*.read, admin:provider.*).
    const canonicalRe = /^admin:(\*|[a-z][a-z0-9-]*(\.\*|\.[a-z][a-z-]*)|\*\.[a-z][a-z-]*)$/;
    const bad: { key: string; target: string }[] = [];
    for (const [key, target] of Object.entries(ACTION_MAP)) {
      if (!canonicalRe.test(target)) {
        bad.push({ key, target });
      }
    }
    if (bad.length > 0) {
      const msg = bad.map((b) => `  '${b.key}' -> '${b.target}'`).join('\n');
      throw new Error(
        `Found ${bad.length} ACTION_MAP target(s) that do not match the ` +
        `canonical admin:<resource>.<verb> shape. The engine glob-matches ` +
        `string-for-string against IamPolicy.document, so a non-canonical ` +
        `target will silently fail at the boundary.\n${msg}`,
      );
    }
  });
});
