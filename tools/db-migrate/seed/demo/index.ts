/**
 * Tier-B demo seeder.
 *
 * Inserts the demo-tenant fixtures a deployment does not have yet and RE-STAMPS
 * every secret column as they go in, so the demo is loginable and VKs are usable
 * under the LOCAL keys. Demo fixtures ship with all secret columns nulled; this
 * module regenerates them from deterministic plaintext under
 * CREDENTIAL_ENCRYPTION_KEY / ADMIN_KEY_HMAC_SECRET.
 *
 * Rows that already exist are left untouched (createMissingRows, not
 * upsertRows). These are sample rows a deployment owns once it has them: an
 * operator revokes a demo key, or pastes a real provider key into one of the
 * pre-wired `*-prod` Credential rows, and the next `docker compose up` must not
 * undo either.
 *
 * Documented demo plaintexts, all derived from the row's own fixture id and all
 * therefore public in this repository — the container migrator rotates every one
 * of them (docker/db-migrator/rotate-demo-secrets.mjs) before a deployment is
 * reachable:
 *   User password — nexus-demo
 *   Virtual key   — nvk_demo_<first-8-chars-of-vk-id>
 *   Admin key     — nak_demo_<first-8-chars-of-admin-key-id>
 *   Credential    — sk-demo_<first-8-chars-of-credential-id>  (AES-256-GCM)
 *
 * The bootstrap super-admin (admin@nexus.ai) is NOT one of these — it belongs to
 * the bootstrap tier, seed/bootstrap/index.ts.
 */
import type { PrismaClient } from '@prisma/client'
import { readFileSync } from 'fs'
import { resolve, dirname } from 'path'
import { fileURLToPath } from 'url'
import { hashPassword, hashVirtualKey, hashAdminKey, fakeEncrypt } from '../lib.ts'
import { createMissingRows, readFixture } from '../reference/loadFixture.ts'
import { loadModelIdByCode, resolveModelRefs } from '../reference/modelRefs.ts'

const __dirname = dirname(fileURLToPath(import.meta.url))
const FIXTURES_DEMO = resolve(__dirname, '../fixtures/demo')

// ─── Documented demo credentials ─────────────────────────────────────────────

/** The password stamped on every local demo user. */
export const DEMO_PASSWORD = 'nexus-demo'

// Deterministic documented plaintext for a demo VirtualKey. The "nvk_" prefix
// is REQUIRED — vkauth rejects keys without it that are also ≤20 chars.
export const demoVkKey = (id: string): string => `nvk_demo_${id.slice(0, 8)}`

// Expiry stamped onto a demo APPLICATION virtual key when it is CREATED: a year
// out, because requireApplicationExpiry rejects a null or past expiry on every
// create / edit / renew, so the snapshot's own long-past timestamps would seed
// rows the admin API itself would refuse to write. It is not refreshed on later
// runs — the row belongs to the deployment once it exists — so a demo
// application key expires a year after the install that created it and is
// renewed through the admin API like any other.
const demoVkExpiry = (): Date => {
  const d = new Date()
  d.setFullYear(d.getFullYear() + 1)
  return d
}

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
      // The snapshot's VKs carried real expiry timestamps now long past, so a
      // created row gets a fresh one rather than the fixture's. Application keys
      // get a year rather than null: the admin API's requireApplicationExpiry
      // rejects a null or past expiry on every create / edit / renew, so a null
      // here would seed rows the API itself would refuse to write — and an admin
      // could not edit them without first supplying an expiry. Personal keys may
      // legitimately be never-expiring.
      const isApplication = normalized.vkType === 'application'
      return {
        ...normalized,
        keyHash: hashVirtualKey(plaintext),
        keyPrefix: plaintext.slice(0, 12),
        expiresAt: isApplication ? demoVkExpiry() : null,
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
 * Re-runnable: a second run against the same database creates nothing and
 * rewrites nothing. Its one write is the removal of the reference catalog's
 * default application-VK quota policy, which the reference tier re-inserts on
 * every run and this tier removes again, by design.
 */
export async function seedDemo(prisma: PrismaClient): Promise<void> {
  if (!process.env.CREDENTIAL_ENCRYPTION_KEY || !process.env.ADMIN_KEY_HMAC_SECRET) {
    throw new Error(
      'seedDemo requires both CREDENTIAL_ENCRYPTION_KEY and ADMIN_KEY_HMAC_SECRET to be set. ' +
        'These must match the values used by the running services.',
    )
  }

  // Reference data (including Model) is already seeded, so the ids read here are
  // the ids the model rows ended up with — which is what a virtual key's
  // allowedModels must name. Seeding a model can adopt a live row and keep that
  // row's id, and allowedModels[].modelId is a plain JSON value with no foreign
  // key, so a fixture-carried id naming no row would silently deny every model.
  const modelIdByCode = await loadModelIdByCode(prisma.model)

  const createdIds = new Set<string>()
  for (const { fixture, delegate } of DEMO_ORDER) {
    const rawRows = resolveModelRefs(
      JSON.parse(readFileSync(resolve(FIXTURES_DEMO, `${fixture}.json`), 'utf8')) as Record<
        string,
        unknown
      >[],
      modelIdByCode,
      `demo/${fixture}`,
    )

    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const del = (prisma as any)[delegate] as Parameters<typeof createMissingRows>[0]
    // The re-stamp is passed as the create-time transform rather than applied
    // up front: rows that already exist are not rewritten, so computing their
    // secret material would only mean 13 scrypt hashes at N=2^17 thrown away on
    // every `docker compose up`.
    const { created, kept } = await createMissingRows(del, rawRows, 'id', (row) => {
      createdIds.add(String(row.id))
      return restampRow(fixture, row)
    })
    console.log(`[seed:demo] ${fixture}: ${created} created, ${kept} left as the deployment has them`)
  }

  // The reference catalog ships a default application-VK monthly quota policy —
  // a sane production default whose own fixture description reads "Removed by
  // the demo seed (demo manages quotas via QuotaOverride)". Drop exactly that
  // row, identified from the reference fixture rather than by shape.
  //
  // By identity, never by shape: a shape predicate here
  // (`scope: 'virtual_key', vkType: 'application'`) is a rule about rows this
  // tier does not own. It does not today reach a policy an admin created — the
  // admin API persists a VK-scoped policy as scope "vk", which the gateway
  // folds to "virtual_key" only at read time (policy_cache.go
  // canonicalQuotaScope) — but nothing keeps that spelling stable, and an
  // identity match does not depend on it.
  //
  // The demo tier deliberately ships no application-VK quota policy of its own.
  // If it did, it would win over this one and make the removal pointless: the
  // highest-priority match is the effective policy (policy_cache.go FindPolicy,
  // ORDER BY priority DESC).
  const referenceAppVkQuotaIds = readFixture('QuotaPolicy')
    .filter((r) => r.scope === 'virtual_key' && r.vkType === 'application')
    .map((r) => String(r.id))
  if (referenceAppVkQuotaIds.length > 0) {
    const removedDefaultQuota = await prisma.quotaPolicy.deleteMany({
      where: { id: { in: referenceAppVkQuotaIds } },
    })
    if (removedDefaultQuota.count) {
      console.log(`[seed:demo] removed ${removedDefaultQuota.count} reference app-VK quota policy`)
    }
  }

  // ── Banner ────────────────────────────────────────────────────────────────
  // Gated on THIS key having just been created, not on the run having created
  // anything: the value below is the public seed plaintext, and it is true only
  // for a row that did not exist a moment ago. A run that adds one unrelated
  // fixture row to an existing install must not reprint it, because that row's
  // key was rotated on first boot and the printed value has been wrong ever
  // since. In the container form factor even a first install rotates it later
  // in the same run (docker/db-migrator/entrypoint.sh), so this banner is for
  // the local `npm run seed` path, which has no rotation step.
  //
  // The admin login is deliberately absent: admin@nexus.ai belongs to the
  // bootstrap tier, not this one. A lookup for it in the demo fixture used to
  // sit here and never matched, so those two lines never printed.
  const vks = JSON.parse(
    readFileSync(resolve(FIXTURES_DEMO, 'VirtualKey.json'), 'utf8'),
  ) as Array<{ id: string; name: string }>
  const primaryVk = vks.find((vk) => vk.name === 'demo01')
  if (!primaryVk || !createdIds.has(primaryVk.id)) return

  console.log('')
  console.log('╔═══════════════════════════════════════════════════════════════╗')
  console.log('║                  DEMO CREDENTIALS (local only)               ║')
  console.log('╠═══════════════════════════════════════════════════════════════╣')
  console.log(`║  User password :  ${DEMO_PASSWORD}`)
  console.log(`║  Primary VK    :  ${demoVkKey(primaryVk.id)}`)
  console.log('╚═══════════════════════════════════════════════════════════════╝')
  console.log('')
}
