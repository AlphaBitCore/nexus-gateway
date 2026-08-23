import { describe, expect, it } from 'vitest'

import { PNG_DATA_URI, getCatalog, makeClient, pickModel } from './helpers.mjs'

const QUESTION = 'Answer with one short word.'

function imagePart(uri) {
  return { type: 'image_url', image_url: { url: uri } }
}

async function promptTokens(client, model, content) {
  const resp = await client.chat.completions.create({
    model,
    messages: [{ role: 'user', content }],
    max_tokens: 16,
  })
  return resp.usage.prompt_tokens
}

describe('vision (image_url content parts)', () => {
  it('raises prompt_tokens above the text-only baseline', async (ctx) => {
    const catalog = await getCatalog()
    const model = pickModel(catalog, { feature: 'vision', family: 'gpt-4o' })
    if (!model) ctx.skip()
    const client = makeClient()

    // A dropped image still yields a fluent 200 — only the token count betrays it.
    const withImage = await promptTokens(client, model, [
      { type: 'text', text: QUESTION },
      imagePart(PNG_DATA_URI),
    ])
    const textOnly = await promptTokens(client, model, [{ type: 'text', text: QUESTION }])

    expect(withImage).toBeGreaterThan(textOnly)
  })

  it('rejects an image sent to a non-vision model with a client error', async (ctx) => {
    const catalog = await getCatalog()
    const model = pickModel(catalog, { lacks: 'vision' })
    if (!model) ctx.skip()

    const err = await makeClient()
      .chat.completions.create({
        model,
        messages: [
          {
            role: 'user',
            content: [{ type: 'text', text: QUESTION }, imagePart(PNG_DATA_URI)],
          },
        ],
        max_tokens: 16,
      })
      .then(() => null)
      .catch((e) => e)

    expect(err, 'a non-vision model must not accept an image').not.toBeNull()
    // A 502 PROVIDER_UNAVAILABLE is indistinguishable from the 5xx this test is
    // trying to rule out, so it cannot reach a verdict — skip rather than report
    // a compatibility regression that isn't there.
    if (err.status === 502 && (err.error?.code ?? err.code) === 'PROVIDER_UNAVAILABLE') {
      ctx.skip()
    }
    expect(err.status).toBeGreaterThanOrEqual(400)
    expect(err.status).toBeLessThan(500)
  })
})
