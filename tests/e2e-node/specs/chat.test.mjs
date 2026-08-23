import { describe, expect, it } from 'vitest'

import { getCatalog, makeClient, pickModel } from './helpers.mjs'

describe('chat completions (non-streaming)', () => {
  it('returns the OpenAI envelope with populated usage', async (ctx) => {
    const catalog = await getCatalog()
    const model = pickModel(catalog, { family: 'gpt-4o' })
    if (!model) ctx.skip()

    const resp = await makeClient().chat.completions.create({
      model,
      messages: [{ role: 'user', content: 'Reply with one short word.' }],
      max_tokens: 16,
      temperature: 0,
    })

    expect(resp.id).toBeTruthy()
    expect(resp.object).toBe('chat.completion')
    expect(resp.choices.length).toBeGreaterThan(0)
    expect(resp.choices[0].message.role).toBe('assistant')
    expect(typeof resp.choices[0].message.content).toBe('string')
    expect(resp.choices[0].message.content).toBeTruthy()
    expect(resp.usage.prompt_tokens).toBeGreaterThan(0)
    expect(resp.usage.completion_tokens).toBeGreaterThan(0)
    expect(resp.usage.total_tokens).toBeGreaterThanOrEqual(
      resp.usage.prompt_tokens + resp.usage.completion_tokens,
    )
  })

  it('lists models with the Nexus capability fields intact', async () => {
    const page = await makeClient().models.list()
    expect(page.data.length).toBeGreaterThan(0)
    for (const entry of page.data) {
      expect(entry.id).toBeTruthy()
      expect(entry.object).toBe('model')
    }
  })

  it('reports finish_reason=length when max_tokens truncates', async (ctx) => {
    const catalog = await getCatalog()
    const model = pickModel(catalog, { family: 'gpt-4o' })
    if (!model) ctx.skip()

    const resp = await makeClient().chat.completions.create({
      model,
      messages: [{ role: 'user', content: 'Write 400 words about the sea.' }],
      max_tokens: 8,
    })
    expect(resp.choices[0].finish_reason).toBe('length')
    expect(resp.usage.completion_tokens).toBeLessThanOrEqual(8)
  })
})
