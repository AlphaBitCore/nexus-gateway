#!/usr/bin/env node
// scripts/check-openapi-semantics.mjs
//
// Semantic-quality gate for the Control Plane admin OpenAPI catalog
// (docs/users/api/openapi/control-plane/*.yaml) — the source the resource
// engine embeds and the AI search cards expose verbatim. Two checks
// (design: docs/superpowers/specs/2026-06-05-resource-catalog-ai-first-design.md §3.4):
//
//   1. Parameter-description coverage = 100%. Every path/query parameter of
//      every operation carries a non-empty `description` — these are the
//      exact strings the AI uses to pick the right filter, and the 2026-06-05
//      sweep enriched the last 25 gaps, so the gate locks full coverage.
//
//   2. Summary-regression guard (PR-diff mode). For each operation in a
//      CHANGED spec file: fail if the old summary was enriched (not the
//      generator placeholder) and the new summary IS the placeholder —
//      i.e. an `openapi-gen` re-run wiped enrichment and the wipe is about
//      to land. Placeholder is recomputed from the generator's deterministic
//      pattern ({Read|Create|Update|Delete|METHOD} + kind, internal/openapigen
//      emit.go generatedSummary). A presence check would be vacuous: the
//      generator always emits a summary.
//
// Modes
//   (default)   coverage over the working tree + regression vs origin/main.
//   --staged    regression vs the index (pre-commit mode).
//   --base=REF  regression vs REF.
//
// Bypass: NEXUS_OPENAPI_SEMANTICS_WAIVE=1 (record the reason in the PR).

import { readFileSync, readdirSync, existsSync } from 'node:fs';
import { execSync } from 'node:child_process';
import { join } from 'node:path';

const REPO_ROOT = process.cwd();
const SPEC_DIR = 'docs/users/api/openapi/control-plane';

if (process.env.NEXUS_OPENAPI_SEMANTICS_WAIVE === '1') {
    console.log('check-openapi-semantics: waived via NEXUS_OPENAPI_SEMANTICS_WAIVE=1');
    process.exit(0);
}

const staged = process.argv.includes('--staged');
const baseArg = process.argv.find((a) => a.startsWith('--base='));
const baseRef = baseArg ? baseArg.slice('--base='.length) : 'origin/main';

// ---------------------------------------------------------------------------
// Minimal YAML walk — the specs are generator-emitted (consistent 4-space
// indentation, block style), so a line-based parse of the fields we need is
// reliable and avoids a yaml dependency at the workspace root. We extract,
// per file: operations [{method, opId, summary}] and parameters
// [{opId, name, hasDescription}].
// ---------------------------------------------------------------------------

function parseSpec(text) {
    const lines = text.split('\n');
    const ops = [];
    const params = [];
    let curMethod = null;
    let curOp = null; // {method, opId, summary}
    let inParams = false;
    let curParam = null;
    for (let i = 0; i < lines.length; i++) {
        const line = lines[i];
        const mMethod = line.match(/^ {8}(get|post|put|patch|delete):\s*$/);
        if (mMethod) {
            curMethod = mMethod[1].toUpperCase();
            curOp = { method: curMethod, opId: null, summary: null };
            ops.push(curOp);
            inParams = false;
            curParam = null;
            continue;
        }
        if (!curOp) continue;
        const mOpId = line.match(/^ {12}operationId:\s*(\S+)\s*$/);
        if (mOpId) { curOp.opId = mOpId[1]; continue; }
        const mSummary = line.match(/^ {12}summary:\s*(.+)$/);
        if (mSummary) { curOp.summary = unquote(mSummary[1].trim()); continue; }
        if (/^ {12}parameters:\s*$/.test(line)) { inParams = true; curParam = null; continue; }
        if (inParams) {
            const mName = line.match(/^ {16}- name:\s*(\S+)\s*$/);
            if (mName) {
                curParam = { opId: curOp.opId, name: mName[1], hasDescription: false };
                params.push(curParam);
                continue;
            }
            if (/^ {12}\S/.test(line)) { inParams = false; curParam = null; continue; } // left the parameters block
            if (curParam && /^ {18}description:\s*\S/.test(line)) curParam.hasDescription = true;
        }
    }
    return { ops, params };
}

function unquote(s) {
    if ((s.startsWith('"') && s.endsWith('"')) || (s.startsWith("'") && s.endsWith("'"))) {
        return s.slice(1, -1);
    }
    return s;
}

// generatedSummary mirrors internal/openapigen/emit.go exactly.
function generatedSummary(method, kind) {
    const verb = { GET: 'Read', POST: 'Create', PUT: 'Update', PATCH: 'Update', DELETE: 'Delete' }[method] ?? method;
    return `${verb} ${kind}`;
}

const kindOf = (file) => file.replace(/\.yaml$/, '');
const specFiles = readdirSync(join(REPO_ROOT, SPEC_DIR))
    .filter((f) => f.endsWith('.yaml') && f !== '_index.yaml');

let failures = 0;
const fail = (msg) => { console.error(`✗ ${msg}`); failures++; };

// --- Check 1: parameter-description coverage = 100% (working tree) ---------
let totalParams = 0;
let totalOps = 0;
for (const file of specFiles) {
    const { ops, params } = parseSpec(readFileSync(join(REPO_ROOT, SPEC_DIR, file), 'utf8'));
    totalOps += ops.length;
    // Vacuous-pass guard: a spec that parses to zero operations means the
    // generator's layout drifted from this line-parser's assumptions and the
    // gate would silently disable itself — fail loudly instead.
    if (ops.length === 0) {
        fail(`${file}: parsed 0 operations — generator layout drifted from check-openapi-semantics' parser; update the parser`);
    }
    for (const p of params) {
        totalParams++;
        if (!p.hasDescription) {
            fail(`${file}: parameter "${p.name}" of ${p.opId} has no description — the AI picks filters by these strings`);
        }
    }
}

// --- Check 2: summary-regression vs base (changed specs only) --------------
function gitShow(ref, path) {
    // ref '' / ':' means the index (git show :path); otherwise ref:path.
    const spec = ref === ':' || ref === '' ? `:${path}` : `${ref}:${path}`;
    try {
        return execSync(`git show ${spec}`, { cwd: REPO_ROOT, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] });
    } catch {
        return null; // new file at base — nothing to regress from
    }
}

let changed = [];
try {
    const cmd = staged
        ? 'git diff --cached --name-only'
        : `git diff --name-only ${baseRef}...HEAD 2>/dev/null || git diff --name-only ${baseRef}`;
    changed = execSync(cmd, { cwd: REPO_ROOT, encoding: 'utf8', shell: '/bin/bash' })
        .split('\n').filter((f) => f.startsWith(SPEC_DIR + '/') && f.endsWith('.yaml') && !f.endsWith('_index.yaml'));
} catch {
    console.warn(`check-openapi-semantics: cannot diff against ${baseRef}; skipping regression check`);
}

for (const path of changed) {
    const file = path.slice(SPEC_DIR.length + 1);
    const kind = kindOf(file);
    const baseText = staged ? gitShow('HEAD', path) : gitShow(baseRef, path);
    if (baseText == null) continue;
    // --staged compares against the INDEX (what will be committed), not the
    // working tree — a regression staged but fixed only in the worktree must
    // still fail.
    let curText;
    if (staged) {
        curText = gitShow(':', path); // index side; null when deleted from index
    } else {
        curText = existsSync(join(REPO_ROOT, path)) ? readFileSync(join(REPO_ROOT, path), 'utf8') : null;
    }
    const cur = curText != null ? parseSpec(curText) : { ops: [] };
    const baseOps = new Map(parseSpec(baseText).ops.map((o) => [o.opId, o]));
    for (const op of cur.ops) {
        const before = baseOps.get(op.opId);
        if (!before) continue;
        const placeholder = generatedSummary(op.method, kind);
        if (before.summary && before.summary !== placeholder && op.summary === placeholder) {
            fail(`${file}: ${op.opId} summary regressed from enriched ("${before.summary}") to the generator placeholder ("${op.summary}") — an openapi-gen re-run wiped enrichment; restore the text or re-run openapi-review`);
        }
    }
}

if (failures > 0) {
    console.error(`\ncheck-openapi-semantics: ${failures} failure(s) across ${specFiles.length} specs / ${totalParams} parameters.`);
    process.exit(1);
}
console.log(`check-openapi-semantics: OK — ${specFiles.length} specs / ${totalOps} operations, ${totalParams}/${totalParams} parameters described, ${changed.length} changed spec(s) regression-checked.`);
