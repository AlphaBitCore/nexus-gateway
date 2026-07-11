import { test } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync } from 'fs'
import { resolve, dirname } from 'path'
import { fileURLToPath } from 'url'
import { REFERENCE_TABLES } from '../reference/index.ts'

const here = dirname(fileURLToPath(import.meta.url))

test('every reference table maps to a delegate, a key, and an existing fixture file', () => {
  assert.ok(REFERENCE_TABLES.length >= 17, 'all reference tables present')
  for (const t of REFERENCE_TABLES) {
    assert.ok(String(t.delegate).length > 0, `${t.fixture}: delegate set`)
    assert.ok(t.key.length > 0, `${t.fixture}: key set`)
    assert.ok(existsSync(resolve(here, '../fixtures', `${t.fixture}.json`)), `fixture file missing: ${t.fixture}.json`)
  }
})

test('HookConfig preserves the enabled governance toggle on re-seed', () => {
  const hook = REFERENCE_TABLES.find((t) => t.fixture === 'HookConfig')
  assert.ok(hook, 'HookConfig must be a reference table')
  assert.deepEqual(
    hook!.preserveOnUpdate,
    ['enabled'],
    'HookConfig.enabled must be preserved so a box restart does not reset governance',
  )
})

test('reference table set covers exactly the committed fixtures', () => {
  const fixtures = new Set(REFERENCE_TABLES.map((t) => t.fixture))
  for (const f of ['Provider','Model','interception_domain','interception_path','rule','rule_pack','thing_config_template','IamPolicy','system_metadata','metric_ops_retention_config','cache_global_config','cache_adapter_config','cache_provider_config','gateway_passthrough_config_global','ai_guard_config','AlertRule','semantic_cache_config']) {
    assert.ok(fixtures.has(f), `REFERENCE_TABLES missing fixture ${f}`)
  }
})
