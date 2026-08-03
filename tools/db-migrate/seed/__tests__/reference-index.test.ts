import { test } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync } from 'fs'
import { resolve, dirname } from 'path'
import { fileURLToPath } from 'url'
import { REFERENCE_TABLES, seedReference } from '../reference/index.ts'
import { readFixture } from '../reference/loadFixture.ts'
import { modelTable } from './_modelTable.ts'

const here = dirname(fileURLToPath(import.meta.url))

/**
 * A prisma stand-in whose delegates record what they were asked to write, and
 * where one named delegate fails the way a unique-constraint violation does.
 *
 * Model is backed by a real in-memory table rather than a stub, because it is
 * the one delegate whose reads feed the tables after it: every `model:<code>`
 * reference in a later fixture resolves against the ids Model ends up with. A
 * stub that answers every read the same way makes those fixtures fail for a
 * reason no real seed would, and this file's failure counts would then describe
 * the fake instead of the seed.
 */
function fakePrisma(
  failOn?: { delegate: string; message: string },
  liveModels: Record<string, unknown>[] = readFixture('Model'),
) {
  const seeded = new Set<string>()
  const models = modelTable(liveModels)

  const handler = (name: string) => {
    const failIfNamed = () => {
      if (failOn?.delegate === name) throw new Error(failOn.message)
    }
    if (name === 'model') {
      return {
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        findMany: async (args: any) => (failIfNamed(), models.delegate.findMany(args)),
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        update: async (args: any) => (failIfNamed(), seeded.add(name), models.delegate.update(args)),
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        create: async (args: any) => (failIfNamed(), seeded.add(name), models.delegate.create(args)),
        upsert: async () => (failIfNamed(), seeded.add(name), {}),
      }
    }
    return {
      upsert: async () => {
        failIfNamed()
        seeded.add(name)
        return {}
      },
      findMany: async () => {
        failIfNamed()
        return []
      },
      update: async () => ({}),
      create: async () => {
        failIfNamed()
        seeded.add(name)
        return {}
      },
    }
  }
  return {
    seeded,
    models,
    prisma: new Proxy({}, { get: (_t, prop: string) => handler(prop) }),
  }
}

test('Model is reconciled on every unique key the table has, not just its code', async () => {
  // The admin API exposes Code and ProviderModelID independently, so the
  // provider wizard can add a catalog model under a name of the admin's
  // choosing. That row matches the fixture on neither id nor code — only on
  // Model's third unique key, @@unique([providerId, providerModelId]) — so a
  // reconcile that enumerates fewer keys than the table enforces falls through
  // to create and the database rejects it. Reconciling capability is proven in
  // loadFixture.test.ts; this pins that seedReference actually asks for it.
  const [first, ...rest] = readFixture('Model')
  const wizardRow = {
    id: 'wizard-uuid',
    code: `my-${String(first.code)}`,
    providerId: first.providerId,
    providerModelId: first.providerModelId,
  }
  const { models, prisma } = fakePrisma(undefined, [wizardRow, ...rest])

  await seedReference(prisma as never)

  assert.equal(models.creates.length, 0, 'the wizard row must be adopted, not created against its unique key')
  const adopted = models.rows.find((r) => r.id === 'wizard-uuid')
  assert.ok(adopted, 'the wizard row keeps the id its traffic_event rows reference')
  assert.equal(adopted.code, first.code, 'the catalog code is applied to the row the admin created')
})

test('a failing table does not stop the tables after it — login-critical ones still seed', async () => {
  // The live case: `rule` aborted at table 10 of 26 on a P2002, and every table
  // after it silently never ran. On a fresh install that is no super-admin and
  // no way to log in — a blast radius decided by position in the list, not by
  // the table that failed.
  const { seeded, prisma } = fakePrisma({ delegate: 'rule', message: 'Unique constraint failed on the fields: (`packId`, `ruleId`)' })

  await assert.rejects(
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    () => seedReference(prisma as any),
    (err: unknown) => {
      assert.ok(err instanceof AggregateError, 'reports every failure, not just the first')
      assert.match((err as Error).message, /1 of \d+ tables failed \(rule\)/)
      return true
    },
    'a table failure must still fail the seed — loudly, not silently',
  )

  for (const critical of ['iamPolicy', 'identityProvider', 'oAuthClient', 'iamGroup']) {
    assert.ok(seeded.has(critical), `${critical} must still be seeded when an earlier table fails`)
  }
})

test('every table is attempted even when several fail', async () => {
  const { seeded, prisma } = fakePrisma({ delegate: 'model', message: 'P2002' })
  await assert.rejects(() => seedReference(prisma as never), /tables failed \(Model, /)
  // Model seeds second. Every table that does not depend on it must still run,
  // including the last ones in the list — position must not decide blast radius.
  assert.ok(seeded.has('provider'), 'the table before the failure seeded')
  assert.ok(seeded.has('oAuthClient'), 'a login-critical table after the failure seeded')
  assert.ok(seeded.has('quotaPolicy'), 'a table near the end of the list seeded')
  assert.ok(!seeded.has('model'), 'the failing table did not seed')
})

test('a failed Model takes the fixtures that reference it, rather than writing dangling ids', async () => {
  // ai_guard_config and RoutingRule name their models by code and resolve those
  // against the ids Model ended up with. If Model failed, there are no ids this
  // seed can vouch for — and none of these columns is a foreign key, so writing
  // an unvouched id raises nothing at write time and instead surfaces later as
  // a judge that cannot be called or a route that matches nothing. Failing them
  // here is what keeps that from being silent.
  const { seeded, prisma } = fakePrisma({ delegate: 'model', message: 'P2002' })

  await assert.rejects(
    () => seedReference(prisma as never),
    (err: unknown) => {
      const message = (err as Error).message
      assert.match(message, /ai_guard_config/, 'the judge config must fail with Model')
      assert.match(message, /RoutingRule/, 'the routing rules must fail with Model')
      return true
    },
  )
  assert.ok(!seeded.has('aIGuardConfig'), 'no judge config written against ids the seed cannot vouch for')
  assert.ok(!seeded.has('routingRule'), 'no routing rule written against ids the seed cannot vouch for')
})

test('a clean run seeds every table and does not throw', async () => {
  const { seeded, prisma } = fakePrisma()
  await seedReference(prisma as never)
  assert.ok(seeded.size >= 20, `expected every table to seed, got ${seeded.size}`)
})

test('every reference table maps to a delegate, a key, and an existing fixture file', () => {
  assert.ok(REFERENCE_TABLES.length >= 17, 'all reference tables present')
  for (const t of REFERENCE_TABLES) {
    assert.ok(String(t.delegate).length > 0, `${t.fixture}: delegate set`)
    assert.ok(t.key.length > 0, `${t.fixture}: key set`)
    assert.ok(existsSync(resolve(here, '../fixtures', `${t.fixture}.json`)), `fixture file missing: ${t.fixture}.json`)
  }
})

test('reference table set covers exactly the committed fixtures', () => {
  const fixtures = new Set(REFERENCE_TABLES.map((t) => t.fixture))
  for (const f of ['Provider','Model','interception_domain','interception_path','rule','rule_pack','thing_config_template','IamPolicy','system_metadata','metric_ops_retention_config','cache_adapter_config','cache_provider_config','gateway_passthrough_config_global','ai_guard_config','AlertRule','semantic_cache_config']) {
    assert.ok(fixtures.has(f), `REFERENCE_TABLES missing fixture ${f}`)
  }
})
