type ModelReadDelegate = {
  findMany: (args: { select: Record<string, boolean> }) => Promise<Record<string, unknown>[]>
}

/**
 * Prefix marking a fixture string as a Model reference to resolve at seed time.
 * The suffix is a Model.code.
 */
export const MODEL_REF_PREFIX = 'model:'

/** Build the `model:<code>` reference a fixture uses to name a Model. */
export function modelRef(code: string): string {
  return MODEL_REF_PREFIX + code
}

/**
 * Map of Model.code → Model.id as the rows actually exist in the database.
 */
export type ModelIdByCode = ReadonlyMap<string, string>

/**
 * Read back every Model row as a code → id map.
 *
 * Must be called AFTER the Model fixture has been seeded, because the ids it
 * returns are the ids the rows ended up with, which is the entire point: seeding
 * a Model matches a live row on code or alias and keeps THAT row's id rather
 * than the fixture's, so any id the fixture carries for a row it did not create
 * names nothing. Reading the ids back after the fact is what makes a dependent
 * fixture point at the surviving row.
 *
 * Model.code is unique, so no two rows contend for a key.
 */
export async function loadModelIdByCode(delegate: ModelReadDelegate): Promise<ModelIdByCode> {
  const rows = await delegate.findMany({ select: { id: true, code: true } })
  const idByCode = new Map<string, string>()
  for (const row of rows) {
    idByCode.set(String(row.code), String(row.id))
  }
  return idByCode
}

/**
 * Replace every `model:<code>` string anywhere in `value` with that model's live
 * id, walking arrays and objects to any depth.
 *
 * Model ids appear in fixtures inside opaque JSON columns (a routing rule's
 * strategy tree, its match conditions and its fallback chain; a virtual key's
 * allowed-model list) as well as in plain columns. None of them is a foreign
 * key, so an id naming no row raises nothing at write time and instead fails
 * later as a route that resolves to nothing or a judge that cannot be called.
 * Naming models by code and resolving here keeps the reference correct whatever
 * id the row holds, and turns an unknown model into a seed-time error rather
 * than a dangling id.
 *
 * `where` names the fixture under resolution so a bad reference reports where it
 * came from.
 */
export function resolveModelRefs<T>(value: T, idByCode: ModelIdByCode | null, where: string): T {
  if (typeof value === 'string') {
    if (!value.startsWith(MODEL_REF_PREFIX)) return value
    const code = value.slice(MODEL_REF_PREFIX.length)
    if (!idByCode) {
      throw new Error(
        `loadFixture: ${where} references model "${code}" but is seeded before Model, ` +
          `so no model ids are known yet; move it after Model`,
      )
    }
    const id = idByCode.get(code)
    if (!id) {
      throw new Error(
        `loadFixture: ${where} references model "${code}", which no Model row provides; ` +
          `use a code the model catalog ships, or add the model to the catalog`,
      )
    }
    return id as unknown as T
  }
  if (Array.isArray(value)) {
    return value.map((item) => resolveModelRefs(item, idByCode, where)) as unknown as T
  }
  // Dates and nulls are values, not row shapes: walk plain objects only.
  if (value !== null && typeof value === 'object' && value.constructor === Object) {
    return Object.fromEntries(
      Object.entries(value as Record<string, unknown>).map(([k, v]) => [
        k,
        resolveModelRefs(v, idByCode, where),
      ]),
    ) as unknown as T
  }
  return value
}

/**
 * Keys whose string value is a Model.id. `models` is a match condition's array of
 * ids; array elements are checked under their container's key.
 */
const MODEL_ID_KEYS = new Set(['modelId', 'model_id', 'routerModelId', 'defaultModelId', 'models'])

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

/**
 * Rewrite every Model.id under a model-id key into the `model:<code>` reference a
 * fixture must carry — the inverse of resolveModelRefs, applied when capturing
 * fixtures from a database.
 *
 * Only uuid-shaped values are ids: an allowed-model entry also accepts globs
 * ("gpt-*"), which name no single row and pass through untouched. A uuid-shaped
 * value that no Model row provides is already dangling in the source database and
 * would be captured as a dead id, so it fails rather than being committed.
 *
 * `where` names the row's origin so a bad id reports where it came from.
 */
export function toModelRefs<T>(
  value: T,
  codeById: ReadonlyMap<string, string>,
  key: string | null,
  where: string,
): T {
  if (Array.isArray(value)) {
    return value.map((item) => toModelRefs(item, codeById, key, where)) as unknown as T
  }
  if (value !== null && typeof value === 'object' && value.constructor === Object) {
    return Object.fromEntries(
      Object.entries(value as Record<string, unknown>).map(([k, v]) => [
        k,
        toModelRefs(v, codeById, k, where),
      ]),
    ) as unknown as T
  }
  if (typeof value !== 'string' || key === null || !MODEL_ID_KEYS.has(key) || !UUID.test(value)) {
    return value
  }
  const code = codeById.get(value)
  if (!code) {
    throw new Error(
      `${where}.${key} holds model id "${value}", which no Model row provides — ` +
        `the source row is already dangling; repoint it before capturing`,
    )
  }
  return modelRef(code) as unknown as T
}
