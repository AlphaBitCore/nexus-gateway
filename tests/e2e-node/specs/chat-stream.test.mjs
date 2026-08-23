import { describe, expect, it } from 'vitest'

import { getCatalog, makeClient, pickModel } from './helpers.mjs'

describe('chat completions (streaming)', () => {
  it('yields chat.completion.chunk frames with content deltas and a finish_reason', async (ctx) => {
    const catalog = await getCatalog()
    const model = pickModel(catalog, { family: 'gpt-4o' })
    if (!model) ctx.skip()

    const stream = await makeClient().chat.completions.create({
      model,
      messages: [{ role: 'user', content: 'Count from 1 to 5, space separated.' }],
      max_tokens: 48,
      temperature: 0,
      stream: true,
    })

    let chunkCount = 0
    let chunksWithText = 0
    let finishReason = null
    for await (const chunk of stream) {
      chunkCount += 1
      expect(chunk.object).toBe('chat.completion.chunk')
      const choice = chunk.choices?.[0]
      if (!choice) continue
      if (choice.delta?.content) chunksWithText += 1
      if (choice.finish_reason) finishReason = choice.finish_reason
    }

    expect(chunkCount).toBeGreaterThan(0)
    expect(chunksWithText).toBeGreaterThan(0)
    expect(['stop', 'length', 'tool_calls', 'content_filter']).toContain(finishReason)
  })

  it('carries a usage frame with empty choices when include_usage is set', async (ctx) => {
    const catalog = await getCatalog()
    const model = pickModel(catalog, { family: 'gpt-4o' })
    if (!model) ctx.skip()

    const stream = await makeClient().chat.completions.create({
      model,
      messages: [{ role: 'user', content: 'Reply with one short word.' }],
      max_tokens: 16,
      stream: true,
      stream_options: { include_usage: true },
    })

    const usageChunks = []
    for await (const chunk of stream) {
      if (chunk.usage) usageChunks.push(chunk)
    }

    expect(usageChunks.length).toBeGreaterThanOrEqual(1)
    const final = usageChunks[usageChunks.length - 1]
    expect(final.usage.prompt_tokens).toBeGreaterThan(0)
    expect(final.choices).toEqual([])
  })
})
