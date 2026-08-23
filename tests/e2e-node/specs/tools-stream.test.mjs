import { describe, expect, it } from 'vitest'

import { WEATHER_TOOL, getCatalog, makeClient, pickModel } from './helpers.mjs'

describe('tool calling (streaming)', () => {
  it('reassembles argument fragments into parseable JSON', async (ctx) => {
    const catalog = await getCatalog()
    const model = pickModel(catalog, { feature: 'function_calling', family: 'gpt-4o' })
    if (!model) ctx.skip()

    const stream = await makeClient().chat.completions.create({
      model,
      messages: [{ role: 'user', content: 'What is the weather in Paris?' }],
      tools: [WEATHER_TOOL],
      max_tokens: 128,
      stream: true,
    })

    const fragments = new Map()
    const names = new Map()
    let finishReason = null
    for await (const chunk of stream) {
      const choice = chunk.choices?.[0]
      if (!choice) continue
      if (choice.finish_reason) finishReason = choice.finish_reason
      for (const deltaCall of choice.delta?.tool_calls ?? []) {
        const idx = deltaCall.index
        expect(idx, 'streamed tool call delta must carry an index').not.toBeUndefined()
        if (deltaCall.function?.name) names.set(idx, deltaCall.function.name)
        if (deltaCall.function?.arguments) {
          fragments.set(idx, (fragments.get(idx) ?? '') + deltaCall.function.arguments)
        }
      }
    }

    expect(finishReason).toBe('tool_calls')
    expect(fragments.size).toBeGreaterThan(0)
    for (const raw of fragments.values()) {
      // Throws on a dropped or misordered fragment, which is the whole point.
      expect(JSON.parse(raw)).toBeTypeOf('object')
    }
    expect([...names.values()]).toContain('get_weather')
  })
})
