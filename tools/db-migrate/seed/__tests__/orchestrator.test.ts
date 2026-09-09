/**
 * SEED_DEMO is read by two halves of the same system that disagreed about it.
 *
 * `docker/db-migrator/entrypoint.sh` tests `[ "${SEED_DEMO:-true}" = "true" ]`
 * — fail-CLOSED. `shouldSeedDemo` tested `envValue !== 'false'` — fail-OPEN on
 * every near-miss spelling. `SEED_DEMO=0` skipped the demo tier in the container
 * and seeded it here. The demo tier's credential plaintexts are derivable from
 * ids committed to this repository, so the open side is a credential disclosure.
 *
 * These tests assert the contract, not the implementation: both directions in
 * the spellings anyone would write, unset stays true, and anything unreadable
 * THROWS rather than silently picking a side.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { shouldSeedDemo } from '../seed.ts'

test('unset seeds the demo tier, so the dev quickstart is unchanged', () => {
  assert.equal(shouldSeedDemo(undefined), true)
  assert.equal(shouldSeedDemo(''), true, 'SEED_DEMO= reads as unset')
})

test('every spelling of yes seeds the demo tier', () => {
  for (const v of ['true', 'TRUE', 'True', ' true ', '1', 'yes', 'y', 'on', 'ON']) {
    assert.equal(shouldSeedDemo(v), true, `SEED_DEMO=${JSON.stringify(v)} should seed`)
  }
})

test('every spelling of no skips the demo tier', () => {
  // Each of these seeded the demo tier before the fix; `0` is the one the
  // container entrypoint has always read as no.
  for (const v of ['false', 'FALSE', 'False', ' false ', '0', 'no', 'n', 'off', 'OFF']) {
    assert.equal(shouldSeedDemo(v), false, `SEED_DEMO=${JSON.stringify(v)} must NOT seed demo credentials`)
  }
})

test('a value we cannot read is refused, not guessed', () => {
  for (const v of ['maybe', 'falsey', '2', 'true false', 'nope']) {
    assert.throws(
      () => shouldSeedDemo(v),
      /SEED_DEMO=.* is not a value I can read/,
      `SEED_DEMO=${JSON.stringify(v)} was answered instead of refused`,
    )
  }
})
