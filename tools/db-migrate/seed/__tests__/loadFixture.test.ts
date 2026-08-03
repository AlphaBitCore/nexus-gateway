import { test } from 'node:test'
import assert from 'node:assert/strict'
import { upsertRows, reconcileRows, createMissingRows } from '../reference/loadFixture.ts'
import { modelTable } from './_modelTable.ts'

function recordingDelegate() {
  const calls: { where: unknown; create: unknown; update: unknown }[] = []
  return {
    calls,
    delegate: {
      upsert: async (args: { where: unknown; create: unknown; update: unknown }) => {
        calls.push(args)
        return args.create
      },
    },
  }
}

test('upsertRows upserts every row keyed by id and is idempotent', async () => {
  const { calls, delegate } = recordingDelegate()
  const rows = [{ id: 'a', name: 'A' }, { id: 'b', name: 'B' }]
  const n = await upsertRows(delegate as never, rows, 'id')
  assert.equal(n, 2)
  assert.deepEqual(calls[0].where, { id: 'a' })
  assert.deepEqual(calls[0].create, { id: 'a', name: 'A' })
  assert.deepEqual(calls[1].where, { id: 'b' })
})

test('upsertRows never carries id into update, so an existing row keeps its identity', async () => {
  const { calls, delegate } = recordingDelegate()
  // Model.json shape: keyed by `code`, but every row also carries the fixture's
  // own id. A row an admin created through the wizard already holds a different
  // id; rewriting it would orphan traffic_event.model_id and
  // VirtualKey.allowedModels[].modelId, neither of which is a foreign key.
  await upsertRows(
    delegate as never,
    [{ id: 'fixture-uuid', code: 'claude-opus-4-8', maxOutputTokens: 128000 }],
    'code',
  )
  assert.deepEqual(calls[0].where, { code: 'claude-opus-4-8' })
  assert.deepEqual(calls[0].create, {
    id: 'fixture-uuid',
    code: 'claude-opus-4-8',
    maxOutputTokens: 128000,
  })
  assert.deepEqual(calls[0].update, { code: 'claude-opus-4-8', maxOutputTokens: 128000 })
  assert.ok(!('id' in (calls[0].update as object)), 'update must not repoint the primary key')
})

test('upsertRows still refreshes every non-identity field on an existing row', async () => {
  const { calls, delegate } = recordingDelegate()
  await upsertRows(
    delegate as never,
    [{ id: 'x', name: 'admin-policy', document: { v: 2 } }],
    'name',
  )
  // The fixture stays the source of truth for content — only identity is spared.
  assert.deepEqual(calls[0].update, { name: 'admin-policy', document: { v: 2 } })
})

test('upsertRows throws when a row lacks the key field', async () => {
  const delegate = { upsert: async () => ({}) }
  await assert.rejects(
    () => upsertRows(delegate as never, [{ name: 'no-id' }], 'id'),
    /missing key field "id"/,
  )
})

/**
 * One fake, enforcing Model's real unique keys and writing updates through, so a
 * reconcile that would collide in the database collides here too.
 */
const reconcileDelegate = modelTable

test('reconcileRows renames a row matched by id, preserving its identity', async () => {
  // The catalog moved claude-sonnet-4-5-20250929 to the floating code while
  // keeping the seed id. Matching on code alone would create a second row and
  // die on the primary key the old row still holds.
  const { updates, creates, delegate } = reconcileDelegate([
    { id: 'seed-uuid', code: 'claude-sonnet-4-5-20250929' },
  ])
  await reconcileRows(delegate as never, [{ id: 'seed-uuid', code: 'claude-sonnet-4-5', maxOutputTokens: 64000 }], ['code'])
  assert.equal(creates.length, 0, 'must update the existing row, not create a duplicate')
  assert.deepEqual(updates[0].where, { id: 'seed-uuid' })
  assert.deepEqual(updates[0].data, { code: 'claude-sonnet-4-5', maxOutputTokens: 64000 })
})

test('reconcileRows updates an admin-created row matched by code, keeping its id', async () => {
  // The provider wizard created this row, so it holds a generated id. Matching
  // on id alone would create and die on the code unique constraint.
  const { updates, creates, delegate } = reconcileDelegate([
    { id: 'wizard-uuid', code: 'claude-fable-5' },
  ])
  await reconcileRows(delegate as never, [{ id: 'fixture-uuid', code: 'claude-fable-5', maxOutputTokens: 128000 }], ['code'])
  assert.equal(creates.length, 0)
  assert.deepEqual(updates[0].where, { id: 'wizard-uuid' }, 'the row keeps the id its traffic_event rows reference')
  assert.ok(!('id' in updates[0].data), 'update must never repoint the primary key')
})

test('reconcileRows adopts an admin-created row still under the pre-rename name', async () => {
  // The live deployment case: a row added through the UI before the catalog
  // renamed the code, so it shares neither the fixture's id nor its new code —
  // only the old name, which the fixture now carries as an alias. Without alias
  // matching this creates a second row and strands the original, along with the
  // traffic_event rows pointing at its id.
  const { updates, creates, delegate } = reconcileDelegate([
    { id: 'wizard-uuid', code: 'claude-haiku-4-5-20251001' },
  ])
  await reconcileRows(
    delegate as never,
    [{ id: 'fixture-uuid', code: 'claude-haiku-4-5', aliases: ['claude-haiku-4-5-20251001'], maxOutputTokens: 64000 }],
    ['code'],
  )
  assert.equal(creates.length, 0, 'must adopt the existing row, not strand it behind a duplicate')
  assert.deepEqual(updates[0].where, { id: 'wizard-uuid' })
  assert.equal((updates[0].data as Record<string, unknown>).code, 'claude-haiku-4-5')
  assert.equal((updates[0].data as Record<string, unknown>).maxOutputTokens, 64000)
})

test('reconcileRows creates a row that matches on no identity at all', async () => {
  const { updates, creates, delegate } = reconcileDelegate([])
  await reconcileRows(delegate as never, [{ id: 'new-uuid', code: 'claude-sonnet-5' }], ['code'])
  assert.equal(updates.length, 0)
  assert.deepEqual(creates[0], { id: 'new-uuid', code: 'claude-sonnet-5' })
})

test('reconcileRows adopts a wizard row that shares only (providerId, providerModelId)', async () => {
  // The admin API exposes Code and ProviderModelID independently, so the wizard
  // can add gpt-5.6 under the name my-gpt-5.6. That row matches the fixture on
  // neither id nor code nor alias — only on Model's third unique key. Without it
  // in the identity list the reconcile creates, and the DB rejects on
  // @@unique([providerId, providerModelId]).
  const { updates, creates, delegate } = reconcileDelegate([
    { id: 'wizard-uuid', code: 'my-gpt-5.6', providerId: 'p-openai', providerModelId: 'gpt-5.6' },
  ])
  await reconcileRows(
    delegate as never,
    [{ id: 'fixture-uuid', code: 'gpt-5.6', providerId: 'p-openai', providerModelId: 'gpt-5.6', maxOutputTokens: 128000 }],
    ['code', ['providerId', 'providerModelId']],
  )
  assert.equal(creates.length, 0, 'must adopt the wizard row, not create against its unique key')
  assert.deepEqual(updates[0].where, { id: 'wizard-uuid' }, 'the wizard row keeps the id its traffic_event rows reference')
  assert.equal((updates[0].data as Record<string, unknown>).maxOutputTokens, 128000)
})

test('reconcileRows refuses to let two fixture rows claim one live row', async () => {
  // An admin renamed a seeded row's code to a string the catalog later assigns
  // to a DIFFERENT model, so one live row satisfies row A by id and row B by
  // code. Silently skipping B is worse than last-write-wins: the live row keeps
  // A's identity, B never lands, and every traffic_event recorded against that
  // id as model B now resolves to model A — historical cost and attribution
  // read the wrong model's prices.
  // Order decides the outcome: with A first the reconcile self-heals (A updates
  // the row, B then matches nothing and creates cleanly). With B first, B
  // updates the live row and A — matching the SAME row by id — overwrites it, so
  // model B is silently gone and no error is raised.
  const { creates, updates, delegate } = reconcileDelegate([
    { id: 'idA', code: 'codeB', providerId: 'p', providerModelId: 'pmB' },
  ])
  await assert.rejects(
    () =>
      reconcileRows(
        delegate as never,
        [
          { id: 'idB', code: 'codeB', providerId: 'p', providerModelId: 'pmB' },
          { id: 'idA', code: 'codeA', providerId: 'p', providerModelId: 'pmA' },
        ],
        ['code', ['providerId', 'providerModelId']],
      ),
    /already claimed by fixture row "idB"/,
    'the collision must be named, not absorbed',
  )
  assert.equal(creates.length, 0, 'neither model was created behind the other')
  assert.equal(updates.length, 1, 'only the first fixture row was applied before the stop')
})

test('reconcileRows adopts a rule whose id was re-derived, matched on the composite key', async () => {
  // rule ids are derived from pack content, so regenerating a pack re-derives an
  // id for a rule that already exists under (packId, ruleId). Matching on id
  // alone creates, and @@unique([packId, ruleId]) rejects it — which aborts the
  // whole reference seed, taking every table after `rule` with it.
  const { updates, creates, delegate } = reconcileDelegate([
    { id: 'a844de54-live', packId: 'pack-pii', ruleId: 'pii-gov-001' },
  ])
  await reconcileRows(
    delegate as never,
    [{ id: '0f923548-rederived', packId: 'pack-pii', ruleId: 'pii-gov-001', severity: 'confidential' }],
    [['packId', 'ruleId']],
  )
  assert.equal(creates.length, 0, 'must adopt the live row, not create a duplicate that violates the unique key')
  assert.deepEqual(updates[0].where, { id: 'a844de54-live' }, 'the live row keeps its id')
  assert.ok(!('id' in updates[0].data), 'update must never repoint the primary key')
  assert.equal((updates[0].data as Record<string, unknown>).severity, 'confidential')
})

test('reconcileRows matches a composite key conjointly, never field-by-field', async () => {
  // packId alone is shared by every rule in a pack and ruleId alone repeats
  // across packs. Treating the fields as separate identities would match a
  // sibling rule and overwrite it — silently rewriting one rule with another.
  const { updates, creates, delegate } = reconcileDelegate([
    { id: 'sibling', packId: 'pack-pii', ruleId: 'pii-con-001' },
    { id: 'namesake', packId: 'pack-dlp', ruleId: 'pii-gov-001' },
  ])
  await reconcileRows(
    delegate as never,
    [{ id: 'fresh', packId: 'pack-pii', ruleId: 'pii-gov-001' }],
    [['packId', 'ruleId']],
  )
  assert.equal(updates.length, 0, 'neither the pack sibling nor the cross-pack namesake is this rule')
  assert.deepEqual(creates[0], { id: 'fresh', packId: 'pack-pii', ruleId: 'pii-gov-001' })
})

test('reconcileRows requires every field of a composite key', async () => {
  const { delegate } = reconcileDelegate([])
  await assert.rejects(
    () => reconcileRows(delegate as never, [{ id: 'x', packId: 'pack-pii' }], [['packId', 'ruleId']]),
    /reconcile needs both "id" and "packId\+ruleId"/,
  )
})

test('reconcileRows errors when a rename half-happened — old and new name both live', async () => {
  const { delegate } = reconcileDelegate([
    { id: 'a-uuid', code: 'claude-haiku-4-5' },
    { id: 'b-uuid', code: 'claude-haiku-4-5-20251001' },
  ])
  await assert.rejects(
    () =>
      reconcileRows(
        delegate as never,
        [{ id: 'a-uuid', code: 'claude-haiku-4-5', aliases: ['claude-haiku-4-5-20251001'] }],
        ['code'],
      ),
    /resolve to different rows/,
  )
})

test('reconcileRows fails loudly when id and code resolve to different rows', async () => {
  const { delegate } = reconcileDelegate([
    { id: 'seed-uuid', code: 'some-other-code' },
    { id: 'wizard-uuid', code: 'claude-fable-5' },
  ])
  await assert.rejects(
    () => reconcileRows(delegate as never, [{ id: 'seed-uuid', code: 'claude-fable-5' }], ['code']),
    /resolve to different rows/,
  )
})

test('reconcileRows requires both id and the business key on every row', async () => {
  const { delegate } = reconcileDelegate([])
  await assert.rejects(
    () => reconcileRows(delegate as never, [{ code: 'no-id' }], ['code']),
    /needs both "id" and "code"/,
  )
})

import { readFixture } from '../reference/loadFixture.ts'

test('readFixture loads a real committed fixture as a non-empty array', () => {
  const models = readFixture('Model')
  assert.ok(Array.isArray(models) && models.length > 0, 'Model.json should parse to a non-empty array')
  assert.ok('id' in models[0], 'each Model row has an id')
})

// A deployment answers on its own domain, so its OAuth callback URL is
// configuration only the install knows — and it lives in a column the seed
// otherwise replaces wholesale. Replacing it deletes the URL, and the authorize
// endpoint rejects an unregistered redirect_uri, so every admin is locked out of
// the console the next time anyone re-seeds for an unrelated reason. That is not
// hypothetical: it took production down for eleven hours, discovered only
// because a prod end-to-end login failed afterwards.
test('a union field keeps what the deployment registered and adds what the fixture ships', async () => {
  const live = [
    {
      id: 'cp-ui',
      redirectUris: ['http://localhost:3000/auth/callback', 'https://nexus.example.com/auth/callback'],
    },
  ]
  const captured: { update: Record<string, unknown> }[] = []
  const delegate = {
    findMany: async () => live,
    upsert: async (args: { where: unknown; create: unknown; update: Record<string, unknown> }) => {
      captured.push({ update: args.update })
      return {}
    },
  }

  await upsertRows(
    delegate as never,
    [{ id: 'cp-ui', name: 'Nexus Control Plane UI', redirectUris: ['http://localhost:3000/auth/callback', 'http://127.0.0.1:3000/auth/callback'] }],
    'id',
    ['redirectUris'],
  )

  const written = captured[0].update.redirectUris as string[]
  assert.ok(
    written.includes('https://nexus.example.com/auth/callback'),
    'the deployment\'s own callback URL must survive the seed — dropping it locks every admin out',
  )
  assert.ok(written.includes('http://127.0.0.1:3000/auth/callback'), 'the fixture still contributes its own URLs')
  assert.equal(new Set(written).size, written.length, 'a URL present on both sides must not be duplicated')
})

test('a non-union field is still replaced by the fixture', async () => {
  const captured: { update: Record<string, unknown> }[] = []
  const delegate = {
    findMany: async () => [{ id: 'cp-ui', redirectUris: ['https://nexus.example.com/auth/callback'] }],
    upsert: async (args: { update: Record<string, unknown> }) => {
      captured.push({ update: args.update })
      return {}
    },
  }
  await upsertRows(
    delegate as never,
    [{ id: 'cp-ui', name: 'Renamed By The Catalog', redirectUris: [] }],
    'id',
    ['redirectUris'],
  )
  assert.equal(captured[0].update.name, 'Renamed By The Catalog', 'the fixture owns every field it is not sharing')
})

// ─── createMissingRows ────────────────────────────────────────────────────────

// Holds row state, so a test can assert what a live row LOOKS LIKE after the
// call rather than only that `create` was not reached. A stateless fake would
// make "the operator's value survived" indistinguishable from "nothing was
// written", which is the whole property under test.
function createMissingDelegate(live: Record<string, unknown>[] = []) {
  const store = new Map(live.map((r) => [String(r.id), { ...r }]))
  const created: Record<string, unknown>[] = []
  const queries: { where: Record<string, unknown>; select: Record<string, boolean> }[] = []
  return {
    store,
    created,
    queries,
    delegate: {
      findMany: async (args: { where: Record<string, unknown>; select: Record<string, boolean> }) => {
        queries.push(args)
        const filter = args.where.id as { in: unknown[] } | undefined
        const asked = new Set((filter?.in ?? []).map(String))
        return [...store.values()]
          .filter((r) => asked.has(String(r.id)))
          .map((r) => ({ id: r.id }))
      },
      create: async (args: { data: Record<string, unknown> }) => {
        const id = String(args.data.id)
        if (store.has(id)) throw new Error(`unique constraint: id ${id} already exists`)
        store.set(id, { ...args.data })
        created.push(args.data)
        return args.data
      },
    },
  }
}

test('createMissingRows inserts only the rows the deployment does not have', async () => {
  const { created, queries, delegate } = createMissingDelegate([{ id: 'live-1', name: 'A' }])
  const result = await createMissingRows(
    delegate as never,
    [{ id: 'live-1', name: 'A' }, { id: 'new-1', name: 'B' }],
    'id',
  )
  assert.deepEqual(result, { created: 1, kept: 1 })
  assert.equal(created.length, 1, 'the row that already exists must not be written again')
  assert.deepEqual(created[0], { id: 'new-1', name: 'B' })
  assert.equal(queries.length, 1, 'liveness is asked once for the whole fixture, not once per row')
  assert.deepEqual(queries[0].where, { id: { in: ['live-1', 'new-1'] } }, 'and only about the fixture ids')
  assert.deepEqual(queries[0].select, { id: true }, 'reading only the key keeps secret columns out of the query')
})

test('createMissingRows leaves a revoked demo key revoked and a real provider key intact', async () => {
  // The two regressions this write mode exists to prevent, stated as stored
  // state: the fixture would re-enable a key an admin disabled, and overwrite
  // the ciphertext of a Credential row an operator pasted a real provider key
  // into. Both live rows below carry the OPERATOR's values, and the fixture
  // rows carry the sample ones.
  const { created, store, delegate } = createMissingDelegate([
    { id: 'a5e8d4b2', name: 'super-admin', enabled: false, status: 'revoked' },
    { id: 'abff2f77', name: 'openai-prod', encryptedKey: 'sk-live-operators-real-key' },
  ])
  const result = await createMissingRows(
    delegate as never,
    [
      { id: 'a5e8d4b2', name: 'super-admin', enabled: true, status: 'active' },
      { id: 'abff2f77', name: 'openai-prod', encryptedKey: 'placeholder-ciphertext' },
    ],
    'id',
  )
  assert.deepEqual(result, { created: 0, kept: 2 })
  assert.deepEqual(created, [], 'no write may reach a row the deployment already owns')
  assert.equal(store.get('a5e8d4b2')?.enabled, false, 'a revoked super-admin key must stay revoked')
  assert.equal(store.get('a5e8d4b2')?.status, 'revoked')
  assert.equal(
    store.get('abff2f77')?.encryptedKey,
    'sk-live-operators-real-key',
    'an operator-entered provider key must not be replaced by the fixture placeholder',
  )
})

test('createMissingRows stamps secret material only on the rows it creates', async () => {
  // The re-stamp is the caller's create-time transform. Applying it to a live
  // row would republish that row's public seed plaintext; not applying it to a
  // created row would seed an unusable one.
  const { store, delegate } = createMissingDelegate([{ id: 'live-1', keyHash: 'rotated-hash' }])
  const prepared: string[] = []
  const result = await createMissingRows(
    delegate as never,
    [{ id: 'live-1' }, { id: 'new-1' }],
    'id',
    (row) => {
      prepared.push(String(row.id))
      return { ...row, keyHash: `public-seed-hash-for-${String(row.id)}` }
    },
  )
  assert.deepEqual(result, { created: 1, kept: 1 })
  assert.deepEqual(prepared, ['new-1'], 'the transform must not run for a row that already exists')
  assert.equal(store.get('live-1')?.keyHash, 'rotated-hash', 'the live row keeps the value it holds')
  assert.equal(store.get('new-1')?.keyHash, 'public-seed-hash-for-new-1')
})

test('createMissingRows converges when a fixture lists the same id twice', async () => {
  // The delegate raises on a duplicate insert, as the database would. Under the
  // old upsert this converged silently; a hard stop here would leave the tier —
  // and every table after it — unapplied on every run.
  const { created, delegate } = createMissingDelegate()
  const result = await createMissingRows(
    delegate as never,
    [{ id: 'dup', name: 'first' }, { id: 'dup', name: 'second' }],
    'id',
  )
  assert.deepEqual(result, { created: 1, kept: 1 })
  assert.equal(created.length, 1, 'the second occurrence must not reach a second insert')
})

test('createMissingRows asks the database nothing for an empty fixture', async () => {
  const { queries, delegate } = createMissingDelegate()
  const result = await createMissingRows(delegate as never, [], 'id')
  assert.deepEqual(result, { created: 0, kept: 0 })
  assert.deepEqual(queries, [], 'an empty tier must not issue an `in: []` round trip')
})

test('createMissingRows refuses a fixture row with no key field', async () => {
  const { delegate } = createMissingDelegate()
  await assert.rejects(
    () => createMissingRows(delegate as never, [{ name: 'no-id-here' }], 'id'),
    /row missing key field "id"/,
    'a keyless row would be inserted on every run, duplicating sample data',
  )
})

test('createMissingRows validates every row before writing any of them', async () => {
  const { created, delegate } = createMissingDelegate()
  await assert.rejects(
    () => createMissingRows(delegate as never, [{ id: 'ok' }, { name: 'broken' }], 'id'),
    /row missing key field "id"/,
  )
  assert.deepEqual(created, [], 'a bad fixture must not leave a half-applied tier behind')
})
