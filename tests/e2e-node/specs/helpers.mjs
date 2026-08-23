// Shared fixtures for the AP-3 Node SDK-compat specs.
//
// This suite is the Node MIRROR of tests/e2e-python/sdk_compat/: it covers each
// scenario category once (chat, stream, tools, structured, vision, reasoning,
// embeddings, errors) to prove the Node SDK is a drop-in too. The exhaustive
// per-case matrix lives on the Python side; keeping the full 60 cases in two
// languages would double the maintenance for a second copy of the same evidence.

import OpenAI from 'openai'

import { load } from '../lib/loadenv.mjs'

export const target = load()

function required(name) {
  const value = process.env[name]
  if (!value || value.startsWith('nvk_REPLACE_ME')) {
    // Hard failure, not a skip — same rule as the Python conftest. A suite that
    // skips when misconfigured is indistinguishable from one that passed.
    throw new Error(
      `sdk_compat(node): missing or placeholder ${name}. Copy tests/.env.${target}.example ` +
        `to tests/.env.${target} and set a real virtual key.`,
    )
  }
  return value
}

export const baseURL = required('NEXUS_AI_GW_URL').replace(/\/$/, '') + '/v1'
export const apiKey = required('NEXUS_TEST_VK')

/** The two-line change under test: base URL + key, nothing else. */
export function makeClient() {
  return new OpenAI({ baseURL, apiKey, maxRetries: 0 })
}

let catalogPromise

/** `GET /v1/models` once per worker, keyed by id. */
export async function getCatalog() {
  catalogPromise ??= (async () => {
    const resp = await fetch(`${baseURL}/models`, {
      headers: { Authorization: `Bearer ${apiKey}` },
    })
    if (!resp.ok) {
      throw new Error(
        `sdk_compat(node): GET ${baseURL}/models returned ${resp.status}. ` +
          `The suite cannot select models without the catalog.`,
      )
    }
    const rows = (await resp.json()).data ?? []
    if (rows.length === 0) {
      throw new Error('sdk_compat(node): the catalog is empty for this virtual key.')
    }
    return Object.fromEntries(rows.filter((r) => r.id).map((r) => [r.id, r]))
  })()
  return catalogPromise
}

/** Port of the Python conftest's model_kind: type field, then modalities, then id. */
export function modelKind(entry) {
  const declared = (entry.type ?? '').toLowerCase()
  if (declared === 'chat' || declared === 'embedding') return declared
  const outs = (entry.outputModalities ?? []).map((m) => m.toLowerCase())
  if (outs.includes('embedding')) return 'embedding'
  if (outs.includes('text')) return 'chat'
  if (entry.id.toLowerCase().includes('embed')) return 'embedding'
  return 'other'
}

/**
 * Capability-gated model selection. Returns null when nothing matches, so the
 * caller can `ctx.skip()` — Vitest has no fixture-level skip, and a provisioning
 * gap is not a compatibility regression.
 */
export function pickModel(catalog, { feature, family, lacks, kind = 'chat' } = {}) {
  for (const modelId of Object.keys(catalog).sort()) {
    const entry = catalog[modelId]
    if (kind !== 'any' && modelKind(entry) !== kind) continue
    const features = (entry.features ?? []).map((f) => f.toLowerCase())
    if (feature && !features.includes(feature.toLowerCase())) continue
    if (lacks && features.includes(lacks.toLowerCase())) continue
    if (family && !modelId.startsWith(family)) continue
    return modelId
  }
  return null
}

export const WEATHER_TOOL = {
  type: 'function',
  function: {
    name: 'get_weather',
    description: 'Get the current weather for a city.',
    parameters: {
      type: 'object',
      properties: { city: { type: 'string', description: 'City name' } },
      required: ['city'],
      additionalProperties: false,
    },
  },
}

export const PNG_8X8_B64 =
  'iVBORw0KGgoAAAANSUhEUgAAAAgAAAAICAIAAABLbSncAAAAGElEQVR42mNgYPj/n4EBC4ldFCw8' +
  'CHUAAOkwP8H8I6eUAAAAAElFTkSuQmCC'
export const PNG_DATA_URI = 'data:image/png;base64,' + PNG_8X8_B64
