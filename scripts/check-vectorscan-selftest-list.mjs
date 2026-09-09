#!/usr/bin/env node
// Keeps the two Vectorscan self-test lists honest.
//
// No CI job compiles the `vectorscan` build tag, so the release image build
// and the tarball build are the only automated path that ever links libhs and
// proves it actually scans. Both do it the same way: an anchored `-run`
// pattern naming the tests exactly, plus an asserted PASS count — because
// `-run` is an unanchored regexp and `go test` exits 0 on "no tests to run",
// so a loose or stale pattern selects nothing and still goes green.
//
// That leaves two failure modes neither build can catch until release day,
// and this gate catches both at commit time:
//
//   1. DRIFT. The pattern lives in two files. Update one, miss the other, and
//      the image and the tarball self-test different sets — with no signal.
//   2. A RENAMED OR DELETED TEST. The count guard catches it, but only when a
//      release is built. Here it is caught by the commit that renames it.
//
// What this gate does NOT do is make CI compile the tag. That needs either a
// job installing Ubuntu's libhyperscan-dev or a published buildbase image used
// as a `container:`, and either has to be proved by watching a real CI run
// rather than by reasoning. Tracked separately.

import { readFileSync, readdirSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import path from 'node:path';

const repoRoot = execFileSync('git', ['rev-parse', '--show-toplevel'], {
  encoding: 'utf8',
}).trim();

const DOCKERFILE = 'docker/services/Dockerfile';
const TARBALL = 'scripts/release/build-tarball.sh';
const MATCHER_DIR = 'packages/shared/policy/hooks/matcher';

const read = (rel) => readFileSync(path.join(repoRoot, rel), 'utf8');

// The anchored alternation, wherever it appears: ^(A|B|C)$
const PATTERN_RE = /\^\((Test[A-Za-z0-9_|]+)\)\$/g;

function extractLists(rel) {
  const src = read(rel);
  const out = [];
  for (const m of src.matchAll(PATTERN_RE)) {
    out.push({ names: m[1].split('|'), raw: m[0] });
  }
  return out;
}

const failures = [];

const dockerLists = extractLists(DOCKERFILE);
const tarballLists = extractLists(TARBALL);

// Floor: if the regex stops finding the lists, the gate would pass by seeing
// nothing. An absence has to be measurable before it can be reported.
if (dockerLists.length !== 1 || tarballLists.length !== 1) {
  console.error(
    `[vectorscan-selftest] found ${dockerLists.length} list(s) in ${DOCKERFILE} and ` +
      `${tarballLists.length} in ${TARBALL}; expected exactly one each.\n` +
      `The extractor is looking for an anchored ^(TestA|TestB)$ alternation. If the ` +
      `builds moved to a different shape, teach this gate the new one — do not delete it.`,
  );
  process.exit(1);
}

const docker = dockerLists[0].names;
const tarball = tarballLists[0].names;

if (docker.join('|') !== tarball.join('|')) {
  failures.push(
    `The two self-test lists have drifted.\n` +
      `  ${DOCKERFILE}: ${docker.join(', ')}\n` +
      `  ${TARBALL}: ${tarball.join(', ')}\n` +
      `The release image and the tarball would prove different things about the linked libhs.`,
  );
}

// The asserted PASS count must equal the number of names, in both files.
//
// `.match` on a global-less regex takes the FIRST hit and ignores the rest, so
// a second spelling of the guard elsewhere in the file — a comment quoting it,
// a second build stage — would be read past in silence and the wrong number
// checked. Collect every hit and insist on exactly one. Both quotings of the
// shell comparison are accepted, because `!= 4` and `!= "4"` are the same test
// and recognising only one reports the other as
// "the guard may have been removed" — a true failure for a false reason.
const COUNT_RES = [
  { rel: DOCKERFILE, re: /if \[ "\$passed" != "?(\d+)"? \]/g },
  { rel: TARBALL, re: /^SELFTEST_COUNT=(\d+)/gm },
];
for (const { rel, re } of COUNT_RES) {
  const hits = [...read(rel).matchAll(re)];
  if (hits.length === 0) {
    failures.push(`${rel}: could not find the asserted PASS count; the guard may have been removed.`);
    continue;
  }
  if (hits.length > 1) {
    failures.push(
      `${rel}: found ${hits.length} asserted PASS counts (${hits.map((h) => h[1]).join(', ')}). ` +
        `This gate can only vouch for one; disambiguate them or teach it which is load-bearing.`,
    );
    continue;
  }
  if (Number(hits[0][1]) !== docker.length) {
    failures.push(
      `${rel}: asserts ${hits[0][1]} passing self-tests but the pattern names ${docker.length}. ` +
        `A mismatch makes the build fail on a correct list or pass on a short one.`,
    );
  }
}

// Every named test must exist AND live behind the `vectorscan` constraint.
//
// Existence alone was the first version of this check and it is not enough.
// Under `-tags vectorscan` the untagged files compile too, so a self-test whose
// name resolves to an UNTAGGED file is selected, runs, passes, and satisfies the
// count guard — while linking nothing and proving nothing about libhs. That is
// the exact outcome both builds exist to rule out, arriving with four green
// PASS lines. The constraint is the property; the name is only how it is found.
//
// Read the directory rather than globbing through a shell: `cat dir/*_test.go`
// on a moved or emptied directory hands back an unexpanded glob and an
// execFileSync stack trace, which reads as a broken gate rather than as the
// real finding that the package is not where this gate thinks it is.
const matcherAbs = path.join(repoRoot, MATCHER_DIR);
let entries;
try {
  entries = readdirSync(matcherAbs).filter((f) => f.endsWith('_test.go'));
} catch (err) {
  console.error(
    `[vectorscan-selftest] cannot read ${MATCHER_DIR}: ${err.message}\n` +
      `The package moved or was renamed. Point this gate at its new home — a gate that ` +
      `cannot see the tests cannot report a missing one.`,
  );
  process.exit(1);
}

// `//go:build vectorscan`, `//go:build vectorscan && linux`, but NOT
// `//go:build !vectorscan`, which is the opposite claim.
const CONSTRAINT_RE = /^\/\/go:build (.+)$/m;
const declared = new Set();
let taggedFiles = 0;
for (const f of entries) {
  const src = readFileSync(path.join(matcherAbs, f), 'utf8');
  const c = src.match(CONSTRAINT_RE);
  if (!c || !/(^|[^!\w])vectorscan\b/.test(c[1])) continue;
  taggedFiles++;
  for (const m of src.matchAll(/^func (Test[A-Za-z0-9_]+)\(/gm)) declared.add(m[1]);
}
if (taggedFiles === 0) {
  console.error(
    `[vectorscan-selftest] no file under ${MATCHER_DIR} carries a \`vectorscan\` build ` +
      `constraint, out of ${entries.length} test file(s). Either the tag was renamed or this ` +
      `parser stopped recognising it — either way an absent self-test could not be reported.`,
  );
  process.exit(1);
}
for (const name of docker) {
  if (!declared.has(name)) {
    failures.push(
      `${name} is named in the self-test list but is not declared in any \`vectorscan\`-tagged ` +
        `file under ${MATCHER_DIR}. Either it does not exist — \`-run\` selects nothing — or it ` +
        `exists untagged, in which case it runs and passes without libhs being exercised at all.`,
    );
  }
}

if (failures.length > 0) {
  console.error('[vectorscan-selftest] FAILED\n');
  for (const f of failures) console.error(`  • ${f}\n`);
  process.exit(1);
}

console.log(
  `[vectorscan-selftest] ✓ ${docker.length} self-tests, listed identically in both builds, ` +
    `all declared behind the vectorscan constraint in ${MATCHER_DIR}`,
);
