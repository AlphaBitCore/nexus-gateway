import { test } from 'node:test'
import assert from 'node:assert/strict'
import { upsertRows } from '../reference/loadFixture.ts'

test('upsertRows upserts every row keyed by id and is idempotent', async () => {
  const calls: { where: unknown; create: unknown; update: unknown }[] = []
  const delegate = {
    upsert: async (args: { where: unknown; create: unknown; update: unknown }) => {
      calls.push(args)
      return args.create
    },
  }
  const rows = [{ id: 'a', name: 'A' }, { id: 'b', name: 'B' }]
  const n = await upsertRows(delegate as never, rows, 'id')
  assert.equal(n, 2)
  assert.deepEqual(calls[0].where, { id: 'a' })
  assert.deepEqual(calls[0].create, { id: 'a', name: 'A' })
  assert.deepEqual(calls[0].update, { id: 'a', name: 'A' })
})

test('upsertRows preserves runtime-mutated fields on update but not on create', async () => {
  // Simulates the governance-toggle clobber: an operator turned a hook ON
  // (enabled=true in the DB); the fixture ships enabled=false. Re-running the
  // seed (db-init on box restart) must NOT reset enabled, but must still
  // converge the definition fields (priority).
  const calls: { where: unknown; create: unknown; update: unknown }[] = []
  const delegate = {
    upsert: async (args: { where: unknown; create: unknown; update: unknown }) => {
      calls.push(args)
      return args.create
    },
  }
  const rows = [{ id: 'h1', name: 'pii', enabled: false, priority: 7 }]
  const n = await upsertRows(delegate as never, rows, 'id', ['enabled'])
  assert.equal(n, 1)
  // CREATE gets the full fixture row including the enabled baseline.
  assert.deepEqual(calls[0].create, { id: 'h1', name: 'pii', enabled: false, priority: 7 })
  // UPDATE omits the preserved field so an operator's live value survives,
  // while definition fields still converge to the fixture.
  assert.deepEqual(calls[0].update, { id: 'h1', name: 'pii', priority: 7 })
  assert.ok(!('enabled' in (calls[0].update as Record<string, unknown>)), 'enabled must be stripped from update')
})

test('upsertRows with an empty preserve list updates every field (default converge)', async () => {
  const calls: { update: unknown }[] = []
  const delegate = { upsert: async (args: { where: unknown; create: unknown; update: unknown }) => { calls.push(args); return args.create } }
  await upsertRows(delegate as never, [{ id: 'x', enabled: true }], 'id', [])
  assert.deepEqual(calls[0].update, { id: 'x', enabled: true })
})

test('upsertRows throws when a row lacks the key field', async () => {
  const delegate = { upsert: async () => ({}) }
  await assert.rejects(
    () => upsertRows(delegate as never, [{ name: 'no-id' }], 'id'),
    /missing key field "id"/,
  )
})

import { readFixture } from '../reference/loadFixture.ts'

test('readFixture loads a real committed fixture as a non-empty array', () => {
  const models = readFixture('Model')
  assert.ok(Array.isArray(models) && models.length > 0, 'Model.json should parse to a non-empty array')
  assert.ok('id' in models[0], 'each Model row has an id')
})
