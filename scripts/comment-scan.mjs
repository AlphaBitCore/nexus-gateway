#!/usr/bin/env node
/**
 * Ranks comment blocks for the cleanup described in
 * docs/handoffs/comment-and-doc-cleanup-method.md.
 *
 * Deliberately NOT named check-* : check-all.mjs discovers every `check:*`
 * script and runs it as a gate, and "this comment over-explains" is not
 * decidable. This produces a shortlist for a person to work through. The one
 * part that IS decidable — a comment naming a file that does not exist — is
 * reported separately and could become a gate.
 *
 *   node scripts/comment-scan.mjs                 # ranked blocks, score >= 6
 *   node scripts/comment-scan.mjs --min-score 4
 *   node scripts/comment-scan.mjs --paths         # only the dead-path report
 *   node scripts/comment-scan.mjs --limit 60
 *   node scripts/comment-scan.mjs --signal used-to,previously   # every hit, any score
 *
 * Complements check-comment-program-refs.mjs, which already blocks PR/Epic/
 * finding references and cites the same rule. Nothing here duplicates it.
 */
import { readFileSync, existsSync } from 'node:fs';
import { execSync } from 'node:child_process';
import { basename, dirname, join } from 'node:path';

const ROOT = process.cwd();
const args = new Map(
  process.argv.slice(2).map((a, i, all) => {
    if (!a.startsWith('--')) return [a, true];
    const k = a.slice(2);
    const v = all[i + 1];
    return [k, v && !v.startsWith('--') ? v : true];
  }),
);
const MIN = Number(args.get('min-score') ?? 6);
const LIMIT = Number(args.get('limit') ?? 40);
// Score is mostly length, so a one-line "we used to do X" never reaches the
// shortlist even though it is the plainest archaeology in the tree. --signal
// lists every block carrying one signal, whatever it scores.
const SIGNAL =
  typeof args.get('signal') === 'string' ? args.get('signal').split(',') : null;

// A sweep that deletes required evidence is worse than no sweep.
// providers/specs carries the "Observed YYYY-MM" citations adapter Rule 7
// mandates; the security findings, the changelog and the archive are history
// by design.
const EXCLUDE =
  /^(packages\/ai-gateway\/internal\/providers\/specs\/|docs\/developers\/security\/|docs\/_archive\/|audit\/|CHANGELOG)/;

const REGISTER = [
  [/\bdeliberately\b/i, 2, 'deliberately'],
  [/\bon purpose\b/i, 2, 'on-purpose'],
  [/\bprecisely because\b|\bexactly because\b/i, 2, 'precisely'],
  [/\bworth (?:noting|saying|stating)\b/i, 2, 'worth-noting'],
  [/\bnote that\b/i, 1, 'note-that'],
  [/\bthe point is\b/i, 2, 'the-point'],
  [/\bwhich is why\b/i, 1, 'which-is-why'],
  [/\bis not\b[^.]{0,40}\bit is\b/i, 2, 'not-x-it-is'],
  [/\brather than\b/i, 1, 'rather-than'],
  [/\bin other words\b/i, 2, 'in-other-words'],
];
// Yields, measured on even samples of the hits each one produces:
//   used to      ~2 in 3 are real archaeology; the rest are "a buffer used to
//                hold the payload".
//   previously   ~3 in 5; the rest describe a prior RUN, not a prior
//                implementation — "the previously-applied config", "a
//                previously-firing alert".
//   the old      ~1 in 5. "the old index" is the blue/green domain term, "no
//                longer referenced" is a lifetime, "principals that no longer
//                exist" is a present fact. Scored 0: still listable with
//                --signal, but it no longer inflates the ranking with 562
//                blocks that are mostly ordinary English.
// `used to` carries two senses and the innocent one dominates: "a helper used
// to drive the failure branch" is employed-to, "the codec used to drop it" is
// archaeology. Measured across three trees, roughly two in three hits are the
// first. USED_TO_ARCH keeps a subject that IS the code — it / this / the depth
// cap / this test — and USED_TO_INNOCENT vetoes the employed-to shapes, so
// --signal used-to lists what is worth reading rather than everything.
const USED_TO_ARCH =
  /\b(it|this|that|they|these|those|which|one|we|both|each|the|its|our)(\s+[A-Za-z_.()[\]`']+){0,3}\s+used to\b/i;
const USED_TO_INNOCENT = /(is|are|—|,|:|\)) used to\b|(^|\.\s+)used to\b/i;

const ARCH = [
  [(t) => USED_TO_ARCH.test(t) && !USED_TO_INNOCENT.test(t), 2, 'used-to'],
  // previously-stored / previously-firing / previously-applied name prior
  // STATE, which is a domain fact rather than narration.
  [/\b(previously|originally)\b(?!-)/i, 2, 'previously'],
  [/\bthe old\b|\bno longer\b|\bhad been\b/i, 0, 'the-old'],
  [/\bCI (?:reported|kept|said)\b|\bturned? red\b/i, 2, 'incident'],
];

const COMMENT = /^(\s*)(\/\/+|#|\*)(\s?)(.*)$/;
// The extension set is the reach. It omitted every JavaScript extension, so a
// comment naming a .mjs file was not a path as far as this scan was concerned
// — which is how the sibling gate's own header sat pointing at a
// file that does not exist, in the one tree whose whole job is checking such
// pointers. A dead reference is dead whatever language the file is written in.
const REL =
  /\b((?:[\w.-]+\/){1,}[\w.-]+\.(?:go|ts|tsx|py|sh|yaml|yml|sql|prisma|json|md|swift|mjs|cjs|js|jsx))\b/g;
const ROOT_ANCHORED = /^(packages|docs|scripts|tools|tests|docker|deploy|nexus-ami|\.github)\//;
const NOISE = /^(https?:|\d+\/|[\w.-]*example\.com\/|github\.com\/|golang\.org\/|go\.opentelemetry)/;

const tracked = execSync('git ls-files', { cwd: ROOT, maxBuffer: 64 << 20 })
  .toString()
  .split('\n')
  .filter(Boolean);
const trackedSet = new Set(tracked);
const byBase = new Map();
for (const p of tracked) {
  const b = basename(p);
  if (!byBase.has(b)) byBase.set(b, []);
  byBase.get(b).push(p);
}

const sources = tracked.filter(
  (f) => /\.(go|ts|tsx|js|mjs|cjs|jsx|py|sh|swift)$/.test(f) && !EXCLUDE.test(f),
);

/**
 * Classify a path named in a comment.
 *
 * The AMBIGUOUS case is the one worth having: an earlier version of this asked
 * only whether SOME tracked file shared the basename, and treated a match as
 * "moved". For a distinctive name that is right; for types.go, handler.go or
 * config.go it silently swallowed genuinely dead references — which is how
 * `providers/types.go` stayed invisible while the lockstep comment that named
 * it pointed at nothing. Common names are both the most referenced and the most
 * likely to be missed, so they are surfaced rather than skipped.
 */
function classifyPath(cand, fromFile, text) {
  if (NOISE.test(cand) || cand.includes('...')) return null;
  // Only root-anchored paths make a checkable claim. `wiring/bridge.go` inside
  // packages/agent is shorthand a reader resolves by eye; flagging it produced
  // 212 "moved" entries that were almost all noise, which is how a tool teaches
  // people to ignore it.
  if (!ROOT_ANCHORED.test(cand)) return null;
  // `../scripts/x.go` is relative to the commenting file. The path regex drops
  // the `../`, and the remainder then looks root-anchored — which flagged
  // correct references as dead.
  if (text.includes('../' + cand) || text.includes('./' + cand)) return null;
  if (trackedSet.has(cand) || existsSync(join(ROOT, cand))) return null;
  if (existsSync(join(ROOT, dirname(fromFile), cand))) return null;
  const suffixed = tracked.filter((t) => t.endsWith('/' + cand));
  if (suffixed.length === 1) return { kind: 'MOVED', to: suffixed[0] };
  if (suffixed.length > 1) return { kind: 'AMBIGUOUS', candidates: suffixed };
  const sameBase = byBase.get(basename(cand)) ?? [];
  if (sameBase.length === 1) return { kind: 'MOVED', to: sameBase[0] };
  if (sameBase.length > 1) return { kind: 'AMBIGUOUS', candidates: sameBase };
  return { kind: 'GONE' };
}

const blocks = [];
const deadPaths = [];
const tally = new Map();
const bump = (k) => tally.set(k, (tally.get(k) ?? 0) + 1);

for (const f of sources) {
  const isTest = /_test\.|^tests\//.test(f);
  let lines;
  try {
    lines = readFileSync(join(ROOT, f), 'utf8').split('\n');
  } catch {
    continue;
  }

  let start = null;
  let body = [];
  const flush = () => {
    if (!body.length) return;
    const text = body.join(' ');
    // Adapter Rule 7 evidence, wherever it lives.
    if (!text.includes('Observed')) {
      let score = 0;
      const why = [];
      const n = body.length;
      if (n >= 30) (score += 5), why.push(`len${n}`);
      else if (n >= 20) (score += 4), why.push(`len${n}`);
      else if (n >= 12) (score += 2), why.push(`len${n}`);
      for (const [rx, w, name] of [...REGISTER, ...ARCH]) {
        const hit = typeof rx === 'function' ? rx(text) : rx.test(text);
        if (hit) (score += w), why.push(name), bump(name);
      }
      const dash = (text.match(/—/g) ?? []).length;
      if (dash >= 3) (score += 2), why.push(`dash${dash}`);
      if (isTest) score -= 1;
      if (score >= MIN || (SIGNAL && SIGNAL.some((g) => why.includes(g)))) {
        blocks.push({ score, f, start, n, why, body: [...body] });
      }
    }
    for (const m of text.matchAll(REL)) {
      if (text.includes('.' + m[1])) continue;
      const verdict = classifyPath(m[1], f, text);
      if (verdict) deadPaths.push({ f, start, path: m[1], ...verdict });
    }
    body = [];
    start = null;
  };

  for (let i = 0; i < lines.length; i++) {
    const m = COMMENT.exec(lines[i]);
    if (m) {
      if (start === null) start = i + 1;
      body.push(m[4]);
    } else flush();
  }
  flush();
}

if (SIGNAL) {
  const hits = blocks.filter((b) => SIGNAL.some((g) => b.why.includes(g)));
  console.log(
    `scanned ${sources.length} files; ${hits.length} blocks carry ${SIGNAL.join(' or ')}`,
  );
  console.log();
  for (const b of hits) {
    console.log(`${b.f}:${b.start}  (${b.n} lines, score ${b.score})`);
    for (const l of b.body) console.log(`    ${l}`);
    console.log();
  }
  process.exit(0);
}

if (!args.get('paths') && !args.get('gate')) {
  blocks.sort((a, b) => b.score - a.score);
  console.log(`scanned ${sources.length} files; ${blocks.length} blocks at score >= ${MIN}`);
  console.log(
    'signals: ' +
      [...tally.entries()].sort((a, b) => b[1] - a[1]).slice(0, 8).map(([k, v]) => `${k}=${v}`).join(', '),
  );
  console.log();
  for (const b of blocks.slice(0, LIMIT)) {
    console.log(`[${String(b.score).padStart(2)}] ${b.f}:${b.start}  (${b.n} lines)  ${b.why.join(',')}`);
    for (const l of b.body.slice(0, 2)) console.log(`       ${l.slice(0, 96)}`);
    if (b.n > 2) console.log(`       ... +${b.n - 2} more`);
    console.log();
  }
}

const byKind = { GONE: [], MOVED: [], AMBIGUOUS: [] };
for (const d of deadPaths) byKind[d.kind].push(d);
console.log(
  `paths named in comments that do not resolve: ` +
    `${byKind.GONE.length} gone, ${byKind.MOVED.length} moved, ${byKind.AMBIGUOUS.length} ambiguous`,
);
for (const d of byKind.MOVED.slice(0, LIMIT)) {
  console.log(`  MOVED      ${d.f}:${d.start}\n               ${d.path}\n            -> ${d.to}`);
}
for (const d of byKind.AMBIGUOUS.slice(0, LIMIT)) {
  console.log(`  AMBIGUOUS  ${d.f}:${d.start}\n               ${d.path}\n            -> ${d.candidates.slice(0, 3).join(', ')}${d.candidates.length > 3 ? ', …' : ''}`);
}
for (const d of byKind.GONE.slice(0, LIMIT)) {
  console.log(`  GONE       ${d.f}:${d.start}  ${d.path}`);
}

// --gate makes the dead-path half a gate. Only this half: whether a file exists
// is decidable, while "this comment over-explains" is a judgement, and a gate
// that fails on a judgement call teaches people to silence it.
//
// The direction matters. check:arch-doc-triggers enforces that changing mapped
// code updates its doc; nothing enforced that a doc a comment NAMES still
// exists, so a deleted architecture doc left every pointer to it reading as
// authoritative while resolving to nothing.
if (args.get('gate')) {
  // A gate that scanned nothing and a gate that found nothing print the same
  // OK. The tree carries a few thousand source files; a collapse to a handful
  // means the walk or the filter broke, and a pass would mean nothing.
  const minSources = 3000;
  if (sources.length < minSources) {
    console.error(
      `[comment-paths] only ${sources.length} files reached, expected at least ` +
        `${minSources} — the scan is not seeing the tree, so a clean result here ` +
        `would mean nothing.`,
    );
    process.exit(1);
  }
  if (deadPaths.length === 0) {
    console.log(
      `[comment-paths] OK — every path named in a comment resolves ` +
        `(${sources.length} files scanned).`,
    );
    process.exit(0);
  }
  console.error(
    `\n[comment-paths] ${deadPaths.length} comment(s) name a path that does not resolve.\n` +
      `A pointer that resolves to nothing is worse than none: it sends a reader after\n` +
      `an authority they cannot reach. Repoint it, or drop the pointer and keep the\n` +
      `sentence — the rule it states usually survives the file that documented it.`,
  );
  process.exit(1);
}
