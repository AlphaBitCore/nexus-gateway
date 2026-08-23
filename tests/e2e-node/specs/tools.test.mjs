import { describe, expect, it } from 'vitest'

import { WEATHER_TOOL, getCatalog, makeClient, pickModel } from './helpers.mjs'

describe('tool calling', () => {
  it('emits a well-formed tool_call with decodable arguments', async (ctx) => {
    const catalog = await getCatalog()
    const model = pickModel(catalog, { feature: 'function_calling', family: 'gpt-4o' })
    if (!model) ctx.skip()

    const resp = await makeClient().chat.completions.create({
      model,
      messages: [{ role: 'user', content: 'What is the weather in Paris?' }],
      tools: [WEATHER_TOOL],
      max_tokens: 128,
    })

    expect(resp.choices[0].finish_reason).toBe('tool_calls')
    const calls = resp.choices[0].message.tool_calls
    expect(calls?.length).toBeGreaterThan(0)
    const call = calls[0]
    expect(call.id).toBeTruthy()
    expect(call.type).toBe('function')
    expect(call.function.name).toBe('get_weather')
    const args = JSON.parse(call.function.arguments)
    expect(args).toHaveProperty('city')
  })

  it('accepts a replayed tool result and produces a final answer', async (ctx) => {
    const catalog = await getCatalog()
    const model = pickModel(catalog, { feature: 'function_calling', family: 'gpt-4o' })
    if (!model) ctx.skip()
    const client = makeClient()

    const messages = [{ role: 'user', content: 'What is the weather in Paris?' }]
    const first = await client.chat.completions.create({
      model,
      messages,
      tools: [WEATHER_TOOL],
      max_tokens: 128,
    })
    const call = first.choices[0].message.tool_calls[0]
    messages.push(first.choices[0].message)
    messages.push({
      role: 'tool',
      tool_call_id: call.id,
      content: JSON.stringify({ temp_c: 18, sky: 'clear' }),
    })

    const second = await client.chat.completions.create({
      model,
      messages,
      tools: [WEATHER_TOOL],
      max_tokens: 128,
    })
    expect(second.choices[0].finish_reason).toBe('stop')
    expect(second.choices[0].message.content).toBeTruthy()
  })

  it('returns several uniquely-identified calls when parallel tools are allowed', async (ctx) => {
    const catalog = await getCatalog()
    const model = pickModel(catalog, { feature: 'function_calling', family: 'gpt-4o' })
    if (!model) ctx.skip()

    const resp = await makeClient().chat.completions.create({
      model,
      messages: [
        { role: 'user', content: 'Weather in Paris and in Tokyo? Call the tool for each.' },
      ],
      tools: [WEATHER_TOOL],
      parallel_tool_calls: true,
      max_tokens: 256,
    })

    const calls = resp.choices[0].message.tool_calls ?? []
    expect(calls.length).toBeGreaterThanOrEqual(2)
    expect(new Set(calls.map((c) => c.id)).size).toBe(calls.length)
  })
})
