import { describe, expect, it } from 'vitest'

import { getCatalog, makeClient, pickModel } from './helpers.mjs'

const PERSON_SCHEMA = {
  type: 'object',
  properties: { name: { type: 'string' }, age: { type: 'integer' } },
  required: ['name', 'age'],
  additionalProperties: false,
}

const STRICT_FORMAT = {
  type: 'json_schema',
  json_schema: { name: 'person', strict: true, schema: PERSON_SCHEMA },
}

const PROMPT = 'Invent a person and return them as JSON with keys name and age.'

describe('structured output', () => {
  it('conforms to a strict json_schema', async (ctx) => {
    const catalog = await getCatalog()
    const model = pickModel(catalog, { feature: 'json_mode', family: 'gpt-4o' })
    if (!model) ctx.skip()

    const resp = await makeClient().chat.completions.create({
      model,
      messages: [{ role: 'user', content: PROMPT }],
      response_format: STRICT_FORMAT,
      max_tokens: 128,
    })

    const obj = JSON.parse(resp.choices[0].message.content)
    expect(Object.keys(obj).sort()).toEqual(['age', 'name'])
    expect(typeof obj.name).toBe('string')
    expect(Number.isInteger(obj.age)).toBe(true)
  })

  it('returns a parseable object in json_object mode', async (ctx) => {
    const catalog = await getCatalog()
    const model = pickModel(catalog, { feature: 'json_mode', family: 'gpt-4o' })
    if (!model) ctx.skip()

    const resp = await makeClient().chat.completions.create({
      model,
      messages: [{ role: 'user', content: PROMPT }],
      response_format: { type: 'json_object' },
      max_tokens: 128,
    })

    const obj = JSON.parse(resp.choices[0].message.content)
    expect(obj).toBeTypeOf('object')
    expect(Array.isArray(obj)).toBe(false)
  })
})
