#!/usr/bin/env node
/**
 * check-sql-columns — assert every aliased column reference in a raw SQL
 * literal names a column the schema actually has.
 *
 * Why this exists. The Go store tests drive their SQL through pgxmock, which
 * replays a canned column set BY POSITION and never parses the statement. A
 * query can name a column that does not exist and every test still passes: the
 * mock hands back the rows the test wrote, in the order the test wrote them.
 * The failure only appears against a real database, as SQLSTATE 42703, in a
 * background job's WARN line — which is where it was found, months after it
 * shipped, on a leg of the identity enricher that had never once worked.
 *
 * The vocabulary is DERIVED from tools/db-migrate/schema/*.prisma, never
 * written here. A hand-kept list of column names would drift from the schema
 * exactly the way the queries did, and would then bless the drift.
 *
 * Scope, stated honestly. This checks `<alias>.<column>` where the alias is
 * bound to a quoted table name in the same SQL literal (`FROM "T" a`,
 * `JOIN "T" a`, `UPDATE "T" a`). That is the shape the aliased store queries
 * use and the shape the bug took. Unaliased columns, dynamically assembled
 * SQL, and unquoted table names are out of scope — this is a lint against one
 * specific, recurring, silent failure, not a SQL parser.
 */

import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'

const ROOT = new URL('..', import.meta.url).pathname.replace(/\/$/, '')
const SCHEMA_DIR = join(ROOT, 'tools/db-migrate/schema')
const SCAN_ROOTS = ['packages']

/**
 * Parse the Prisma model graph into { tableName -> Set(columnName) }.
 *
 * Prisma's mapping rules, which are the whole point of the check: a model maps
 * to `@@map("...")` when present and to the model name otherwise; a field maps
 * to `@map("...")` when present and to the FIELD NAME otherwise — so an
 * unmapped `deviceId` really is a camelCase column, and writing `device_id`
 * against it is the bug this catches. Relation fields declare no column of
 * their own and are skipped.
 */
function loadSchema() {
  const sources = readdirSync(SCHEMA_DIR)
    .filter((n) => n.endsWith('.prisma'))
    .map((n) => readFileSync(join(SCHEMA_DIR, n), 'utf8'))

  // Pass 1 — the set of MODEL names. It is the only way to tell a relation
  // field from a scalar one: `String`, `DateTime` and every enum name are
  // capitalised too, so "starts with a capital" identifies nothing. Enums DO
  // produce a column and must not be skipped; models do not.
  const modelNames = new Set()
  for (const src of sources) {
    for (const m of src.matchAll(/^model\s+(\w+)\s*\{/gm)) modelNames.add(m[1])
  }

  // Pass 2 — the columns of each table.
  const tables = new Map()
  for (const src of sources) {
    for (const m of src.matchAll(/^model\s+(\w+)\s*\{([\s\S]*?)^\}/gm)) {
      const [, modelName, body] = m
      const tableMap = body.match(/@@map\("([^"]+)"\)/)
      const table = tableMap ? tableMap[1] : modelName
      const cols = new Set()
      for (const line of body.split('\n')) {
        const t = line.trim()
        if (!t || t.startsWith('//') || t.startsWith('@@')) continue
        const field = t.match(/^(\w+)\s+(\S+)/)
        if (!field) continue
        const [, name, type] = field
        // A relation field carries the related MODEL as its type and declares
        // no column of its own; the scalar backing it is declared separately.
        if (modelNames.has(type.replace(/[?[\]]/g, ''))) continue
        const colMap = t.match(/@map\("([^"]+)"\)/)
        cols.add(colMap ? colMap[1] : name)
      }
      tables.set(table, cols)
    }
  }
  return tables
}

/** Every Go file under the scan roots, tests included — a test's SQL is SQL. */
function goFiles() {
  const out = []
  const walk = (dir) => {
    for (const entry of readdirSync(dir)) {
      if (entry === 'node_modules' || entry === '.git' || entry === 'dist') continue
      const p = join(dir, entry)
      const st = statSync(p)
      if (st.isDirectory()) walk(p)
      else if (entry.endsWith('.go')) out.push(p)
    }
  }
  for (const r of SCAN_ROOTS) walk(join(ROOT, r))
  return out
}

/**
 * Pull raw SQL out of a Go source file: backtick literals that look like
 * statements. Anything without a FROM/JOIN/UPDATE of a quoted table cannot
 * bind an alias, so it cannot be checked and is skipped.
 */
function sqlLiterals(src) {
  const out = []
  for (const m of src.matchAll(/`([^`]*)`/g)) {
    const body = m[1]
    if (!/\b(SELECT|UPDATE|INSERT|DELETE)\b/i.test(body)) continue
    out.push({ body, index: m.index })
  }
  return out
}

/** alias -> table, for aliases bound to a QUOTED table name in this literal. */
function aliasBindings(sql) {
  const binds = new Map()
  const re = /\b(?:FROM|JOIN|UPDATE|INTO)\s+"([A-Za-z_][\w]*)"\s+(?:AS\s+)?([a-z][\w]*)\b/gi
  for (const m of sql.matchAll(re)) {
    const [, table, alias] = m
    // `FROM "T" WHERE` and friends: a SQL keyword is not an alias.
    if (/^(where|set|on|using|left|right|inner|outer|full|cross|group|order|limit|returning|values)$/i.test(alias)) continue
    binds.set(alias, table)
  }
  return binds
}

function lineOf(src, index) {
  return src.slice(0, index).split('\n').length
}

const tables = loadSchema()
const findings = []

for (const file of goFiles()) {
  const src = readFileSync(file, 'utf8')
  for (const { body, index } of sqlLiterals(src)) {
    const binds = aliasBindings(body)
    if (binds.size === 0) continue
    for (const m of body.matchAll(/\b([a-z][\w]*)\.("?)([A-Za-z_][\w]*)\2/g)) {
      const [, alias, quote, column] = m
      const table = binds.get(alias)
      if (!table) continue
      const cols = tables.get(table)
      // A table the model graph does not describe (a view, a partition, an
      // extras-only object) cannot be judged — say nothing rather than guess.
      if (!cols) continue
      if (cols.has(column)) continue
      // Report the nearest correct spelling when one exists; the bug is almost
      // always a snake/camel slip, and naming the right column makes the fix
      // obvious without opening the schema.
      const wanted = [...cols].find(
        (c) => c.toLowerCase().replace(/_/g, '') === column.toLowerCase().replace(/_/g, '')
      )
      findings.push({
        file: relative(ROOT, file),
        line: lineOf(src, index) + body.slice(0, m.index).split('\n').length - 1,
        table,
        alias,
        column,
        wanted,
      })
    }
  }
}

if (findings.length === 0) {
  console.log(`check:sql-columns — OK (${tables.size} tables from the model graph)`)
  process.exit(0)
}

console.error(`check:sql-columns — ${findings.length} column reference(s) name nothing in the schema:\n`)
for (const f of findings) {
  const hint = f.wanted ? `  → did you mean ${f.alias}."${f.wanted}"?` : '  (no similarly-spelled column on that table)'
  console.error(`  ${f.file}:${f.line}`)
  console.error(`    ${f.alias}.${f.column}  —  "${f.table}" has no column ${f.column}${hint}`)
}
console.error(
  '\nThese fail at runtime as SQLSTATE 42703 and pass every pgxmock test, because\n' +
    'the mock replays columns by position and never parses the statement.'
)
process.exit(1)
