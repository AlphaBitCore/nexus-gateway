# Workbench Forking Guide

*Audience: engineers who want to adopt the AI vibe-coding workbench on a different codebase.*

The AI vibe-coding workbench — `CLAUDE.md`, `.cursor/rules/`, `.claude/skills/`, and `scripts/check-*` — is designed to be forked. The entire layer set is in the repo. The meta-rules (plan-first, self-audit, worktree discipline, no auto-commit) are generic; the domain rules (NE fail-open, adapter canonical format, IAM impact review) are specific to Nexus Gateway and will not apply to most other projects. This guide walks through extracting the workbench, adapting it to your stack, and running your first disciplined session.

---

## Extraction and initial setup

1. Fork or clone this repo and copy the four workbench directories to your own repo root: `CLAUDE.md`, `.cursor/rules/`, `.claude/skills/`, and the `scripts/check-*` files. If your repo is not a Go + React monorepo, the Go-specific and React-specific check scripts will not apply — pick the ones that match your stack. The most universally applicable scripts are `check-no-prod-todos.mjs`, `check-no-yaml-secrets.mjs`, and `check-doc-lockstep.mjs`.

2. Wire the pre-commit hook. Copy `.githooks/pre-commit` and adapt the script list to the check scripts you kept. Run `git config core.hooksPath .githooks` in your new repo. The hook must run in under 2 seconds on a staged-file scope or developers will route around it; profile each check script before adding it.

3. Wire CI. The CI pipeline runs the same checks in full-tree strict mode. For GitHub Actions, adapt `.github/workflows/ci.yml` — the `governance-lints` job is the relevant section. Run check scripts with `--all` or equivalent full-sweep mode, not just staged-file scope.

---

## Adapting `CLAUDE.md` to your tech stack

4. Keep the meta-rules verbatim. The following rules are universal and apply to any AI-assisted development context: plan-first, todo-list-stays-live, complex-task-Plan+Todo-non-waivable, goal-anchored execution, 2-round self-audit, no auto-commit, worktree per session, sub-agent dispatch discipline, real-implementation-only, English-only repository. These rules encode failure modes that appear in any codebase with AI agents — they are not Nexus-specific.

5. Replace the tech-stack section. The `## Current state`, `## Project structure`, and `## Tech stack` sections describe Nexus Gateway. Replace them with your repo's service inventory, module layout, and the stable technical facts your agent should know without rediscovering from code. These sections save the agent from re-reading the whole codebase at the start of every session.

6. Replace or remove the domain bindings. Most of the `## Mandatory rules` entries beyond the meta-rules are Nexus-specific: the macOS NE fail-open rule, the provider adapter canonical format rule, the AI Gateway smoke rule, the IAM impact review rule. Remove any rule that does not apply to your domain. Add your own domain bindings as incidents accumulate — write each rule once a failure mode has cost real engineering time, not preemptively.

7. Keep the structure sparse. `CLAUDE.md` is always loaded into the agent's context. Overloading it with content that changes frequently, content that belongs in an architecture doc, or rules that have no incident backing turns it into noise the agent learns to skim. The file should be dense, stable, and every sentence should earn its place.

---

## Adapting the cursor rules

8. Pull the always-apply rules verbatim. The 14 always-apply rules are the most portable part of the workbench. Copy: `sdd-workflow.mdc`, `pre-edit-reading.mdc`, `architecture-doc-triggers.mdc`, `binding-rules-quick-reference.mdc`, `completion-time-self-audit.mdc`, `complex-task-plan-todo.mdc`, `no-backward-compatibility.mdc`, `english-only.mdc`, `session-handoff.mdc`, `sub-agent-dispatch.mdc`, `unit-test-coverage-95.mdc`, `worktree-per-session.mdc`, `adversarial-product-review.mdc`, and `code-doc-lockstep.mdc`. Update the file paths they reference (architecture README, doc-lockstep config, coverage script) to match your repo.

9. Drop the Nexus-specific glob-scoped rules and grow your own. `ne-fail-open.mdc`, `provider-adapter-canonical-openai.mdc`, `token-field-stamp-sweep.mdc`, `vk-org-resolution.mdc`, `nrn-builder-canonical.mdc`, `agent-runtime-invariants.mdc`, `agent-ui-terminology.mdc` are all domain-specific to Nexus Gateway. Delete them. As incidents accumulate in your codebase, write new glob-scoped rules: one rule per incident, fired by the file globs relevant to that incident. The glob scope keeps domain-specific rules from loading into every context.

---

## Porting skills

10. Start with the five portable skills. The following skills work in any repo with minimal adaptation: `spec-writing` (SDD discipline), `project-review` (multi-role review), `pre-edit-reader` (3-doc enforcement), `gap-review` (SDD parity audit), `i18n-gap-check` (if you have a multi-locale UI). Copy them and update the file paths inside each `SKILL.md` to match your repo's doc structure.

11. Adapt the prod operations skills for your hosting. `prod-deploy`, `prod-login`, and `prod-debug` are the most tightly coupled skills. To adapt `prod-deploy`: replace the EC2 SSH target with your hosting (container registry push, GitHub Actions release, etc.), replace the binary paths with your service layout, and replace the restart sequence with your service manager. Keep the mandatory post-deploy smoke binding — it is the single most load-bearing rule in the skill. To adapt `prod-login`: replace the OAuth endpoint and credentials. To adapt `prod-debug`: replace DB connection strings, log paths, and the failure-pattern catalog with your own incidents as they accumulate.

12. Write new skills from procedures you repeat three or more times. When you find yourself explaining "and then you do this in exactly this order" for the third time, turn it into a skill. The shape is fixed: pre-conditions, numbered steps, binding rules, verification gate, recovery notes. The length scales with the procedure's complexity — a simple audit skill is 50 lines; a full deploy skill is 500 lines. Do not make a skill for a one-off task or a single command.

---

## Adopting the discipline

The value of the workbench compounds over time and sessions. A few notes for the first weeks:

Run the pre-commit hook on every commit from day one. Developers who route around the hook early build a habit that persists after the gate is tightened.

Start the SDD pipeline even for small changes. A one-line bug fix still gets a one-paragraph plan, a single todo, and a self-audit. The habit of planning before editing is more valuable than the plan itself.

Write rules only from incidents. A rule written preemptively, before any failure mode has materialized, tends to be either redundant (already implied by another rule) or too broad (blocks legitimate work). Rules written from incidents are precisely scoped and easy to defend.

Expect the discipline to slow the first session and accelerate the tenth. An agent operating without binding rules is faster to start and slower to converge — it produces code that needs revision, leaves test coverage gaps, and drifts between sessions. An agent operating with a stable rule set builds on the previous session's state and rarely needs to be corrected for the same class of mistake twice.

---

## Common pitfalls when forking

**Putting too much in `CLAUDE.md`.** The file loads into every prompt. Content that changes frequently (sprint goals, current-sprint context, per-feature notes) should live in handoff documents or architecture docs, not in `CLAUDE.md`. Overloading `CLAUDE.md` trains the agent to skim it; every sentence should earn its place by being stable and binding.

**Wiring CI but not pre-commit.** CI runs on PR merge, not on every local commit. Pre-commit gates catch problems before they reach the branch, when the fix cost is lowest. Both layers are needed: pre-commit for fast local feedback, CI for the full-tree strict sweep that can afford to be slower.

**Writing skills too early.** A skill for a procedure you have done once is a premature investment. The skill will be too narrow (it won't cover the variation you encounter the second time) or too broad (it will carry steps that turn out to be irrelevant). Write skills on the third repetition, after the procedure has stabilized.

**Keeping Nexus-specific glob rules.** The Nexus-specific rules (`ne-fail-open.mdc`, `provider-adapter-canonical-openai.mdc`, `token-field-stamp-sweep.mdc`, `vk-org-resolution.mdc`) fire on file globs that do not exist in most repos. Keeping them creates noise and trains the agent to ignore glob-scoped rules. Delete them on day one; add your own as incidents accumulate.

**Skipping the SDD pipeline for "small" changes.** The plan-first rule applies even to one-liners. A one-paragraph plan and a single todo take under a minute. The discipline of stopping to write the goal before touching code catches a surprising fraction of "I thought you wanted X but you wanted Y" misunderstandings, especially in asynchronous multi-session work.

## Canonical docs

- [`ai-workflow.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/ai-workflow.md) — the "Adopting this workflow in a fork" section with a 5-step checklist
- [`ai-skill-catalog.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/ai-skill-catalog.md) — portability ratings and adaptation steps for every skill
- [`CLAUDE.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CLAUDE.md) — the canonical binding file to fork from

**Adjacent wiki pages**: [Workbench Overview](Workbench-Overview) · [Workbench CLAUDE md Anatomy](Workbench-CLAUDE-md-Anatomy) · [Workbench Cursor Rules](Workbench-Cursor-Rules) · [Workbench Claude Code Skills](Workbench-Claude-Code-Skills) · [Workbench Lessons Learned](Workbench-Lessons-Learned)
