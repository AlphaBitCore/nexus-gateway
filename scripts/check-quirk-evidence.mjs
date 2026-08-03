#!/usr/bin/env node
// Evidence lint — provider-adapter-architecture.md §3a Rule 7, machine-checked.
//
// Every per-model wire-quirk rule must cite an OBSERVED vendor rejection, not
// speculation. Fails when:
//   1. a registered Go quirk site (scripts/quirk-coverage.config.mjs →
//      goQuirkSites) is missing from its file — the rule moved without the
//      registry following;
//   2. the contiguous comment block above a quirk site does not cite an
//      observed rejection (must mention "400" and observed/probed language);
//   3. a registry family row carries no substantive evidence, or a
//      strips row's evidence does not cite a 400.
//
// Usage:
//   node scripts/check-quirk-evidence.mjs             # lint the repo
//   node scripts/check-quirk-evidence.mjs --self-test # prove each rule bites

import { readFileSync, existsSync } from 'node:fs';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { families, goQuirkSites } from './quirk-coverage.config.mjs';

const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');

const OBSERVED_RE = /observed|probed?/i;
const MIN_EVIDENCE_LEN = 40;

// ── pure checks (unit of self-test) ─────────────────────────────────────────

/** The contiguous `//` comment block directly above a symbol's declaration. */
export function commentAbove(goSource, symbol, kind) {
  const decl = kind === 'var'
    ? new RegExp(`^var ${symbol}\\b`, 'm')
    : new RegExp(`^func ${symbol}\\(`, 'm');
  const m = goSource.match(decl);
  if (!m) return null;
  const lines = goSource.slice(0, m.index).split('\n');
  lines.pop(); // partial line at the declaration boundary
  const block = [];
  for (let i = lines.length - 1; i >= 0; i--) {
    const t = lines[i].trim();
    if (t.startsWith('//')) block.unshift(t);
    else break;
  }
  return block.join('\n');
}

/** Checks 1+2 — every quirk site exists and cites an observed rejection. */
export function checkGoSites(sites, readFile) {
  const errors = [];
  for (const s of sites) {
    const src = readFile(s.file);
    if (src === null) {
      errors.push(`quirk site file missing: ${s.file}`);
      continue;
    }
    const block = commentAbove(src, s.symbol, s.kind);
    if (block === null) {
      errors.push(
        `quirk symbol ${s.symbol} not found in ${s.file} — if the rule moved, ` +
        `update goQuirkSites in scripts/quirk-coverage.config.mjs in the same commit`,
      );
      continue;
    }
    if (!block.includes('400')) {
      errors.push(`${s.file} → ${s.symbol}: comment block cites no 400 (§3a Rule 7 — quote the vendor's rejection)`);
    }
    if (!OBSERVED_RE.test(block)) {
      errors.push(`${s.file} → ${s.symbol}: comment block has no observed/probed language (§3a Rule 7 — evidence, not speculation)`);
    }
  }
  return errors;
}

/** Check 3 — registry rows carry substantive evidence; strips rows cite a 400. */
export function checkRegistryEvidence(rows) {
  const errors = [];
  for (const f of rows) {
    const tag = `${f.provider}:${f.prefix || '(all)'}`;
    if (!f.evidence || f.evidence.trim().length < MIN_EVIDENCE_LEN) {
      errors.push(`registry row ${tag}: evidence is empty or too thin to be a citation`);
      continue;
    }
    if (f.decision === 'strips' && !f.evidence.includes('400')) {
      errors.push(`registry row ${tag}: a strips decision must quote the observed 400`);
    }
    if (!['strips', 'accepts', 'forward-unprobed'].includes(f.decision)) {
      errors.push(`registry row ${tag}: unknown decision ${JSON.stringify(f.decision)}`);
    }
  }
  return errors;
}

// ── self-test: prove every rule bites (removal → red) ───────────────────────

function selfTest() {
  const goodGo = [
    '// IsFussyModel reports the family that rejects temperature.',
    '// Observed (vendor API): 400 "invalid temperature".',
    'func IsFussyModel(id string) bool {',
  ].join('\n');
  const no400 = goodGo.replace('400 "invalid temperature"', 'a rejection');
  const noObserved = goodGo.replace('Observed', 'We believe');
  const site = { file: 'fake.go', symbol: 'IsFussyModel', kind: 'func' };
  const varGo = '// Observed: 400 quoted here.\nvar fussyList = []string{}\n';
  const reader = (files) => (path) => (path in files ? files[path] : null);

  const stripsRow = {
    provider: 'acme', prefix: 'x', decision: 'strips',
    evidence: 'Observed (vendor): 400 "invalid temperature: only 1 is allowed for this model".',
  };

  const cases = [
    ['clean go site passes', () =>
      checkGoSites([site], reader({ 'fake.go': goodGo })).length === 0],
    ['var-kind site passes', () =>
      checkGoSites([{ file: 'fake.go', symbol: 'fussyList', kind: 'var' }], reader({ 'fake.go': varGo })).length === 0],
    ['missing file fails', () =>
      checkGoSites([site], reader({})).length === 1],
    ['missing symbol fails', () =>
      checkGoSites([site], reader({ 'fake.go': 'func Other() {}' })).length === 1],
    ['comment without 400 fails', () =>
      checkGoSites([site], reader({ 'fake.go': no400 })).some((e) => e.includes('no 400'))],
    ['comment without observed language fails', () =>
      checkGoSites([site], reader({ 'fake.go': noObserved })).some((e) => e.includes('observed/probed'))],
    ['comment separated from decl is not credited', () => {
      const detached = '// Observed: 400.\n\nfunc IsFussyModel(id string) bool {';
      return checkGoSites([site], reader({ 'fake.go': detached })).length > 0;
    }],
    ['citation must sit on the SYMBOL\'s block, not anywhere in the file', () => {
      // Two symbols; only the sibling's block cites the 400. The registered
      // site's own block is bare — a file-scoped citation check would pass.
      const twoSyms = [
        '// Observed (vendor): 400 "quoted on the sibling".',
        'func SiblingRule(id string) bool {',
        '}',
        '',
        '// Bare block with no citation at all.',
        'func IsFussyModel(id string) bool {',
      ].join('\n');
      return checkGoSites([site], reader({ 'fake.go': twoSyms })).length === 2;
    }],
    ['clean registry row passes', () =>
      checkRegistryEvidence([stripsRow]).length === 0],
    ['thin evidence fails', () =>
      checkRegistryEvidence([{ ...stripsRow, evidence: '400' }]).length === 1],
    ['evidence one char under the threshold fails', () =>
      checkRegistryEvidence([{ ...stripsRow, evidence: 'x'.repeat(MIN_EVIDENCE_LEN - 1).replace(/^xxx/, '400') }]).length === 1],
    ['evidence exactly at the threshold passes', () =>
      checkRegistryEvidence([{ ...stripsRow, evidence: 'x'.repeat(MIN_EVIDENCE_LEN).replace(/^xxx/, '400') }]).length === 0],
    ['strips without a 400 citation fails', () =>
      checkRegistryEvidence([{ ...stripsRow, evidence: 'The vendor rejects this parameter; trust me on this one.' }]).length === 1],
    ['strips citing 4xx but not a literal 400 fails', () =>
      checkRegistryEvidence([{ ...stripsRow, evidence: 'Observed (vendor): rejects with HTTP 4xx, only default value 1 is allowed.' }]).length === 1],
    ['unknown decision fails', () =>
      checkRegistryEvidence([{ ...stripsRow, decision: 'maybe' }]).length === 1],
  ];

  let failed = 0;
  for (const [name, fn] of cases) {
    let ok = false;
    try { ok = fn(); } catch { ok = false; }
    console.log(`${ok ? 'ok  ' : 'FAIL'} ${name}`);
    if (!ok) failed++;
  }
  if (failed) {
    console.error(`\ncheck-quirk-evidence self-test: ${failed} case(s) failed`);
    process.exit(1);
  }
  console.log('\ncheck-quirk-evidence self-test: all cases pass');
}

// ── main ─────────────────────────────────────────────────────────────────────

function main() {
  if (process.argv.includes('--self-test')) {
    selfTest();
    return;
  }
  const readFile = (p) => {
    const abs = resolve(REPO_ROOT, p);
    return existsSync(abs) ? readFileSync(abs, 'utf8') : null;
  };
  const errors = [
    ...checkGoSites(goQuirkSites, readFile),
    ...checkRegistryEvidence(families),
  ];
  if (errors.length) {
    console.error('✗ quirk-evidence lint failed:\n');
    for (const e of errors) console.error(`  - ${e}`);
    console.error('\n  §3a Rule 7: every prefix-list rule cites an observed 400 — no speculative strips.');
    process.exit(1);
  }
  console.log('✓ quirk-evidence: every quirk rule and registry row cites observed vendor behaviour');
}

main();
