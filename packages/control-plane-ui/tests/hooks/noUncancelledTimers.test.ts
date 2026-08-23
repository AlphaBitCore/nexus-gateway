import { describe, it, expect } from 'vitest';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';

// A bare `setTimeout(() => setX(...), ms)` in a component keeps running after
// that component unmounts, and its callback then updates a dead tree. When the
// surrounding environment is gone too, React reaches for `window` during the
// update and throws `ReferenceError: window is not defined`. That took CI down
// once already — from a 2-second "copied!" reset outliving its dialog — and the
// failure is attributed to whatever test file happened to be running, so it
// reads as an unrelated flake.
//
// The rule: components arm timers through `useTimeouts()`, which cancels them
// on unmount. This guard exists because 14 sites drifted into the bare form
// before anyone noticed; a lint is cheaper than finding them again.
const SRC = resolve(process.cwd(), 'src');

// Timers whose lifetime is governed by something other than a React unmount.
const ALLOWED = new Map<string, string>([
  [
    'components/assistant/streamChat.ts',
    'reconnect backoff inside an AbortSignal-driven loop: the sleep is followed ' +
      'by a `signal.aborted` check, so cancellation is the controller\'s job, ' +
      'not a cleanup function\'s',
  ],
]);

function walk(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      walk(full, out);
    } else if (/\.tsx?$/.test(entry) && !/\.(test|stories)\.tsx?$/.test(entry)) {
      out.push(full);
    }
  }
  return out;
}

function offenders(): string[] {
  return walk(SRC)
    .filter((f) => {
      const rel = relative(SRC, f);
      if (ALLOWED.has(rel)) return false;
      if (rel === join('hooks', 'useTimeouts.ts')) return false; // the hook itself
      const src = readFileSync(f, 'utf8');
      if (!/\bsetTimeout\s*\(/.test(src)) return false;
      // Either it cancels its own handle, or it arms through the hook.
      return !/\bclearTimeout\s*\(/.test(src) && !/\barmTimeout\s*\(/.test(src);
    })
    .map((f) => relative(SRC, f));
}

describe('no uncancelled timers in components', () => {
  it('actually scans the source tree (guard is not vacuous)', () => {
    // A wrong root or a broken filter would make the rule below pass while
    // inspecting nothing.
    const files = walk(SRC);
    expect(files.length).toBeGreaterThan(100);
    expect(files.some((f) => /\bsetTimeout\s*\(/.test(readFileSync(f, 'utf8')))).toBe(true);
  });

  it('arms every component timer through useTimeouts', () => {
    expect(
      offenders(),
      'these files call setTimeout without cancelling it — use `useTimeouts()` so the ' +
        'timer dies with the component, or add an entry to ALLOWED explaining what ' +
        'else governs its lifetime',
    ).toEqual([]);
  });
});
