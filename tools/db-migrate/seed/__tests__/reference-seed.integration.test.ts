import { test, before, after } from 'node:test'
import assert from 'node:assert/strict'
import { PrismaClient } from '@prisma/client'
import { PrismaPg } from '@prisma/adapter-pg'
import { seedReference } from '../reference/index.ts'

const url = process.env.DATABASE_URL
const adapter = url ? new PrismaPg({ connectionString: url }) : null
const prisma = adapter ? new PrismaClient({ adapter }) : null

before(async () => { /* schema must already be pushed to the scratch DB by the runner */ })
after(async () => { await prisma?.$disconnect() })

test('seedReference loads the catalog and is idempotent', { skip: !url ? 'DATABASE_URL unset' : false }, async () => {
  assert.ok(prisma, 'PrismaClient initialized')
  await seedReference(prisma)
  const providers1 = await prisma.provider.count()
  const models1 = await prisma.model.count()
  assert.ok(providers1 >= 7, `providers seeded (got ${providers1})`)
  assert.ok(models1 >= 38, `models seeded (got ${models1})`)
  // idempotent: second run yields identical counts, no duplicate-key error
  await seedReference(prisma)
  assert.equal(await prisma.provider.count(), providers1)
  assert.equal(await prisma.model.count(), models1)
  // business outcome: a managed IAM policy exists and a known provider resolves
  assert.ok((await prisma.iamPolicy.count({ where: { type: 'managed' } })) >= 1)
  assert.ok(await prisma.provider.findFirst({ where: { name: 'openai' } }))
})

test('re-seed preserves an operator-enabled hook (governance survives box restart)', { skip: !url ? 'DATABASE_URL unset' : false }, async () => {
  assert.ok(prisma, 'PrismaClient initialized')
  await seedReference(prisma)
  const hook = await prisma.hookConfig.findFirst()
  assert.ok(hook, 'at least one hook seeded')
  // Operator turns governance ON at runtime (fixture ships enabled=false).
  await prisma.hookConfig.update({ where: { id: hook.id }, data: { enabled: true } })
  // db-init re-runs on the next box restart …
  await seedReference(prisma)
  // … and must NOT clobber the operator's toggle back to the fixture default.
  const after = await prisma.hookConfig.findUnique({ where: { id: hook.id } })
  assert.equal(after?.enabled, true, 'enabled must survive a re-seed')
})
