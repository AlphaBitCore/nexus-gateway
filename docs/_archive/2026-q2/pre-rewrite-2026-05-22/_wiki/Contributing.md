# Contributing

*Audience: first-time contributors to Nexus Gateway.*

Nexus Gateway welcomes contributions to all five services, the shared libraries, the admin UI, documentation, and the AI vibe-coding workbench. This page covers where to start, the three mandatory reads before editing code, the development pipeline at a high level, and where to ask for help. For the full procedural checklist, read [`CONTRIBUTING.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CONTRIBUTING.md) at the repo root.

---

## Before writing any code

Three documents must be read before the first code edit. This is the 3-doc pre-edit rule, binding for every contributor:

1. **Architecture doc(s)** — find the edit area in [`docs/developers/architecture/README.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/README.md). That file is the canonical "what to read when editing X" trigger map; it covers every package and subsystem. If the edit area has no row, that gap is worth raising.

2. **Feature doc(s)** — for any user-visible surface change, open the matching page under `docs/users/features/cp-ui/`, `docs/users/features/agent-ui/`, or `docs/users/features/flows/`. The index is at [`docs/README.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/README.md).

3. **Conventions** — [`docs/developers/workflow/conventions.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/conventions.md) covers Go idioms, TypeScript bindings, CSS token rules, commit format, and the PR review checklist.

Each document catches a different class of mistake: the architecture doc prevents structural errors, the feature doc keeps the user experience consistent, and the conventions doc prevents accumulating micro-debts. Skipping any of the three requires an explicit reason in the PR description.

---

## The development pipeline

Every change — including single-line fixes — follows this sequence:

```
Plan + Todo  →  Architecture  →  Requirements  →  SDD  →  OpenAPI
  →  Code  →  Unit Tests  →  Verify  →  Ask-about-commit
```

The pipeline is not bureaucracy. Each step removes a distinct failure mode:

- **Plan + Todo** — write what will be done (approach, scope, risks, files touched) before any code touches disk. Capture the plan as discrete, verifiable tasks.
- **Architecture** — identify the relevant doc(s) via the trigger map. Read them. If the change affects boundaries, data flow, or service contracts, update the architecture doc in the same PR.
- **Requirements** — for new features: `docs/developers/specs/e{epic}-{name}.md`. Captures functional and non-functional requirements, user roles, constraints, and MoSCoW priority.
- **SDD** — `docs/developers/specs/e{epic}-s{story}-{name}.md`. Stories, tasks, and acceptance criteria.
- **OpenAPI** — `docs/users/api/openapi/e{epic}-s{story}-{name}.yaml`. Every story with an API endpoint gets an OpenAPI 3.1 spec. The Control Plane UI service layer must match.
- **Code** — implementations conform to the OpenAPI and SDD. No placeholder production code.
- **Unit tests** — `go test -race -count=1` per package; Vitest for the UI. Each test asserts named behavior, not just that the function returned without error.
- **Verify** — green tests, the 4-question completion self-audit, acceptance criteria met.
- **Ask-about-commit** — never auto-commit. The human confirms.

The full rationale and the 2-round completion self-audit format are in [Dev SDD Pipeline](Dev-SDD-Pipeline).

---

## Pre-commit checks

Run these before pushing. CI runs identical checks on every PR:

| Command | What it enforces |
|---|---|
| `npm run check:i18n` | All user-visible strings go through `t()`; key counts match across EN / ES / ZH |
| `npm run check:design-tokens` | No hex or raw numeric in `*.module.css` or `style={{}}` — CSS variables only |
| `npm run check:terminology` | IoT-internal terms not present on user-facing surfaces |
| `npm run check:doc-lockstep` | Code changes ship with their matching docs in the same commit |
| `npm run check:no-prod-todos` | No `TODO` / `FIXME` / `XXX` / `unimplemented` / `stub` in production code |
| `npm run check:no-yaml-secrets` | No secret fields in committed YAML |
| `npm run check:coverage` | Go package statement coverage ≥95% or explicit allowlist entry |
| `npm run check:migration-timestamps` | No two Prisma migration folders share the same `YYYYMMDDHHMMSS` prefix |

Run all at once with `npm run check:all`. The complete tooling table is in [`docs/developers/workflow/conventions.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/conventions.md) §10.

---

## High-blast-radius surfaces

Some areas require extra care. The full table is in [`CONTRIBUTING.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CONTRIBUTING.md). Two representative entries:

- **`packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/`** — the macOS Network Extension sits in the host's outbound packet path. A bug that hangs or panics here takes down the entire machine's network (DNS, DHCP, VPN, Apple Push). Recovery is manual. The five fail-open rules in `CLAUDE.md` are non-negotiable.

- **Any admin endpoint, sidebar nav item, or route path** — mismatches between the UI's `allowedActions` array and the backend handler's `iamMW(...)` call produce silent 403s. An IAM impact review (5 steps, documented in [`iam-identity-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/control-plane/iam-identity-architecture.md)) is mandatory for every such change, in the same PR.

---

## The AI vibe-coding workbench

This repo ships not just a product but a disciplined methodology for AI-assisted engineering. `CLAUDE.md`, `.cursor/rules/`, `.claude/skills/`, and the `scripts/check-*` lint suite together form a pair-programming practice: the human supplies intent and judgment, the agent supplies execution, and a layered set of guardrails keeps the two converging on production-quality output.

The workbench is documented in detail at [Workbench Overview](Workbench-Overview). As a first-time contributor, the key takeaway is: the binding rules in `CLAUDE.md` are not optional, and they each cite the incident or principle that motivated them.

---

## Getting help

For **bugs**: open a GitHub issue. Include the affected service, the commit SHA or release tag, and a minimal reproduction.

For **design questions**: use GitHub Discussions. If the question touches auth, IAM, data model, or encryption — areas where a wrong call is expensive — reference the relevant architecture doc and proposed decision.

For **security issues**: do not open a public issue. Email [security@alphabitcore.com](mailto:security@alphabitcore.com) or use [GitHub Private Vulnerability Reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability). The full disclosure policy is in [`SECURITY.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/SECURITY.md).

---

## Canonical docs

- [`CONTRIBUTING.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CONTRIBUTING.md) — the procedural checklist this page expands
- [`CLAUDE.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CLAUDE.md) — binding rules: English-only, plan-first, worktree per session, coverage gate, no yaml secrets, lockstep
- [`docs/developers/workflow/ai-workflow.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/ai-workflow.md) — the AI vibe-coding workflow in full
- [`docs/developers/architecture/README.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/README.md) — the 3-doc pre-edit trigger map

**Adjacent wiki pages**: [Dev Repo Structure](Dev-Repo-Structure) · [Dev Local Development](Dev-Local-Development) · [Dev SDD Pipeline](Dev-SDD-Pipeline) · [Workbench Overview](Workbench-Overview) · [Quickstart](Quickstart)
