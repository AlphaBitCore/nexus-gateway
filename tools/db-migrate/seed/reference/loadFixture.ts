import { readFileSync } from 'fs'
import { resolve, dirname } from 'path'
import { fileURLToPath } from 'url'

type UpsertDelegate = {
  upsert: (args: {
    where: Record<string, unknown>
    create: Record<string, unknown>
    update: Record<string, unknown>
  }) => Promise<unknown>
}

/**
 * Upsert every row keyed by `key`. Idempotent: re-running converges.
 *
 * `preserveFields` names columns that carry per-deployment OPERATIONAL state an
 * operator mutates at runtime (e.g. HookConfig.enabled — the governance
 * on/off toggle). Those fields are set on first CREATE (so a fresh install
 * still gets the fixture baseline) but STRIPPED from the UPDATE payload, so a
 * re-run of the seed (db-init on every container/box restart) leaves the live
 * value untouched instead of clobbering it back to the fixture default. Every
 * other column still converges to the fixture, so reference-definition changes
 * (name, stage, priority, …) continue to propagate. Without this, an admin who
 * toggled governance ON saw it silently reset to OFF on the next restart.
 */
export async function upsertRows(
  delegate: UpsertDelegate,
  rows: Record<string, unknown>[],
  key: string,
  preserveFields: readonly string[] = [],
): Promise<number> {
  for (const row of rows) {
    if (!(key in row)) {
      throw new Error(
        `loadFixture: row missing key field "${key}": ${JSON.stringify(row)}`,
      )
    }
    const update = preserveFields.length
      ? Object.fromEntries(
          Object.entries(row).filter(([k]) => !preserveFields.includes(k)),
        )
      : row
    await delegate.upsert({ where: { [key]: row[key] }, create: row, update })
  }
  return rows.length
}

const __dirname = dirname(fileURLToPath(import.meta.url))
const FIXTURES = resolve(__dirname, '../fixtures')

export function readFixture(table: string): Record<string, unknown>[] {
  return JSON.parse(
    readFileSync(resolve(FIXTURES, `${table}.json`), 'utf8'),
  ) as Record<string, unknown>[]
}
