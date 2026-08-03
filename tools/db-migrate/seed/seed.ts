/**
 * Seed entry point — three tiers, applied in order:
 *   Tier A — reference catalog (providers, models, IAM policies/groups, rule
 *            packs, hooks, jobs, …). Always loads.
 *   Bootstrap — the minimal tenant every install needs to be usable: one org,
 *            one project, the super-admin (admin@nexus.ai), its IAM binding, and
 *            the system-assistant VK. Always loads.
 *   Tier B — demo playground (rich multi-org tenant, extra users, demo VKs +
 *            credentials). Loads unless SEED_DEMO=false. The appliance runs with
 *            SEED_DEMO=false so no repo-committed demo credential ships on an
 *            internet-facing deployment.
 * Schema is applied separately via `prisma db push`; this only seeds data.
 */
import 'dotenv/config'
import { fileURLToPath } from 'node:url'
import { PrismaClient } from '@prisma/client'
import { PrismaPg } from '@prisma/adapter-pg'
import { seedReference } from './reference/index.ts'
import { seedBootstrap } from './bootstrap/index.ts'
import { seedDemo } from './demo/index.ts'

export function shouldSeedDemo(envValue: string | undefined): boolean {
  return envValue !== 'false'
}

/**
 * Run a tier, collecting its failure rather than letting it cancel the tiers
 * after it. Tier A failing is not a reason to skip Bootstrap: a data conflict in
 * one reference table used to mean a fresh install finished with no super-admin
 * and no way to log in, because the throw unwound before Bootstrap ever ran. The
 * seed still fails overall — main() reports every failed tier at the end — but
 * an install that can be logged into and repaired beats one that cannot.
 */
async function runTier(
  label: string,
  run: () => Promise<void>,
  failures: { tier: string; error: unknown }[],
): Promise<void> {
  console.log(label)
  try {
    await run()
  } catch (error) {
    failures.push({ tier: label, error })
    console.error(`[seed] ${label}: FAILED — ${error instanceof Error ? error.message : String(error)}`)
  }
}

async function main(): Promise<void> {
  if (!process.env.DATABASE_URL) throw new Error('seed: DATABASE_URL is required. Set it in tools/db-migrate/.env.')
  const prisma = new PrismaClient({ adapter: new PrismaPg({ connectionString: process.env.DATABASE_URL }) })
  const failures: { tier: string; error: unknown }[] = []
  try {
    await runTier('[seed] Tier A: reference catalog', () => seedReference(prisma), failures)
    await runTier(
      '[seed] Bootstrap: minimal tenant (org, project, super-admin, system-assistant VK)',
      () => seedBootstrap(prisma),
      failures,
    )
    if (shouldSeedDemo(process.env.SEED_DEMO)) {
      await runTier('[seed] Tier B: demo tenant (set SEED_DEMO=false to skip)', () => seedDemo(prisma), failures)
    } else {
      console.log('[seed] SEED_DEMO=false — skipping demo tenant')
    }
    if (failures.length > 0) {
      throw new AggregateError(
        failures.map((f) => f.error),
        `seed: ${failures.length} tier(s) failed (${failures.map((f) => f.tier).join('; ')})`,
      )
    }
    console.log('[seed] Done.')
  } finally {
    await prisma.$disconnect()
  }
}

// Run only when executed directly (not when imported by tests).
if (fileURLToPath(import.meta.url) === process.argv[1]) {
  main().catch((err) => { console.error('[seed] FAILED:', err); process.exit(1) })
}
