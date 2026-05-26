# Community Maintainers

Nexus Gateway is maintained by a small core team. Maintainers own one or more areas of the codebase and are the primary reviewers for changes in those areas. This page lists current maintainers and their ownership. The project is open to new contributors — see [Contributing](Contributing) for the path from first contribution to regular committer.

---

## Project lead

| Name | GitHub | Contact | Ownership |
|---|---|---|---|
| Nexus | `nexus` | nexus@alphabitcore.com | Full codebase; final decision authority on architecture, scope, and release |

The project lead holds final authority on all architectural decisions, binding workflow rules (`CLAUDE.md`, `.cursor/rules/`), release timing, and changes to cross-cutting contracts (API surfaces, data models, config keys, IAM policy shapes).

## Core contributors

| Name  | GitHub  | Contact                | Primary areas |
|-------|---------|------------------------|---|
| Nexus | `nexus` | nexus@alphabitcore.com | Architecture direction; code and design reviews across all services |

Nexus co-shaped the system architecture and participated in design reviews and code reviews throughout the project. The Acknowledgments section of [`README.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/README.md) credits his contribution.

## Area ownership

The table below maps high-blast-radius or frequently-changed areas to a primary owner. Changes in these areas should be reviewed by the listed owner, or at minimum the area owner is consulted before merge.

| Area | Owner | Why high-blast-radius |
|---|---|---|
| `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/` | Nexus | A misbehaving Network Extension kills the host's network |
| `packages/shared/` — cross-cutting libraries | Nexus | Used by all 5 services; additive-only once shipped in a released Agent binary |
| Admin IAM policies, NRN model, endpoint registration | Nexus | Mismatched UI/backend IAM actions produce silent 403s |
| `tools/db-migrate/` — Prisma schema + migrations | Nexus | Duplicate migration timestamps cause Prisma to silently skip migrations |
| Token-field stamping in AI Gateway (`packages/ai-gateway/`) | Nexus | Missing cache-side stamp sites NULLs all cache traffic |
| `CLAUDE.md` and `.cursor/rules/` | Nexus | These are the binding rules governing every contributor and AI assistant session |

## Becoming a contributor

Nexus Gateway welcomes contributors. The path:

1. Read [`CONTRIBUTING.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CONTRIBUTING.md) and [`CLAUDE.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CLAUDE.md).
2. Start with an issue or Discussion to align on scope before writing code.
3. Submit a pull request following the mandatory development workflow.
4. Regular, high-quality contributions in a specific area lead naturally to area ownership recognition.

There is no formal CODEOWNERS file yet. As the contributor base grows, area ownership will be formalized. The governance model and this page will be updated when that happens.

---

## Canonical docs

- [`MAINTAINERS.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/MAINTAINERS.md) — **canonical source** for maintainer list and area ownership; this page is kept in sync at release cadence.
- [`CONTRIBUTING.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CONTRIBUTING.md) — contribution workflow
- [`README.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/README.md) — project acknowledgments

**Adjacent wiki pages**: [Community Governance](Community-Governance) · [Community Support Channels](Community-Support-Channels) · [Contributing](Contributing) · [Dev Release Process](Dev-Release-Process)

---

> Canonical source: [`MAINTAINERS.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/MAINTAINERS.md). This page is kept in sync at release cadence.
