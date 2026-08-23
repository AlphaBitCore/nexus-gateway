import { describe, expect, it } from 'vitest'

import { getCatalog, makeClient, modelKind } from './helpers.mjs'

/**
 * The embedding model supporting the most of what this spec tests, so all cases
 * exercise the same one. Picking "first by id" would choose by alphabetical
 * accident — embed-english-v3.0 sorts before text-embedding-3-small while
 * supporting neither an alternate dimension nor base64. Fails hard: the ticket
 * names embeddings explicitly.
 */
function embeddingModel(catalog) {
  const candidates = Object.keys(catalog)
    .sort()
    .filter((id) => modelKind(catalog[id]) === 'embedding')
  if (candidates.length === 0) {
    throw new Error(
      'no embedding-modality model in the catalog, so the embeddings acceptance ' +
        'criterion cannot be verified. Enable one (e.g. text-embedding-3-small).',
    )
  }
  const richness = (id) => {
    const spec = catalog[id].capabilityJson?.embeddings ?? {}
    return (
      (spec.supported_encoding_formats?.length ?? 0) * 100 +
      (spec.supported_dimensions?.length ?? 0)
    )
  }
  return candidates.reduce((best, id) => (richness(id) > richness(best) ? id : best))
}

/**
 * A declared dimension for `modelId` other than its default, or null when its
 * only supported dimension IS the default (Cohere's v3 family) — then
 * `dimensions` cannot be shown to shorten anything.
 */
function alternateDimension(catalog, modelId) {
  const spec = catalog[modelId].capabilityJson?.embeddings ?? {}
  const alternates = (spec.supported_dimensions ?? []).filter(
    (d) => Number.isInteger(d) && d !== spec.default_dimension,
  )
  return alternates.length > 0 ? Math.min(...alternates) : null
}

describe('embeddings', () => {
  it('returns one float vector per input with populated usage', async () => {
    const catalog = await getCatalog()
    const model = embeddingModel(catalog)

    const resp = await makeClient().embeddings.create({
      model,
      input: 'the quick brown fox',
    })

    expect(resp.object).toBe('list')
    expect(resp.data.length).toBe(1)
    expect(resp.data[0].index).toBe(0)
    expect(Array.isArray(resp.data[0].embedding)).toBe(true)
    expect(resp.data[0].embedding.length).toBeGreaterThan(0)
    expect(resp.data[0].embedding.every((v) => typeof v === 'number')).toBe(true)
    expect(resp.usage.prompt_tokens).toBeGreaterThan(0)
  })

  it('returns one correctly-indexed row per batched input', async () => {
    const catalog = await getCatalog()
    const model = embeddingModel(catalog)
    const inputs = ['first text', 'second text', 'third text']

    const resp = await makeClient().embeddings.create({ model, input: inputs })

    expect(resp.data.length).toBe(inputs.length)
    expect(resp.data.map((r) => r.index)).toEqual([0, 1, 2])
    expect(new Set(resp.data.map((r) => r.embedding.length)).size).toBe(1)
  })

  it('honours an explicitly requested dimension', async (ctx) => {
    const catalog = await getCatalog()
    const model = embeddingModel(catalog)
    const want = alternateDimension(catalog, model)
    if (want === null) ctx.skip()

    const resp = await makeClient().embeddings.create({
      model,
      input: 'dimension probe',
      dimensions: want,
    })
    expect(resp.data[0].embedding.length).toBe(want)
  })
})
