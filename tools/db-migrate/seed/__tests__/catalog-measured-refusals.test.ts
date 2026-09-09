/**
 * The catalog's `features` are not documentation. `function_calling`,
 * `reasoning` and `structured_outputs` are ROUTING DIMENSIONS — the smart
 * router's capability filter treats them as eligibility, so a model tagged
 * with one is a model `auto` may hand that kind of request to. A tag the wire
 * refuses is therefore a live mis-route: the caller gets a 400 they cannot
 * explain, and every gate inside the gateway is green while it happens.
 *
 * Measured 2026-08-29, probing each Cohere chat model directly:
 *
 *   command-a-vision-07-2025   tools -> 400 TOOL_USE_NOT_SUPPORTED
 *   command-a-03-2025          tools -> 200
 *   command-r7b-12-2024        tools -> 200
 *   command-r-plus-08-2024     tools -> 200
 *   command-r-08-2024          tools -> 200
 *
 * The catalog claimed `function_calling` for all five. The vision model's tag
 * was inherited from its family, which is how this class of error is made: a
 * specialised model gets the family's feature list, and nobody sends it the
 * one request that would prove the difference. The sibling
 * catalog-modality-consistency check exists for the same reason one level
 * over — 94 models advertised vision with text-only input.
 *
 * This file is the ledger of capabilities MEASURED as refused. It is
 * deliberately not a list of what each model supports: a claim needs a probe,
 * and enumerating everything would invite guesses. Adding a row means running
 * the probe and pasting what came back, the same discipline the adapter
 * prefix-list rule imposes (provider-adapter-architecture.md §3a Rule 7).
 *
 * Re-measure with:
 *   node tools/db-migrate/probe-model-capabilities.mjs cohere
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

interface Model {
  code: string
  type?: string
  features?: string[] | null
}
interface Provider {
  key: string
  models?: Model[]
}

/** Capabilities a provider answered an error for, with the error it answered. */
const MEASURED_REFUSALS: {
  provider: string
  model: string
  feature: string
  observed: string
}[] = [
  {
    provider: 'cohere',
    model: 'command-a-vision-07-2025',
    feature: 'function_calling',
    observed:
      '400 TOOL_USE_NOT_SUPPORTED — "tool use is not supported by the provided ' +
      'model: command-a-vision-07-2025" (2026-08-29)',
  },
]

const here = dirname(fileURLToPath(import.meta.url))
const catalog = JSON.parse(
  readFileSync(resolve(here, '../../model-catalog.json'), 'utf8'),
) as { providers: Provider[] }

test('the catalog declares no capability a provider was measured refusing', () => {
  for (const row of MEASURED_REFUSALS) {
    const provider = catalog.providers.find((p) => p.key === row.provider)
    assert.ok(
      provider,
      `no provider ${row.provider} in the catalog — this ledger row names a provider that ` +
        `no longer exists, so either the row is stale or the key changed`,
    )
    const model = provider!.models?.find((m) => m.code === row.model)
    assert.ok(
      model,
      `no model ${row.model} under ${row.provider} — a removed model should lose its ledger ` +
        `row too, rather than leaving a check that can never fail`,
    )
    assert.ok(
      !(model!.features ?? []).includes(row.feature),
      `${row.model} declares "${row.feature}", which the provider refuses: ${row.observed}. ` +
        `Routing treats this tag as eligibility, so restoring it sends real traffic to a 400. ` +
        `If the provider has since added support, re-run the probe and remove this ledger row ` +
        `with the passing result quoted — do not delete the row to make the test green.`,
    )
  }
})

test('every ledgered model still carries the capabilities measured as working', () => {
  // The inverse guard. Removing a wrong tag is easy to overdo — dropping
  // `structured_outputs` from the vision model alongside `function_calling`
  // would have been just as wrong, and just as invisible, because nothing
  // fails when a model is under-declared: it simply stops being routable for
  // work it can do.
  const vision = catalog.providers
    .find((p) => p.key === 'cohere')
    ?.models?.find((m) => m.code === 'command-a-vision-07-2025')
  assert.ok(vision, 'command-a-vision-07-2025 is missing from the Cohere catalog')
  for (const kept of ['streaming', 'structured_outputs']) {
    assert.ok(
      (vision!.features ?? []).includes(kept),
      `command-a-vision-07-2025 lost "${kept}", which it answered 200 for when probed ` +
        `2026-08-29. An under-declared model silently stops being eligible for work it can do.`,
    )
  }
})
