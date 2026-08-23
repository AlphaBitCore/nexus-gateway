// tests/e2e-node/lib/loadenv.mjs — target-aware env loader for the Node suite.
//
// Mirrors tests/lib/loadenv.py (itself a mirror of loadenv.sh) so all three
// halves of the harness read the same files with the same precedence and the
// same safety guards. Hand-rolled parser rather than `dotenv` for the same
// reason loadenv.py hand-rolls one: the ~25 lines are cheaper than reasoning
// about a third party's override semantics, and they cannot drift from the
// Python twin's behaviour.

import fs from 'node:fs'
import path from 'node:path'

const VALID_TARGETS = ['local', 'dev', 'stg', 'prod']
const URL_VARS = [
  'NEXUS_HUB_URL',
  'NEXUS_CP_URL',
  'NEXUS_AI_GW_URL',
  'NEXUS_PROXY_URL',
  'NEXUS_UI_URL',
]

/** Walk up from this file looking for the tests/ layout marker. */
function repoTestsRoot() {
  let dir = path.dirname(new URL(import.meta.url).pathname)
  for (;;) {
    if (fs.existsSync(path.join(dir, '.env.local.example'))) return dir
    if (fs.existsSync(path.join(dir, 'tests', '.env.local.example'))) {
      return path.join(dir, 'tests')
    }
    const parent = path.dirname(dir)
    if (parent === dir) {
      throw new Error('loadenv.mjs: tests/.env.local.example not found in any ancestor')
    }
    dir = parent
  }
}

function parseEnvFile(file) {
  const out = {}
  if (!fs.existsSync(file)) return out
  for (const raw of fs.readFileSync(file, 'utf8').split('\n')) {
    let line = raw.trim()
    if (!line || line.startsWith('#')) continue
    if (line.startsWith('export ')) line = line.slice('export '.length)
    const eq = line.indexOf('=')
    if (eq === -1) continue
    const key = line.slice(0, eq).trim()
    let value = line.slice(eq + 1).trim()
    if (
      value.length >= 2 &&
      value[0] === value[value.length - 1] &&
      (value[0] === '"' || value[0] === "'")
    ) {
      value = value.slice(1, -1)
    }
    out[key] = value
  }
  return out
}

/**
 * Load tests/.env.<target>.example then tests/.env.<target> into process.env,
 * honouring non-overload semantics, and return the resolved target.
 */
export function load(target) {
  const chosen = target || process.env.NEXUS_TEST_TARGET || 'local'
  if (!VALID_TARGETS.includes(chosen)) {
    throw new Error(
      `loadenv.mjs: unknown target '${chosen}' (allowed: ${VALID_TARGETS.join('|')})`,
    )
  }
  const testsRoot = repoTestsRoot()
  const example = path.join(testsRoot, `.env.${chosen}.example`)
  const user = path.join(testsRoot, `.env.${chosen}`)
  if (!fs.existsSync(example) && !fs.existsSync(user)) {
    throw new Error(
      `loadenv.mjs: neither ${example} nor ${user} exists. Copy .env.${chosen}.example ` +
        `to .env.${chosen} and fill in values.`,
    )
  }

  // Snapshot pre-existing NEXUS_* keys so file values only fill gaps.
  const preexisting = new Set(Object.keys(process.env).filter((k) => k.startsWith('NEXUS_')))
  process.env.NEXUS_TEST_TARGET = chosen
  preexisting.add('NEXUS_TEST_TARGET')

  for (const file of [example, user]) {
    for (const [key, value] of Object.entries(parseEnvFile(file))) {
      if (preexisting.has(key)) continue
      process.env[key] = value
    }
  }

  // Safety guard, same direction as the Python/bash twins: a local run must not
  // be able to point at a remote deployment. dev/stg are remote by definition.
  if (chosen === 'local') {
    for (const name of URL_VARS) {
      const value = process.env[name]
      if (value && !value.includes('localhost') && !value.includes('127.0.0.1')) {
        throw new Error(
          `loadenv.mjs: target=local but ${name}=${value} does not reference localhost`,
        )
      }
    }
  } else if (chosen === 'prod') {
    const cp = process.env.NEXUS_CP_URL || ''
    if (!cp || cp.includes('localhost') || cp.includes('127.0.0.1')) {
      throw new Error(`loadenv.mjs: target=prod but NEXUS_CP_URL=${cp} is loopback`)
    }
  }

  return chosen
}
