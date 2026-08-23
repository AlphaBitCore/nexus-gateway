import { describe, expect, it } from 'vitest';
import { readFileSync, existsSync } from 'node:fs';
import { resolve } from 'node:path';

// Read the stylesheet from disk rather than importing it: this project's vitest
// config resolves a `?raw` CSS import to an EMPTY string, which would make every
// assertion below pass while reading nothing. The vacuity guard catches that,
// but reading the file directly avoids the trap entirely.
const candidates = [
  resolve(process.cwd(), 'src/styles/global.css'),
  resolve(process.cwd(), 'packages/ui-shared/src/styles/global.css'),
];
const cssPath = candidates.find(existsSync);
const css = cssPath ? readFileSync(cssPath, 'utf8') : '';

// global.css applies a hover/active background wash to every bare <button>.
// Radix renders its controls — Checkbox, Switch, Tabs, RadioGroup, Accordion —
// as bare <button>s that carry state in `data-state` and paint their own
// background from it. An element selector like `button:hover:not(:disabled)`
// scores (0,2,1) and OUTRANKS `.root[data-state='checked']` at (0,2,0), so the
// wash silently overwrote those fills: a checked Checkbox turned grey under the
// cursor and its white tick vanished against it. Sidebar's six `!important`
// background declarations are the fossil of the same fight.
//
// The guard is on the SELECTOR rather than on rendered colour because the bug
// only exists in the cascade: jsdom does not resolve `:hover`, so a component
// test cannot see it, and by the time it is visible it is on someone's screen.
/** Selectors that set a background on a bare `button` element (no class/id). */
function bareButtonBackgroundSelectors(): string[] {
  const out: string[] = [];
  // Comments must go first: a `/* … */` block sitting above a rule would
  // otherwise be swallowed into that rule's selector text, and every `^button`
  // check would miss. That is what the vacuity guard below caught.
  const source = css.replace(/\/\*[\s\S]*?\*\//g, '');
  // NOT anchored on the preceding `}`: that form CONSUMES the closing brace of
  // the previous rule, so the next rule has nothing to anchor against and is
  // skipped — consecutive rules drop every other one, and `button:hover` was
  // exactly the one that vanished.
  const ruleRe = /([^{}]+)\{([^}]*)\}/g;
  let m: RegExpExecArray | null;
  while ((m = ruleRe.exec(source)) !== null) {
    const [, selectorList, body] = m;
    if (!/(^|[\s;])background(-color)?\s*:/.test(body)) continue;
    for (const sel of selectorList.split(',').map((s) => s.trim())) {
      if (/^button(?![\w-])/.test(sel) && !sel.includes('.') && !sel.includes('#')) {
        out.push(sel);
      }
    }
  }
  return out;
}

describe('global button wash', () => {
  it('finds the bare-button background rules at all (guard is not vacuous)', () => {
    // If the parser silently matched nothing, every assertion below would pass
    // for the wrong reason.
    expect(bareButtonBackgroundSelectors().length).toBeGreaterThan(0);
  });

  it('never paints a background over a control that owns its state', () => {
    const offenders = bareButtonBackgroundSelectors().filter(
      (sel) => !sel.includes(':not([data-state])'),
    );
    expect(
      offenders,
      'a bare-button background rule must exclude [data-state], or it will overwrite ' +
        'the fill of every Radix control (Checkbox/Switch/Tabs/RadioGroup) on hover',
    ).toEqual([]);
  });
});
