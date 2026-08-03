/**
 * In-memory stand-in for a Prisma delegate over the Model table, used by the
 * seed tests. Not in the test glob (leading '_').
 *
 * It enforces what the real table enforces, because a fake that does not cannot
 * fail the way the database fails:
 *
 *  - a created row is visible to the next read, so a read-back sees it;
 *  - an update writes THROUGH to the row, so a read-back sees the new value —
 *    without this, a test cannot tell whether the seed adopted a live row or
 *    merely recorded an intent to;
 *  - every unique key the table declares is enforced, so a fixture that would
 *    collide in the database collides here rather than silently landing a
 *    duplicate.
 */

/**
 * Model's real unique keys (`schema/providers.prisma`). A fake that does not
 * enforce these cannot fail a `create` the way the database does, and a
 * create-time constraint violation is the entire failure mode reconcileRows
 * exists to prevent — so a fake without them can only ever prove the happy path.
 *
 * The composite key is the one most easily left out and the one that bites: an
 * admin-created row can share it with a fixture while matching neither the id
 * nor the code, so a reconcile blind to it creates and the database rejects.
 */
const MODEL_UNIQUES = [['id'], ['code'], ['providerId', 'providerModelId']]

/**
 * @param initial  rows already in the "database"
 * @param uniques  unique keys the fake enforces, like the DB does. A key is only
 *   checked when the row carries every one of its columns — Postgres does not
 *   collide NULLs, and neither should this.
 */
export function modelTable(
  initial: Record<string, unknown>[] = [],
  uniques: string[][] = MODEL_UNIQUES,
) {
  const rows: Record<string, unknown>[] = initial.map((r) => ({ ...r }))
  const updates: { where: Record<string, unknown>; data: Record<string, unknown> }[] = []
  const creates: Record<string, unknown>[] = []

  /** The unique key `data` would collide with among `others`, or null. */
  function collidingKey(
    data: Record<string, unknown>,
    others: Record<string, unknown>[],
  ): string[] | null {
    for (const key of uniques) {
      if (!key.every((f) => f in data)) continue
      if (others.some((r) => key.every((f) => r[f] === data[f]))) return key
    }
    return null
  }

  /** Shaped like Prisma's own, so a test asserting on it asserts what a real failure says. */
  function uniqueViolation(key: string[]): Error {
    return Object.assign(
      new Error(`Unique constraint failed on the fields: (\`${key.join('`, `')}\`)`),
      { code: 'P2002' },
    )
  }

  const delegate = {
    // Mirrors Prisma's { where: { OR: [...] } }: a row matches when it satisfies
    // every field of at least one branch. No `where` reads the whole table.
    findMany: async (
      args: { where?: Record<string, unknown>; select?: Record<string, boolean> } = {},
    ) => {
      const branches = args.where?.OR as Record<string, unknown>[] | undefined
      const matched = branches
        ? rows.filter((row) =>
            branches.some((branch) =>
              Object.entries(branch).every(([field, value]) => row[field] === value),
            ),
          )
        : rows
      const picked = args.select ? Object.keys(args.select).filter((k) => args.select![k]) : null
      return matched.map((row) =>
        picked ? Object.fromEntries(picked.map((k) => [k, row[k]])) : { ...row },
      )
    },

    update: async (args: { where: Record<string, unknown>; data: Record<string, unknown> }) => {
      updates.push(args)
      const row = rows.find((r) => r.id === args.where.id)
      if (!row) {
        throw new Error(`fake: update targets id "${String(args.where.id)}", which no row holds`)
      }
      const collision = collidingKey(
        { ...row, ...args.data },
        rows.filter((r) => r !== row),
      )
      if (collision) throw uniqueViolation(collision)
      Object.assign(row, args.data)
      return { ...row }
    },

    create: async (args: { data: Record<string, unknown> }) => {
      const { data } = args
      const collision = collidingKey(data, rows)
      if (collision) throw uniqueViolation(collision)
      creates.push(data)
      rows.push({ ...data })
      return { ...data }
    },
  }

  return { rows, updates, creates, delegate }
}
