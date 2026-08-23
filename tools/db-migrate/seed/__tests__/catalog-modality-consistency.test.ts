/**
 * The catalog's `features`, `type` and modality arrays are three descriptions
 * of the same model, written at different times, and they had drifted:
 * 94 models advertised `vision` while their `inputModalities` said text only,
 * and 33 more contradicted their own type — embeddings that emitted text, an
 * image model that emitted text, transcribe models that took no audio.
 *
 * Nothing broke, for exactly one reason: the routing guard reads `type` and
 * ignores the arrays. That makes the drift latent rather than harmless — the
 * day anything starts trusting those fields, half the catalog answers wrong.
 *
 * So this runs as a standing check rather than a one-off repair, which is the
 * difference between fixing the data and fixing the way it goes bad.
 *
 * It reads the top-level fields, which is now the only place they live. They
 * used to sit under `seed` for a seeded row and at the top level for a
 * template-only one, so this file had to ask both — and an earlier version
 * asked only the top level, a key the generator did not consume, certifying
 * data the database never saw while 40 seeded rows carried vision with
 * text-only input. A check pointed at the wrong field is worse than no check:
 * it reports the property as held. The two-homes shape made that mistake
 * available, so the fields were moved to one home and the generator now
 * refuses a catalog that puts them back under `seed`.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, readdirSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

interface Model {
  code: string
  seed?: Record<string, unknown> | null
  type?: string
  features?: string[] | null
  inputModalities?: string[] | null
  outputModalities?: string[] | null
  requiredModalities?: string[] | null
  inputPricePerMillion?: number | null
  outputPricePerMillion?: number | null
}

const here = dirname(fileURLToPath(import.meta.url))
const catalog = JSON.parse(
  readFileSync(resolve(here, '../../model-catalog.json'), 'utf8'),
) as { providers: Array<{ key: string; models?: Model[] }> }

const models: Array<{ provider: string; m: Model }> = []
for (const p of catalog.providers) for (const m of p.models ?? []) models.push({ provider: p.key, m })

// One home. The generator reads these for both the seeded fixture and the
// wizard template, so this is what ships in every direction.
const ins = (m: Model) => m.inputModalities ?? []
const outs = (m: Model) => m.outputModalities ?? []
const feats = (m: Model) => m.features ?? []
const label = (provider: string, m: Model) => `${provider}/${m.code}`

test('the catalog is large enough for these checks to mean anything', () => {
  assert.ok(models.length > 100, `only ${models.length} models — did the catalog fail to load?`)
})

test('no model stores `vision` — the fact lives in inputModalities', () => {
  // `vision` and `inputModalities ∋ image` were the same claim in two
  // vocabularies, and the two drifted: 94 models advertised vision while
  // declaring text-only input. The arrays won because they can also express
  // audio, video and files, and because the router reads them. The string is
  // now derived back onto GET /v1/models for SDK callers, so this asserts it
  // is not ALSO stored — a stored copy is the second vocabulary returning.
  const bad = models
    .filter(({ m }) => feats(m).includes('vision'))
    .map(({ provider, m }) => label(provider, m))
  assert.deepEqual(bad, [], `models still storing the vision feature:\n  ${bad.join('\n  ')}`)
})


// `type` answers WHICH ENDPOINT serves a model, and each endpoint implies a
// modality that model must handle. The arrays must not contradict the
// endpoint named on the same row.
for (const [type, field, modality, pick] of [
  ['stt', 'inputModalities', 'audio', ins],
  ['tts', 'outputModalities', 'audio', outs],
  ['image', 'outputModalities', 'image', outs],
  ['video', 'outputModalities', 'video', outs],
  ['embedding', 'outputModalities', 'embedding', outs],
  // realtime was missing entirely, so gpt-realtime-2.1 kept an
  // image-only inputModalities through a sweep that fixed everything else.
  ['realtime', 'inputModalities', 'audio', ins],
  ['realtime', 'outputModalities', 'audio', outs],
] as const) {
  test(`every ${type} model declares ${field} containing ${modality}`, () => {
    const bad = models
      .filter(({ m }) => m.type === type && !pick(m).includes(modality))
      .map(({ provider, m }) => label(provider, m))
    assert.deepEqual(bad, [], `${type} models contradicting their own type:\n  ${bad.join('\n  ')}`)
  })
}

test('no model carries the retired "audio" type', () => {
  // It used to be minted by the discovery heuristic for any id containing
  // "audio", and the models that got it — gpt-audio-* — are served on chat
  // completions, so the routing guard rejected every one of their requests.
  const bad = models.filter(({ m }) => m.type === 'audio').map(({ provider, m }) => label(provider, m))
  assert.deepEqual(bad, [], `"audio" is not an endpoint kind:\n  ${bad.join('\n  ')}`)
})

test('rerank models price per search unit on the input column, with no output price', () => {
  // Unified pricing semantic (cost_formula_registry.go): inputPricePerMillion is
  // "USD per 1M billable input units" where the unit is the endpoint's own —
  // tokens for chat, search units for rerank. Every modality formula prices via
  // costForInputUnits, so the rate lives on the INPUT column. Cohere rerank bills
  // $2.00 / 1,000 searches = 2000 USD per 1M search units; it has no per-output
  // billing, so the output column stays null. (A per-search rate on the input
  // column is not a "per-token" price — it is the same field reinterpreted per
  // endpoint, exactly as stt=100/1M-seconds and video=100000/1M-seconds do.)
  const rerank = models.filter(({ m }) => m.type === 'rerank')
  assert.ok(rerank.length > 0, 'no rerank models catalogued')
  for (const { provider, m } of rerank) {
    assert.ok(
      typeof m.inputPricePerMillion === 'number' && m.inputPricePerMillion > 0,
      `${label(provider, m)} has no per-search-unit input price; rerank bills per search unit`,
    )
    assert.equal(m.outputPricePerMillion, null, `${label(provider, m)} has an output price; rerank has no per-output billing`)
  }
})

test('every model declares at least one input and one output modality', () => {
  const bad = models
    .filter(({ m }) => ins(m).length === 0 || outs(m).length === 0)
    .map(({ provider, m }) => label(provider, m))
  assert.deepEqual(bad, [], `models with an empty modality array:\n  ${bad.join('\n  ')}`)
})

test('every non-stt model accepts text input', () => {
  // The repair that added `image` to vision models once REPLACED the array
  // instead of merging, leaving 94 rows declaring image-only input. A
  // modality-aware guard reading that would have refused TEXT on gpt-4o and
  // every Claude — the exact inverse of the failure the ordering rule exists
  // to prevent, which is why this is asserted rather than assumed.
  const bad = models
    .filter(({ m }) => m.type !== 'stt' && !ins(m).includes('text'))
    .map(({ provider, m }) => label(provider, m))
  assert.deepEqual(bad, [], `models that do not accept text:\n  ${bad.join('\n  ')}`)
})

test('single-purpose endpoints declare exactly the one modality they produce', () => {
  // An embeddings call returns vectors and an images call returns images;
  // `revised_prompt` is metadata riding the response, not a second output
  // modality. Leaving both shapes in the catalogue is one concept with two
  // shapes.
  const sole: Record<string, string> = {
    embedding: 'embedding', image: 'image', video: 'video', tts: 'audio',
  }
  const bad = models
    .filter(({ m }) => m.type! in sole
      ? outs(m).length !== 1 || outs(m)[0] !== sole[m.type!]
      : false)
    .map(({ provider, m }) => `${label(provider, m)} -> ${JSON.stringify(outs(m))}`)
  assert.deepEqual(bad, [], `single-purpose outputs that are not single:\n  ${bad.join('\n  ')}`)
})

test('every wizard template model carries its modalities', () => {
  // The wizard creates real models from these files. A template model without
  // modalities posts none, and the create API then derives a default OVER the
  // catalog's own answer — which is how a template gpt-audio would come out
  // text-only. The generator refuses a catalog missing them and the drift
  // check pins the templates to the catalog, so this holds today by
  // implication; asserted directly because the artifact the wizard actually
  // fetches is the thing that has to be right.
  const dir = resolve(here, '../../../../packages/control-plane-ui/public/provider-templates')
  const files = readdirSync(dir).filter((f) => f.endsWith('.json') && f !== 'index.json')
  assert.ok(files.length > 0, 'no provider templates found — wrong path?')
  const bad: string[] = []
  for (const f of files) {
    const t = JSON.parse(readFileSync(resolve(dir, f), 'utf8')) as { models?: Model[] }
    for (const m of t.models ?? []) {
      if (!ins(m).length || !outs(m).length) bad.push(`${f}/${m.code}`)
    }
  }
  assert.deepEqual(bad, [], `template models without modalities:\n  ${bad.join('\n  ')}`)
})

test('a model never requires a modality it cannot accept', () => {
  // requiredModalities is the floor, inputModalities the ceiling. A floor
  // above the ceiling is a row that can never be served — it would pass
  // routing's "can you take this?" test and then be dropped by "do you need
  // something the request lacks?", or worse, be selected and 400 upstream.
  const bad = models
    .filter(({ m }) => (m.requiredModalities ?? []).some((r) => !ins(m).includes(r)))
    .map(({ provider, m }) =>
      `${label(provider, m)} requires ${JSON.stringify(m.requiredModalities)} but accepts ${JSON.stringify(ins(m))}`)
  assert.deepEqual(bad, [], `models requiring what they cannot accept:\n  ${bad.join('\n  ')}`)
})

test('only models that share a pool with text-only requests carry a floor', () => {
  // A floor only does work where a model competes for requests it cannot
  // serve, which means chat: every other type has its own endpoint and never
  // sees a plain text chat request. Filling in "requires audio" on an stt row
  // would be a value nothing ever reads, and the next reader would reasonably
  // assume it does something.
  const bad = models
    .filter(({ m }) => (m.requiredModalities ?? []).length > 0 && m.type !== 'chat')
    .map(({ provider, m }) => `${label(provider, m)} (type=${m.type})`)
  assert.deepEqual(bad, [], `non-chat models carrying a floor nothing reads:\n  ${bad.join('\n  ')}`)
})
