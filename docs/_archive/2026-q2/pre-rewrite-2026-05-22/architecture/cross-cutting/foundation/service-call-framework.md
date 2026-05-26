---
doc: service-call-framework
area: cross-cutting
service: foundation
tier: 1
updated: 2026-05-20
---

# Service Call Framework — Platform Architecture Index

> **Tier 1 architecture doc.** Canonical entry index into the per-area architecture docs. Read this when you need a one-page roadmap to the whole platform; jump from here into the focused docs for depth.

Every section below points to the live doc that owns that area today.

---

## Origin

This file dates from 2026-04-17 as the unified design proposal: "Thing Model, Device Shadow, Messaging, Config Sync, Dashboard UI, Migration Plan." The proposal was approved and implemented across Phases 0-4 of the platform overhaul. The migration shipped; the design is now reality.

Rather than maintain a 2000-line document that duplicates content now living in focused per-area docs, this file became the **index** in the docs+governance refresh (2026-05-16). Each section below is a one-line summary + a pointer to the canonical doc.

## Section index

### 1. Overview

The five-service split, Hub-centric Thing model, three traffic paths (AI Gateway / Compliance Proxy / Agent), control plane vs data plane separation.

**Live doc:** [`architecture.md`](../../../../../docs/users/product/architecture.md) — the system-level mental model.

### 2. Decisions (architectural intent)

The "why we picked Hub-centric Thing model over Redis pub/sub" decision record. Captured inline in the relevant per-area docs as context.

**Live record:** binding rule `.cursor/rules/redis-cache-only.mdc` + `cache-multi-tier-architecture.md` §8 "deletion artefact".

### 3. Communication Channels

Three transports: Hub WebSocket (config change-signal + heartbeat), HTTP (CRUD + body overflow presign + audit fallback), NATS JetStream (bulk events).

**Live docs:**
- WS / HTTP control-plane channel: [`thing-config-sync-architecture.md`](thing-config-sync-architecture.md) §4 (change-signal) + §5 (apply path)
- MQ: [`mq-architecture.md`](mq-architecture.md)
- Trace correlation: [`trace-id-propagation-architecture.md`](../../../../../docs/developers/architecture/cross-cutting/observability/trace-id-propagation-architecture.md)

### 4. Thing Model

The `thing` / `thing_service` / `thing_agent` data model, status enum, ID generation, terminology boundary, RBAC integration.

**Live doc:** [`thing-model.md`](thing-model.md). The terminology boundary (Thing / Shadow internal-only) is enforced via [`scripts/check-terminology.sh`](../../../../../scripts/check-terminology.sh).

### 5. Thing Protocol

Shadow desired/reported, Cat A/B/C key classification, change-signal-then-pull flow, OnConfigChanged callback contract, heartbeat + report cadence.

**Live doc:** [`thing-config-sync-architecture.md`](thing-config-sync-architecture.md).

### 6. MQ Abstraction

`shared/mq` interface, NATS JetStream as the default driver, stream layout + subject taxonomy, dual-write dedup, MQ-vs-HTTP/WS decision rule.

**Live doc:** [`mq-architecture.md`](mq-architecture.md).

### 7. Config Change Flow

End-to-end: admin → CP HTTP → Hub HTTP → shadow write → change-signal → Thing pull → apply → reported.

**Live docs:**
- Mechanics: [`thing-config-sync-architecture.md`](thing-config-sync-architecture.md) §4
- Cross-service golden flows (incl. routing rule + hook + kill switch): [`multi-endpoint-coordination-architecture.md`](multi-endpoint-coordination-architecture.md) §2-§4

### 8. Hub API Surface

Hub HTTP endpoints (Thing CRUD, shadow CRUD, audit upload, presign), WebSocket protocol, mTLS auth.

**Live docs:**
- Enrollment + mTLS: [`agent-enrollment-architecture.md`](../../../../../docs/developers/architecture/services/agent/agent-enrollment-architecture.md)
- Audit upload: [`audit-pipeline-architecture.md`](../../../../../docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md)
- Spillstore presign: [`spillstore-architecture.md`](../../../../../docs/developers/architecture/cross-cutting/storage/spillstore-architecture.md)

### 9. Hub Responsibility Split

What Hub owns (Thing registry, shadow, scheduler, alert evaluator, audit sink) vs what CP owns (admin API surface, IAM evaluator, OAuth+PKCE AS).

**Live docs:**
- High-level split: [`architecture.md`](../../../../../docs/users/product/architecture.md) — see "System Context" and "Three-Layer Architecture" sections
- IAM: [`iam-identity-architecture.md`](../../../../../docs/developers/architecture/services/control-plane/iam-identity-architecture.md)
- Auth surfaces: [`oauth-pkce-admin-auth-architecture.md`](../../../../../docs/developers/architecture/services/control-plane/oauth-pkce-admin-auth-architecture.md) + [`idp-sso-architecture.md`](../../../../../docs/developers/architecture/services/control-plane/idp-sso-architecture.md)
- Jobs: [`jobs-architecture.md`](jobs-architecture.md)
- Alerts: [`alerting-architecture.md`](../../../../../docs/developers/architecture/cross-cutting/observability/alerting-architecture.md)

### 10. Dashboard UI Structure

Sidebar IA, route-to-IAM-action mapping, design-token + i18n + useApi bindings.

**Live docs:**
- Sidebar IA: [`sidebar-ia-architecture.md`](../../../../../docs/developers/architecture/cross-cutting/ui/sidebar-ia-architecture.md)
- Design tokens: [`design-tokens-architecture.md`](../../../../../docs/developers/architecture/cross-cutting/ui/design-tokens-architecture.md)
- i18n: [`i18n-pipeline-architecture.md`](../../../../../docs/developers/architecture/cross-cutting/ui/i18n-pipeline-architecture.md)
- useApi + QueryClient: [`useapi-queryclient-architecture.md`](../../../../../docs/developers/architecture/cross-cutting/ui/useapi-queryclient-architecture.md)
- Feature surfaces per section: [`docs/users/features/cp-ui/`](../../../../users/features/cp-ui/)

### 11. Migration Plan

The phased rollout that delivered the Hub-centric model. **Migration is complete.** Historical phases archived; no further migration work scheduled.

**Live record:** Historical phases preserved in git history (`git log --all --follow -- docs/developers/architecture/ docs/developers/workflow/ | head -50` for the migration commits). Archaeology lives in git.

### 12. File Map

The original "what code lives where" map. Now reflected in:

- [`README.md`](../../README.md) — onboarding entry + the canonical doc index by tier
- [`shared-package-architecture.md`](../../../../../docs/developers/architecture/cross-cutting/shared/shared-package-architecture.md) — `packages/shared/*` subpackages
- [`project-structure.md`](../../../../../docs/developers/architecture/project-structure.md) — top-level repo layout

---

## Why this file isn't deleted

The trigger map (`architecture-doc-triggers.md`) references this file as the entry point for "end-to-end flows across services" alongside `multi-endpoint-coordination-architecture.md`. Removing it would break the trigger map; keeping it as a thin index preserves the cross-reference and gives a one-page roadmap for new contributors.

When making changes that span multiple per-area docs (cross-cutting refactors), update this index alongside the per-area docs so the high-level pointer stays accurate.

## Cross-references

The full architecture doc set is indexed in:

- [`README.md`](../../README.md) — by tier + audience.
- [`README.md`](../../README.md) — by edit area (the "what to read when editing X" map; this IS the trigger map).
- [`architecture.md`](../../../../../docs/users/product/architecture.md) — the system-level mental model.
