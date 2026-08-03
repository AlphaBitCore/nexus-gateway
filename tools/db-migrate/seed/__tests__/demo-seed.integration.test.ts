/**
 * Integration test for the Tier-B demo seeder.
 *
 * Requires a scratch database already loaded with Tier-A reference data AND the
 * bootstrap tenant (demo VKs / admin keys are owned by the bootstrap
 * super-admin), with DATABASE_URL set. Skips when DATABASE_URL is absent
 * (CI / pure-unit runs).
 *
 * Run manually:
 *   docker exec nexus-postgres dropdb -U postgres --if-exists nexus_scratch
 *   docker exec nexus-postgres createdb -U postgres nexus_scratch
 *   DATABASE_URL='postgresql://postgres:postgres@localhost:55532/nexus_scratch?sslmode=disable' \
 *     npx prisma db push --schema-path schema/schema.prisma 2>/dev/null || \
 *     npx prisma db push
 *   # seed Tier-A reference + bootstrap first (demo FKs depend on them)
 *   CREDENTIAL_ENCRYPTION_KEY=$(printf '0%.0s' {1..64}) \
 *   ADMIN_KEY_HMAC_SECRET=demo-secret \
 *   DATABASE_URL='postgresql://...' \
 *     node --import tsx/esm tools/db-migrate/seed/__tests__/_preseed-reference.ts
 *   # then run this test:
 *   CREDENTIAL_ENCRYPTION_KEY=$(printf '0%.0s' {1..64}) \
 *   ADMIN_KEY_HMAC_SECRET=demo-secret \
 *   DATABASE_URL='postgresql://...' \
 *     npx tsx --test seed/__tests__/demo-seed.integration.test.ts
 */
import { test, before, after } from 'node:test'
import assert from 'node:assert/strict'
import { createDecipheriv } from 'crypto'
import { PrismaClient } from '@prisma/client'
import { PrismaPg } from '@prisma/adapter-pg'
import { seedDemo, demoVkKey } from '../demo/index.ts'
import { hashVirtualKey } from '../lib.ts'

const url = process.env.DATABASE_URL
const adapter = url ? new PrismaPg({ connectionString: url }) : null
const prisma = adapter ? new PrismaClient({ adapter }) : null

const SKIP = !url ? 'DATABASE_URL unset — skipping integration test' : false

before(async () => {
  // Tier-A reference + bootstrap tenant are seeded by the test runner
  // (_preseed-reference.ts). Seed the demo tenant here so every assertion below
  // runs against freshly-stamped demo rows. (The super-admin the demo VKs are
  // owned by is a bootstrap row — its password is covered by the bootstrap
  // integration test, not here.)
  if (prisma) await seedDemo(prisma)
})
after(async () => {
  await prisma?.$disconnect()
})

// ─── Business outcome: VK keyHash resolves via documented key ─────────────────

test(
  "demo VirtualKey 'super-admin' keyHash equals hashVirtualKey(demoVkKey(id))",
  { skip: SKIP },
  async () => {
    assert.ok(prisma)

    // The VK named 'demo01' owned by nexus-user-super-admin is the
    // primary demo VK advertised in the banner (id 0c101489-...).
    const vk = await prisma.virtualKey.findFirst({
      where: { name: 'demo01', ownerId: 'nexus-user-super-admin' },
    })
    assert.ok(vk, 'demo01 VK must exist')
    assert.ok(vk.keyHash, 'keyHash must be non-null after seeding')
    assert.ok(vk.keyPrefix, 'keyPrefix must be non-null after seeding')

    const documentedPlaintext = demoVkKey(vk.id)
    const expectedHash = hashVirtualKey(documentedPlaintext)
    assert.equal(
      vk.keyHash,
      expectedHash,
      'VK keyHash must equal hashVirtualKey(documented plaintext) — key is resolvable',
    )
    assert.equal(
      vk.keyPrefix,
      documentedPlaintext.slice(0, 12),
      'keyPrefix must be first 12 chars of documented key',
    )
  },
)

// ─── Business outcome: Credential decrypts to expected sk-demo-... ───────────

test(
  'demo Credential (openai-prod) decrypts to expected sk-demo-<id-prefix> under CREDENTIAL_ENCRYPTION_KEY',
  { skip: SKIP },
  async () => {
    assert.ok(prisma)

    const cred = await prisma.credential.findFirst({ where: { name: 'openai-prod' } })
    assert.ok(cred, 'openai-prod credential must exist')
    assert.ok(cred.encryptedKey, 'encryptedKey must be non-null')
    assert.ok(cred.encryptionIv, 'encryptionIv must be non-null')
    assert.ok(cred.encryptionTag, 'encryptionTag must be non-null')

    const keyHex = process.env.CREDENTIAL_ENCRYPTION_KEY!
    const key = Buffer.from(keyHex, 'hex')
    const iv = Buffer.from(cred.encryptionIv, 'hex')
    const tag = Buffer.from(cred.encryptionTag, 'hex')
    const d = createDecipheriv('aes-256-gcm', key, iv, { authTagLength: 16 })
    d.setAuthTag(tag)
    const plaintext = Buffer.concat([
      d.update(Buffer.from(cred.encryptedKey, 'hex')),
      d.final(),
    ]).toString('utf8')

    assert.equal(
      plaintext,
      `sk-demo-${cred.id.slice(0, 8)}`,
      'Credential must decrypt to sk-demo-<id-prefix>',
    )
  },
)

// ─── Idempotency ──────────────────────────────────────────────────────────────

test('seedDemo is idempotent: running twice yields stable row counts', { skip: SKIP }, async () => {
  assert.ok(prisma)

  // First run already done in the password test — run once more here.
  await seedDemo(prisma)

  const users = await prisma.nexusUser.count()
  const vks = await prisma.virtualKey.count()
  const creds = await prisma.credential.count()
  const orgs = await prisma.organization.count()

  // Run again — counts must not change.
  await seedDemo(prisma)

  assert.equal(await prisma.nexusUser.count(), users, 'NexusUser count must be stable')
  assert.equal(await prisma.virtualKey.count(), vks, 'VirtualKey count must be stable')
  assert.equal(await prisma.credential.count(), creds, 'Credential count must be stable')
  assert.equal(await prisma.organization.count(), orgs, 'Organization count must be stable')
})

test(
  'seedDemo leaves a row the deployment modified exactly as it is',
  { skip: SKIP },
  async () => {
    assert.ok(prisma)

    // Row counts alone cannot tell an upsert from an insert-only write — they
    // were stable under both. This is the property the write mode exists for:
    // an operator disables a demo key and pastes a real provider key, and the
    // next seed must not undo either.
    const vk = await prisma.virtualKey.findFirst({ where: { name: 'demo01' } })
    assert.ok(vk, 'demo01 must exist after the first seed')
    await prisma.virtualKey.update({
      where: { id: vk.id },
      data: { enabled: false, vkStatus: 'revoked' },
    })
    const cred = await prisma.credential.findFirst({ where: { name: 'openai-prod' } })
    assert.ok(cred, 'openai-prod must exist after the first seed')
    await prisma.credential.update({
      where: { id: cred.id },
      data: { encryptedKey: 'operator-pasted-provider-key' },
    })

    try {
      await seedDemo(prisma)

      const vkAfter = await prisma.virtualKey.findUnique({ where: { id: vk.id } })
      assert.equal(vkAfter?.enabled, false, 'a revoked demo key must stay revoked')
      assert.equal(vkAfter?.vkStatus, 'revoked')
      const credAfter = await prisma.credential.findUnique({ where: { id: cred.id } })
      assert.equal(
        credAfter?.encryptedKey,
        'operator-pasted-provider-key',
        'an operator-entered provider key must not be replaced by the fixture placeholder',
      )
    } finally {
      // Restored in `finally`, because seedDemo will never repair these rows
      // again — that is the property under test. Without it, a failing
      // assertion above leaves the scratch database mutated, and the arms that
      // decrypt openai-prod and authenticate demo01 then fail on every later
      // run, masking whatever went red first.
      await prisma.virtualKey.update({
        where: { id: vk.id },
        data: { enabled: vk.enabled, vkStatus: vk.vkStatus },
      })
      await prisma.credential.update({
        where: { id: cred.id },
        data: { encryptedKey: cred.encryptedKey },
      })
    }
  },
)

// ─── Env guard at seedDemo level ──────────────────────────────────────────────

test(
  'seedDemo throws when CREDENTIAL_ENCRYPTION_KEY is absent',
  { skip: SKIP },
  async () => {
    assert.ok(prisma)
    const saved = process.env.CREDENTIAL_ENCRYPTION_KEY
    delete process.env.CREDENTIAL_ENCRYPTION_KEY
    try {
      await assert.rejects(
        () => seedDemo(prisma!),
        /CREDENTIAL_ENCRYPTION_KEY/,
        'must throw on missing CREDENTIAL_ENCRYPTION_KEY',
      )
    } finally {
      process.env.CREDENTIAL_ENCRYPTION_KEY = saved
    }
  },
)

test(
  'seedDemo throws when ADMIN_KEY_HMAC_SECRET is absent',
  { skip: SKIP },
  async () => {
    assert.ok(prisma)
    const saved = process.env.ADMIN_KEY_HMAC_SECRET
    delete process.env.ADMIN_KEY_HMAC_SECRET
    try {
      await assert.rejects(
        () => seedDemo(prisma!),
        /ADMIN_KEY_HMAC_SECRET/,
        'must throw on missing ADMIN_KEY_HMAC_SECRET',
      )
    } finally {
      process.env.ADMIN_KEY_HMAC_SECRET = saved
    }
  },
)
