/**
 * The seed must not own the `job` table.
 *
 * As a REFERENCE fixture `Job` would run as part of `npm run seed:prod`, and
 * the reference upsert writes every fixture column over a live row. All 47 rows
 * carry `enabled: true`, so a production re-seed would re-enable every job an
 * operator had switched off — data-retention included.
 *
 * The Hub's own store refuses to do exactly this: `jobs/store/job.go`'s
 * UpsertJob omits `enabled` from its update columns so that "a restart must not
 * clobber an admin's disable action". The seed must not be the one writer
 * breaking the rule the rest of the system states.
 *
 * BOTH HALVES ARE ASSERTED, and so is the non-vacuity of the absence: a
 * truncated registry, or a rename of the exported symbol, would otherwise make
 * "Job is not in the list" true for the wrong reason and this file would pass
 * while proving nothing.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

import { REFERENCE_TABLES } from '../reference/index.ts'

const here = dirname(fileURLToPath(import.meta.url))

test('the reference registry does not seed the job table', () => {
  // Non-vacuity first: the list must be real and populated, or its not
  // containing Job says nothing.
  assert.ok(
    Array.isArray(REFERENCE_TABLES) && REFERENCE_TABLES.length >= 10,
    `REFERENCE_TABLES has ${REFERENCE_TABLES?.length} entries — too few to trust an absence in it`,
  )
  assert.ok(
    REFERENCE_TABLES.some((t) => t.fixture === 'IamPolicy'),
    'a known reference fixture is missing, so this list is not the one that ships',
  )

  const job = REFERENCE_TABLES.find((t) => t.fixture === 'Job' || t.delegate === 'job')
  assert.equal(
    job,
    undefined,
    'the job table is seeded again — a production re-seed will re-enable every job an operator disabled',
  )
})

test('no Job fixture file exists to be picked up again', () => {
  const fixture = resolve(here, '../fixtures/Job.json')
  assert.equal(
    existsSync(fixture),
    false,
    `${fixture} exists — a fixture that exists is one registry line away from resetting the enable toggle again`,
  )
  // Non-vacuity: prove the path shape resolves for a fixture that DOES ship.
  assert.ok(
    existsSync(resolve(here, '../fixtures/IamPolicy.json')),
    'the fixtures directory did not resolve, so the absence above proves nothing',
  )
})
