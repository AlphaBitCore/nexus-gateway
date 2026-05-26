# Nexus Gateway docs

## I want to…

| Goal | Start here |
|---|---|
| **Use the gateway** (admin, integrator, end-user) | [`users/`](./users/) — product overview, UI features, API references |
| **Understand how it works** | [`developers/architecture/`](./developers/architecture/README.md) — the trigger map indexes every architecture doc by editing area |
| **Build a feature** | [`developers/workflow/ai-workflow.md`](./developers/workflow/ai-workflow.md) — the SDD pipeline + AI vibe-coding workbench |
| **Read formal specs** (requirements + SDDs) | [`developers/specs/`](./developers/specs/README.md) — bundled per epic |
| **Operate in production** | [`operators/ops/`](./operators/ops/) — deployment, runbooks, monitoring |
| **Dig through history** | [`_archive/`](./_archive/README.md) — design explorations, program plans, handoffs |

## Top-level layout

```
docs/
├── README.md             ← you are here (navigation hub)
├── users/                ← what end-users (admins, integrators) need
│   ├── product/          ← overview, features, deployment models
│   ├── features/         ← per-UI-section + cross-feature flow docs
│   └── api/              ← public + admin API references (incl. OpenAPI)
├── developers/           ← what contributors need
│   ├── architecture/     ← system architecture (services + cross-cutting)
│   │   ├── README.md     ← the trigger map (binding: read this before code edits)
│   │   ├── services/     ← per-service: agent, ai-gateway, compliance-proxy, …
│   │   └── cross-cutting/← foundation, storage, safety, observability, shared, ui
│   ├── workflow/         ← AI vibe-coding workbench: SDD pipeline, conventions, handoff template
│   ├── specs/            ← per-epic spec bundles (requirements + SDDs)
│   └── roadmap.md        ← in-flight + queued maintainer roadmap
├── operators/            ← what SREs running prod need
│   └── ops/              ← deployment, runbooks, monitoring
└── _archive/             ← historical artifacts (frozen by quarter)
    └── 2026-q2/          ← brainstorms, programs, handoffs
```

## What makes this repo unusual

This repo doubles as an **AI vibe-coding workbench**. The
[`CLAUDE.md`](../CLAUDE.md) rule set, [`.cursor/rules/`](../.cursor/rules/),
[`.claude/skills/`](../.claude/skills/), and [`scripts/check-*`](../scripts/)
lint suite together form a disciplined AI pair-programming practice — every
binding rule cites the incident or principle that motivated it, every lint
is wired into both pre-commit and CI, and the workflow doc above explains
how a fork can adopt the pattern.

If you're here for the AI-driven engineering practices, start at
**[`developers/workflow/ai-workflow.md`](./developers/workflow/ai-workflow.md)**.

## Conventions

- **English-only.** All repo text is English (`CLAUDE.md` → English-only rule).
- **Docs follow the SDD pipeline.** Architecture → requirements → SDD → OpenAPI → code → tests. See [`developers/workflow/ai-workflow.md`](./developers/workflow/ai-workflow.md).
- **Architecture-doc triggers are binding.** [`developers/architecture/README.md`](./developers/architecture/README.md) lists "if you touch X, read Y first." Enforced by `npm run check:arch-doc-triggers`.
- **3-doc pre-edit rule.** Architecture doc + feature doc + conventions — read all three before code edits. See `CLAUDE.md` → "Pre-edit reading".

## Authoritative documents (read these for binding rules)

1. [`CLAUDE.md`](../CLAUDE.md) — binding charter
2. [`CONTRIBUTING.md`](../CONTRIBUTING.md) — contribution workflow
3. [`developers/architecture/README.md`](./developers/architecture/README.md) — what to read when editing X
4. [`developers/workflow/ai-workflow.md`](./developers/workflow/ai-workflow.md) — the AI vibe-coding workflow itself
