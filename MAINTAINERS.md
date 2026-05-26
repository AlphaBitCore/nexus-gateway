# Maintainers

This file is the canonical list of people with merge authority and the areas they own. It is the single source of truth for maintainer status. The [`docs/_wiki/Community-Maintainers.md`](docs/_wiki/Community-Maintainers.md) page mirrors this list and is kept in sync at release cadence.

---

## Project lead

**Nexus** `<nexus@alphabitcore.com>` — project lead, all areas.

Holds final decision authority on architecture boundaries, API contracts, data models, binding workflow rules (`CLAUDE.md`, `.cursor/rules/`), release timing, and changes to cross-cutting contracts (IAM policy shapes, config key catalog, service topology).

---

## Core maintainers

| Name  | Area(s) of ownership | Contact                  |
|-------|---|--------------------------|
| Nexus | Control Plane UI (React/TypeScript/Vite); frontend architecture, i18n, design-token system, API service layer; code and design reviews across all services | `nexus@alphabitcore.com` |

---

## How to become a maintainer

Nexus Gateway uses a project-lead governance model. A single project lead makes final decisions on architecture, scope, and release timing, with input from maintainers and the broader community.

The path to maintainer status is through sustained, high-quality contribution to a specific area: participating in code reviews, following the mandatory development workflow (Plan → SDD → OpenAPI → Code → Tests), keeping documentation in lockstep with code changes, and demonstrating familiarity with the binding rules in `CLAUDE.md`. There is no minimum commit count; quality and consistency matter more than volume.

When a contributor has established ownership of an area — meaning they are the person others naturally turn to for review on that surface — an existing maintainer may nominate them. Nominations are discussed openly in GitHub Discussions. The project lead approves all additions and removals. Any change to this file is made via pull request.

---

## Communication

- GitHub Issues for bugs and concrete change requests.
- GitHub Discussions for design questions, feature proposals, and roadmap input.
- Security disclosures per [`SECURITY.md`](SECURITY.md) — do not file security issues publicly.
- See [`docs/_wiki/Community-Support-Channels.md`](docs/_wiki/Community-Support-Channels.md) for the full channel list.

---

## Updating this file

This file is the source of truth for maintainers. Update via pull request; the project lead approves additions and removals. The `docs/_wiki/Community-Maintainers.md` page is kept in sync at release cadence.
