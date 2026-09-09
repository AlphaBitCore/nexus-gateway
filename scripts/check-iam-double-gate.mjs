#!/usr/bin/env node
/**
 * Double-gated route check.
 *
 * Echo CONCATENATES a group's middleware with a route's own, so a route
 * registered inside a group that already carries `iamMW(A)` and that also
 * carries `iamMW(B)` requires the caller to hold BOTH A and B. The IAM model
 * does not imply one verb from another — holding settings.write does not grant
 * settings.read — so a principal holding exactly the action the ROUTE declares
 * is refused, and the refusal reads as a permissions problem on the caller's
 * side rather than a wiring mistake on ours.
 *
 * Found on its first run: RegisterSetupRoutes put settings.read on the group
 * and settings.write on the PATCH, so the one write in the setup surface
 * demanded both. The rule-pack registrar shows the pattern that works — it
 * registers its writes on the PARENT group rather than inside its read-gated
 * subgroup — so the tree already contained the answer.
 *
 * Two shapes are fine and are not flagged:
 *   - A group with iamMW whose routes carry none (every route wants that one
 *     action).
 *   - A group with NO iamMW whose routes each carry their own.
 *
 * Scope: the Go registrars under packages/<service>/internal. The check is
 * per-file and syntactic — it needs the group variable and its routes in one
 * function, which is how every registrar in this tree is written.
 */

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';

const ROOT = process.cwd();
const SCAN_DIRS = [
  'packages/control-plane/internal',
  'packages/nexus-hub/internal',
  'packages/ai-gateway/internal',
  'packages/compliance-proxy/internal',
];

/** `x := g.Group("/p", iamMW(...))` — a subgroup that carries an action. */
const GATED_GROUP = /(\w+)\s*:?=\s*\w+\.Group\(\s*"[^"]*"\s*,\s*[^)]*iamMW/g;
/** `x.VERB("/p", handler, iamMW(...))` — a route that carries its own. */
const GATED_ROUTE = /(\w+)\.(GET|POST|PUT|PATCH|DELETE)\(\s*"([^"]*)"[^\n]*iamMW/g;

/**
 * Floor: below this the scan is broken and "no violations" means nothing.
 *
 * Measured, not guessed: this tree has 65 iamMW-bearing files and
 * check-iam-route-coverage.mjs independently reports 61 registrars. 50 sits
 * comfortably under both while still catching a scan that has stopped
 * reaching the tree.
 */
const MIN_FILES = 50;

function walk(dir, out) {
  let entries;
  try {
    entries = readdirSync(dir);
  } catch {
    return out;
  }
  for (const e of entries) {
    const full = join(dir, e);
    if (statSync(full).isDirectory()) walk(full, out);
    else out.push(full);
  }
  return out;
}

function main() {
  const violations = [];
  let scanned = 0;

  for (const dir of SCAN_DIRS) {
    for (const full of walk(join(ROOT, dir), [])) {
      if (!full.endsWith('.go') || full.endsWith('_test.go')) continue;
      const src = readFileSync(full, 'utf-8');
      if (!src.includes('iamMW')) continue;
      scanned++;

      const gatedGroups = new Set();
      for (const m of src.matchAll(GATED_GROUP)) gatedGroups.add(m[1]);
      if (gatedGroups.size === 0) continue;

      const lines = src.split('\n');
      for (let i = 0; i < lines.length; i++) {
        for (const m of lines[i].matchAll(GATED_ROUTE)) {
          if (!gatedGroups.has(m[1])) continue;
          violations.push({
            where: `${full.slice(ROOT.length + 1)}:${i + 1}`,
            recv: m[1],
            verb: m[2],
            path: m[3],
          });
        }
      }
    }
  }

  if (scanned < MIN_FILES) {
    console.error(
      `[check:iam-double-gate] FAIL — scanned only ${scanned} iamMW-bearing file(s) ` +
        `(floor ${MIN_FILES}). The scan is broken, so "no violations" would mean nothing.`,
    );
    process.exit(1);
  }

  if (violations.length > 0) {
    console.error(
      `[check:iam-double-gate] FAIL — ${violations.length} route(s) carry their own iamMW ` +
        `INSIDE a group that already carries one. Echo concatenates the two, so the caller ` +
        `must hold BOTH actions — and a principal holding exactly the action the route ` +
        `declares is refused. Register the route on the parent group instead, or drop the ` +
        `group's action and give every route its own:`,
    );
    for (const v of violations) {
      console.error(`    ${v.verb} ${v.path}  (on the gated group \`${v.recv}\`)\n        ${v.where}`);
    }
    process.exit(1);
  }

  console.log(
    `[check:iam-double-gate] OK — no route is gated twice across ${scanned} registrar file(s).`,
  );
}

main();
