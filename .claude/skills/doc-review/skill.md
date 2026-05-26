# doc-review

Audit a doc claim-by-claim against the codebase. Pre-commit gate for the `doc-write` skill — every doc must pass this audit CLEAN before commit. Also usable standalone for retroactive drift audits of existing docs.

Use this skill when:

- About to commit a new or rewritten doc (binding gate for `doc-write`).
- Retroactively auditing an existing doc suspected of drift.
- Verifying a doc still reflects current code after a refactor.

This skill is the pre-commit twin of `doc-write`. They share the same hard rules.

---

## Hard rules (mirrors `doc-write`)

A doc is CLEAN only if it satisfies ALL of:

1. **Code-anchored.** Every factual claim is backed by code that exists on disk right now.
2. **Forward-looking present-state.** No archaeology language.
3. **No line numbers in body.** Paths only.
4. **No dates.** Timeless.
5. **No Epic / SDD / bug / incident references.** Describes the system, not the program that built it.
6. **`## References` section exists** and lists canonical paths (no line numbers).
7. **Audit surfaces system issues, not just doc issues.** When per-claim verification finds a product / architecture / technical issue — half-built feature with no producer, dual code paths doing the same thing, wire semantic inversion across services, dead constant the UI anticipates, anything the doc would have to soften / hedge to remain accurate — output it explicitly in the report. For each issue give: (a) concrete finding, (b) code evidence, (c) brainstormed best-product / best-architecture recommendation, (d) suggested next action (rewire / delete / new epic). The audit pass is the cheapest checkpoint to catch architectural smells; issues that survive into the published wiki become permanent debt.

Any violation = DRIFTED or FABRICATION verdict on that claim.

---

## The 5-pass audit

### Pass 1 — Mechanical sweep (deterministic regex)

Run before dispatching the per-claim sub-agent — these are the easy catches.

```bash
DOC=<doc-path>

# Line numbers in body
grep -nE '\.(go|ts|tsx|sql|yaml|prisma|json):[0-9]+' "$DOC" && echo "DRIFT: line numbers found"

# Dates
grep -nE '20[0-9]{2}-[0-9]{2}(-[0-9]{2})?|20[0-9]{2}-Q[1-4]|20[0-9]{2}/[0-9]{2}' "$DOC" && echo "DRIFT: dates found"

# Archaeology vocabulary
grep -nEi 'after.*(cleanup|rewrite|removal|change|migration)|previously|formerly|since the.*(removal|rewrite|cleanup|change)|was renamed|\bdeprecated\b|\blegacy\b|used to|no longer|as of [0-9]|post[- ]?20[0-9]{2}|decision log|rejected alternative|supersede' "$DOC" && echo "DRIFT: archaeology found"

# Program-tracking references
grep -nE '\bE[0-9]+(-S[0-9]+)?\b|\bSDD\b|bug #[0-9]+|incident [A-Z0-9]|PR #[0-9]+|commit [a-f0-9]{7,}' "$DOC" && echo "DRIFT: program-tracking refs found"

# Missing References section
grep -qE '^## References' "$DOC" || echo "DRIFT: missing ## References section"

# Path existence (every packages/ or tools/ citation must resolve)
grep -oE '(packages|tools)/[a-zA-Z0-9_./-]+' "$DOC" | sort -u | while read p; do
  test -e "$p" || echo "MISSING: $p"
done
```

Every hit must be triaged. False positives are rare but possible — flag them, don't auto-ignore.

### Pass 2 — Per-claim audit (independent sub-agent)

Dispatch a `general-purpose` sub-agent (needs Read + grep). Mechanical pass output goes in as context. Sub-agent walks the doc top-to-bottom and verdicts every factual claim.

**Required sub-agent prompt structure:**

```
You are a strict code-anchored doc auditor. Worktree: <path>.

Audit <doc-path> claim-by-claim against the real codebase. The author
asserted N claims; verify each one independently. Do NOT trust a
batch-grep-verify pass — that's the failure mode this skill catches.

For EVERY factual claim in the doc (not narrative framing):
1. State the claim in one sentence.
2. Cite the grep command or file:line that proves or disproves it.
3. Verdict:
   - VERIFIED — claim matches code exactly.
   - DRIFTED — claim is partially wrong (wrong path, wrong field name,
     wrong count, missing case, outdated description).
   - UNVERIFIABLE — claim can't be checked from code (too vague, or
     about runtime behavior that needs a live test).
   - FABRICATION — claim references a file / symbol / behavior that
     does not exist in code.

Focus extra scrutiny on supporting prose, not just headline identifiers:
- "all writers go through X" — check ALL writers, not just one.
- "delegates to Y" — open the delegate and confirm wiring.
- "no overlapping window" — confirm the atomicity claim.
- Per-provider mapping tables — spot-check every row.

Hard rules (any violation = DRIFT regardless of factual accuracy):
- No line numbers in committed doc body.
- No dates.
- No archaeology vocabulary.
- No Epic / SDD / bug / incident references.
- ## References section exists with paths only.

Report under 800 words. Format:
- VERIFIED claims: count.
- For each DRIFTED / UNVERIFIABLE / FABRICATION: claim, evidence, suggested edit.
- Hard-rule violations: line + suggested rewrite.
- Final verdict: CLEAN / N issues.

If you find zero issues, say "CLEAN — N claims verified, 0 drift" and stop.
```

### Pass 3 — Triage the verdicts

For each DRIFTED / UNVERIFIABLE / FABRICATION:

- **FABRICATION** — must fix or delete the claim. No exceptions.
- **DRIFTED** — must fix. The fix may be: correct the wrong name/path, add missing cases, soften an overclaim, narrow scope to what's actually true.
- **UNVERIFIABLE** — fix if possible (replace with a verifiable claim). If genuinely runtime-only (e.g. "request completes in <100ms typical"), either delete or move into a clearly-marked operational expectation section.

For each hard-rule violation: fix per the rule.

### Pass 4 — Re-run if fixed anything substantial

If you applied non-trivial fixes (> 3 claim rewrites, or any FABRICATION removal), re-dispatch the per-claim audit. The fixes themselves can introduce new drift. Cheap insurance.

Skip the re-run only if fixes were purely mechanical (line-number strip, date removal, regex-found archaeology word swap).

### Pass 5 — Final verdict + handoff

Report to the calling skill / user:

```
doc-review: <doc-path>
  Pass 1 (mechanical): N hits → fixed
  Pass 2 (per-claim): N claims verified, N drift → fixed
  Pass 3 (re-audit): CLEAN
  Verdict: CLEAN, ready to commit
```

OR

```
doc-review: <doc-path>
  Pass 2 (per-claim): N drift, N fabrication
  Verdict: NEEDS REWRITE — see findings above
```

The calling skill (`doc-write`) must not proceed to commit on a non-CLEAN verdict.

---

## Anti-patterns to refuse

- Trusting the sub-agent's "verified clean" report without spot-checking at least the highest-blast-radius claims. Sub-agents have small context windows and bias toward closing tasks; their batch-verify misses multi-paragraph fabrications.
- Running only the mechanical pass and declaring the doc CLEAN. The mechanical pass catches the easy stuff (regex-detectable); the per-claim pass is where real drift surfaces.
- Skipping pass 4 (re-audit after substantial fixes). Fixes introduce drift too.
- Letting an UNVERIFIABLE claim survive without justification. Either fix it into something verifiable or delete it.
- Marking a doc CLEAN with the `## References` section missing. That section is the future-reader's entry point for re-verification.
