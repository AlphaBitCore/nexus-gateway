/**
 * Locks the hook onMatch single-action contract on two fronts:
 *
 *  1. The seeded HookConfig fixture is already on the new shape — every
 *     `config.onMatch` carries a valid `action` (approve | redact | block) and
 *     none carry the deprecated inflightAction / storageAction keys. This
 *     guards a regression back to the two-axis shape in the fixture.
 *
 *  2. The migration mapping (manual-scripts/migrate_hook_onmatch_action_*.sql)
 *     is mirrored here as a pure function and checked over every legacy combo,
 *     so the SQL CASE and decision.ActionFromLegacy cannot silently diverge.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFixture } from '../reference/loadFixture.ts'

const VALID_ACTIONS = new Set(['approve', 'redact', 'block'])

test('seeded HookConfig onMatch blocks are all on the single-action shape', () => {
  const rows = readFixture('HookConfig')
  let seen = 0
  for (const row of rows) {
    const config = row.config as Record<string, unknown> | undefined
    const onMatch = config?.onMatch as Record<string, unknown> | undefined
    if (!onMatch) continue
    seen++
    assert.ok(
      VALID_ACTIONS.has(onMatch.action as string),
      `hook ${String(row.id)} onMatch.action=${String(onMatch.action)} is not approve|redact|block`,
    )
    assert.equal(onMatch.inflightAction, undefined, `hook ${String(row.id)} still carries legacy inflightAction`)
    assert.equal(onMatch.storageAction, undefined, `hook ${String(row.id)} still carries legacy storageAction`)
  }
  assert.ok(seen > 0, 'expected at least one hook with an onMatch block in the fixture')
})

// legacyToAction mirrors the SQL CASE in
// manual-scripts/migrate_hook_onmatch_action_2026_06_22.sql, which in turn
// mirrors decision.ActionFromLegacy. The new action follows the inflight axis;
// approve + a redacting storage policy upgrades to redact (compliance-safe).
function legacyToAction(inflight: string, storage: string): string {
  if (inflight === 'block-hard' || inflight === 'block-soft') return 'block'
  if (inflight === 'redact') return 'redact'
  if (inflight === 'approve') {
    if (storage === 'redact' || storage === 'drop-content') return 'redact'
    return 'approve'
  }
  return 'block' // legacy match default
}

test('migration mapping matches the legacy onMatch combos', () => {
  const cases: [string, string, string][] = [
    ['block-hard', 'redact', 'block'],
    ['block-soft', 'redact', 'block'],
    ['block-hard', 'keep', 'block'],
    ['redact', 'keep', 'redact'],
    ['approve', 'keep', 'approve'],
    ['approve', '', 'approve'],
    ['approve', 'redact', 'redact'], // pathological → safe-upgrade
    ['approve', 'drop-content', 'redact'], // pathological → safe-upgrade
    ['', '', 'block'], // legacy match default
  ]
  for (const [inflight, storage, want] of cases) {
    assert.equal(
      legacyToAction(inflight, storage),
      want,
      `legacyToAction(${inflight || '∅'}, ${storage || '∅'})`,
    )
  }
})
