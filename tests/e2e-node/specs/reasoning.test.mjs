import { describe, expect, it } from 'vitest'

import { getCatalog, makeClient, pickModel } from './helpers.mjs'

const REASONING_PROMPT =
  'A shop sells pens at 3 for 7 dollars and books at 2 for 11 dollars. ' +
  'What is the cost of 9 pens and 6 books? Reply with just the number.'

function reasoningTokens(usage) {
  return usage?.completion_tokens_details?.reasoning_tokens
}

describe('reasoning tokens', () => {
  it('reports reasoning_tokens as a subset of completion_tokens on gpt-5', async (ctx) => {
    const catalog = await getCatalog()
    const model = pickModel(catalog, { family: 'gpt-5.5' })
    if (!model) ctx.skip()

    const resp = await makeClient().chat.completions.create({
      model,
      messages: [{ role: 'user', content: REASONING_PROMPT }],
      max_completion_tokens: 2048,
    })

    const tokens = reasoningTokens(resp.usage)
    expect(tokens, 'completion_tokens_details.reasoning_tokens absent').not.toBeUndefined()
    expect(tokens).toBeGreaterThan(0)
    expect(tokens).toBeLessThanOrEqual(resp.usage.completion_tokens)
  })

  it('does not invent reasoning_tokens on a non-reasoning model', async (ctx) => {
    const catalog = await getCatalog()
    const model = pickModel(catalog, { family: 'gpt-4o' })
    if (!model) ctx.skip()

    const resp = await makeClient().chat.completions.create({
      model,
      messages: [{ role: 'user', content: 'Reply with one short word.' }],
      max_tokens: 16,
    })

    // A normalizer that defaulted this to a plausible number would overbill
    // every caller silently.
    expect([undefined, null, 0]).toContain(reasoningTokens(resp.usage) ?? undefined)
  })
})
