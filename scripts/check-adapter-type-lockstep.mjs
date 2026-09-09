#!/usr/bin/env node
/**
 * The admin API's adapter-type allowlist, the gateway's provider set, the
 * published spec, and the admin UI's picker must all name the same wire
 * formats.
 *
 * Nothing else enforces it. There is no DB CHECK on provider.adapter_type, so
 * the Control Plane handler is the only gate, and control-plane does not depend
 * on the ai-gateway module — a Go test cannot see both lists. The one test that
 * looks at ValidAdapterTypes checks it against IsValidAdapterType, which is
 * built from the same slice, so it passes whatever the slice says.
 *
 * Drift is asymmetric and both directions are real:
 *   in gateway, not in CP  -> the admin API refuses a provider the gateway can
 *                             speak, so the adapter is unreachable
 *   in CP, not in gateway  -> the API accepts a provider that fails at request
 *                             time, which is the one a customer notices
 *   in both, not in spec   -> the published contract calls a value invalid
 *                             that the handler accepts, and the agent's
 *                             embedded catalog is generated from that spec
 *   not in the UI list     -> the format cannot be configured at all, and a
 *                             provider created through the API renders in a
 *                             select whose options exclude its own value
 *
 * The gateway side is read from AllFormats(), which IS the provider-adapter
 * set: format.go declares one more constant, FormatOpenAIResponses, and states
 * that it is deliberately excluded there because it is an ingress format. Read
 * the const block instead and that distinction has to be re-derived by hand,
 * which goes stale the day a second ingress format appears — and the natural
 * response to the false failure would be to add it to ValidAdapterTypes, the
 * exact defect this gate exists to prevent.
 */
import { readFileSync } from 'node:fs';

const CP = 'packages/control-plane/internal/ai/providers/handler/adapter_types.go';
const GW = 'packages/ai-gateway/internal/providers/core/format.go';
const SPEC = 'docs/users/api/openapi/control-plane/providers.yaml';
const UI = 'packages/control-plane-ui/src/pages/ai-gateway/providers/_shared/adapterTypes.ts';

// The spec states the enum once per request body that carries an adapterType:
// create, update, and the two connectivity probes. A parser that finds fewer
// has stopped seeing one of them, and an enum this gate cannot see is an enum
// it cannot keep in step.
const SPEC_ENUMS = 4;

function cpTypes() {
  const src = readFileSync(CP, 'utf8');
  const m = src.match(/ValidAdapterTypes = \[\]string\{([\s\S]*?)\n\}/);
  if (!m) throw new Error(`${CP}: ValidAdapterTypes not found — this gate has drifted from the source`);
  // Strip line comments first. A commented-out entry is how someone disables an
  // adapter, and counting it would report the CP list as accepting a value the
  // handler rejects — the precise drift this gate is here to catch.
  const body = m[1].replace(/\/\/.*$/gm, '');
  return new Set([...body.matchAll(/"([^"]+)"/g)].map((x) => x[1]));
}

function gwFormats() {
  const src = readFileSync(GW, 'utf8');

  const values = new Map();
  for (const [, name, value] of src.matchAll(/^\s*(Format\w+)\s+Format\s*=\s*"([^"]+)"/gm)) {
    values.set(name, value);
  }
  if (values.size === 0) throw new Error(`${GW}: no Format constants found — this gate has drifted from the source`);

  const fn = src.match(/func AllFormats\(\) \[\]Format \{[\s\S]*?return \[\]Format\{([\s\S]*?)\n\t\}/);
  if (!fn) throw new Error(`${GW}: AllFormats() not found — this gate has drifted from the source`);

  const out = new Set();
  for (const [, name] of fn[1].replace(/\/\/.*$/gm, '').matchAll(/^\s*(Format\w+),/gm)) {
    const v = values.get(name);
    if (!v) throw new Error(`${GW}: AllFormats() lists ${name}, which has no Format constant`);
    out.add(v);
  }
  if (out.size === 0) throw new Error(`${GW}: AllFormats() parsed to an empty set — refusing to report success`);
  return out;
}

/**
 * Every adapterType enum in the spec.
 *
 * Keyed on the `adapterType:` property, NOT on "the block lists openai". The
 * shorter key looks equivalent and is not: an enum that DROPS openai — one of
 * the drifts this gate exists to catch — stops being recognised as an
 * adapterType enum at all, so the count returns to four the moment any other
 * openai-listing enum exists, and the gate reports OK on the very change it
 * was built for.
 *
 * Both YAML sequence styles are read, and a value may be quoted or carry a
 * trailing comment. All three appear in this spec already; treating any of
 * them as unparseable would fail with "this gate has drifted from the source"
 * on a legal document.
 */
function specEnums() {
  const lines = readFileSync(SPEC, 'utf8').split('\n');
  const indentOf = (l) => l.length - l.trimStart().length;
  const clean = (v) =>
    v
      .replace(/\s+#.*$/, '')
      .trim()
      .replace(/^['"]|['"]$/g, '');

  const found = [];
  for (let i = 0; i < lines.length; i++) {
    if (lines[i].trim() !== 'adapterType:') continue;
    const propIndent = indentOf(lines[i]);
    // Walk the property's own block only: the next line at or above its
    // indent ends it, so an enum belonging to a sibling property is never
    // attributed here.
    for (let j = i + 1; j < lines.length; j++) {
      const t = lines[j].trim();
      if (t === '') continue;
      if (indentOf(lines[j]) <= propIndent) break;
      const flow = /^enum:\s*\[(.*)\]\s*$/.exec(t);
      if (flow) {
        found.push({ line: j + 1, values: new Set(flow[1].split(',').map(clean).filter(Boolean)) });
        break;
      }
      if (t !== 'enum:') continue;
      const values = [];
      for (let k = j + 1; k < lines.length && lines[k].trim().startsWith('- '); k++) {
        values.push(clean(lines[k].trim().slice(2)));
      }
      found.push({ line: j + 1, values: new Set(values) });
      break;
    }
  }
  if (found.length !== SPEC_ENUMS) {
    throw new Error(
      `${SPEC}: found ${found.length} adapterType enum(s), expected ${SPEC_ENUMS} — ` +
        `either an operation gained or lost one, or this gate has drifted from the source`,
    );
  }
  for (const { line, values } of found) {
    if (values.size === 0) {
      throw new Error(`${SPEC}:${line}: adapterType enum parsed to an empty set — refusing to report success`);
    }
  }
  return found;
}

/** `PROVIDER_ADAPTER_TYPES`, the list the admin UI offers in every provider form. */
function uiTypes() {
  const src = readFileSync(UI, 'utf8');
  const m = src.match(/PROVIDER_ADAPTER_TYPES = \[([\s\S]*?)\n\] as const;/);
  if (!m) throw new Error(`${UI}: PROVIDER_ADAPTER_TYPES not found — this gate has drifted from the source`);
  // Strip line comments first, for the same reason the CP reader does: a
  // commented-out entry is how someone disables an adapter, and counting it
  // would report the UI as offering a value it does not.
  const body = m[1].replace(/\/\/.*$/gm, '');
  const out = new Set([...body.matchAll(/'([^']+)'/g)].map((x) => x[1]));
  if (out.size === 0) throw new Error(`${UI}: PROVIDER_ADAPTER_TYPES parsed to an empty set — refusing to report success`);
  return out;
}

const cp = cpTypes();
const gw = gwFormats();
const specs = specEnums();
const ui = uiTypes();
const missingInCp = [...gw].filter((f) => !cp.has(f)).sort();
const missingInGw = [...cp].filter((f) => !gw.has(f)).sort();

const specDrift = [];
for (const { line, values } of specs) {
  const missingInSpec = [...gw].filter((f) => !values.has(f)).sort();
  const extraInSpec = [...values].filter((f) => !gw.has(f)).sort();
  if (missingInSpec.length || extraInSpec.length) {
    specDrift.push({ line, missingInSpec, extraInSpec });
  }
}

const missingInUi = [...gw].filter((f) => !ui.has(f)).sort();
const extraInUi = [...ui].filter((f) => !gw.has(f)).sort();

if (
  missingInCp.length === 0 &&
  missingInGw.length === 0 &&
  specDrift.length === 0 &&
  missingInUi.length === 0 &&
  extraInUi.length === 0
) {
  console.log(
    `[adapter-type-lockstep] OK — ${cp.size} adapter types match AllFormats() across the ` +
      `admin handler, ${specs.length} spec enums and the UI picker.`,
  );
  process.exit(0);
}

console.error('[adapter-type-lockstep] the four lists have drifted.\n');
if (missingInCp.length) {
  console.error(`  the gateway speaks these, the admin API rejects them: ${missingInCp.join(', ')}`);
  console.error(`  -> add them to ValidAdapterTypes in ${CP}`);
}
if (missingInGw.length) {
  console.error(`  the admin API accepts these, AllFormats() does not list them: ${missingInGw.join(', ')}`);
  console.error(`  -> a provider created with one of these fails at request time`);
}
if (missingInUi.length) {
  console.error(`  ${UI} omits: ${missingInUi.join(', ')}`);
  console.error(`  -> those formats cannot be configured from the admin UI at all`);
}
if (extraInUi.length) {
  console.error(`  ${UI} offers formats the gateway does not have: ${extraInUi.join(', ')}`);
  console.error(`  -> the form would submit a value the admin API rejects`);
}
for (const { line, missingInSpec, extraInSpec } of specDrift) {
  if (missingInSpec.length) {
    console.error(`  ${SPEC}:${line} omits: ${missingInSpec.join(', ')}`);
    console.error(`  -> the published contract calls these invalid; the handler accepts them`);
  }
  if (extraInSpec.length) {
    console.error(`  ${SPEC}:${line} lists formats the gateway does not have: ${extraInSpec.join(', ')}`);
  }
}
process.exit(1);
