import type { PrismaClient } from '@prisma/client'
import { readFixture, upsertRows, reconcileRows } from './loadFixture.ts'
import { loadModelIdByCode, resolveModelRefs, type ModelIdByCode } from './modelRefs.ts'

/** Remap DB snake_case column name to camelCase Prisma field name. */
function snakeToCamel(s: string): string {
  return s.replace(/_([a-z])/g, (_, c: string) => c.toUpperCase())
}

/** Remap every key in a row from snake_case to camelCase. */
function camelizeRow(row: Record<string, unknown>): Record<string, unknown> {
  return Object.fromEntries(
    Object.entries(row).map(([k, v]) => [snakeToCamel(k), v]),
  )
}

export const REFERENCE_TABLES: { fixture: string; delegate: keyof PrismaClient; key: string }[] = [
  { fixture: 'Provider', delegate: 'provider', key: 'id' },
  // Matched on id OR code (see RECONCILE_FIXTURES): the provider wizard lets
  // an admin add a catalog model by hand under a generated id, and catalog
  // codes get renamed. Either case defeats a single-key upsert. Order no longer
  // decides blast radius — seedReference collects per-table failures rather than
  // aborting — but a Model conflict is still a seed failure and still worth
  // matching correctly.
  { fixture: 'Model', delegate: 'model', key: 'code' },
  { fixture: 'interception_domain', delegate: 'interceptionDomain', key: 'id' },
  { fixture: 'interception_path', delegate: 'interceptionPath', key: 'id' },
  { fixture: 'rule_pack', delegate: 'rulePack', key: 'id' },
  // Required config: hooks, scheduled jobs, installed rule-packs.
  { fixture: 'HookConfig', delegate: 'hookConfig', key: 'id' },
  { fixture: 'rule_pack_install', delegate: 'rulePackInstall', key: 'id' },
  { fixture: 'Job', delegate: 'job', key: 'id' },
  { fixture: 'rule', delegate: 'rule', key: 'id' },
  { fixture: 'thing_config_template', delegate: 'thingConfigTemplate', key: 'type' },
  { fixture: 'IamPolicy', delegate: 'iamPolicy', key: 'name' },
  // Standard local-password IdP — required or login's /authserver/* is skipped.
  { fixture: 'IdentityProvider', delegate: 'identityProvider', key: 'id' },
  // Standard IAM groups + their policy attachments are reference RBAC
  // scaffolding (groups before attachments: attachment.groupId → group.id;
  // attachment.policyId → IamPolicy.id seeded just above).
  { fixture: 'IamGroup', delegate: 'iamGroup', key: 'id' },
  { fixture: 'IamGroupPolicyAttachment', delegate: 'iamGroupPolicyAttachment', key: 'id' },
  // Standard public OAuth clients — required for any CP/agent/TUI login.
  { fixture: 'OAuthClient', delegate: 'oAuthClient', key: 'id' },
  { fixture: 'system_metadata', delegate: 'systemMetadata', key: 'key' },
  { fixture: 'metric_ops_retention_config', delegate: 'metricOpsRetentionConfig', key: 'layer' },
  { fixture: 'cache_adapter_config', delegate: 'cacheAdapterConfig', key: 'adapterType' },
  { fixture: 'cache_provider_config', delegate: 'cacheProviderConfig', key: 'providerId' },
  { fixture: 'gateway_passthrough_config_global', delegate: 'gatewayPassthroughConfigGlobal', key: 'id' },
  { fixture: 'ai_guard_config', delegate: 'aIGuardConfig', key: 'id' },
  { fixture: 'AlertRule', delegate: 'alertRule', key: 'id' },
  // Default application-VK monthly spend cap ($50k). Production default; the demo
  // seed deletes it (demo manages quotas via QuotaOverride fixtures).
  { fixture: 'QuotaPolicy', delegate: 'quotaPolicy', key: 'id' },
  { fixture: 'semantic_cache_config', delegate: 'semanticCacheConfig', key: 'id' },
  // Standard routing rules (only smart-auto-routing enabled; default route off).
  { fixture: 'RoutingRule', delegate: 'routingRule', key: 'id' },
]

/**
 * Fixtures whose JSON uses DB snake_case column names that @map to camelCase
 * Prisma field names. Rows must be camelized before upsert so the Prisma
 * create/update payload uses the correct Prisma field names.
 *
 * Derivation: compare the Prisma-generated *CreateInput type against the
 * fixture JSON keys — only tables where the field names differ are listed here.
 * Tables like ai_guard_config and semantic_cache_config intentionally use
 * snake_case Prisma field names (the schema declares them that way), so they
 * need no remapping.
 */
const CAMELIZE_FIXTURES = new Set([
  'Provider',                          // adapter_type → adapterType, streaming_* etc.
  'interception_domain',               // host_pattern → hostPattern, adapter_id → adapterId etc.
  'interception_path',                 // domain_id → domainId, path_pattern → pathPattern etc.
  'system_metadata',                   // updated_at → updatedAt, updated_by → updatedBy
  'metric_ops_retention_config',       // retention_days → retentionDays, updated_at → updatedAt
  'cache_adapter_config',              // adapter_type → adapterType, updated_at → updatedAt
  'cache_provider_config',             // provider_id → providerId, updated_at → updatedAt
  'gateway_passthrough_config_global', // expires_at → expiresAt, enabled_by → enabledBy, updated_at → updatedAt
  'IamGroup',                          // idp_group_name → idpGroupName, identity_provider_id → identityProviderId
])

/**
 * Tables with composite primary keys that require a compound where clause.
 * Key: fixture name. Value describes how to build the Prisma where clause.
 *
 * Prisma compound-key upsert uses a named wrapper object in `where`:
 *   { <compoundKeyName>: { <field1>: ..., <field2>: ... } }
 * not individual fields at the top level.
 */
const COMPOUND_KEY_FIXTURES: Record<string, { whereKey: string; fields: string[] }> = {
  thing_config_template: { whereKey: 'type_config_key', fields: ['type', 'config_key'] },
}

/**
 * Tables matched on the fixture's `id` OR its unique business key, rather than
 * on one of them. Both keys of such a row can independently already exist in a
 * live database — under a row the admin created through the UI (own id, our
 * business key) or under a row we are renaming (our id, old business key) — and
 * either case makes a single-key upsert fall through to create and die on the
 * other key's unique constraint. See reconcileRows.
 *
 * `rule` qualifies for the same reason Model does, via a composite key. Its ids
 * are derived from pack content, so regenerating a pack re-derives an id for a
 * rule that already exists under (packId, ruleId) — matching on `id` alone then
 * creates, and the unique constraint rejects it. That is not hypothetical: a
 * live deployment carried 358 rules whose ids matched and 65 whose did not, and
 * the first of the 65 aborted the whole reference seed. Because the loop has no
 * try/catch, every table after `rule` — IamPolicy, IdentityProvider and
 * OAuthClient among them — silently never ran.
 */
const RECONCILE_FIXTURES: Record<string, (string | string[])[]> = {
  // Every unique key Model has besides id. A key omitted here is still enforced
  // by the database at create time.
  Model: ['code', ['providerId', 'providerModelId']],
  rule: [['packId', 'ruleId']],
}

/**
 * Fields the DEPLOYMENT owns and the fixture only contributes to: the seed's
 * values are added, and nothing the deployment registered is removed. Every
 * other field is replaced by the fixture, which is right for a catalog fact and
 * wrong for a value only the install can know.
 *
 * `OAuthClient.redirectUris` is the case that exists. Each install answers on
 * its own domain, so its callback URL is deployment configuration; the fixture
 * ships the localhost URLs a developer needs and cannot know the rest.
 * Replacing the column deletes the deployment's URL, and the authorize endpoint
 * rejects an unregistered redirect_uri — so the next re-seed, run for any
 * unrelated reason, locks every admin out of the console. See upsertRows.
 */
const UNION_FIXTURE_FIELDS: Record<string, string[]> = {
  OAuthClient: ['redirectUris'],
}

/**
 * Fixture whose ids every `model:<code>` reference resolves against. Seeding it
 * can adopt a live row and keep that row's id, so the ids are read back from the
 * database once it has been seeded, and every later fixture resolves against
 * those. A reference in a fixture seeded before this one has no ids to resolve
 * against and fails rather than passing an unresolved string through.
 */
const MODEL_FIXTURE = 'Model'

async function seedTable(
  prisma: PrismaClient,
  { fixture, delegate, key }: { fixture: string; delegate: keyof PrismaClient; key: string },
  modelIdByCode: ModelIdByCode | null,
): Promise<number> {
  const rawRows = resolveModelRefs(readFixture(fixture), modelIdByCode, fixture)
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const del = (prisma as any)[delegate] as {
    upsert: (args: { where: unknown; create: unknown; update: unknown }) => Promise<unknown>
  }

  if (RECONCILE_FIXTURES[fixture]) {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    return await reconcileRows(del as any, rawRows, RECONCILE_FIXTURES[fixture])
  }
  if (COMPOUND_KEY_FIXTURES[fixture]) {
    // Composite-key table: Prisma requires the compound key wrapped under its
    // named alias (e.g. { type_config_key: { type, config_key } }).
    // thing_config_template uses snake_case Prisma field names directly.
    const { whereKey, fields } = COMPOUND_KEY_FIXTURES[fixture]
    let n = 0
    for (const row of rawRows) {
      const compoundValue: Record<string, unknown> = {}
      for (const k of fields) {
        compoundValue[k] = row[k]
      }
      await del.upsert({ where: { [whereKey]: compoundValue }, create: row, update: row })
      n++
    }
    return n
  }
  const unionFields = UNION_FIXTURE_FIELDS[fixture] ?? []
  if (CAMELIZE_FIXTURES.has(fixture)) {
    // Fixture JSON uses DB snake_case column names; Prisma expects camelCase.
    return await upsertRows(del, rawRows.map(camelizeRow), key, unionFields)
  }
  // Fixture JSON already matches Prisma field names — pass through directly.
  return await upsertRows(del, rawRows, key, unionFields)
}

/**
 * Seed every reference table, then report. A table that fails does NOT stop the
 * ones after it.
 *
 * The loop used to abort on the first throw, which made one table's data
 * conflict a deployment outage: a live deployment aborted at `rule` (table 10 of
 * 26) and silently skipped IamPolicy, IdentityProvider, IamGroup,
 * IamGroupPolicyAttachment and OAuthClient — on a fresh install, no super-admin
 * and no way to log in. The blast radius had nothing to do with the table that
 * failed; it was decided by position in this list.
 *
 * So a failure is collected and the loop continues. Downstream tables that
 * genuinely depend on a failed one (IamGroupPolicyAttachment on IamPolicy, rule
 * on rule_pack) fail too and are collected in turn — every real problem is still
 * reported, and one is no longer able to hide the others. The seed still fails
 * overall: the aggregate throw at the end names every table, so this converts a
 * silent partial seed into a loud complete report, never into a green one.
 */
export async function seedReference(prisma: PrismaClient): Promise<void> {
  const failures: { fixture: string; error: unknown }[] = []
  let modelIdByCode: ModelIdByCode | null = null

  for (const table of REFERENCE_TABLES) {
    try {
      const n = await seedTable(prisma, table, modelIdByCode)
      console.log(`[seed:ref] ${table.fixture}: ${n} rows`)
      if (table.fixture === MODEL_FIXTURE) {
        // Read back only when Model actually seeded. A failed Model leaves the
        // table in whatever state the failure stopped at, and resolving later
        // fixtures against that would point them at rows this seed never
        // vouched for — silently, because none of these columns is a foreign
        // key. Leaving the map null instead fails every dependent fixture on
        // the reference it could not resolve, which the loop collects and the
        // aggregate throw names. The seed fails either way; this decides
        // whether it fails honestly.
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        modelIdByCode = await loadModelIdByCode((prisma as any)[table.delegate])
      }
    } catch (error) {
      failures.push({ fixture: table.fixture, error })
      const message = error instanceof Error ? error.message : String(error)
      console.error(`[seed:ref] ${table.fixture}: FAILED — ${message}`)
    }
  }
  if (failures.length > 0) {
    throw new AggregateError(
      failures.map((f) => f.error),
      `seedReference: ${failures.length} of ${REFERENCE_TABLES.length} tables failed ` +
        `(${failures.map((f) => f.fixture).join(', ')}); every other table was seeded`,
    )
  }
}
