/**
 * Reference-tier seed ONLY — the Tier A half of seed.ts, without Bootstrap or
 * the demo tenant.
 *
 * The deploy runbook's value rule for a Model-row change is "run the reference
 * seed": reconcileRows matches each fixture row against a live row by id, code
 * or alias and updates in place, so a wizard-created row under an old dated name
 * is adopted and corrected while keeping the id that traffic_event.model_id and
 * VirtualKey.allowedModels[].modelId reference. Rows the catalog does not carry
 * are left untouched; nothing is deleted.
 *
 * Bootstrap is excluded deliberately. It is idempotent, but it mints the
 * super-admin and the system-assistant VK and requires BOOTSTRAP_PASSWORD — none
 * of which a Model-row repair needs. Writing less to a live deployment is the
 * point.
 */
import 'dotenv/config'
import { fileURLToPath } from 'node:url'
import { PrismaClient } from '@prisma/client'
import { PrismaPg } from '@prisma/adapter-pg'
import { seedReference } from './reference/index.ts'

async function main(): Promise<void> {
  if (!process.env.DATABASE_URL) throw new Error('reference-only: DATABASE_URL is required')
  const prisma = new PrismaClient({ adapter: new PrismaPg({ connectionString: process.env.DATABASE_URL }) })
  try {
    console.log('[seed:reference-only] Tier A: reference catalog')
    await seedReference(prisma)
    console.log('[seed:reference-only] Done.')
  } finally {
    await prisma.$disconnect()
  }
}

// Run only when executed directly (not when imported by tests).
if (fileURLToPath(import.meta.url) === process.argv[1]) {
  main().catch((err) => { console.error('[seed:reference-only] FAILED:', err); process.exit(1) })
}
