# Community Support Channels

Nexus Gateway is an open-source project maintained on GitHub. Support is community-driven — there are no commercial SLAs or paid tiers. The channels below cover the full range of needs: general questions, bug reports, feature requests, and security disclosures. Pick the channel that matches the nature of the request.

---

## GitHub Discussions — questions and ideas

[GitHub Discussions](https://github.com/AlphaBitCore/nexus-gateway/discussions) is the first stop for questions, design conversations, and exploratory ideas.

Use Discussions for:

- "How do I configure X?" or "Why does Y behave like Z?"
- Architecture / design feedback before writing code.
- Sharing what worked (or didn't) when adopting Nexus in a real environment.
- Feature requests and capability proposals (open a Discussion first; a PR without a Discussion for non-trivial features will be asked to follow this path).

There is no response-time guarantee. The maintainer team reads Discussions regularly, and community members often answer faster than maintainers do.

## GitHub Issues — bugs and confirmed defects

[GitHub Issues](https://github.com/AlphaBitCore/nexus-gateway/issues) is for confirmed or strongly suspected bugs in the codebase.

Before opening an issue:

1. Search existing issues and Discussions for the same symptom.
2. Reproduce on the `main` branch if possible.
3. Include the service name (`packages/ai-gateway`, `packages/agent`, etc.), the version or commit SHA, and a minimal reproduction sequence.

Issues without a reproduction path or without the affected component identified may be closed with a request for more detail. Enhancement requests belong in Discussions, not Issues.

## Security vulnerabilities — private email

**Do not report vulnerabilities through public GitHub Issues or Discussions.** Public disclosure before a fix is available harms downstream operators.

Report security issues privately:

- **Email**: `security@alphabitcore.com`
- **GitHub Private Vulnerability Reporting**: enabled on this repository via the [Security Advisories](https://github.com/AlphaBitCore/nexus-gateway/security/advisories) tab.

For the full process — what to include, expected response timelines, coordinated disclosure policy — see [`SECURITY.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/SECURITY.md) and the dedicated wiki page [Security Reporting A Vulnerability](Security-Reporting-A-Vulnerability).

## Code contributions — pull requests

Contributing code starts with reading three documents:

1. [`CLAUDE.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CLAUDE.md) — binding workflow rules (plan, todo, test coverage, IAM impact review, etc.)
2. [`CONTRIBUTING.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CONTRIBUTING.md) — condensed workflow and pre-commit checklist
3. [`docs/developers/architecture/README.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/README.md) — the "what to read when editing X" map

For a step-by-step contributor onboarding path, see [Contributing](Contributing) in this wiki.

## What this project does not provide

- **Real-time chat** (Slack, Discord, IRC) — there is no official project chat room.
- **Support tickets** — no ticketing system or paid support tier.
- **Priority response SLAs** — response times depend on maintainer availability and issue priority.
- **Backports** — the project is pre-GA; fixes target `main` only.

---

## Canonical docs

- [`SECURITY.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/SECURITY.md) — full vulnerability disclosure process and scope
- [`CONTRIBUTING.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CONTRIBUTING.md) — contribution workflow and pre-commit checklist

**Adjacent wiki pages**: [Security Reporting A Vulnerability](Security-Reporting-A-Vulnerability) · [Contributing](Contributing) · [Community Code Of Conduct](Community-Code-Of-Conduct) · [Community Governance](Community-Governance) · [Community Maintainers](Community-Maintainers)
