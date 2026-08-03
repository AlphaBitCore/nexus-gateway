import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

import { loadCatalog, buildAll } from '../../gen-model-catalog.mjs'

const TEMPLATE_NUMERICS = [
  'inputPricePerMillion',
  'outputPricePerMillion',
  'cachedInputReadPricePerMillion',
  'cachedInputWritePricePerMillion',
  'maxContextTokens',
  'maxOutputTokens',
]

const pathEndingWith = (files: Map<string, string>, suffix: string) =>
  [...files.keys()].find((p) => p.endsWith(suffix))!

// The core guarantee: the committed Model.json + provider-templates are exactly what
// the generator produces from model-catalog.json. This is what CI's check enforces.
test('generated files match the committed source of truth (no drift)', () => {
  const files = buildAll(loadCatalog())
  assert.ok(files.size >= 21, `expected >=21 generated files, got ${files.size}`)
  for (const [path, content] of files) {
    assert.equal(content, readFileSync(path, 'utf8'), `drift in ${path}`)
  }
})

// The gate has teeth: a single changed vendor fact in the catalog changes the output.
test('drift is detected — mutating one catalog fact changes the generated output', () => {
  const catalog = loadCatalog()
  const before = buildAll(catalog)
  const prov = catalog.providers.find((p: any) => p.models.some((m: any) => m.seed))
  const m = prov.models.find((x: any) => x.seed)
  m.maxContextTokens = (m.maxContextTokens ?? 0) + 12345
  const after = buildAll(catalog)
  const modelJson = pathEndingWith(before, 'seed/fixtures/Model.json')
  assert.notEqual(after.get(modelJson), before.get(modelJson))
})

// Model.json is exactly the seeded subset, id-stable and id-sorted (upsert key is id).
test('Model.json holds exactly the seeded models, each with an id, sorted by id', () => {
  const catalog = loadCatalog()
  const seeded = catalog.providers.flatMap((p: any) => (p.models || []).filter((m: any) => m.seed))
  const files = buildAll(catalog)
  const rows = JSON.parse(files.get(pathEndingWith(files, 'seed/fixtures/Model.json'))!)
  assert.equal(rows.length, seeded.length)
  assert.ok(rows.every((r: any) => typeof r.id === 'string' && r.id.length > 0))
  const ids = rows.map((r: any) => r.id)
  assert.deepEqual(ids, [...ids].sort())
})

// Template models are the wizard's ApiTemplateModel projection: no lifecycle `status`,
// and optional numerics are omitted (never emitted as null) so the `?: number` type holds.
test('template models carry no status and no null optional numerics', () => {
  const files = buildAll(loadCatalog())
  for (const [path, content] of files) {
    if (!path.includes('provider-templates') || path.endsWith('index.json')) continue
    for (const m of JSON.parse(content).models) {
      assert.ok(!('status' in m), `${path} ${m.code}: unexpected status field`)
      for (const f of TEMPLATE_NUMERICS) {
        assert.ok(!(f in m) || m[f] !== null, `${path} ${m.code}.${f} is null (should be omitted)`)
      }
      assert.ok(!('aliases' in m) || (Array.isArray(m.aliases) && m.aliases.length > 0),
        `${path} ${m.code}.aliases is present but empty (should be omitted)`)
    }
  }
})

// A model's aliases reach BOTH consumers, because they are identity rather than
// seed state. The wizard needs them to recognise a provider row still carrying a
// name the catalog renamed away from; without them that row reads as unknown and
// the admin is offered a duplicate of a model they already have.
test('every alias in the catalog reaches both Model.json and the provider template', () => {
  const catalog = loadCatalog()
  const files = buildAll(catalog)
  const fixture = JSON.parse(files.get([...files.keys()].find((p) => p.endsWith('Model.json'))!)!)

  let checked = 0
  for (const p of catalog.providers) {
    for (const m of p.models ?? []) {
      if (!m.aliases?.length) continue
      checked++
      if (m.seed) {
        const row = fixture.find((r: { code: string }) => r.code === m.code)
        assert.deepEqual(row.aliases, m.aliases, `Model.json ${m.code}: aliases not carried`)
      }
      if (m.inTemplate) {
        const path = [...files.keys()].find((k) => k.endsWith(`/${p.key}.json`))!
        const tm = JSON.parse(files.get(path)!).models.find((x: { code: string }) => x.code === m.code)
        assert.deepEqual(tm.aliases, m.aliases, `${p.key}.json ${m.code}: aliases not projected`)
      }
    }
  }
  assert.ok(checked > 0, 'no aliased model in the catalog — this test would pass vacuously')
})

// index.json must agree with the detail files it lists (the wizard trusts modelCount).
test('index.json modelCount equals each template model count', () => {
  const files = buildAll(loadCatalog())
  const index = JSON.parse(files.get(pathEndingWith(files, 'provider-templates/index.json'))!)
  for (const e of index.templates) {
    const detail = JSON.parse(files.get(pathEndingWith(files, `provider-templates/${e.name}.json`))!)
    assert.equal(e.modelCount, detail.models.length, `modelCount mismatch for ${e.name}`)
  }
})
