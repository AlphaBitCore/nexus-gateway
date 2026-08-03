#!/usr/bin/env node
/**
 * Model-catalog generator — the single source of truth is `model-catalog.json`.
 *
 * WHY: the same per-model fact (price, context/output caps, features) used to
 * live in three hand-maintained copies — the seed fixture, the provider-wizard
 * templates, and the prod DB — and they drifted (a stale 131072 output cap
 * shipped a prod 400). This generator makes drift impossible by construction:
 * humans edit ONLY `model-catalog.json`; the two committed consumers are
 * regenerated from it and byte-checked in CI by `npm run check:model-catalog`.
 *
 * CONSUMERS GENERATED FROM THE CATALOG:
 *   1. tools/db-migrate/seed/fixtures/Model.json
 *        Flat list of the SEEDED models (those carrying a `seed` block), full
 *        DB-row shape, sorted by `id`. Upserted by the reference seed.
 *   2. packages/control-plane-ui/public/provider-templates/<key>.json
 *        One file per provider that has a `template` block; its `models[]` is
 *        every catalog model with `inTemplate: true`, projected to the wizard's
 *        12 vendor-fact fields (ApiTemplateModel).
 *   3. packages/control-plane-ui/public/provider-templates/index.json
 *        The wizard's provider list with per-provider `modelCount`.
 *
 * CATALOG SCHEMA (`model-catalog.json`):
 *   { providers: [ ProviderBlock, ... ] }
 *   ProviderBlock = {
 *     key: string,                 // provider code (== template filename stem)
 *     seedProviderId?: string,     // Provider.json UUID; required iff any model is seeded
 *     template?: {                 // present iff this provider emits a wizard template + index entry
 *       name, displayName, description, baseUrl, adapterType
 *     },
 *     models: [ ModelEntry, ... ]
 *   }
 *   ModelEntry = {
 *     // shared vendor facts (drift-prone data — the reason this file exists):
 *     code, name, description, providerModelId, type, features,
 *     aliases?,                    // other names the vendor answers to for this
 *                                  // same model; identity, not seed state, so it
 *                                  // reaches BOTH consumers
 *     inputPricePerMillion, outputPricePerMillion,
 *     cachedInputReadPricePerMillion, cachedInputWritePricePerMillion,
 *     maxContextTokens, maxOutputTokens,
 *     inTemplate: boolean,         // emit into the provider's wizard template models[]
 *     seed?: {                     // present iff this model is a Model.json seed row
 *       id, status, deprecationDate, replacedBy, enabled,
 *       inputModalities, outputModalities, lifecycle, capabilityJson,
 *       createdAt, updatedAt
 *     }
 *   }
 *
 * A model with a `seed` block goes to Model.json; with `inTemplate: true` it goes
 * to its provider template. The two are independent: a model can be in both,
 * either, and (in the seed case) under a provider that has no template.
 *
 * NUMBER / STRING FORMATTING: every output is `JSON.stringify(value, null, 2) + "\n"`.
 * That reproduces the seed fixture's existing bytes exactly (whole floats render
 * as integers, e.g. 5 not 5.0; raw UTF-8, no \u escapes). Do not hand-format the
 * generated files — edit the catalog and regenerate.
 *
 * USAGE:
 *   node gen-model-catalog.mjs           # write the generated files
 *   node gen-model-catalog.mjs --check   # regenerate in memory, assert byte-equality, exit 1 on drift
 */
import { readFileSync, writeFileSync, readdirSync, rmSync } from 'node:fs';
import { resolve, dirname, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const CATALOG = join(HERE, 'model-catalog.json');
const MODEL_FIXTURE = join(HERE, 'seed/fixtures/Model.json');
const TEMPLATES_DIR = resolve(HERE, '../../packages/control-plane-ui/public/provider-templates');

// DB-row field order for Model.json (must match the committed fixture exactly).
const MODEL_ROW_ORDER = [
  'id', 'code', 'name', 'description', 'providerId', 'providerModelId', 'type', 'features',
  'inputPricePerMillion', 'outputPricePerMillion',
  'cachedInputReadPricePerMillion', 'cachedInputWritePricePerMillion',
  'maxContextTokens', 'maxOutputTokens',
  'status', 'deprecationDate', 'replacedBy', 'aliases', 'enabled',
  'inputModalities', 'outputModalities', 'lifecycle', 'capabilityJson',
  'createdAt', 'updatedAt',
];
// Optional numeric fields on a template model, emitted (in this order) only when non-null.
const TEMPLATE_OPTIONAL_NUMERIC = [
  'inputPricePerMillion', 'outputPricePerMillion',
  'cachedInputReadPricePerMillion', 'cachedInputWritePricePerMillion',
  'maxContextTokens', 'maxOutputTokens',
];

const serialize = (value) => JSON.stringify(value, null, 2) + '\n';

function loadCatalog() {
  const catalog = JSON.parse(readFileSync(CATALOG, 'utf8'));
  validateCatalog(catalog);
  return catalog;
}

/** Fail loudly on catalog shapes the generator cannot faithfully emit. */
function validateCatalog(catalog) {
  if (!catalog || !Array.isArray(catalog.providers)) {
    throw new Error('model-catalog.json: missing top-level "providers" array');
  }
  const seenModelId = new Map();
  for (const p of catalog.providers) {
    if (!p.key) throw new Error('provider block missing "key"');
    const seenCode = new Set();
    for (const m of p.models || []) {
      if (!m.code) throw new Error(`${p.key}: model missing "code"`);
      if (seenCode.has(m.code)) throw new Error(`${p.key}: duplicate model code "${m.code}"`);
      seenCode.add(m.code);
      if (m.seed) {
        if (!p.seedProviderId) throw new Error(`${p.key}/${m.code}: seeded model but provider has no seedProviderId`);
        if (!m.seed.id) throw new Error(`${p.key}/${m.code}: seed block missing "id"`);
        const dup = seenModelId.get(m.seed.id);
        if (dup) throw new Error(`duplicate seed id ${m.seed.id} on ${dup} and ${p.key}/${m.code}`);
        seenModelId.set(m.seed.id, `${p.key}/${m.code}`);
      }
      if (m.inTemplate && !p.template) {
        throw new Error(`${p.key}/${m.code}: inTemplate=true but provider has no template block`);
      }
    }
    // The wizard fetches `<name>.json`, so the template's name must equal the
    // file-stem key; otherwise index.json points at a file we never write.
    if (p.template && p.template.name !== p.key) {
      throw new Error(`${p.key}: template.name "${p.template.name}" must equal provider key`);
    }
  }
}

/** Model.json — every seeded model, full DB-row shape, sorted by id. */
function buildModelFixture(catalog) {
  const rows = [];
  for (const p of catalog.providers) {
    for (const m of p.models || []) {
      if (!m.seed) continue;
      const row = {
        id: m.seed.id,
        code: m.code,
        name: m.name,
        description: m.description,
        providerId: p.seedProviderId,
        providerModelId: m.providerModelId,
        type: m.type,
        features: m.features,
        inputPricePerMillion: m.inputPricePerMillion,
        outputPricePerMillion: m.outputPricePerMillion,
        cachedInputReadPricePerMillion: m.cachedInputReadPricePerMillion,
        cachedInputWritePricePerMillion: m.cachedInputWritePricePerMillion,
        maxContextTokens: m.maxContextTokens,
        maxOutputTokens: m.maxOutputTokens,
        status: m.seed.status,
        deprecationDate: m.seed.deprecationDate,
        replacedBy: m.seed.replacedBy,
        aliases: m.aliases ?? [],
        enabled: m.seed.enabled,
        inputModalities: m.seed.inputModalities,
        outputModalities: m.seed.outputModalities,
        lifecycle: m.seed.lifecycle,
        capabilityJson: m.seed.capabilityJson,
        createdAt: m.seed.createdAt,
        updatedAt: m.seed.updatedAt,
      };
      // Assert the literal above stays in sync with the fixture's column order.
      const keys = Object.keys(row);
      if (keys.length !== MODEL_ROW_ORDER.length || keys.some((k, i) => k !== MODEL_ROW_ORDER[i])) {
        throw new Error(`Model.json row field order drift for ${m.code}`);
      }
      rows.push(row);
    }
  }
  rows.sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
  return serialize(rows);
}

/** One template model, projected to the wizard's vendor-fact fields (null numerics omitted). */
function templateModel(m) {
  const o = {
    code: m.code,
    name: m.name,
    description: m.description,
    providerModelId: m.providerModelId,
    type: m.type,
    features: m.features,
  };
  // Aliases are part of the model's identity, so the wizard needs them to
  // recognise a provider row still carrying a name this catalog has since
  // renamed away from — without them such a row reads as unknown and gets
  // duplicated. Omitted when empty, like the null numerics below.
  if (Array.isArray(m.aliases) && m.aliases.length > 0) o.aliases = m.aliases;
  for (const f of TEMPLATE_OPTIONAL_NUMERIC) {
    if (m[f] !== null && m[f] !== undefined) o[f] = m[f];
  }
  return o;
}

/** Map of "<key>.json" -> serialized template detail file. */
function buildTemplateFiles(catalog) {
  const out = {};
  for (const p of catalog.providers) {
    if (!p.template) continue;
    const detail = {
      name: p.template.name,
      displayName: p.template.displayName,
      description: p.template.description,
      baseUrl: p.template.baseUrl,
      adapterType: p.template.adapterType,
      models: (p.models || []).filter((m) => m.inTemplate).map(templateModel),
    };
    out[`${p.key}.json`] = serialize(detail);
  }
  return out;
}

/** index.json — provider list with per-provider modelCount. */
function buildIndex(catalog) {
  const templates = [];
  for (const p of catalog.providers) {
    if (!p.template) continue;
    templates.push({
      name: p.template.name,
      displayName: p.template.displayName,
      description: p.template.description,
      baseUrl: p.template.baseUrl,
      adapterType: p.template.adapterType,
      modelCount: (p.models || []).filter((m) => m.inTemplate).length,
    });
  }
  return serialize({ templates });
}

function buildAll(catalog) {
  const files = new Map();
  files.set(MODEL_FIXTURE, buildModelFixture(catalog));
  files.set(join(TEMPLATES_DIR, 'index.json'), buildIndex(catalog));
  for (const [name, content] of Object.entries(buildTemplateFiles(catalog))) {
    files.set(join(TEMPLATES_DIR, name), content);
  }
  return files;
}

// Exported for the unit test (drives buildAll against the committed catalog).
export { CATALOG, loadCatalog, buildAll, buildModelFixture, buildTemplateFiles, buildIndex };

function main() {
  const check = process.argv.includes('--check');
  const catalog = loadCatalog();
  const files = buildAll(catalog);

  // Template files the catalog no longer produces (e.g. a provider whose `template`
  // block was removed): stale on disk and must be flagged/removed, else the check
  // would pass green with an orphan the wizard could still fetch.
  const producedTemplateFiles = new Set(
    [...files.keys()].filter((p) => p.startsWith(TEMPLATES_DIR + '/')).map((p) => p.slice(TEMPLATES_DIR.length + 1)),
  );
  const orphans = readdirSync(TEMPLATES_DIR)
    .filter((f) => f.endsWith('.json') && !producedTemplateFiles.has(f))
    .map((f) => join(TEMPLATES_DIR, f));

  if (check) {
    const drifted = [];
    for (const [path, content] of files) {
      let current = null;
      try { current = readFileSync(path, 'utf8'); } catch { /* missing file = drift */ }
      if (current !== content) drifted.push(path);
    }
    if (drifted.length || orphans.length) {
      console.error('check:model-catalog FAILED — generated files are out of sync with model-catalog.json:');
      for (const p of drifted) console.error(`  drift:  ${p.replace(resolve(HERE, '../..') + '/', '')}`);
      for (const p of orphans) console.error(`  orphan: ${p.replace(resolve(HERE, '../..') + '/', '')} (no provider in the catalog produces it)`);
      console.error('\nFix: edit tools/db-migrate/model-catalog.json (the source of truth), then run');
      console.error('  node tools/db-migrate/gen-model-catalog.mjs');
      process.exit(1);
    }
    console.log(`check:model-catalog OK — ${files.size} generated files match model-catalog.json`);
    return;
  }

  for (const [path, content] of files) writeFileSync(path, content);
  for (const p of orphans) rmSync(p);
  const removed = orphans.length ? ` (removed ${orphans.length} orphan template file(s))` : '';
  console.log(`gen-model-catalog: wrote ${files.size} files from model-catalog.json${removed}`);
}

// Run only when invoked directly (`node gen-model-catalog.mjs`), not on import.
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main();
}
