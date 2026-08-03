#!/usr/bin/env node
// Catalog↔quirk↔smoke coverage lint (provider-adapter-architecture.md §3a
// Rule 7, sampling-params incident class).
//
// Fails when:
//   1. a chat model in tools/db-migrate/model-catalog.json matches no family
//      row in scripts/quirk-coverage.config.mjs — a new family shipped with
//      no explicit sampling-quirk decision (how the kimi-k2.7 incident began);
//   2. an anchored row's goFile is missing or no longer contains goMatch —
//      the registry claims adapter behaviour the code no longer anchors
//      (strips rows must be anchored; accepts rows that mirror a codec
//      allowlist are verified the same way);
//   3. a `strips` goFile has no SEEDED catalog model across the rows citing
//      it — the smoke cannot exercise the strip, so a regression would ship
//      unprobed;
//   4. an entry in the smoke's _REASONING_MODELS set matches no catalog
//      model — the staleness that once produced 30 false failures in a
//      production run;
//   5. a non-catch-all registry row matches no catalog chat model — stale
//      registry rows are the same disease in the other direction;
//   6. an anthropic family the registry strips is missing from the smoke's
//      rejects set — native /v1/messages applies no strip today, so the arm
//      would eat the vendor 400 as a false red (delete this check together
//      with the smoke's claude-messages suppression when the anthropic
//      native leg starts stripping);
//   7. _send_temperature itself loses its shape — removing the send-the-param
//      coverage is exactly the historical regression, so its suppression
//      branches and its `return True` fall-through are pinned here.
//
// Usage:
//   node scripts/check-quirk-coverage.mjs             # lint the repo
//   node scripts/check-quirk-coverage.mjs --self-test # prove each rule bites

import { readFileSync, existsSync } from 'node:fs';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { families, CATALOG_PATH, SMOKE_PATH } from './quirk-coverage.config.mjs';

const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');

// ── pure checks (unit of self-test) ─────────────────────────────────────────

/** Chat models flattened from the catalog: {provider, id, code, seeded}. */
export function chatModels(catalog) {
  const out = [];
  for (const p of catalog.providers ?? []) {
    for (const m of p.models ?? []) {
      if (m.type !== 'chat') continue;
      out.push({ provider: p.key, id: m.providerModelId, code: m.code, seeded: Boolean(m.seed) });
    }
  }
  return out;
}

/** Family-boundary prefix match, mirroring the Go predicates' rule
 * (rewrites.go hasFamilyPrefix): the id is the family itself, the prefix
 * already ends at a separator, or the id continues with '-'/'.'. Keeps a
 * future catalog "gpt-5.40" from silently inheriting the "gpt-5.4" row —
 * the fail-unsafe direction the bare startsWith allowed. Empty prefix =
 * provider catch-all. */
export function familyMatch(id, prefix) {
  if (prefix === '') return true;
  if (!id.startsWith(prefix)) return false;
  if (id.length === prefix.length) return true;
  const last = prefix[prefix.length - 1];
  if (last === '-' || last === '.') return true;
  const next = id[prefix.length];
  return next === '-' || next === '.';
}

function rowsFor(model, rows) {
  return rows.filter((f) => f.provider === model.provider && familyMatch(model.id, f.prefix));
}

/** Check 1 — every chat model has a recorded decision. */
export function checkCatalogCoverage(catalog, rows) {
  const errors = [];
  for (const m of chatModels(catalog)) {
    if (rowsFor(m, rows).length === 0) {
      errors.push(
        `no sampling-quirk decision for ${m.provider}:${m.id} — probe the family ` +
        `(send it a temperature), then record the decision in scripts/quirk-coverage.config.mjs`,
      );
    }
  }
  return errors;
}

/** Checks 2+3 — every anchored row points at real code (strips rows MUST be
 * anchored; accepts rows that mirror a codec list verify the same way, so
 * removing a family from an allowlist without a probe re-reddens here), and
 * every strip rule has a seeded smoke probe. */
export function checkAnchors(catalog, rows, readFile) {
  const errors = [];
  const models = chatModels(catalog);
  const seededByGoFile = new Map(); // goFile → true when any citing strips row matches a seeded model
  for (const f of rows) {
    const tag = `${f.decision} row ${f.provider}:${f.prefix || '(all)'}`;
    if (f.decision !== 'strips' && !f.goFile && !f.goMatch) continue;
    if (!f.goFile || !f.goMatch) {
      errors.push(`${tag} has no (or a partial) goFile/goMatch anchor — strips rows require one, and an anchor needs both fields`);
      continue;
    }
    const src = readFile(f.goFile);
    if (src === null) {
      errors.push(`${tag} cites missing file ${f.goFile}`);
      continue;
    }
    if (!src.includes(f.goMatch)) {
      errors.push(
        `${tag}: anchor ${JSON.stringify(f.goMatch)} ` +
        `not found in ${f.goFile} — the adapter rule moved or was removed; update the registry in the same commit`,
      );
      continue;
    }
    if (f.decision !== 'strips') continue;
    const seededHit = models.some(
      (m) => m.provider === f.provider && familyMatch(m.id, f.prefix) && m.seeded,
    );
    seededByGoFile.set(f.goFile, (seededByGoFile.get(f.goFile) ?? false) || seededHit);
  }
  for (const [goFile, anySeeded] of seededByGoFile) {
    if (!anySeeded) {
      errors.push(
        `strip rule ${goFile} is exercised by NO seeded catalog model — the smoke cannot ` +
        `probe it; seed a covered model or record why not`,
      );
    }
  }
  return errors;
}

/** Parse the _REASONING_MODELS set literal out of the smoke script. */
export function parseSmokeRejectsSet(smokeSource) {
  const m = smokeSource.match(/_REASONING_MODELS = \{([\s\S]*?)\n\}/);
  if (!m) return null;
  const entries = [];
  for (const line of m[1].split('\n')) {
    const noComment = line.split('#')[0];
    for (const q of noComment.matchAll(/"([^"]+)"/g)) entries.push(q[1]);
  }
  return entries;
}

/** Check 4 — smoke rejects-temperature entries must still exist in the catalog. */
export function checkSmokeSetFresh(catalog, smokeSource) {
  const entries = parseSmokeRejectsSet(smokeSource);
  if (entries === null) {
    return ['could not locate the _REASONING_MODELS set literal in the smoke script — update parseSmokeRejectsSet'];
  }
  const known = new Set();
  for (const m of chatModels(catalog)) {
    known.add(m.id);
    known.add(m.code);
  }
  return entries
    .filter((e) => !known.has(e))
    .map((e) => `stale smoke rejects-temperature entry ${JSON.stringify(e)} — no catalog chat model carries this id/code`);
}

/** Check 5 — a registry row (other than a provider catch-all) that matches no
 * catalog chat model is stale; delete or fix it. Symmetric with check 4. */
export function checkRegistryRowsFresh(catalog, rows) {
  const models = chatModels(catalog);
  const errors = [];
  for (const f of rows) {
    if (f.prefix === '') continue; // provider catch-all: matches whatever the provider ships
    if (!models.some((m) => m.provider === f.provider && familyMatch(m.id, f.prefix))) {
      errors.push(
        `stale registry row ${f.provider}:${f.prefix} — no catalog chat model matches it; delete or fix the row`,
      );
    }
  }
  return errors;
}

/** Check 6 — every anthropic chat model that a strips row covers (and no
 * accepts row exempts) MUST be in the smoke's rejects set: the native
 * /v1/messages leg applies NO strip today, so the smoke must suppress
 * temperature there or the arm 400s as a false red — the original stale-list
 * incident class. DELETE this check (and the smoke's claude-messages
 * suppression it pairs with) when the anthropic native leg starts applying
 * the sampling strip. */
export function checkAnthropicNativeSetParity(catalog, rows, smokeSource) {
  const entries = parseSmokeRejectsSet(smokeSource);
  if (entries === null) return []; // check 4 already reports the parse failure
  const set = new Set(entries);
  const errors = [];
  for (const m of chatModels(catalog)) {
    if (m.provider !== 'anthropic') continue;
    const matched = rowsFor(m, rows);
    const stripped = matched.some((f) => f.decision === 'strips');
    const excepted = matched.some((f) => f.decision === 'accepts');
    if (stripped && !excepted && !set.has(m.id) && !set.has(m.code)) {
      errors.push(
        `anthropic model ${m.id} is sampling-stripped cross-format but missing from the smoke's ` +
        `_REASONING_MODELS set — native /v1/messages has no strip, so the smoke would send it a ` +
        `temperature and eat the vendor 400; add it to the set (with the probed 400 quoted)`,
      );
    }
  }
  return errors;
}

/** Check 7 — pin the send-the-param coverage itself. The historical incident
 * was exactly this coverage being REMOVED; a rewrite of _send_temperature
 * must consciously update these anchors. */
export function checkSendTemperatureGuard(smokeSource) {
  // The function body = the def line plus every following indented-or-blank
  // line (python block shape); stops at the first non-indented statement.
  const m = smokeSource.match(/\ndef _send_temperature\([^\n]*\n(?:[ \t][^\n]*\n|\n)*/);
  if (!m) {
    return ['could not locate def _send_temperature in the smoke script — the send-the-param coverage was removed or renamed; restore it or update this guard consciously'];
  }
  const body = m[0];
  const errors = [];
  if (!/if ingress == "direct":/.test(body)) {
    errors.push('_send_temperature lost its "direct" suppression branch — the direct-to-vendor arm has no gateway strip in the path');
  }
  if (!/ingress == "messages"/.test(body)) {
    errors.push('_send_temperature lost its claude/"messages" suppression branch — native /v1/messages forwards sampling params verbatim until the anthropic native leg strips');
  }
  // Exactly the two pinned suppressions may return False. A THIRD suppression
  // branch is how a future session would "fix" a red arm by silently killing
  // the send-the-param coverage on a whole wire — the historical regression.
  const suppressions = (body.match(/return False/g) ?? []).length;
  if (suppressions !== 2) {
    errors.push(
      `_send_temperature has ${suppressions} suppression returns; exactly 2 are sanctioned ` +
      '(direct, claude-on-messages). A vendor 400 on a gateway wire is a REAL failure — fix the strip, do not suppress the probe. ' +
      'If the suppression set legitimately changes (e.g. the anthropic native-leg slice), update this guard in the same commit.',
    );
  }
  const lastReturn = body.lastIndexOf('return ');
  if (lastReturn === -1 || !body.slice(lastReturn).trimEnd().endsWith('return True')) {
    errors.push('_send_temperature no longer ends on the `return True` fall-through (or the text pin broke on a benign edit — then update this guard consciously): the gateway-wire probes are the coverage that caught kimi-k2.7 and must not be suppressed');
  }
  // Call sites: the helper being intact means nothing if a wire's body
  // builder stops consulting it (the exact shape of the historical
  // regression: `if not is_reasoning(model)` guarding temperature). Each
  // wire has one sync + one stream call site.
  for (const wire of ['chat', 'responses', 'messages', 'gemini', 'direct']) {
    const literal = `_send_temperature(model, "${wire}")`;
    const count = smokeSource.split(literal).length - 1;
    if (count < 2) {
      errors.push(
        `smoke call-site pin: ${literal} appears ${count}× (expected ≥2 — sync + stream). ` +
        'A body builder stopped consulting the temperature decision; restore it or update this guard consciously.',
      );
    }
  }
  return errors;
}

export function runChecks(catalog, rows, smokeSource, readFile) {
  return [
    ...checkCatalogCoverage(catalog, rows),
    ...checkAnchors(catalog, rows, readFile),
    ...checkSmokeSetFresh(catalog, smokeSource),
    ...checkRegistryRowsFresh(catalog, rows),
    ...checkAnthropicNativeSetParity(catalog, rows, smokeSource),
    ...checkSendTemperatureGuard(smokeSource),
  ];
}

// ── self-test: prove every rule bites (removal → red) ───────────────────────

function selfTest() {
  const catalog = {
    providers: [
      {
        key: 'acme',
        models: [
          // id deliberately differs from code so a matcher reading the wrong
          // field fails these cases (the real catalog has azure-* codes).
          { providerModelId: 'acme-chat-1', code: 'acme-alias-1', type: 'chat', seed: { id: 'x' } },
          { providerModelId: 'acme-embed-1', code: 'acme-embed-1', type: 'embedding' },
        ],
      },
    ],
  };
  const stripsRow = {
    provider: 'acme', prefix: 'acme-chat', decision: 'strips',
    goFile: 'fake.go', goMatch: '"acme-chat"', evidence: 'Observed 400.',
  };
  const goSrc = 'var list = []string{"acme-chat"}';
  const fn = [
    '',
    'def _send_temperature(model, ingress):',
    '    """Docstring mentioning "direct" and "messages" in prose, so the',
    '    branch anchors must match the CODE, not this text."""',
    '    if model not in _REASONING_MODELS:',
    '        return True',
    '    if ingress == "direct":',
    '        return False',
    '    if model.startswith("claude-") and ingress == "messages":',
    '        return False',
    '    return True',
    '',
  ].join('\n');
  // Call sites live in other functions after a column-0 def, as in the real
  // file — the body-capture regex must stop before them.
  const callSites = 'def body_builders():\n' + ['chat', 'responses', 'messages', 'gemini', 'direct']
    .flatMap((w) => [
      `    if _send_temperature(model, "${w}"):\n`,
      `    if _send_temperature(model, "${w}"):\n`,
    ]).join('');
  const smoke = '_REASONING_MODELS = {\n    "acme-chat-1",\n}\n' + fn + callSites;
  const reader = (files) => (path) => (path in files ? files[path] : null);

  const cases = [
    ['clean config passes', () =>
      runChecks(catalog, [stripsRow], smoke, reader({ 'fake.go': goSrc })).length === 0],
    ['uncovered chat family fails', () =>
      checkCatalogCoverage(catalog, []).length === 1],
    ['prefix must anchor at the id start, not mid-string', () =>
      checkCatalogCoverage(catalog, [{ ...stripsRow, prefix: 'chat-1' }]).length === 1],
    ['prefix matches only at a version boundary (gpt-5.40 must not inherit gpt-5.4)', () => {
      const cat = { providers: [{ key: 'acme', models: [
        { providerModelId: 'acme-chat-10', code: 'acme-chat-10', type: 'chat' },
      ] }] };
      return checkCatalogCoverage(cat, [{ provider: 'acme', prefix: 'acme-chat-1', decision: 'accepts', evidence: 'Probed: 200.' }]).length === 1;
    }],
    ['non-chat models are out of scope', () =>
      !checkCatalogCoverage(catalog, [stripsRow]).some((e) => e.includes('acme-embed-1'))],
    ['strips row without goFile fails', () =>
      checkAnchors(catalog, [{ ...stripsRow, goFile: undefined }], reader({})).length === 1],
    ['strips row with missing file fails', () =>
      checkAnchors(catalog, [stripsRow], reader({})).length === 1],
    ['strips anchor removed from Go file fails', () =>
      checkAnchors(catalog, [stripsRow], reader({ 'fake.go': 'nothing here' })).length === 1],
    ['ANCHORED accepts row is verified too (allowlist mirror)', () =>
      checkAnchors(catalog, [{ ...stripsRow, decision: 'accepts' }], reader({ 'fake.go': 'allowlist emptied' }))
        .length === 1],
    ['unanchored accepts row is fine', () =>
      checkAnchors(catalog, [{ provider: 'acme', prefix: 'acme-chat', decision: 'accepts', evidence: 'Probed: 200.' }], reader({})).length === 0],
    ['strips family with no seeded model fails', () => {
      const unseeded = JSON.parse(JSON.stringify(catalog));
      delete unseeded.providers[0].models[0].seed;
      return checkAnchors(unseeded, [stripsRow], reader({ 'fake.go': goSrc }))
        .some((e) => e.includes('NO seeded'));
    }],
    ['stale smoke set entry fails', () =>
      checkSmokeSetFresh(catalog, '_REASONING_MODELS = {\n    "gone-model",\n}\n').length === 1],
    ['smoke set entry credited via code alias', () =>
      checkSmokeSetFresh(catalog, '_REASONING_MODELS = {\n    "acme-alias-1",\n}\n').length === 0],
    ['smoke set comments are ignored', () =>
      checkSmokeSetFresh(catalog, '_REASONING_MODELS = {\n    # "not-an-entry"\n    "acme-chat-1",\n}\n').length === 0],
    ['unparseable smoke set fails loudly', () =>
      checkSmokeSetFresh(catalog, 'no set here').length === 1],
    ['stale registry row fails', () =>
      checkRegistryRowsFresh(catalog, [{ ...stripsRow, prefix: 'acme-gone' }]).length === 1],
    ['catch-all registry row is never stale', () =>
      checkRegistryRowsFresh(catalog, [{ provider: 'acme', prefix: '', decision: 'forward-unprobed', evidence: 'x' }]).length === 0],
    ['anthropic stripped family missing from smoke set fails', () => {
      const anth = { providers: [{ key: 'anthropic', models: [
        { providerModelId: 'claude-new-9', code: 'claude-new-9', type: 'chat', seed: { id: 'x' } },
      ] }] };
      const row = { provider: 'anthropic', prefix: 'claude-', decision: 'strips', goFile: 'fake.go', goMatch: 'x', evidence: 'Observed 400.' };
      return checkAnthropicNativeSetParity(anth, [row], '_REASONING_MODELS = {\n}\n').length === 1;
    }],
    ['anthropic accepts row exempts the parity requirement', () => {
      const anth = { providers: [{ key: 'anthropic', models: [
        { providerModelId: 'claude-ok-1', code: 'claude-ok-1', type: 'chat' },
      ] }] };
      const rows = [
        { provider: 'anthropic', prefix: 'claude-ok-1', decision: 'accepts', evidence: 'Probed: 200.' },
        { provider: 'anthropic', prefix: 'claude-', decision: 'strips', goFile: 'fake.go', goMatch: 'x', evidence: 'Observed 400.' },
      ];
      return checkAnthropicNativeSetParity(anth, rows, '_REASONING_MODELS = {\n}\n').length === 0;
    }],
    ['_send_temperature removal fails loudly', () =>
      checkSendTemperatureGuard('_REASONING_MODELS = {\n}\n').length === 1],
    ['_send_temperature losing a suppression branch fails', () =>
      checkSendTemperatureGuard(smoke.replace('    if ingress == "direct":\n        return False\n', '')).length >= 1],
    ['_send_temperature suppressing the gateway-wire probes fails', () =>
      checkSendTemperatureGuard(smoke.replace('\n    return True\n' + callSites, '\n    return model not in _REASONING_MODELS\n' + callSites)).length === 1],
    ['a THIRD suppression branch fails (silent wire kill)', () =>
      checkSendTemperatureGuard(smoke.replace(
        '    return True\n' + callSites,
        '    if ingress == "responses":\n        return False\n    return True\n' + callSites,
      )).some((e) => e.includes('suppression returns'))],
    ['a call site reverting to is_reasoning fails', () =>
      checkSendTemperatureGuard(smoke.replace('    if _send_temperature(model, "chat"):\n', '    if not is_reasoning(model):\n'))
        .some((e) => e.includes('call-site pin'))],
    ['smoke set entry naming a non-chat model is stale', () =>
      checkSmokeSetFresh(catalog, '_REASONING_MODELS = {\n    "acme-embed-1",\n}\n').length === 1],
    ['seeded-probe credit requires the SAME provider', () => {
      const twoProv = { providers: [
        { key: 'acme', models: [{ providerModelId: 'acme-chat-1', code: 'acme-chat-1', type: 'chat' }] },
        { key: 'other', models: [{ providerModelId: 'acme-chat-1', code: 'other-alias', type: 'chat', seed: { id: 'x' } }] },
      ] };
      return checkAnchors(twoProv, [stripsRow], reader({ 'fake.go': goSrc }))
        .some((e) => e.includes('NO seeded'));
    }],
    ['registry-row freshness requires the SAME provider', () => {
      const twoProv = { providers: [
        { key: 'acme', models: [{ providerModelId: 'acme-chat-1', code: 'acme-chat-1', type: 'chat' }] },
        { key: 'other', models: [{ providerModelId: 'zeta-1', code: 'zeta-1', type: 'chat' }] },
      ] };
      return checkRegistryRowsFresh(twoProv, [{ provider: 'acme', prefix: 'zeta', decision: 'accepts', evidence: 'Probed: 200.' }]).length === 1;
    }],
  ];

  let failed = 0;
  for (const [name, fn] of cases) {
    let ok = false;
    try { ok = fn(); } catch { ok = false; }
    console.log(`${ok ? 'ok  ' : 'FAIL'} ${name}`);
    if (!ok) failed++;
  }
  if (failed) {
    console.error(`\ncheck-quirk-coverage self-test: ${failed} case(s) failed`);
    process.exit(1);
  }
  console.log('\ncheck-quirk-coverage self-test: all cases pass');
}

// ── main ─────────────────────────────────────────────────────────────────────

function main() {
  if (process.argv.includes('--self-test')) {
    selfTest();
    return;
  }
  const catalog = JSON.parse(readFileSync(resolve(REPO_ROOT, CATALOG_PATH), 'utf8'));
  const smokeSource = readFileSync(resolve(REPO_ROOT, SMOKE_PATH), 'utf8');
  const readFile = (p) => {
    const abs = resolve(REPO_ROOT, p);
    return existsSync(abs) ? readFileSync(abs, 'utf8') : null;
  };
  const errors = runChecks(catalog, families, smokeSource, readFile);
  if (errors.length) {
    console.error('✗ quirk-coverage lint failed:\n');
    for (const e of errors) console.error(`  - ${e}`);
    console.error(
      '\n  Every catalog chat family needs an explicit sampling-quirk decision.\n' +
      '  See scripts/quirk-coverage.config.mjs → "HOW TO ADD A FAMILY".',
    );
    process.exit(1);
  }
  console.log('✓ quirk-coverage: every catalog chat family has a recorded decision; strip anchors and smoke set are fresh');
}

main();
