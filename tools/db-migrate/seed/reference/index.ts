import type { PrismaClient } from '@prisma/client'
import { readFixture, upsertRows } from './loadFixture.ts'

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
  { fixture: 'Model', delegate: 'model', key: 'id' },
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
  { fixture: 'cache_global_config', delegate: 'cacheGlobalConfig', key: 'id' },
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
  'cache_global_config',               // updated_at → updatedAt, updated_by → updatedBy
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

export async function seedReference(prisma: PrismaClient): Promise<void> {
  for (const { fixture, delegate, key } of REFERENCE_TABLES) {
    const rawRows = readFixture(fixture)
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const del = (prisma as any)[delegate] as {
      upsert: (args: { where: unknown; create: unknown; update: unknown }) => Promise<unknown>
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
      console.log(`[seed:ref] ${fixture}: ${n} rows`)
    } else if (CAMELIZE_FIXTURES.has(fixture)) {
      // Fixture JSON uses DB snake_case column names; Prisma expects camelCase.
      const rows = rawRows.map(camelizeRow)
      const n = await upsertRows(del, rows, key)
      console.log(`[seed:ref] ${fixture}: ${n} rows`)
    } else {
      // Fixture JSON already matches Prisma field names — pass through directly.
      const n = await upsertRows(del, rawRows, key)
      console.log(`[seed:ref] ${fixture}: ${n} rows`)
    }
  }
}
