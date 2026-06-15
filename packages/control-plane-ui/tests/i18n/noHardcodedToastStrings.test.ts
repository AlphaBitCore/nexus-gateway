/**
 * i18n blind-spot guard (F-0285).
 *
 * The locale-parity CI scans JSX text + user-facing attributes, so a hardcoded
 * English string passed as an ARGUMENT to `addToast(...)` in a plain `.ts` file
 * (hooks, services, form state) slips through — it renders untranslated English
 * to es/zh users. This guard closes that gap: no `addToast(...)` call in a `.ts`
 * file may take a bare string/template literal as its first argument; the
 * message must come from `t(...)` (or a variable already resolved via `t`).
 *
 * Scoped to `.ts` (not `.tsx`): toast calls in components are reachable by the
 * JSX-aware i18n tooling, and `.tsx` toast args are overwhelmingly `t(...)`
 * already; the systemic blind spot is the `.ts` hook/service layer.
 */
import { describe, it, expect } from 'vitest';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';

const SRC = join(__dirname, '..', '..', 'src');

function walkTsFiles(dir: string, out: string[] = []): string[] {
  for (const name of readdirSync(dir)) {
    const full = join(dir, name);
    const st = statSync(full);
    if (st.isDirectory()) {
      if (name === 'node_modules' || name === 'dist') continue;
      walkTsFiles(full, out);
    } else if (/\.ts$/.test(name) && !/\.(test|d)\.ts$/.test(name)) {
      out.push(full);
    }
  }
  return out;
}

describe('i18n: no hardcoded toast strings in .ts files (F-0285)', () => {
  it('every addToast(...) message in a .ts file is sourced from t(), not a literal', () => {
    // First arg is a single- or double-quote string OR a template literal.
    const re = /addToast\(\s*(['"`])/g;
    const offenders: string[] = [];

    for (const file of walkTsFiles(SRC)) {
      const text = readFileSync(file, 'utf8');
      const lines = text.split('\n');
      for (let i = 0; i < lines.length; i++) {
        re.lastIndex = 0;
        if (re.test(lines[i])) {
          offenders.push(`  ${relative(SRC, file)}:${i + 1}  ${lines[i].trim()}`);
        }
      }
    }

    if (offenders.length > 0) {
      throw new Error(
        `Found ${offenders.length} addToast(...) call(s) in .ts files passing a ` +
          `string/template literal as the message. Route the message through ` +
          `t('<ns>:<key>') so es/zh render translated text (F-0285).\n` +
          offenders.join('\n'),
      );
    }
    expect(offenders).toHaveLength(0);
  });
});
