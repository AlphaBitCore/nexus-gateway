# Dev Code Review Checklist

*Audience: reviewers approving pull requests.*

Reviewing a Nexus Gateway PR means verifying that the SDD pipeline was followed, the binding rules were respected, and the change is complete and correct. This page presents the 11-point reviewer checklist, the CI binding checks that must pass before merge, and how to use the project-review skill for a full-system audit when a major change warrants it.

---

## The 11-point PR review checklist

Run through each item before approving. Items marked **binding** have an automated check in CI, but the reviewer also confirms the intent behind them, not just the passing gate.

- [ ] **Plan and Todo were followed.** The PR description or linked issue shows a written plan and a completed task list. Every task is either done or explicitly cancelled with a reason. No open todos left in a "falsely completed" state.

- [ ] **All edits are English.** Source comments, doc text, user-facing strings, commit messages — no non-English text committed to the repo.

- [ ] **No `TODO`/`FIXME`/`XXX`/`unimplemented`/`stub` in production code.** `npm run check:no-prod-todos` catches this mechanically; the reviewer also spot-checks that the check ran and passed. Test mocks are fine.

- [ ] **Architecture docs consulted per the trigger map.** For every changed package, the contributor confirmed they read the relevant doc in `docs/developers/architecture/README.md`. If the change has architectural impact, the architecture doc updated in the same PR.

- [ ] **Code / doc lockstep satisfied.** `npm run check:doc-lockstep` passed. The reviewer verifies the intent: are the updated docs actually accurate, or just touched to clear the gate?

- [ ] **IAM impact reviewed if applicable.** Any PR that adds, moves, renames, or removes an admin API endpoint, sidebar nav item, or admin route path must include the 5-step IAM impact review. The decision ("kept on `admin:settings.read`" / "carved out as new action") is recorded in the PR description or commit message. Drift between UI `allowedActions` and `iamMW(...)` produces silent 403s.

- [ ] **Migration timestamp unique if a migration is included.** `npm run check:migration-timestamps` passed. No two migration folders share the same `YYYYMMDDHHMMSS` prefix.

- [ ] **Token-field stamp sites swept if a new token/usage field was added.** AI Gateway proxy handlers have 5 stamp sites: the non-stream path, the stream path, and 3 cache-side paths. Missing the cache-side sites nulls all prod cache traffic for that field.

- [ ] **NE fail-open rules respected if the macOS NE extension was touched.** The five rules in `CLAUDE.md` (synchronous `handleNewFlow` decision, 2s daemon timeout, no hardcoded enforcement lists, no `isLikelyXyz = true` patterns, system DNS/DHCP processes must not have their UDP closed) are safety-critical. A reviewer familiar with the NE fail-open architecture should sign off on any change to `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/`.

- [ ] **New architecture doc trigger-map row added if a new `*-architecture.md` was created.** `npm run check:arch-doc-triggers` catches this, but the reviewer confirms the row in `docs/developers/architecture/README.md` is accurate and complete.

- [ ] **Tests pass locally.** `go test -race -count=1 ./...` green for all touched packages; `npm test` green for UI. The CI green is the gate, but the contributor should have confirmed locally first.

---

## CI binding checks

These checks run on every PR push. A PR cannot merge if any fails:

| Check | Command | What it catches |
|---|---|---|
| Coverage gate | `npm run check:coverage` | Any touched Go package below 95% not in the allowlist |
| Doc lockstep | `npm run check:doc-lockstep` | Code in a locked glob changed without a matching doc update |
| No prod TODOs | `npm run check:no-prod-todos` | `TODO`/`FIXME`/`XXX`/`stub` strings in production code |
| No yaml secrets | `npm run check:no-yaml-secrets` | Secret fields in committed YAML |
| i18n parity | `npm run check:i18n` | Locale key counts out of sync across EN/ZH/ES |
| Design tokens | `npm run check:design-tokens` | Hex or raw numeric values in CSS module files |
| IoT terminology | `npm run check:terminology` | Internal Thing/Shadow/desired terms on user-facing surfaces |
| Arch doc triggers | `npm run check:arch-doc-triggers` | Architecture doc with no trigger-map row |
| Migration timestamps | `npm run check:migration-timestamps` | Duplicate migration timestamp prefixes |
| Workspace replace | `npm run check:workspace-replace` | Sibling `replace` directives missing or using non-`v0.0.0` versions |

---

## Using the project-review skill

For major changes — a new subsystem, a significant refactor, a new service boundary — invoke the `/project-review` skill (`.claude/skills/project-review/SKILL.md`) to run a 9-role full-system audit. The roles cover architecture, requirements, API design, security, compliance, performance, frontend, and integration.

Each role produces JIRA-ready findings ordered by severity (Critical → High → Medium → Low). Every finding includes an impact assessment and a fix plan following the SDD pipeline steps. Run it when:

- A PR touches IAM, credential handling, or config sync in a non-trivial way.
- A new admin endpoint is added or an existing one is restructured.
- The macOS NE extension is modified.
- A change affects the `packages/shared/` API (additive-only once shipped in an Agent binary).

---

## Canonical docs

- [`docs/developers/workflow/conventions.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/conventions.md) — the full PR review checklist in §11, plus coding conventions
- [`CLAUDE.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CLAUDE.md) — the binding rules each checklist item enforces
- [`docs/developers/architecture/services/control-plane/iam-identity-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/iam-identity-architecture.md) — IAM impact review 5-step procedure

**Adjacent wiki pages**: [Contributing](Contributing) · [Dev SDD Pipeline](Dev-SDD-Pipeline) · [Dev Testing Coverage](Dev-Testing-Coverage) · [Dev Code Doc Lockstep](Dev-Code-Doc-Lockstep) · [Dev Release Process](Dev-Release-Process)
