// Single source of truth for rule packs: the YAML files in seed/rule-packs/ are
// authored by hand (and are what the Go differential / Vectorscan-compat tests
// load); the DB seed loads seed/fixtures/{rule_pack,rule}.json. This generator
// derives those two fixtures from the YAMLs so the two can never drift.
//
//   npm run gen:rulepacks          # regenerate the fixtures from the YAMLs
//   npm run gen:rulepacks -- --check   # CI: fail if fixtures are stale
//
// IDs are STABLE: an existing pack keeps its id (matched by name) and an
// existing rule keeps its id (matched by packId+ruleId), so foreign keys
// (rule.packId, rule_pack_install.packId) never churn. New packs/rules get a
// deterministic UUIDv5, so regeneration is reproducible. rule_pack_install.json
// is NOT touched — which packs are installed/enabled is separate config.
import { readFileSync, writeFileSync, readdirSync } from 'fs'
import { resolve, join, dirname } from 'path'
import { fileURLToPath } from 'url'
import { createHash } from 'crypto'
import { parse } from 'yaml'

const SEED_DIR = dirname(fileURLToPath(import.meta.url))
const PACKS_DIR = join(SEED_DIR, 'rule-packs')
const FIX = join(SEED_DIR, 'fixtures')

// Fixed namespace for deterministic UUIDv5 of new packs/rules (RFC 4122 §4.3).
const NS = '6ba7b811-9dad-11d1-80b4-00c04fd430c8'
// Stable createdAt for packs that don't already have one (avoids churn).
const CREATED_DEFAULT = '2026-06-22T00:00:00.000+00:00'

function uuidv5(name: string): string {
  const ns = Buffer.from(NS.replace(/-/g, ''), 'hex')
  const h = createHash('sha1').update(ns).update(name, 'utf8').digest()
  h[6] = (h[6] & 0x0f) | 0x50 // version 5
  h[8] = (h[8] & 0x3f) | 0x80 // RFC 4122 variant
  const x = h.subarray(0, 16).toString('hex')
  return `${x.slice(0, 8)}-${x.slice(8, 12)}-${x.slice(12, 16)}-${x.slice(16, 20)}-${x.slice(20, 32)}`
}

interface PackRow {
  id: string
  name: string
  version: string
  maintainer: string
  description: string | null
  createdAt: string
}
interface RuleRow {
  id: string
  packId: string
  ruleId: string
  category: string
  severity: string
  pattern: string
  flags: string | null
  description: string | null
  labels: string[]
}

function readJSON<T>(file: string): T[] {
  try {
    return JSON.parse(readFileSync(join(FIX, file), 'utf8')) as T[]
  } catch {
    return []
  }
}

function generate(): { packs: PackRow[]; rules: RuleRow[] } {
  const existingPacks = readJSON<PackRow>('rule_pack.json')
  const existingRules = readJSON<RuleRow>('rule.json')
  const packIdByName = new Map(existingPacks.map((p) => [p.name, p.id]))
  const createdByName = new Map(existingPacks.map((p) => [p.name, p.createdAt]))
  const ruleIdByKey = new Map(existingRules.map((r) => [`${r.packId}/${r.ruleId}`, r.id]))

  const packs: PackRow[] = []
  const rules: RuleRow[] = []
  const seenPackName = new Set<string>()
  const seenRuleKey = new Set<string>()

  for (const file of readdirSync(PACKS_DIR).filter((f) => f.endsWith('.yaml')).sort()) {
    const pack = parse(readFileSync(join(PACKS_DIR, file), 'utf8'))
    if (!pack?.name) throw new Error(`${file}: missing pack name`)
    if (seenPackName.has(pack.name)) throw new Error(`${file}: duplicate pack name ${pack.name}`)
    seenPackName.add(pack.name)

    const packId = packIdByName.get(pack.name) ?? uuidv5(`pack:${pack.name}`)
    packs.push({
      id: packId,
      name: pack.name,
      version: pack.version,
      maintainer: pack.maintainer,
      description: pack.description ?? null,
      createdAt: createdByName.get(pack.name) ?? CREATED_DEFAULT,
    })

    for (const r of pack.rules ?? []) {
      if (!r.id) throw new Error(`${file}: a rule is missing id`)
      const key = `${packId}/${r.id}`
      if (seenRuleKey.has(key)) throw new Error(`${file}: duplicate rule id ${r.id}`)
      seenRuleKey.add(key)
      rules.push({
        id: ruleIdByKey.get(key) ?? uuidv5(`rule:${key}`),
        packId,
        ruleId: r.id,
        category: r.category,
        severity: r.severity,
        pattern: r.pattern,
        flags: r.flags ?? null,
        description: r.description ?? null,
        labels: r.labels ?? [],
      })
    }
  }

  // Deterministic order so regeneration produces a stable diff. rule.json is
  // already id-sorted; packs become id-sorted here (a one-time reorder).
  const byId = (a: { id: string }, b: { id: string }) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0)
  packs.sort(byId)
  rules.sort(byId)
  return { packs, rules }
}

const { packs, rules } = generate()
const packJSON = JSON.stringify(packs, null, 2) + '\n'
const ruleJSON = JSON.stringify(rules, null, 2) + '\n'

if (process.argv.includes('--check')) {
  const curPack = readFileSync(join(FIX, 'rule_pack.json'), 'utf8')
  const curRule = readFileSync(join(FIX, 'rule.json'), 'utf8')
  if (curPack !== packJSON || curRule !== ruleJSON) {
    console.error(
      'rule-pack fixtures are STALE — run `npm run gen:rulepacks` and commit ' +
        'seed/fixtures/{rule_pack,rule}.json (they are generated from seed/rule-packs/*.yaml).',
    )
    process.exit(1)
  }
  console.log(`rule-pack fixtures up to date (${packs.length} packs, ${rules.length} rules).`)
} else {
  writeFileSync(join(FIX, 'rule_pack.json'), packJSON)
  writeFileSync(join(FIX, 'rule.json'), ruleJSON)
  console.log(`generated ${packs.length} packs, ${rules.length} rules into seed/fixtures/.`)
}
