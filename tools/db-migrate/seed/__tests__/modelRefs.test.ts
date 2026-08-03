import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFixture, reconcileRows } from '../reference/loadFixture.ts'
import { loadModelIdByCode, resolveModelRefs, toModelRefs, modelRef } from '../reference/modelRefs.ts'
import { modelTable } from './_modelTable.ts'

/**
 * Seed the real Model fixture into a database that already holds every catalog
 * model under a DIFFERENT id — the state of any deployment whose admins added
 * models through the provider wizard before the catalog shipped them. Seeding
 * matches each row on its code and keeps the live id, so every id the fixture
 * carries is dead the moment the run finishes.
 */
async function afterAdoptingLiveModels() {
  const fixtureModels = readFixture('Model')
  const live = fixtureModels.map((m) => ({ id: `live-${String(m.code)}`, code: m.code }))
  const table = modelTable(live)
  // The identity list REFERENCE_TABLES seeds Model under: every unique key the
  // table has besides id.
  await reconcileRows(table.delegate as never, fixtureModels, ['code', ['providerId', 'providerModelId']])
  const idByCode = await loadModelIdByCode(table.delegate)
  return { fixtureModels, idByCode, table }
}

test('seeding Model adopts the live rows, so every fixture id is dead afterwards', async () => {
  const { fixtureModels, idByCode, table } = await afterAdoptingLiveModels()
  assert.equal(table.creates.length, 0, 'every model matched a live row, so none is created')
  assert.equal(idByCode.get('gpt-4o-mini'), 'live-gpt-4o-mini')
  const fixtureIds = new Set(fixtureModels.map((m) => String(m.id)))
  for (const id of idByCode.values()) {
    assert.ok(!fixtureIds.has(id), `model kept live id, not the fixture's (${id})`)
  }
})

test('ai_guard_config resolves its judge model to the adopted id', async () => {
  const { fixtureModels, idByCode } = await afterAdoptingLiveModels()
  const rows = resolveModelRefs(readFixture('ai_guard_config'), idByCode, 'ai_guard_config')

  // The judge is named by code, so it reaches the row that survived seeding.
  // model_id has no foreign key: a dead id here leaves AI Guard's
  // configured_provider backend pointing at nothing, with nothing raised.
  assert.equal(rows[0].model_id, 'live-gpt-4o-mini')
  assertNoFixtureIdSurvives(rows, fixtureModels, 'ai_guard_config')
})

test('RoutingRule resolves every model in its strategy, matches and fallbacks', async () => {
  const { fixtureModels, idByCode } = await afterAdoptingLiveModels()
  const rows = resolveModelRefs(readFixture('RoutingRule'), idByCode, 'RoutingRule')

  const smart = rows.find((r) => r.name === 'smart-auto-routing') as Record<string, never>
  const cfg = smart.config as Record<string, unknown>
  assert.equal(cfg.routerModelId, 'live-gpt-4o', 'the router model the smart strategy calls')
  assert.equal(cfg.defaultModelId, 'live-gpt-4o-mini', 'the model it falls back to')
  assert.deepEqual(
    (smart.fallbackChain as { modelId: string }[]).map((e) => e.modelId),
    ['live-gpt-4o-mini', 'live-moonshot-v1-128k'],
  )

  // matchConditions.models decides whether a rule fires at all: a dead id here
  // silently stops the rule matching any request.
  const costAware = rows.find((r) => r.name === 'cost-aware-routing') as Record<string, never>
  assert.deepEqual((costAware.matchConditions as { models: string[] }).models, [
    'live-gpt-4o',
    'live-gpt-4o-mini',
  ])

  // A weighted target names a model nested two levels inside the strategy tree.
  const lb = rows.find((r) => r.name === 'load-balance-mini') as Record<string, never>
  assert.deepEqual(
    ((lb.config as { weightedTargets: { node: { modelId: string } }[] }).weightedTargets).map(
      (t) => t.node.modelId,
    ),
    ['live-gpt-4o-mini', 'live-gemini-2.5-flash'],
  )

  assertNoFixtureIdSurvives(rows, fixtureModels, 'RoutingRule')
})

test('demo VirtualKey resolves every allowed model to the adopted id', async () => {
  const { fixtureModels, idByCode } = await afterAdoptingLiveModels()
  const rows = resolveModelRefs(readFixture('demo/VirtualKey'), idByCode, 'demo/VirtualKey')

  // allowedModels is the VK's allow-list: a dead id denies the model outright.
  const demo01 = rows.find((r) => r.name === 'demo01') as Record<string, never>
  assert.deepEqual(
    (demo01.allowedModels as { modelId: string }[]).map((m) => m.modelId),
    [
      'live-claude-haiku-4-5',
      'live-claude-opus-4-7',
      'live-claude-sonnet-4-6',
      'live-gpt-4o-mini',
      'live-gpt-5.5',
      'live-gpt-5.4-nano',
    ],
  )
  assertNoFixtureIdSurvives(rows, fixtureModels, 'demo/VirtualKey')
})

/** No id the Model fixture carries may survive into a seeded dependent row. */
function assertNoFixtureIdSurvives(
  rows: unknown,
  fixtureModels: Record<string, unknown>[],
  where: string,
) {
  const json = JSON.stringify(rows)
  for (const m of fixtureModels) {
    assert.ok(
      !json.includes(String(m.id)),
      `${where} still carries the fixture id for ${String(m.code)} (${String(m.id)}), ` +
        `which names no row once the live model is adopted`,
    )
  }
}

test('a model reference no catalog row provides fails the seed instead of dangling', async () => {
  const { idByCode } = await afterAdoptingLiveModels()
  assert.throws(
    () => resolveModelRefs({ modelId: modelRef('gemini-2.0-flash') }, idByCode, 'RoutingRule'),
    /references model "gemini-2.0-flash", which no Model row provides/,
  )
})

test('a model reference in a fixture seeded before Model fails rather than passing through', () => {
  assert.throws(
    () => resolveModelRefs({ modelId: modelRef('gpt-4o') }, null, 'Provider'),
    /is seeded before Model/,
  )
})

test('resolveModelRefs leaves every value that is not a model reference untouched', async () => {
  const { idByCode } = await afterAdoptingLiveModels()
  const row = {
    providerId: '6b6d307f-a80b-4dcb-801b-1ffa07e25cab',
    // allowedModels also accepts globs, which name no single row.
    allowedModels: [{ modelId: 'gpt-*', providerId: 'p1' }],
    systemPrompt: 'Return ONLY valid JSON: {"modelId": "<exact ID from list>"}',
    enabled: true,
    retryPolicy: null,
    priority: 25,
    updatedAt: new Date('2026-05-14T17:12:19.657Z'),
  }
  assert.deepEqual(resolveModelRefs(row, idByCode, 'RoutingRule'), row)
})

test('every model a committed fixture names is a model the catalog ships', () => {
  const codes = new Set(readFixture('Model').map((m) => String(m.code)))
  for (const fixture of ['ai_guard_config', 'RoutingRule', 'demo/VirtualKey']) {
    for (const ref of collectModelRefs(readFixture(fixture))) {
      assert.ok(
        codes.has(ref),
        `${fixture} references model "${ref}", which the catalog does not ship — ` +
          `the seed would abort on it`,
      )
    }
  }
})

/** Every `model:<code>` code appearing anywhere in a fixture. */
function collectModelRefs(value: unknown, found: string[] = []): string[] {
  if (typeof value === 'string') {
    if (value.startsWith('model:')) found.push(value.slice('model:'.length))
  } else if (Array.isArray(value)) {
    for (const v of value) collectModelRefs(v, found)
  } else if (value !== null && typeof value === 'object') {
    for (const v of Object.values(value)) collectModelRefs(v, found)
  }
  return found
}

// ─── Capturing fixtures from a database ──────────────────────────────────────

const SOURCE_MODELS = new Map([
  ['aaaaaaaa-1111-4111-8111-111111111111', 'gpt-4o'],
  ['bbbbbbbb-2222-4222-8222-222222222222', 'gpt-4o-mini'],
])

test('capturing a routing rule names its models by code and leaves providers alone', () => {
  const captured = toModelRefs(
    {
      name: 'cost-aware-routing',
      config: {
        default: { modelId: 'aaaaaaaa-1111-4111-8111-111111111111', providerId: 'prov-1' },
        conditions: [{ then: { modelId: 'bbbbbbbb-2222-4222-8222-222222222222' } }],
      },
      matchConditions: { models: ['aaaaaaaa-1111-4111-8111-111111111111'] },
      fallbackChain: [{ modelId: 'bbbbbbbb-2222-4222-8222-222222222222', providerId: 'prov-1' }],
    },
    SOURCE_MODELS,
    null,
    'RoutingRule',
  )

  assert.deepEqual(captured, {
    name: 'cost-aware-routing',
    config: {
      // providerId is a plain id the fixture keeps: providers are seeded by id.
      default: { modelId: 'model:gpt-4o', providerId: 'prov-1' },
      conditions: [{ then: { modelId: 'model:gpt-4o-mini' } }],
    },
    matchConditions: { models: ['model:gpt-4o'] },
    fallbackChain: [{ modelId: 'model:gpt-4o-mini', providerId: 'prov-1' }],
  })
})

test('capturing keeps an allowed-model glob, which names no single row', () => {
  const captured = toModelRefs(
    { allowedModels: [{ modelId: 'gpt-*', providerId: 'prov-1' }] },
    SOURCE_MODELS,
    null,
    'demo/VirtualKey',
  )
  assert.deepEqual(captured, { allowedModels: [{ modelId: 'gpt-*', providerId: 'prov-1' }] })
})

test('capturing a model id the source database no longer provides fails the extraction', () => {
  assert.throws(
    () =>
      toModelRefs(
        { modelId: 'cccccccc-3333-4333-8333-333333333333' },
        SOURCE_MODELS,
        null,
        'RoutingRule',
      ),
    /holds model id "cccccccc-3333-4333-8333-333333333333", which no Model row provides/,
  )
})

test('a captured fixture replayed into a database that renamed the ids follows the models', () => {
  // Capture against one database, seed into another whose model rows hold
  // entirely different ids: the reference survives the move, the id would not.
  const captured = toModelRefs(
    { model_id: 'bbbbbbbb-2222-4222-8222-222222222222' },
    SOURCE_MODELS,
    null,
    'ai_guard_config',
  )
  const elsewhere = new Map([['gpt-4o-mini', 'a-completely-different-id']])
  assert.deepEqual(resolveModelRefs(captured, elsewhere, 'ai_guard_config'), {
    model_id: 'a-completely-different-id',
  })
})

test('the committed fixtures name their models by code, never by a raw id', () => {
  const idKeys = new Set(['modelId', 'model_id', 'routerModelId', 'defaultModelId', 'models'])
  const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i
  const offenders: string[] = []

  const walk = (value: unknown, key: string | null, where: string) => {
    if (Array.isArray(value)) {
      for (const v of value) walk(v, key, where)
    } else if (value !== null && typeof value === 'object') {
      for (const [k, v] of Object.entries(value)) walk(v, k, where)
    } else if (typeof value === 'string' && key && idKeys.has(key) && uuid.test(value)) {
      offenders.push(`${where}.${key} = ${value}`)
    }
  }

  for (const fixture of ['ai_guard_config', 'RoutingRule', 'demo/VirtualKey']) {
    walk(readFixture(fixture), null, fixture)
  }
  assert.deepEqual(
    offenders,
    [],
    `a fixture hardcodes a Model id; seeding adopts live rows and keeps their ids, ` +
      `so the hardcoded one names nothing. Name the model by code instead.`,
  )
})
