#!/usr/bin/env node
/**
 * Probe what a provider's chat models actually accept, and print it beside what
 * the catalog claims.
 *
 * The catalog's `function_calling`, `reasoning` and `structured_outputs` tags
 * are routing dimensions: the smart router treats them as eligibility, so a tag
 * the wire refuses sends real traffic to a 400 that no gateway-side gate can
 * see. They are therefore claims that need measuring, and this is how they get
 * measured — the seed test `catalog-measured-refusals.test.ts` is the ledger of
 * what came back.
 *
 * The class of error this catches is a specialised model inheriting its
 * family's feature list. Found that way on 2026-08-29: Command A Vision, the
 * only Cohere chat model that takes images, is also the only one that refuses
 * tools, and it carried the family's `function_calling` tag.
 *
 * Usage:
 *   node tools/db-migrate/probe-model-capabilities.mjs <provider-key> [model-code ...]
 *
 * Keys come from ~/.nexus/provider-keys.json, the same file the corpus capture
 * scripts read. Nothing is written; read the output and edit the ledger by hand,
 * because a probe that rewrites the catalog would turn one bad night's network
 * into a silent capability removal.
 */

import { readFileSync } from 'node:fs'
import { homedir } from 'node:os'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const catalog = JSON.parse(readFileSync(resolve(here, 'model-catalog.json'), 'utf8'))
const keys = JSON.parse(readFileSync(resolve(homedir(), '.nexus/provider-keys.json'), 'utf8'))

/**
 * Per-provider wire details: where chat lives, how it authenticates, and how
 * each routing dimension is spelled. Only providers with an entry can be
 * probed — a guessed request shape would measure our own mistake.
 */
const WIRES = {
  cohere: {
    url: () => 'https://api.cohere.com/v2/chat',
    auth: () => ({ authorization: `Bearer ${keys.COHERE_API_KEY}` }),
    ask: (model) => ({ model, messages: [{ role: 'user', content: 'weather in Paris?' }] }),
    features: {
      function_calling: {
        tools: [
          {
            type: 'function',
            function: {
              name: 'get_weather',
              description: 'get weather',
              parameters: {
                type: 'object',
                properties: { city: { type: 'string' } },
                required: ['city'],
              },
            },
          },
        ],
      },
      structured_outputs: {
        response_format: {
          type: 'json_object',
          json_schema: {
            type: 'object',
            properties: { answer: { type: 'string' } },
            required: ['answer'],
          },
        },
      },
      streaming: { stream: true },
    },
  },
}

async function probe(wire, model, extra) {
  const res = await fetch(wire.url(model), {
    method: 'POST',
    headers: {
      ...wire.auth(),
      'content-type': 'application/json',
      'user-agent': 'nexus-gateway-capability-probe/1.0',
    },
    body: JSON.stringify({ ...wire.ask(model), ...extra }),
  })
  // Drain even on success: a streaming probe leaves an SSE body open, and an
  // unread body keeps the socket alive until the next request fails on it with
  // `other side closed` — which reads exactly like the provider refusing.
  if (res.ok) {
    await res.text()
    return '200'
  }
  const text = (await res.text()).slice(0, 200)
  try {
    const j = JSON.parse(text)
    return `${res.status} ${j.error_type ?? String(j.message ?? '').slice(0, 80)}`
  } catch {
    return `${res.status} ${text.slice(0, 80)}`
  }
}

const [providerKey, ...only] = process.argv.slice(2)
const wire = WIRES[providerKey]
if (!wire) {
  console.error(
    `no wire recipe for ${providerKey ?? '(none given)'} — known: ${Object.keys(WIRES).join(', ')}.\n` +
      `Add one rather than adapting another provider's shape; a guessed request measures nothing.`,
  )
  process.exit(2)
}

const provider = catalog.providers.find((p) => p.key === providerKey)
const models = (provider?.models ?? []).filter(
  (m) => m.type === 'chat' && (only.length === 0 || only.includes(m.code)),
)
if (models.length === 0) {
  console.error(`no chat models matched under ${providerKey}`)
  process.exit(2)
}

let mismatches = 0
for (const model of models) {
  const claimed = new Set(model.features ?? [])
  const parts = []
  for (const [feature, extra] of Object.entries(wire.features)) {
    const answer = await probe(wire, model.providerModelId ?? model.code, extra)
    const works = answer === '200'
    const says = claimed.has(feature)
    if (works !== says) {
      mismatches++
      parts.push(`${feature}: catalog=${says} wire=${works} <- ${answer}  ** MISMATCH **`)
    } else {
      parts.push(`${feature}: ${says ? 'declared' : 'not declared'}, wire ${answer}`)
    }
  }
  console.log(`${model.code}\n  ${parts.join('\n  ')}`)
}

console.log(
  mismatches === 0
    ? '\nno mismatches — every probed tag matches the wire'
    : `\n${mismatches} mismatch(es). A tag the wire refuses belongs in the ledger ` +
        `(seed/__tests__/catalog-measured-refusals.test.ts) AND out of the catalog; ` +
        `a capability the wire accepts but the catalog omits makes the model ineligible ` +
        `for work it can do.`,
)
process.exit(mismatches === 0 ? 0 : 1)
