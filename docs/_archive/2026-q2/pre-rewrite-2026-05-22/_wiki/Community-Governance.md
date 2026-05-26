# Community Governance

Nexus Gateway uses a project-lead governance model. A single project lead makes final decisions on architecture, scope, and release timing, with input from maintainers and the broader community. This page describes how decisions get made, what the contribution acceptance criteria are, and how the community can influence direction.

---

## Decision authority

**Project lead** — Nexus (`nexus@alphabitcore.com`, GitHub: `nexus`) holds final decision authority on:

- Architecture boundaries and cross-cutting design (API contracts, data models, service topology).
- Scope decisions for features, especially anything that adds a new admin surface, config dimension, or public API endpoint.
- Release timing and the production deployment process.
- Changes to binding workflow rules in `CLAUDE.md` and `.cursor/rules/`.

Decisions that are non-trivial, cross-cutting, or carry significant risk are documented as a plan + written rationale before implementation. For large feature additions, this means going through the full SDD pipeline (Architecture → Requirements → SDD → OpenAPI → Code → Tests).

**Maintainers** participate in design reviews and PR approvals. The current maintainer list and ownership areas are on [Community Maintainers](Community-Maintainers).

## How contributions are accepted

Pull requests are accepted when they satisfy all of the following criteria:

**Process criteria** (enforced by CI and pre-commit hooks):

- The change has a plan and a todo list (documented in the PR description or a linked Discussion for non-trivial changes).
- All 19+ lint scripts pass (`npm run check:all`).
- Go test coverage remains ≥95% per package (or the affected packages are in the allowlist with an explicit rationale approved by the project lead).
- For API endpoint additions, moves, or renames: the IAM impact review is complete and the decision is recorded in the PR description.
- For code in an area covered by an architecture doc: the matching doc is updated in the same PR.

**Quality criteria** (assessed during review):

- No placeholder production code (no `TODO` / `FIXME` / `XXX` / stub logic in non-test files).
- No hardcoded secrets; secrets are env-only.
- English only in all committed artifacts.
- New config dimensions have a documented user journey ("why can't a global setting cover this?").

**Review process:**

1. Open a GitHub Discussion for non-trivial features before writing code. This ensures scope agreement before effort is spent.
2. Open a pull request against `main`. Link the Discussion if applicable.
3. The project lead or a designated maintainer reviews. Two rounds of feedback are normal for non-trivial changes.
4. CI must be green before merge.

The full checklist is in [`docs/developers/workflow/conventions.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/conventions.md) §11.

## How project direction is influenced

**GitHub Discussions** is the primary venue for proposing changes to project direction, architecture, or scope. A well-reasoned Discussion with concrete use cases is the strongest signal for prioritizing work.

**Roadmap visibility** — the active and queued roadmap is public in [`docs/developers/roadmap.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/roadmap.md) and summarized on [Roadmap Active](Roadmap-Active) and [Roadmap Queued](Roadmap-Queued). Community members can comment on roadmap items or propose additions via Discussions.

**Less-is-more principle** — Nexus Gateway follows a "sensible defaults, minimal knobs" design philosophy. New features are evaluated against whether a default or existing surface can cover the use case before a new config dimension is added. This is documented in `CLAUDE.md` as the "adversarial product review + less-is-more" binding rule. Proposals that add admin surface area should address: what user journey this serves, how frequently real users will hit this path, and why nothing existing covers it.

## Governance model evolution

The project is pre-GA and currently single-lead. As the contributor base grows, the governance model may evolve toward a steering committee or CODEOWNERS-based area ownership. Any such change would be announced in GitHub Discussions and documented here before taking effect.

---

## Canonical docs

- [`CONTRIBUTING.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CONTRIBUTING.md) — condensed workflow and high-blast-radius surface guidance
- [`CLAUDE.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CLAUDE.md) — the full set of binding workflow rules
- [`docs/developers/workflow/conventions.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/conventions.md) — PR review checklist and code-style guidance

**Adjacent wiki pages**: [Community Maintainers](Community-Maintainers) · [Community Support Channels](Community-Support-Channels) · [Community Code Of Conduct](Community-Code-Of-Conduct) · [Contributing](Contributing) · [Dev SDD Pipeline](Dev-SDD-Pipeline)
