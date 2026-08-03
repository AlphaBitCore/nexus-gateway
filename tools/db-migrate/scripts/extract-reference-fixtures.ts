import 'dotenv/config'
import { mkdirSync, writeFileSync } from 'fs'
import { resolve, dirname } from 'path'
import { fileURLToPath } from 'url'
import pg from 'pg'
import { toModelRefs } from '../seed/reference/modelRefs.ts'

// One-time / re-runnable extractor: materialize the 1.0 reference catalog as
// versioned JSON fixtures from a source DB. Point DATABASE_URL at a scratch DB
// that has been loaded with the full legacy seed pipeline (schema push + the
// operational snapshot + every default-setting domain step), so the extracted
// rows already carry the lossless column set (cachedInput* prices,
// capabilityJson, outputModalities, embedding catalog, time-sensitive rules)
// that pg_dump's short-form INSERT used to drop.
//
// Reference tables ship in every install. Operational instances (Thing,
// NexusUser, Organization, VirtualKey, Credential, Alert, Job, memberships,
// assignments, tokens) are intentionally absent — they are demo-only (Tier B)
// or runtime state. `where` filters operational rows out at the SQL layer; the
// CURATION map below normalizes captured-drift values back to product defaults.
const REFERENCE: { table: string; where?: string; orderBy: string }[] = [
  { table: 'Provider', orderBy: 'id' },
  // Model.json is intentionally NOT extracted here: it is GENERATED from
  // tools/db-migrate/model-catalog.json (the single source of truth) by
  // gen-model-catalog.mjs. Extracting it from a DB would fight the catalog and
  // trip `check:model-catalog`. Edit the catalog + `npm run gen:model-catalog`.
  { table: 'interception_domain', orderBy: 'id' },
  { table: 'interception_path', orderBy: 'id' },
  { table: 'rule', orderBy: 'id' },
  { table: 'rule_pack', orderBy: 'id' },
  // Required config: compliance hooks, scheduled-job definitions, and which
  // rule-packs are installed (rule_pack_install FK → rule_pack + HookConfig).
  { table: 'HookConfig', orderBy: 'id' },
  { table: 'rule_pack_install', orderBy: 'id' },
  { table: 'Job', orderBy: 'id' },
  { table: 'thing_config_template', orderBy: 'type, config_key' },
  { table: 'IamPolicy', where: `type = 'managed'`, orderBy: 'name' },
  // The standard local-password IdP — login's /authserver/* routes are skipped
  // entirely when no IdentityProvider row exists (authserver mount.go).
  { table: 'IdentityProvider', orderBy: 'id' },
  // Standard IAM groups ship in every install (source='local', no managed flag).
  // Attachments link each group to its managed policy; both are reference data.
  { table: 'IamGroup', orderBy: 'name' },
  { table: 'IamGroupPolicyAttachment', orderBy: 'id' },
  // Standard public PKCE OAuth clients (cp-ui / agent-desktop / tui) ship in
  // every install — login can't resolve a client without them. cp-ui's
  // redirectUris are curated to localhost in the committed fixture (admins add
  // their own deployment domain via the CP UI).
  { table: 'OAuthClient', orderBy: 'id' },
  // Operational system_metadata rows are excluded: a runtime version counter,
  // a real HMAC secret (must never live in the repo), the per-deployment setup
  // wizard state, and a SIEM forward watermark.
  {
    table: 'system_metadata',
    where: `key NOT IN ('agent.config.version', 'hub.spill_upload_secret', 'setup.state', 'siem.bridge.checkpoint')`,
    orderBy: 'key',
  },
  { table: 'metric_ops_retention_config', orderBy: 'layer' },
  { table: 'cache_adapter_config', orderBy: 'adapter_type' },
  { table: 'cache_provider_config', orderBy: 'provider_id' },
  { table: 'gateway_passthrough_config_global', orderBy: 'id' },
  { table: 'ai_guard_config', orderBy: 'id' },
  { table: 'AlertRule', orderBy: 'id' },
  { table: 'semantic_cache_config', orderBy: 'id' },
  // Standard routing rules — only smart-auto-routing ('auto') ships enabled so a
  // fresh install can route/test out of the box; the rest (incl. the default
  // single route) ship disabled.
  { table: 'RoutingRule', orderBy: 'priority DESC' },
]

// Value normalizations applied after extraction. The operational snapshot
// captured a few rows in a transient runtime state that the default-setting
// steps do not reset (they use ON CONFLICT DO NOTHING). Normalize them to the
// product default so a fresh install ships clean.
const CURATION: Record<string, (row: Record<string, unknown>) => void> = {
  gateway_passthrough_config_global(row) {
    // Snapshot caught a temporary bypass-all state; the product default (and
    // the schema column default) is every bypass flag off.
    const cfg = row.config as Record<string, unknown> | null
    if (cfg && typeof cfg === 'object') {
      cfg.bypassCache = false
      cfg.bypassHooks = false
      cfg.bypassNormalize = false
    }
  },
  thing_config_template(row) {
    // The ai-gateway/virtual_keys template captured a transient
    // 'op: invalidate' cache-invalidation command holding an operational VK
    // id. Reset to empty desired state, matching sibling config keys
    // (models/providers/organizations).
    if (row.config_key === 'virtual_keys') {
      row.state = {}
      row.version = 1
    }
  },
}

// ─── Model references ─────────────────────────────────────────────────────────
//
// A fixture must not carry the source database's Model ids. Seeding a model
// matches a live row on its code or an alias and keeps THAT row's id, so an id
// captured here names nothing in any database that already knew the model — and
// none of the columns below is a foreign key, so nothing would raise. Extraction
// rewrites each id to the `model:<code>` reference the seed resolves against the
// live catalog.
const MODEL_REF_TABLES = new Set(['ai_guard_config', 'RoutingRule'])
const DEMO_MODEL_REF_TABLES = new Set(['VirtualKey'])

// Tier-B demo tenant: operational-instance rows extracted into seed/fixtures/demo/.
// Ordered FK-safe AND principals-first: NexusUser / AdminApiKey / VirtualKey are
// extracted before the polymorphic-principal tables (IamGroupMembership,
// IamPolicyAttachment) so those can be filtered to existing principals.
const DEMO: { table: string; where?: string; orderBy: string }[] = [
  { table: 'Organization', orderBy: 'id' },
  { table: 'Project', orderBy: 'id' },
  { table: 'NexusUser', orderBy: 'id' },
  { table: 'AdminApiKey', orderBy: 'id' },
  { table: 'VirtualKey', orderBy: 'id' },
  { table: 'QuotaPolicy', orderBy: 'id' },
  { table: 'QuotaOverride', orderBy: 'id' },
  { table: 'Credential', orderBy: 'id' },
  { table: 'IamGroupMembership', orderBy: 'id' },
  { table: 'IamPolicyAttachment', orderBy: 'id' },
]

// Tables with a polymorphic (principalType, principalId) reference. The snapshot
// carries dangling memberships for principals that no longer exist (e.g. 30
// api_key memberships for deleted admin keys); drop any row whose principal is
// absent from the extracted demo set so the demo tenant is referentially clean.
const PRINCIPAL_TABLES = new Set(['IamGroupMembership', 'IamPolicyAttachment'])
const PRINCIPAL_SOURCE: Record<string, string> = {
  nexus_user: 'NexusUser',
  api_key: 'AdminApiKey',
  virtual_key: 'VirtualKey',
}

// Columns whose values are source-system secret material (hashed passwords,
// encrypted ciphertext, key hashes/prefixes). Nulled during demo extraction so
// that committed fixtures never carry real credentials; the seed-time builder
// re-stamps these fields with freshly generated test values.
const DEMO_NULL: Record<string, string[]> = {
  NexusUser: ['passwordHash'],
  AdminApiKey: ['keyHash', 'keyPrefix'],
  Credential: ['encryptedKey', 'encryptionIv', 'encryptionTag'],
  VirtualKey: ['keyHash', 'keyPrefix'],
}

const __dirname = dirname(fileURLToPath(import.meta.url))
const OUT = resolve(__dirname, '../seed/fixtures')

async function main() {
  const url = process.env.DATABASE_URL
  if (!url) throw new Error('extract: DATABASE_URL required (point at scratch DB loaded with the full legacy seed pipeline)')
  const client = new pg.Client({ connectionString: url })
  await client.connect()
  mkdirSync(OUT, { recursive: true })
  try {
    const { rows: modelRows } = await client.query<{ id: string; code: string }>(
      `SELECT id, code FROM "Model"`,
    )
    const codeById = new Map(modelRows.map((m) => [m.id, m.code]))

    for (const { table, where, orderBy } of REFERENCE) {
      const sql = `SELECT row_to_json(t) AS row FROM "${table}" t ${where ? `WHERE ${where}` : ''} ORDER BY ${orderBy}`
      const { rows } = await client.query<{ row: Record<string, unknown> }>(sql)
      const curate = CURATION[table]
      const data = rows.map((r) => {
        if (curate) curate(r.row)
        if (!MODEL_REF_TABLES.has(table)) return r.row
        return toModelRefs(r.row, codeById, null, table) as Record<string, unknown>
      })
      writeFileSync(resolve(OUT, `${table}.json`), JSON.stringify(data, null, 2) + '\n')
      console.log(`[extract] ${table}: ${data.length} rows${curate ? ' (curated)' : ''}`)
    }

    const DEMO_OUT = resolve(OUT, 'demo')
    mkdirSync(DEMO_OUT, { recursive: true })
    // id sets of already-extracted demo tables, for principal-orphan filtering.
    const extractedIds: Record<string, Set<unknown>> = {}
    for (const { table, where, orderBy } of DEMO) {
      const sql = `SELECT row_to_json(t) AS row FROM "${table}" t ${where ? `WHERE ${where}` : ''} ORDER BY ${orderBy}`
      const { rows } = await client.query<{ row: Record<string, unknown> }>(sql)
      const nullCols = DEMO_NULL[table]
      let data = rows.map((r) => {
        const row = r.row
        if (nullCols) {
          for (const col of nullCols) row[col] = null
        }
        if (!DEMO_MODEL_REF_TABLES.has(table)) return row
        return toModelRefs(row, codeById, null, `demo/${table}`) as Record<string, unknown>
      })
      let dropped = 0
      if (PRINCIPAL_TABLES.has(table)) {
        const before = data.length
        data = data.filter((row) => {
          const src = PRINCIPAL_SOURCE[String(row.principalType)]
          // Unknown principal types are kept (not our concern); known types must
          // resolve to an extracted principal of that table.
          if (!src) return true
          return extractedIds[src]?.has(row.principalId) ?? false
        })
        dropped = before - data.length
      }
      extractedIds[table] = new Set(data.map((row) => row.id))
      writeFileSync(resolve(DEMO_OUT, `${table}.json`), JSON.stringify(data, null, 2) + '\n')
      const tags = [nullCols ? `nulled: ${nullCols.join(', ')}` : '', dropped ? `dropped ${dropped} orphan(s)` : ''].filter(Boolean).join('; ')
      console.log(`[extract:demo] ${table}: ${data.length} rows${tags ? ` (${tags})` : ''}`)
    }
  } finally {
    await client.end()
  }
}
main().catch((e) => { console.error(e); process.exit(1) })
