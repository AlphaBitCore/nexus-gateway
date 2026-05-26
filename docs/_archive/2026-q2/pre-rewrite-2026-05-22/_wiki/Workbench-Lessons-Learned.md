# Workbench Lessons Learned

*Audience: engineers evaluating whether to adopt the AI vibe-coding workbench, and fork-adopters calibrating which patterns to prioritize.*

This page documents which workbench patterns proved load-bearing and what failure modes each one prevents. The framing is forward-looking: "these patterns work because…" rather than a history of incidents. Readers considering adoption get the practical signal they need — which rules carry the most weight, which are most portable, and which require the most domain-specific tuning.

---

## Patterns that proved most load-bearing

### Plan-first + 2-round self-audit

The two meta-rules that interact most tightly are the plan-first requirement and the 2-round completion self-audit. Together they close the gap between "agent produces an output" and "output is actually done". Plan-first prevents the agent from solving the wrong problem or overreaching scope. The self-audit prevents the agent from claiming done while test coverage is incomplete, while `TODO` strings are in production code, or while a todo item was silently dropped rather than completed.

The 2-round structure matters: a single audit pass surfaces issues, but without a second pass that specifically checks whether each issue was fixed, the agent can misrepresent a partial fix as complete. Two consecutive clean rounds are the verifiable condition. This pattern works because AI agents are good at identifying what was done and poor at tracking what was supposed to be done — the audit forces explicit accounting.

### 95% coverage gate with allowlist discipline

A per-package coverage gate without an allowlist either blocks legitimate exceptions (DB-bound tests, OS-bound tests, integration-only code) or gets disabled entirely. An allowlist without categories and explicit rationale accumulates dead entries as packages improve. The combination — hard gate + categorized allowlist + strict-allowlist mode to find removable entries — creates a system where the threshold is high, exceptions are documented, and the threshold tightens over time as packages mature.

The test quality companion rule is equally important: tests must assert observable business behavior and named failure modes, not just call the function and check `err == nil`. Coverage that pads the number without asserting behavior is a false positive. The gate catches the quantity problem; the quality rule catches the validity problem.

### Code/doc lockstep

The lockstep enforcement (`scripts/check-doc-lockstep.mjs`) prevents the pattern where architecture docs gradually drift from the code they describe until they become unreliable as planning inputs. The key insight is that the lockstep map (`scripts/doc-lockstep.config.mjs`) is the code-glob → doc-files registry — not a list of "try to remember to update the doc." When the map is maintained, a PR that changes routing code without updating the routing architecture doc fails CI automatically, not by human review.

The doc tree matters too. The lockstep covers architecture docs, feature docs, OpenAPI specs, runbooks, and SDD stories. A code change that only updates architecture docs but not the OpenAPI spec still fails. A change that updates the spec but not the runbook still fails. The multi-tree requirement keeps all surfaces aligned, not just the most obvious one.

### Worktree per session

Running multiple AI agent sessions concurrently in the same working tree produces `.git/index.lock` contention, `git stash` interference, and merged working-tree state that is hard to debug. Each session in its own `git worktree` isolates the working tree and index, so all standard git operations (`git stash`, `git add -A`, `git restore`) are safe inside the session's own worktree. The constraint that two worktrees cannot share a branch is enforced by git itself, which prevents the worst collision case.

The shared-module coordination note matters for monorepos: worktrees isolate the working tree, not the module graph. Two sessions editing the same `packages/shared/<subpkg>/` file concurrently will conflict at merge. The fix is to coordinate at the subpackage level before starting parallel sessions, not at the git level.

### No backward compatibility in pre-GA code

The no-backward-compatibility rule eliminates an entire class of accumulated technical debt in pre-GA projects. Without it, the default behavior is to add compatibility shims every time something changes, resulting in parallel code paths that are never cleaned up, `@deprecated` markers that live indefinitely, and feature flags for rollback that stay in the codebase long after they are needed. The rule replaces all of those patterns with a single default: delete the old code, update the callers, ship. This works in pre-GA because there are no installed users with data in the old format — the constraint that makes backward compatibility necessary does not apply.

### Glob-scoped rules for domain invariants

The most dangerous invariants in a codebase are the ones that are almost always obvious but occasionally violated with catastrophic consequences. The macOS NE fail-open invariants are an example: `handleNewFlow` must decide synchronously, every async callback must have a fail-open timeout, enforcement lists must come from the shadow blob not from hardcoded Swift. A developer editing Network Extension code does not need these rules in every context, but must see them exactly when editing that code.

Glob-scoped rules are the right mechanism for this class of invariant: they load automatically when the relevant files are open, carry the exact binding text at the point of need, and do not pollute the context for unrelated work. The pattern generalizes: any invariant that is "only relevant in subsystem X" belongs in a glob-scoped rule, not in `CLAUDE.md` (which loads everywhere).

---

## Failure modes the workbench prevents

**Silent scope creep.** Without plan-first and todo tracking, an agent refactoring a function "for cleanliness" while the task was a bug fix causes a PR that is hard to review and may introduce regressions in adjacent code. The plan defines scope; the todo list tracks what was asked; the self-audit catches whether anything outside scope was changed.

**Test coverage theater.** Tests that exist only to bump the percentage number — calling a function and asserting only `err == nil` — provide false confidence. The 95% gate catches the absence of tests; the behavioral-assertion quality rule catches tests that are present but vacuous.

**Documentation archaeology.** Architecture docs that describe a design from two months ago rather than the current code are worse than no docs — they actively mislead. The lockstep check enforces recency, and the wiki style guide's "no archaeology" rule keeps the wiki forward-looking. Together they make documentation an operational asset rather than a historical record.

**Agent session state loss.** When a session approaches context-full, auto-compact summarizes but loses fidelity. The handoff document pattern — writing an explicit on-disk file capturing program state, architecture facts, completed work, and next steps before starting a fresh session — is more reliable than relying on compacted context. The pattern works because the handoff file is a first-class document that the next session reads as part of its startup, not a fallback for when memory fails.

**IAM drift.** Adding an admin API endpoint without updating the UI `allowedActions`, the handler `iamMW(...)`, the IAM action catalog, and the seed fixture simultaneously produces silent 403s: users see a menu item, click it, and get a 403. The IAM impact review binding and the corresponding 5-step skill catch this before merge by requiring all five layers to be audited in the same PR.

**Production stubs.** Merging skeleton code with `// TODO: implement` placeholders that never get filled in is a chronic failure mode in AI-assisted development — the agent produces a stub, both the agent and the human treat it as "mostly done", and the stub ships. `scripts/check-no-prod-todos.mjs` gates every commit and makes the choice explicit: either implement the code or delete the stub.

---

## What the workbench does not prevent

The workbench is not a substitute for human judgment at decision points. Plan approval, waivers for complex tasks, commit-time review, and architectural decisions that change scope require a human in the loop — the workbench's job is to make those decision points explicit and to ensure the agent's work is solid enough that human review is meaningful rather than catching basic errors.

The workbench also does not prevent all classes of agent drift. An agent operating under the binding rules will still occasionally misread a requirement, overlook an edge case, or propose an architecture that has unexamined consequences. The rules raise the floor significantly; they do not raise the ceiling to perfection. The layers are complementary, not redundant — each one catches what the layer above missed, and the combination is what makes the floor high.

---

## Calibrating adoption

Not all patterns deliver equal value in all codebases. The three patterns with the widest applicability — plan-first, 2-round self-audit, and real-implementation-only — address failure modes that appear in any AI-assisted development context and require no domain-specific setup. Start with these three if adopting incrementally.

The coverage gate and code/doc lockstep deliver compounding value over time as the codebase grows. They are worth the upfront setup cost (writing the allowlist, configuring the lockstep map) because the gate becomes more valuable as the codebase grows larger and the failure modes they prevent become more expensive to fix.

The glob-scoped rules and domain-specific skills are the most codebase-specific and should be grown from incidents rather than copied wholesale. See [Workbench Forking Guide](Workbench-Forking-Guide) for the adoption sequencing.

## Canonical docs

- [`ai-workflow.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/ai-workflow.md) — the four artifact layers and the "What this isn't" section
- [`CLAUDE.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CLAUDE.md) — the canonical binding rules with incident citations
- [`coverage-allowlist-methodology.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/coverage-allowlist-methodology.md) — the 95% gate's allowlist policy and audit methodology

**Adjacent wiki pages**: [Workbench Overview](Workbench-Overview) · [Workbench CLAUDE md Anatomy](Workbench-CLAUDE-md-Anatomy) · [Workbench Cursor Rules](Workbench-Cursor-Rules) · [Workbench Claude Code Skills](Workbench-Claude-Code-Skills) · [Workbench Forking Guide](Workbench-Forking-Guide)
