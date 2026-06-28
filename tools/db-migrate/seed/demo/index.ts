/**
 * Tier-B demo seeder.
 *
 * Upserts demo-tenant fixtures and RE-STAMPS every secret column at seed time
 * so the demo is loginable and VKs are usable under the LOCAL keys. Demo
 * fixtures ship with all secret columns nulled; this module regenerates them
 * from deterministic plaintext under CREDENTIAL_ENCRYPTION_KEY /
 * ADMIN_KEY_HMAC_SECRET.
 *
 * Documented demo credentials (printed in the banner at the end):
 *   Admin login  — admin@nexus.ai / nexus-demo
 *   Primary VK   — nvk-demo-<first-8-chars-of-vk-id>
 *   Admin key    — nak-demo-<first-8-chars-of-admin-key-id>
 *   Credential   — sk-demo-<first-8-chars-of-credential-id>  (AES-256-GCM)
 */
import type { PrismaClient } from '@prisma/client'
import { readFileSync } from 'fs'
import { resolve, dirname } from 'path'
import { fileURLToPath } from 'url'
import { hashPassword, hashVirtualKey, hashAdminKey, fakeEncrypt } from '../lib.ts'
import { upsertRows } from '../reference/loadFixture.ts'

const __dirname = dirname(fileURLToPath(import.meta.url))
const FIXTURES_DEMO = resolve(__dirname, '../fixtures/demo')

// ─── Documented demo credentials ─────────────────────────────────────────────

/** The password stamped on every local demo user. */
export const DEMO_PASSWORD = 'nexus-demo'

// Deterministic documented plaintext for a demo VirtualKey. The "nvk_" prefix
// is REQUIRED — vkauth rejects keys without it that are also ≤20 chars.
export const demoVkKey = (id: string): string => `nvk_demo_${id.slice(0, 8)}`

/** Deterministic documented plaintext for a demo AdminApiKey. */
export const demoAdminKey = (id: string): string => `nak_demo_${id.slice(0, 8)}`

// ─── Field-name normalization per table ───────────────────────────────────────
//
// Demo fixtures use camelCase for most fields (already matching Prisma field
// names) but retain snake_case for a handful of @map()-d columns that were
// exported verbatim from the DB:
//
//   AdminApiKey.key_version       → keyVersion     (@map("key_version"))
//   VirtualKey.key_version        → keyVersion     (@map("key_version"))
//   Credential.encryption_key_id  → encryptionKeyId (@map("encryption_key_id"))
//   IamPolicyAttachment.expires_at — the Prisma field IS named expires_at but
//     the schema @@map does NOT rename it, so Prisma DOES accept expires_at
//     directly. However the field is declared `expires_at` in the model and
//     Prisma's generated client exposes it as `expiresAt` (camelCase JS). We
//     must rename it for upsert to work.
//
// All other fields are already camelCase and pass through unchanged.

/**
 * Rename known snake_case fixture fields to their Prisma camelCase equivalents.
 * Applied BEFORE the re-stamp so the re-stamp writes into the final field names.
 */
function normalizeFieldNames(table: string, row: Record<string, unknown>): Record<string, unknown> {
  const out = { ...row }

  if (table === 'AdminApiKey' || table === 'VirtualKey') {
    if ('key_version' in out) {
      out.keyVersion = out.key_version
      delete out.key_version
    }
  }

  if (table === 'Credential') {
    if ('encryption_key_id' in out) {
      out.encryptionKeyId = out.encryption_key_id
      delete out.encryption_key_id
    }
  }

  return out
}

// ─── Per-table re-stamp logic ─────────────────────────────────────────────────

/**
 * Apply the credential re-stamp rules to a single normalized (camelCase) row.
 *
 * Exported for unit testing — this is pure logic with no DB dependency.
 * Input `row` must already have Prisma camelCase field names (i.e. run
 * normalizeFieldNames first, or pass a row that never had snake_case keys).
 */
export function restampRow(table: string, row: Record<string, unknown>): Record<string, unknown> {
  // Apply field-name normalization first so re-stamp logic uses camelCase names.
  const normalized = normalizeFieldNames(table, row)

  switch (table) {
    case 'NexusUser': {
      // Only local-auth users can log in with a password; SSO/SCIM users
      // have passwordHash null by design.
      if (normalized.source === 'local') {
        return {
          ...normalized,
          passwordHash: hashPassword(DEMO_PASSWORD),
        }
      }
      return normalized
    }

    case 'AdminApiKey': {
      const plaintext = demoAdminKey(normalized.id as string)
      return {
        ...normalized,
        keyHash: hashAdminKey(plaintext),
        keyPrefix: plaintext.slice(0, 12),
      }
    }

    case 'VirtualKey': {
      const plaintext = demoVkKey(normalized.id as string)
      return {
        ...normalized,
        keyHash: hashVirtualKey(plaintext),
        keyPrefix: plaintext.slice(0, 12),
        // The snapshot's VKs carried real expiry timestamps now in the past;
        // clear them so the demo keys are usable, and keep the row admittable.
        expiresAt: null,
        enabled: true,
        vkStatus: 'active',
      }
    }

    case 'Credential': {
      const enc = fakeEncrypt(`sk-demo-${(normalized.id as string).slice(0, 8)}`)
      return {
        ...normalized,
        encryptedKey: enc.ciphertext,
        encryptionIv: enc.iv,
        encryptionTag: enc.tag,
      }
    }

    default:
      return normalized
  }
}

// ─── Upsert order (FK-safe) ───────────────────────────────────────────────────

const DEMO_ORDER: { fixture: string; delegate: string }[] = [
  { fixture: 'Organization', delegate: 'organization' },
  { fixture: 'Project', delegate: 'project' },
  { fixture: 'NexusUser', delegate: 'nexusUser' },
  { fixture: 'AdminApiKey', delegate: 'adminApiKey' },
  { fixture: 'QuotaPolicy', delegate: 'quotaPolicy' },
  { fixture: 'QuotaOverride', delegate: 'quotaOverride' },
  { fixture: 'Credential', delegate: 'credential' },
  { fixture: 'VirtualKey', delegate: 'virtualKey' },
  { fixture: 'IamGroupMembership', delegate: 'iamGroupMembership' },
  { fixture: 'IamPolicyAttachment', delegate: 'iamPolicyAttachment' },
]

// ─── Main entrypoint ──────────────────────────────────────────────────────────

/**
 * Seed all demo tenant fixtures with secrets re-stamped under the local
 * CREDENTIAL_ENCRYPTION_KEY and ADMIN_KEY_HMAC_SECRET.
 *
 * Prerequisites:
 *  - Tier-A reference data (Providers, Models, IamPolicy, IamGroup, etc.) must
 *    already be seeded (FK dependencies).
 *  - CREDENTIAL_ENCRYPTION_KEY and ADMIN_KEY_HMAC_SECRET must be set in env.
 *
 * Idempotent: safe to run multiple times.
 */
export async function seedDemo(prisma: PrismaClient): Promise<void> {
  if (!process.env.CREDENTIAL_ENCRYPTION_KEY || !process.env.ADMIN_KEY_HMAC_SECRET) {
    throw new Error(
      'seedDemo requires both CREDENTIAL_ENCRYPTION_KEY and ADMIN_KEY_HMAC_SECRET to be set. ' +
        'These must match the values used by the running services.',
    )
  }

  for (const { fixture, delegate } of DEMO_ORDER) {
    const rawRows = JSON.parse(
      readFileSync(resolve(FIXTURES_DEMO, `${fixture}.json`), 'utf8'),
    ) as Record<string, unknown>[]

    const rows = rawRows.map((row) => restampRow(fixture, row))

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const del = (prisma as any)[delegate] as {
      upsert: (args: { where: unknown; create: unknown; update: unknown }) => Promise<unknown>
    }
    const n = await upsertRows(del, rows, 'id')
    console.log(`[seed:demo] ${fixture}: ${n} rows`)
  }

  // The reference catalog ships a default application-VK monthly quota policy (a
  // sane production default). The demo playground drives quotas via the
  // QuotaOverride fixtures above instead, so drop that reference default here.
  const removedDefaultQuota = await prisma.quotaPolicy.deleteMany({
    where: { scope: 'virtual_key', vkType: 'application' },
  })
  if (removedDefaultQuota.count) {
    console.log(`[seed:demo] removed ${removedDefaultQuota.count} default app-VK quota policy`)
  }

  // ── Banner ────────────────────────────────────────────────────────────────
  // Find the super-admin user (id = 'nexus-user-super-admin') for the banner.
  const superAdmin = JSON.parse(
    readFileSync(resolve(FIXTURES_DEMO, 'NexusUser.json'), 'utf8'),
  ) as Array<{ id: string; email: string; source: string }>
  const adminUser = superAdmin.find((u) => u.id === 'nexus-user-super-admin')

  // Find the primary demo VK (name = 'demo01', owner = nexus-user-super-admin).
  const vks = JSON.parse(
    readFileSync(resolve(FIXTURES_DEMO, 'VirtualKey.json'), 'utf8'),
  ) as Array<{ id: string; name: string }>
  const primaryVk = vks.find((vk) => vk.name === 'demo01')

  console.log('')
  console.log('╔═══════════════════════════════════════════════════════════════╗')
  console.log('║                  DEMO CREDENTIALS (local only)               ║')
  console.log('╠═══════════════════════════════════════════════════════════════╣')
  if (adminUser) {
    console.log(`║  Admin login  :  ${adminUser.email}`)
    console.log(`║  Password     :  ${DEMO_PASSWORD}`)
  }
  if (primaryVk) {
    console.log(`║  Primary VK   :  ${demoVkKey(primaryVk.id)}`)
  }
  console.log('╚═══════════════════════════════════════════════════════════════╝')
  console.log('')
}
