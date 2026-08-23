import OpenAI from 'openai'
import { describe, expect, it } from 'vitest'

import { apiKey, baseURL, getCatalog, makeClient, pickModel } from './helpers.mjs'

const BOGUS_MODEL = 'nexus-definitely-not-a-real-model'

/**
 * The inner `error` object, however the SDK unwrapped it. openai-node normally
 * hands back the inner object already; accept both shapes.
 */
function errorBody(err) {
  const body = err?.error ?? err?.body
  if (body && typeof body === 'object') {
    return 'error' in body ? body.error : body
  }
  return {}
}

describe('error scenarios', () => {
  it('maps an unroutable model to NotFoundError with a Nexus code', async () => {
    const err = await makeClient()
      .chat.completions.create({
        model: BOGUS_MODEL,
        messages: [{ role: 'user', content: 'hi' }],
        max_tokens: 8,
      })
      .catch((e) => e)

    expect(err).toBeInstanceOf(OpenAI.NotFoundError)
    expect(err.status).toBe(404)
    const body = errorBody(err)
    expect(body.code).toBe('ROUTING_NO_MATCH')
    expect(body.type).toBe('not_found_error')
    expect(body.param).toBe('model')
  })

  it('maps a bad virtual key to AuthenticationError', async () => {
    const client = new OpenAI({
      baseURL,
      apiKey: 'nvk_bogus_key_that_will_never_exist',
      maxRetries: 0,
    })
    const err = await client.chat.completions
      .create({
        model: BOGUS_MODEL,
        messages: [{ role: 'user', content: 'hi' }],
        max_tokens: 8,
      })
      .catch((e) => e)

    expect(err).toBeInstanceOf(OpenAI.AuthenticationError)
    expect(err.status).toBe(401)
    const body = errorBody(err)
    expect(body.code).toBe('AUTH_INVALID_KEY')
    expect(body.type).toBe('authentication_error')
  })

  it('answers an unmounted path with a JSON envelope, not text/plain', async () => {
    // Go's default ServeMux 404 is `404 page not found` as text/plain, which the
    // SDKs surface as an error carrying no message at all.
    const resp = await fetch(`${baseURL}/moderations`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${apiKey}`, 'Content-Type': 'application/json' },
      body: '{}',
    })
    expect(resp.status).toBe(404)
    expect(resp.headers.get('content-type')).toMatch(/^application\/json/)
    const { error } = await resp.json()
    expect(error.code).toBe('ENDPOINT_NOT_SUPPORTED')
    expect(error.type).toBe('not_found_error')
    expect(error.message).toContain('/moderations')
  })

  it('rejects an invalid tool schema as a 400', async (ctx) => {
    const catalog = await getCatalog()
    const model = pickModel(catalog, { feature: 'function_calling', family: 'gpt-4o' })
    if (!model) ctx.skip()

    const err = await makeClient()
      .chat.completions.create({
        model,
        messages: [{ role: 'user', content: 'hi' }],
        tools: [
          {
            type: 'function',
            function: { name: 'broken', parameters: { type: 'not-a-json-schema-type' } },
          },
        ],
        max_tokens: 16,
      })
      .catch((e) => e)

    expect(err).toBeInstanceOf(OpenAI.BadRequestError)
    expect(err.status).toBe(400)
    expect(err.message).toBeTruthy()
  })
})
