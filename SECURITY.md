# Security Policy

We take the security of Nexus Gateway seriously. This document describes how to
report vulnerabilities and what to expect from us in return.

## Reporting a Vulnerability

**Please do not report security vulnerabilities through public GitHub issues,
discussions, or pull requests.**

Report security issues privately by emailing:

> **security@alphabitcore.com**

If you prefer encrypted communication, request our PGP public key at the same
address before sending sensitive details.

Alternatively, use [GitHub Private Vulnerability Reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
on this repository.

### What to include

To help us triage and reproduce the issue quickly, include:

- A clear description of the vulnerability and its impact.
- The affected component (e.g. `packages/ai-gateway`, `packages/agent`,
  `packages/compliance-proxy`, `packages/nexus-hub`, `packages/control-plane`,
  `packages/control-plane-ui`).
- The version, commit SHA, or release tag where you observed the issue.
- A minimal proof of concept or reproduction steps.
- Any logs, network traces, or screenshots that demonstrate the problem.
- Your contact details and whether you would like to be credited in the fix.

## What to expect

| Stage | Target time |
|---|---|
| Initial acknowledgement | within 3 business days |
| Triage & severity assessment | within 7 business days |
| Status update cadence during investigation | every 7 business days |
| Coordinated disclosure window after a fix is ready | 90 days (negotiable) |

We follow a coordinated disclosure model: we will work with you on a release
schedule that gives downstream operators time to patch before details become
public. If we cannot meet a timeline you propose, we will say so explicitly and
explain why.

## Scope

In scope:

- Code under `packages/` in this repository.
- Default configuration shipped in `docker-compose.yml`, `scripts/`,
  `tools/db-migrate/seed/`, and the documented local-dev bootstrap.
- Build, signing, and notarization tooling under `packages/agent/`.

Out of scope:

- Third-party services (OpenAI, Anthropic, Google, etc.) — report those
  directly to the upstream vendor.
- Bugs requiring an attacker with administrative privileges on the host
  running Nexus Gateway (operator security is your responsibility).
- Social engineering or attacks against contributors / maintainers.
- Denial-of-service caused by exhausting resources you control (e.g. running
  every component on a single t2.micro).

## Disclosure policy

Once a fix is released, we publish an advisory describing the vulnerability,
its impact, affected versions, and the fix. Reporters who request credit are
named in the advisory and the release notes. Reporters who request anonymity
are not.

We do not operate a paid bug-bounty program at this time.

## Supported versions

This project is in active development. Security fixes target the `main` branch
and the latest tagged release. Older releases are best-effort.
