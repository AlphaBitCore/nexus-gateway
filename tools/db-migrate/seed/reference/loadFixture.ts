import { readFileSync } from 'fs'
import { resolve, dirname } from 'path'
import { fileURLToPath } from 'url'

type UpsertDelegate = {
  upsert: (args: {
    where: Record<string, unknown>
    create: Record<string, unknown>
    update: Record<string, unknown>
  }) => Promise<unknown>
  // Only needed when a fixture declares union fields; see upsertRows.
  findMany?: (args: { select: Record<string, boolean> }) => Promise<Record<string, unknown>[]>
}

type CreateMissingDelegate = {
  findMany: (args: {
    where: Record<string, unknown>
    select: Record<string, boolean>
  }) => Promise<Record<string, unknown>[]>
  create: (args: { data: Record<string, unknown> }) => Promise<unknown>
}

type ReconcileDelegate = {
  findMany: (args: { where: Record<string, unknown> }) => Promise<Record<string, unknown>[]>
  update: (args: { where: Record<string, unknown>; data: Record<string, unknown> }) => Promise<unknown>
  create: (args: { data: Record<string, unknown> }) => Promise<unknown>
}

/**
 * Upsert rows for a table whose fixture carries BOTH a surrogate `id` and a
 * unique business key, matching a live row by any identity the fixture claims.
 * No single key is sufficient:
 *
 *  - `id` alone misses a row the admin created through the UI, which holds a
 *    generated id, so the upsert falls to create and dies on the business key's
 *    unique constraint.
 *  - The business key alone misses a row the fixture RENAMED (Model codes moved
 *    from dated ids to floating aliases), so the upsert falls to create and dies
 *    on the primary key the old row still holds.
 *  - Both together still miss an admin-created row that predates the rename and
 *    so carries neither the fixture's id nor its new code — only the old name.
 *    That name is exactly what the fixture now lists in `aliases`, so the
 *    aliases are part of the row's identity and are matched too. This mirrors
 *    the gateway, which already resolves a request's model against `code` OR
 *    `aliases`; a row reachable under a name is the row that name denotes.
 *
 * Matching on any of them finds the row and updates it in place, preserving the
 * identity that traffic_event.model_id and VirtualKey.allowedModels[].modelId
 * reference as plain, unconstrained columns — a rewritten id orphans them with
 * no error.
 *
 * `uniqueKeys` must list EVERY unique key the table has besides `id`, because a
 * key left out is a key the database will still enforce at `create` time. Model
 * has three (`id`, `code`, and `(providerId, providerModelId)`); listing only
 * the first two is how a wizard row named `my-gpt-5.6` — matching on neither id
 * nor code — reached `create` and died on the third.
 *
 * A key may be one column or several. A composite key is ONE identity: `rule` is
 * unique on (packId, ruleId) together, and either field alone is shared by
 * hundreds of rows, so its fields are matched conjointly, never as separate
 * alternatives. `aliases` is an alternate value for the FIRST key, which is the
 * one a rename moves, so it applies only where that key is a single column.
 */
export async function reconcileRows(
  delegate: ReconcileDelegate,
  rows: Record<string, unknown>[],
  uniqueKeys: (string | string[])[],
): Promise<number> {
  const keys = uniqueKeys.map((k) => (Array.isArray(k) ? k : [k]))
  const label = keys.map((k) => k.join('+')).join('", "')
  // A live row may satisfy two fixture rows' identities — e.g. a row holding
  // fixture A's id under fixture B's code. Whoever reaches it second must not
  // silently skip: the row would keep A's identity while B never lands, and
  // traffic_event rows recorded against it as B would resolve to A.
  const claimed = new Map<string, unknown>()
  for (const row of rows) {
    const missing = keys.flat().filter((k) => !(k in row))
    if (!('id' in row) || missing.length > 0) {
      throw new Error(
        `loadFixture: reconcile needs both "id" and "${label}": ${JSON.stringify(row)}`,
      )
    }
    const aliases = keys[0].length === 1 && Array.isArray(row.aliases) ? (row.aliases as unknown[]) : []
    const identities: Record<string, unknown>[] = [{ id: row.id }]
    for (const key of keys) {
      const identity: Record<string, unknown> = {}
      for (const f of key) identity[f] = row[f]
      identities.push(identity)
    }
    identities.push(...aliases.map((a) => ({ [keys[0][0]]: a })))

    const matches = await delegate.findMany({ where: { OR: identities } })
    const describe = () =>
      keys
        .map((k) => k.map((f) => `${f}="${String(row[f])}"`).join('+'))
        .join(', ') + ` (aliases: ${aliases.map(String).join(', ') || 'none'}) and id="${String(row.id)}"`
    if (matches.length > 1) {
      // The fixture's identities resolve to more than one live row — e.g. one
      // row already renamed and a second still under the old name. Updating
      // either would collide with the other, so fail with both ids named
      // rather than surface a raw constraint violation from deep in the seed.
      throw new Error(
        `loadFixture: ${describe()} resolve to different rows ` +
          `(${matches.map((m) => String(m.id)).join(', ')}); reconcile them by hand before seeding`,
      )
    }
    const { id: _identity, ...data } = row
    if (matches.length === 1) {
      const liveId = String(matches[0].id)
      const prior = claimed.get(liveId)
      if (prior !== undefined) {
        throw new Error(
          `loadFixture: ${describe()} resolves to live row "${liveId}", already claimed by ` +
            `fixture row "${String(prior)}"; one live row cannot be two models — ` +
            `reconcile them by hand before seeding`,
        )
      }
      claimed.set(liveId, row.id)
      await delegate.update({ where: { id: matches[0].id }, data })
    } else {
      await delegate.create({ data: row })
      claimed.set(String(row.id), row.id)
    }
  }
  return rows.length
}

/**
 * Upsert every row keyed by `key`. Idempotent: re-running converges.
 *
 * `id` is carried in `create` but never in `update`. When `key` is a business
 * column (Model.code, IamPolicy.name), a row that already exists under a
 * different id — anything an admin created through the UI rather than the seed
 * — would otherwise have its primary key rewritten to the fixture's. Nothing
 * raises: traffic_event.model_id, VirtualKey.allowedModels[].modelId and
 * IamGroupPolicyAttachment.policyId all hold ids as plain columns with no
 * foreign key, so the repoint silently orphans them. Keeping `id` out of
 * `update` makes the fixture own every field except the identity of a row it
 * did not create.
 */
export async function upsertRows(
  delegate: UpsertDelegate,
  rows: Record<string, unknown>[],
  key: string,
  unionFields: string[] = [],
): Promise<number> {
  const live = unionFields.length > 0 ? await readUnionFields(delegate, key, unionFields) : null

  for (const row of rows) {
    if (!(key in row)) {
      throw new Error(
        `loadFixture: row missing key field "${key}": ${JSON.stringify(row)}`,
      )
    }
    const { id: _identity, ...update } = row
    if (live) {
      const existing = live.get(String(row[key]))
      for (const field of unionFields) {
        update[field] = unionOf(existing?.[field], row[field], key, String(row[key]), field)
      }
    }
    await delegate.upsert({ where: { [key]: row[key] }, create: row, update })
  }
  return rows.length
}

/**
 * Insert the fixture rows that are absent and leave every live row exactly as
 * it is. Returns how many were created and how many were left alone. A fixture
 * row whose key repeats an earlier one is warned about and counted as left
 * alone — the count says how many rows this call did not write, not how many
 * the deployment already had.
 *
 * This is the write mode for sample data — rows a deployment is handed once and
 * then owns. `upsertRows` is the wrong tool there: it sends every non-id column
 * as the update payload, so a re-seed reasserts the fixture over whatever the
 * deployment did afterwards. The migrator re-seeds on EVERY `docker compose up`
 * (the documented upgrade path is `docker compose pull && docker compose up
 * -d`), which turns that into three concrete regressions on demo rows: an
 * AdminApiKey or VirtualKey an admin revoked comes back `enabled: true` /
 * `status: 'active'`, a real provider key an operator pasted into one of the
 * pre-wired `*-prod` Credential rows is overwritten with the fixture's
 * placeholder ciphertext and is unrecoverable, and every row's public seed
 * secret is republished into the database on each run for the rotation step to
 * clean up afterwards. Creating only what is missing removes all three: the
 * fixture defines what a fresh install starts with, and nothing more.
 *
 * A row the fixture ADDS later still lands, because absence is judged per row.
 * What no longer propagates is a change to a row that already exists — which is
 * the point: the deployment's copy wins.
 *
 * Absence is the only signal, so a row an operator DELETED is indistinguishable
 * from one that was never added, and comes back on the next run. Sample rows are
 * meant to be disabled rather than deleted; deleting one is not a way to keep it
 * gone.
 *
 * `prepare` runs only on rows that are actually being created. Secret material
 * is stamped there, so it is a creation-time property by construction rather
 * than by convention — and the caller does not pay to compute (or, for scrypt,
 * to spend a second on) values that would be discarded.
 */
export async function createMissingRows(
  delegate: CreateMissingDelegate,
  rows: Record<string, unknown>[],
  key: string,
  prepare?: (row: Record<string, unknown>) => Record<string, unknown>,
): Promise<{ created: number; kept: number }> {
  for (const row of rows) {
    if (!(key in row)) {
      throw new Error(
        `loadFixture: row missing key field "${key}": ${JSON.stringify(row)}`,
      )
    }
  }
  if (rows.length === 0) return { created: 0, kept: 0 }

  const live = await delegate.findMany({
    where: { [key]: { in: rows.map((r) => r[key]) } },
    select: { [key]: true },
  })
  const present = new Set(live.map((r) => String(r[key])))

  // Checked against the FIXTURE, not against what the deployment happens to
  // hold: a duplicate detected only while inserting would be reported on a
  // fresh database and then go silent on every upgrade run, which is when an
  // operator is more likely to be reading the output.
  const seenInFixture = new Set<string>()
  for (const row of rows) {
    const k = String(row[key])
    if (seenInFixture.has(k)) {
      console.warn(`[seed] fixture lists ${key}="${k}" more than once; only the first occurrence is applied`)
    }
    seenInFixture.add(k)
  }

  let created = 0
  for (const row of rows) {
    if (present.has(String(row[key]))) continue
    await delegate.create({ data: prepare ? prepare(row) : row })
    // Recorded so a fixture that repeats a key converges instead of hitting a
    // primary-key violation on the second create, which would leave this
    // table's remaining rows and every later table in the tier unapplied on
    // every run. The warning above is what keeps that from being silent.
    present.add(String(row[key]))
    created++
  }
  return { created, kept: rows.length - created }
}

/** Read the union fields of every live row, keyed by the fixture's business key. */
async function readUnionFields(
  delegate: UpsertDelegate,
  key: string,
  fields: string[],
): Promise<Map<string, Record<string, unknown>>> {
  if (!delegate.findMany) {
    throw new Error(
      `loadFixture: union fields ${fields.join(', ')} need to read the live rows, ` +
        `but this delegate exposes no findMany`,
    )
  }
  const select: Record<string, boolean> = { [key]: true }
  for (const f of fields) select[f] = true
  const rows = await delegate.findMany({ select })
  return new Map(rows.map((r) => [String(r[key]), r]))
}

/**
 * The live values plus the fixture's, in that order, deduplicated.
 *
 * A union field is one the DEPLOYMENT owns and the fixture only contributes to.
 * `OAuthClient.redirectUris` is the case: every install answers on its own
 * domain, so its callback URL is deployment configuration, and the fixture —
 * which ships the localhost URLs a developer needs — cannot know it. Replacing
 * the column, as every other field is replaced, deletes that URL on the next
 * seed and the deployment's admins can no longer log in: the authorize endpoint
 * rejects an unregistered redirect_uri, so the failure is total and arrives
 * whenever someone re-seeds for an unrelated reason.
 *
 * Union is deliberately one-way. The seed guarantees its own values are
 * present and never removes one it did not ship, because it cannot tell a URL
 * an admin registered on purpose from one left over — and between re-adding a
 * URL an admin removed and deleting one they still need, only the second locks
 * them out. Retracting a value is the admin API's job, which is where the rest
 * of a client's runtime state already lives.
 */
function unionOf(
  liveValue: unknown,
  fixtureValue: unknown,
  key: string,
  rowKey: string,
  field: string,
): unknown {
  if (!Array.isArray(fixtureValue)) {
    throw new Error(
      `loadFixture: ${key}=${rowKey}: union field "${field}" must be an array in the fixture, got ${typeof fixtureValue}`,
    )
  }
  const liveItems = Array.isArray(liveValue) ? liveValue : []
  return [...new Set([...liveItems, ...fixtureValue])]
}

const __dirname = dirname(fileURLToPath(import.meta.url))
const FIXTURES = resolve(__dirname, '../fixtures')

export function readFixture(table: string): Record<string, unknown>[] {
  return JSON.parse(
    readFileSync(resolve(FIXTURES, `${table}.json`), 'utf8'),
  ) as Record<string, unknown>[]
}
