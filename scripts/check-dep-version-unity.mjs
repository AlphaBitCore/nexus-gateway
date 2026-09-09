#!/usr/bin/env node
/**
 * One version per shared dependency, across every module in the repo.
 *
 * Go has no parent pom: each go.mod carries its own require lines and nothing
 * inherits. `go work sync` is the closest mechanism — it computes one build
 * list over the workspace and writes that selection back into each member —
 * but it reaches only the modules go.work lists in `use (...)`.
 *
 * Two of this repo's modules are deliberately outside the workspace:
 * tests/scenarios (a broken scenario must not break repo-wide `go build`) and
 * tests/agent/gap_closure (darwin build tags). Nothing propagates a version
 * into them, and that is exactly where drift accumulated — x/sync v0.17.0 and
 * x/text v0.29.0 against a repo on v0.22.0 and v0.40.0.
 *
 * This gate covers what no Go mechanism does: it reads every go.mod, workspace
 * member or not, and fails when one dependency is required at two versions.
 *
 * Not every dependency — only the ones more than one module shares. A module
 * with a dependency of its own has nothing to agree with.
 */
import { execSync } from 'node:child_process';
import { readFileSync } from 'node:fs';

const REQUIRE_LINE = /^\s*([a-z0-9][^\s]*\/[^\s]+)\s+(v[0-9][^\s]*)/;

// A version that appears once is not a disagreement. These are the families
// where two modules on different minors have actually bitten: the x/ set moves
// together, and a split there means two copies of the same transitive tree.
const SCOPE = /^(golang\.org\/x\/|github\.com\/(goccy|coder|prometheus|jackc|redis)\/)/;

function moduleDirs() {
  const out = execSync('git ls-files "*/go.mod" go.mod', { encoding: 'utf-8' });
  return out
    .split('\n')
    .filter(Boolean)
    .filter((p) => !p.includes('/testdata/'))
    .map((p) => p.replace(/\/?go\.mod$/, '') || '.');
}

const dirs = moduleDirs();
// A gate that reached nothing would print the same OK as one that found
// nothing. This repo has thirteen modules; a listing that collapses is a
// broken glob, not a clean tree.
const MIN_MODULES = 8;
if (dirs.length < MIN_MODULES) {
  console.error(
    `[dep-version-unity] FAILED -- only ${dirs.length} module(s) found, expected at ` +
      `least ${MIN_MODULES}. The listing is not seeing the tree, so a clean result ` +
      `here would mean nothing.`,
  );
  process.exit(1);
}

/** dep -> version -> [modules] */
const seen = new Map();
for (const dir of dirs) {
  const text = readFileSync(dir === '.' ? 'go.mod' : `${dir}/go.mod`, 'utf-8');
  for (const raw of text.split('\n')) {
    const line = raw.replace(/\/\/.*$/, '');
    if (/^\s*(module|go|toolchain|replace|exclude|retract)\b/.test(line)) continue;
    const m = REQUIRE_LINE.exec(line.replace(/^\s*require\s+/, '\t'));
    if (!m) continue;
    const [, dep, ver] = m;
    if (!SCOPE.test(dep)) continue;
    if (!seen.has(dep)) seen.set(dep, new Map());
    const byVer = seen.get(dep);
    if (!byVer.has(ver)) byVer.set(ver, []);
    byVer.get(ver).push(dir);
  }
}

const split = [...seen.entries()].filter(([, byVer]) => byVer.size > 1);
if (split.length === 0) {
  console.log(
    `[dep-version-unity] OK -- ${dirs.length} modules agree on every shared dependency ` +
      `(${seen.size} in scope).`,
  );
  process.exit(0);
}

console.error(
  `\n[dep-version-unity] ${split.length} dependency/dependencies required at more than ` +
    `one version:\n`,
);
for (const [dep, byVer] of split.sort((a, b) => a[0].localeCompare(b[0]))) {
  console.error(`  ${dep}`);
  for (const [ver, mods] of [...byVer.entries()].sort()) {
    console.error(`    ${ver.padEnd(12)} ${mods.join(', ')}`);
  }
}
console.error(
  `\nFor a workspace member: run \`go work sync\`, which writes the workspace's own\n` +
    `selection into every member's go.mod. For a module outside go.work —\n` +
    `tests/scenarios, tests/agent/gap_closure — nothing propagates into it, so pin it\n` +
    `by hand with \`GOWORK=off go get <dep>@<version>\` and re-run.\n`,
);
process.exit(1);
