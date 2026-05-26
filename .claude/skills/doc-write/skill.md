# doc-write

Write a single architecture / feature / runbook doc end-to-end against the **code-anchored protocol**. The doc is a forward-looking description of current state, every claim grounded in code that exists on disk right now.

Use this skill when:

- Writing or rewriting any file under `docs/developers/architecture/`, `docs/users/features/`, `docs/operators/`, or `docs/developers/workflow/`.
- Filling in a placeholder doc on the `feature/docs-backfill` program.
- Replacing a known-drifted archived doc.

This skill is one half of a pair — pair with `doc-review` (the pre-commit audit gate). Every doc this skill produces MUST pass `doc-review` before commit.

---

## Hard rules (binding, no exceptions)

These rules are non-negotiable. A doc that violates any of them must be rewritten before commit.

1. **Code-anchored only.** Every factual claim must be backed by code that exists on disk at the time of writing. No fabrication, no invention, no "I think the system does X." If you can't grep-find a claim, drop the claim.
2. **Forward-looking present-state only.** Describe what the system IS today. Never narrate history: no "after X was removed", "previously this was Y", "since the rewrite", "this was added to fix Z", "used to be", "deprecated", "legacy", "formerly", "post-2026".
3. **No line numbers in committed docs.** Cite file paths only (e.g. `packages/shared/transport/typology/wireshape.go`). Line numbers rot the moment anyone edits the file. Line numbers are scratch-only — use them while building the fact ledger and during grep-verify, then strip every one before commit.
4. **No dates in docs.** No `2026-05-25`, no `2026-Q2`, no `as of <date>`. Docs are timeless descriptions of current state.
5. **No Epic / SDD / bug / incident references.** Don't write `E87`, `SDD §3a`, `bug #123`, `incident 2026-05-15`, `per PR #42`. The doc describes the system, not the program that built it. Cross-references between architecture docs are fine (e.g. "see [iam-identity-architecture.md](...)").
6. **References section at the end.** Every doc ends with a `## References` section listing the canonical code paths the doc describes — paths only, no line numbers. This is where future readers and CI look to recheck the doc.
7. **One doc per commit.** Commit message: `docs: <doc-slug> — <one-line summary>`. Per-doc commits keep review + revert surgical.
8. **Surface every issue you discover.** While reading the code to write the doc, if you find any product / architecture / technical issue — a half-built feature, dual code paths doing the same thing, an unused constant the UI anticipates, a wire field with inverted semantics across services, a fabrication site that ships data nobody reads, anything that smells wrong — STOP, surface it to the user with: (a) the concrete finding, (b) the code evidence, (c) a brainstormed best-product / best-architecture recommendation. Do not paper over it in the doc. The doc-writing pass is the highest-fidelity audit of the system that ever happens; issues caught here are cheap to fix, issues that ship into a published wiki become permanent technical debt.

---

## The 9-step workflow

### 1. Identify code anchors

From the doc filename + the lockstep config (`scripts/doc-lockstep.config.mjs`) + any archived trigger map, list the canonical code paths this doc must reflect. Examples:

- `cost-estimation-architecture.md` → `packages/ai-gateway/internal/execution/estimator/**` + cost-stamp sites in `proxy.go` / `proxy_cache.go` + codec pricing fields.
- `credentials-architecture.md` → `packages/control-plane/internal/platform/crypto/` + `packages/control-plane/internal/ai/providers/credstore/` + the gateway-side `packages/ai-gateway/internal/credentials/`.

The lockstep config is the authoritative `code → doc` map; reverse-look it to find your anchors.

### 2. Read the actual current code

Use `Read` on every canonical file. Build the mental model from what's on disk **today**, not from training data, not from the archived doc, not from memory of how the system "used to" work. If a memory entry conflicts with current code, the code wins.

### 3. Build a fact ledger (scratch)

Keep at `/tmp/doc-ledger-<doc-slug>.md` for the duration of the doc. Every load-bearing claim gets a `file:line` or `pkg.Symbol` citation **in the ledger**. The ledger is the auditable trail behind every sentence the doc will contain — but the line numbers in the ledger stay in the ledger; they never make it into the doc body.

Dispatch a sub-agent for the ledger if the code surface spans many files. The sub-agent prompt must:

- Inline the hard rules from this skill (anti-fabrication is the most-violated one for sub-agents).
- State the worktree path + the specific doc filename in scope.
- Demand per-claim `file:line` citations + the exact `grep` commands run.
- Refuse fabrication: "If you can't find code that backs a claim, drop the claim. Don't paraphrase an archived doc as a substitute."

### 4. Present outline + key facts to user, iterate

Show the doc structure + the load-bearing claims to the user (use the user's preferred language if not English). User reviews, corrects misunderstandings, adjusts scope / structure / emphasis. Iterate until the user is satisfied with the substance **before** writing English prose. Catching a structural issue here costs minutes; catching it after the English draft is committed costs hours.

### 5. Translate to final English doc

Repo is English-only (project mandatory rule). Translation happens AFTER substance is agreed.

- Cite **file paths only** — strip every line number before committing.
- No dates, no Epic/SDD/bug references, no archaeology language (see Hard rules above).
- Use present tense. "The vault encrypts with AES-256-GCM" — not "The vault was rewritten to use AES-256-GCM."

### 6. Add the `## References` section

End the doc with:

```markdown
## References

- `packages/control-plane/internal/platform/crypto/` — vault implementations
- `packages/ai-gateway/internal/credentials/` — gateway-side decrypt path
- `tools/db-migrate/schema.prisma` — Credential model
```

Paths only. No line numbers, no descriptions longer than one phrase per path.

### 7. Self-verify pass

Two greps before you call `doc-review`:

**(a) Path/symbol existence.** Every `packages/...` / `tools/...` path cited in the doc must exist on disk. Every `pkg.Symbol` must be findable.

```bash
# extract every path-looking citation and verify
grep -oE '(packages|tools)/[a-zA-Z0-9_./-]+' <doc-path> | sort -u | while read p; do
  test -e "$p" || echo "MISSING: $p"
done
```

**(b) Hard-rule sweep.** No line numbers, no dates, no archaeology, no Epic/bug references.

```bash
# Line numbers in committed body
grep -nE '\.(go|ts|tsx|sql|yaml|prisma):[0-9]+' <doc-path> && echo "DRIFT: line numbers found"

# Dates
grep -nE '20[0-9]{2}-[0-9]{2}(-[0-9]{2})?|20[0-9]{2}-Q[1-4]' <doc-path> && echo "DRIFT: dates found"

# Archaeology
grep -nEi 'after.*(cleanup|rewrite|removal|change|migration)|previously|formerly|since the.*(removal|rewrite|cleanup|change)|was renamed|deprecated|legacy|used to|no longer|as of <date>|post[- ]?20[0-9]{2}' <doc-path> && echo "DRIFT: archaeology found"

# Epic / SDD / bug / incident
grep -nE '\bE[0-9]+\b|SDD|bug #[0-9]+|incident |PR #[0-9]+' <doc-path> && echo "DRIFT: program-tracking refs found"
```

Each hit must be fixed (or explicitly justified as not a violation — e.g. "E87" inside a code-block that quotes a struct tag is fine).

### 8. Invoke `doc-review` (binding pre-commit gate)

```
/doc-review <doc-path>
```

The `doc-review` skill dispatches an independent audit sub-agent that walks every factual claim, grep-verifies, and reports VERIFIED / DRIFTED / UNVERIFIABLE / FABRICATION per claim. Do not commit a doc with any DRIFTED or FABRICATION verdicts unfixed. Re-run after fixes until CLEAN.

### 9. Present summary to user → wait for explicit approval → commit

After `doc-review` is CLEAN, present a short summary of what the doc says to the user and **wait for explicit approval** before committing. Use the user's preferred language for the summary if not English.

The summary must cover:
- Per-section content gist.
- Key terminology choices.
- Any code-vs-existing-doc tensions noticed during verify.

Only after the user signals approval → `git commit` (one doc per commit, message `docs: <doc-slug> — <one-line summary>`).

If the project has a `--no-verify` waiver in effect for the docs program (check the worktree's memory), use it; otherwise let the normal pre-commit hooks run.

---

## Anti-patterns to refuse

- Lifting prose verbatim from `docs/_archive/**` or any pre-existing doc without re-verifying every claim against current code. Archived docs are archived because they're known-drifted.
- Writing "the system does X" when the code does Y because X was the design intent. The doc reflects what the code does, not what someone wanted it to do.
- Inventing file paths, function names, struct fields, enum values, or schema columns from memory. Every name must grep-verify on disk right now.
- Claiming "this protects against Z" without pointing at the specific code that enforces Z.
- Hand-waving a "high level overview" that doesn't ground in concrete file / symbol citations.
- Calling the doc "done" after only headline-identifier grep-verify. The drift hides in supporting prose ("delegates to X", "all writers go through Y", "no overlapping window") — those need per-claim audit too.
- Skipping step 4 (user iteration on substance) because the doc "looks straightforward." Substance issues caught post-commit cost orders of magnitude more than caught pre-write.
- Skipping step 8 (`doc-review`). The audit gate exists specifically because batch grep-verify misses drift.
- Skipping step 9 (user approval). The post-write summary is the last chance to catch design issues before they ship.
