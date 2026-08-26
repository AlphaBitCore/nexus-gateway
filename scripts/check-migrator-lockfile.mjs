#!/usr/bin/env node
/**
 * tools/db-migrate carries its OWN package-lock.json, deliberately scoped to
 * just that package's dependency closure rather than derived from the
 * monorepo root's single-lockfile npm workspace (see
 * docker/db-migrator/Dockerfile for why). Nothing else in the repo regenerates
 * it: a dependency bump that edits tools/db-migrate/package.json updates the
 * ROOT lockfile and leaves this one behind.
 *
 * The consequence is not a warning. `docker/db-migrator/entrypoint.sh` installs
 * with `npm ci`, which refuses outright when the two files disagree, so the
 * migrator exits non-zero, and every service in deploy/docker-compose.yml gates
 * on `db-migrator: service_completed_successfully` — the whole quickstart stops
 * starting. Nothing catches it earlier, because no other build path installs
 * from this lockfile.
 *
 * The check is a string comparison of the dependency RANGES, not a semver
 * evaluation: npm records the package.json ranges verbatim in the lockfile's
 * root importer (`packages[""]`), so drift shows up as two maps that differ,
 * with no resolver, no network, and no semver implementation to get wrong.
 */

import { readFileSync } from 'node:fs';
import { join } from 'node:path';

const PKG_DIR = join(process.cwd(), 'tools', 'db-migrate');
const PKG = join(PKG_DIR, 'package.json');
const LOCK = join(PKG_DIR, 'package-lock.json');

const read = (p) => JSON.parse(readFileSync(p, 'utf8'));

const pkg = read(PKG);
const lock = read(LOCK);
const importer = lock.packages?.[''];

if (!importer) {
    console.error('[check:migrator-lockfile] tools/db-migrate/package-lock.json has no root importer (packages[""]).');
    console.error('  Expected a lockfileVersion 2/3 lockfile. Regenerate it (see below).');
    process.exit(1);
}

const FIELDS = ['dependencies', 'devDependencies', 'optionalDependencies'];
const problems = [];

if (importer.name !== pkg.name) {
    problems.push(`name: package.json says "${pkg.name}", lockfile says "${importer.name}"`);
}

for (const field of FIELDS) {
    const want = pkg[field] ?? {};
    const have = importer[field] ?? {};
    for (const [name, range] of Object.entries(want)) {
        if (!(name in have)) {
            problems.push(`${field}: "${name}" is in package.json (${range}) but missing from the lockfile`);
        } else if (have[name] !== range) {
            problems.push(`${field}: "${name}" is ${range} in package.json but ${have[name]} in the lockfile`);
        }
    }
    for (const name of Object.keys(have)) {
        if (!(name in want)) {
            problems.push(`${field}: "${name}" is in the lockfile (${have[name]}) but no longer in package.json`);
        }
    }
}

if (problems.length > 0) {
    console.error('\n───── db-migrator LOCKFILE DRIFT ─────\n');
    for (const p of problems) console.error(`  - ${p}`);
    console.error(`
tools/db-migrate/package-lock.json no longer matches tools/db-migrate/package.json.
\`npm ci\` refuses to install from a mismatched pair, so db-migrator would exit
non-zero and every service gated on it would stay blocked — the quickstart stack
would not start at all.

Regenerate the lockfile in isolation (NOT inside the repo, where npm would treat
the package as part of the root workspace and produce a workspace lockfile):

  tmp="$(mktemp -d)"
  cp tools/db-migrate/package.json "$tmp/"
  ( cd "$tmp" && npm install --package-lock-only --ignore-scripts )
  cp "$tmp/package-lock.json" tools/db-migrate/package-lock.json
`);
    process.exit(1);
}

const counted = FIELDS.reduce((n, f) => n + Object.keys(pkg[f] ?? {}).length, 0);
console.log(`[check:migrator-lockfile] OK -- ${counted} declared dependenc(ies) match tools/db-migrate/package-lock.json.`);
