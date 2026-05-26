# Control Plane Admin UI Tour

*Audience: new admins getting oriented in the Control Plane UI.*

The Control Plane UI is a React + TypeScript single-page application served on `:3000` in local development and at the root of the production domain. It is organized into top-level sidebar sections, each gating its routes by IAM action. The sections below correspond to the nav items an authenticated admin sees; actual visibility depends on which actions the user's IAM groups grant.

---

## Overview section

The Overview section (`/`) is the day-one landing for traffic and analytics surfaces.

| Page | Path | What it shows |
|---|---|---|
| Dashboard | `/` | Top-level health + activity summary; entry point to the surfaces below |
| Traffic | `/traffic` | Real-time and historical traffic events with filters; body drill-down; hook decisions; routing trace |
| Analytics | `/analytics` | Aggregate views: volume, latency, top providers/models, error rates, cost trend |
| Metrics Explorer | `/metrics` | Prometheus-backed metric explorer for ad-hoc dashboards |
| Quota Usage | `/quota-usage` | Current usage vs limit per quota policy + projected exhaustion |
| Cache ROI | `/cache-roi` | Prompt-cache hit-rate by tier, cost-saved estimates |

Common workflows: investigate a single request by filtering Traffic by VK or `request_id` → drill into the row to view the body, hook trace, and routing trace. Spot a cost spike on Analytics, then cross to Quota Usage to see if a quota is being consumed.

## AI Gateway section

The AI Gateway section manages provider configuration, virtual keys, routing, and compliance hooks.

| Page | Path | What it shows |
|---|---|---|
| Providers | `/ai-gateway/providers` | Provider catalog and credential management |
| Models | `/ai-gateway/models` | Model catalog with capability flags and per-model price rows |
| Virtual Keys | `/ai-gateway/virtual-keys` | VK creation, scopes, expiry, org/project association |
| Routing Rules | `/ai-gateway/routing` | Declarative match → resolved-model rules; ordering; simulation |
| Cache | `/ai-gateway/cache` | Fleet-wide prompt-cache and response-cache configuration |
| Quotas | `/ai-gateway/quotas` | Quota policies at org and project scope |
| Hooks | `/ai-gateway/hooks` | 3-stage hook pipeline configuration |
| Rule Packs | `/ai-gateway/rule-packs` | Compliance rule packs (import, activate, deactivate) |
| Passthrough | `/ai-gateway/passthrough` | Emergency bypass configuration; requires `admin:passthrough.emergency-enable` |

## IAM section

The IAM section controls who can do what. Sidebar labels "Roles" but the underlying resource is IAM group.

| Page | Path | What it shows |
|---|---|---|
| Organizations | `/iam/organizations` | Tenant tree: create, rename, move, soft-delete |
| Projects | `/iam/projects` | Project CRUD within orgs |
| Users | `/iam/users` | Local + IdP-federated users; suspend |
| Roles | `/iam/roles` | IAM groups (bundle policies + users) |
| Policies | `/iam/policies` | AWS-IAM-shaped policy documents |
| Simulator | `/iam/simulator` | "Would this principal be allowed this action on this resource?" |
| Identity Providers | `/iam/identity-providers` | External IdP configurations (Okta, Azure AD, OIDC; SAML planned) |

## Compliance section

The Compliance section manages exemptions, DSAR requests, and the AI guard configuration.

## Alerts section

| Page | Path | What it shows |
|---|---|---|
| Alerts Inbox | `/alerts` | Currently-firing and recently-resolved alerts; ack, mute, snooze |
| Rules | `/alerts/rules` | DB-managed `AlertRule` CRUD paired with Go built-in rules |
| Channels | `/alerts/channels` | Webhook, SIEM, and email destinations; per-channel severity filter; test |

Triage a firing alert by expanding the row to see labels (provider, model, org) and sample `request_id`s, then click through to the unified audit timeline.

## Infrastructure section

The Infrastructure section is the operational console for the fleet itself. It is covered in depth in [Control Plane Infrastructure Pages](Control-Plane-Infrastructure-Pages).

| Page | Path | What it shows |
|---|---|---|
| Nodes | `/infrastructure/nodes` | All registered services and agents; status, version |
| Config Sync | `/infrastructure/config-sync` | Per-service desired vs applied config; drift surface |
| Jobs | `/infrastructure/jobs` | Scheduled job catalogue; last-run; duration |
| Kill Switch | `/infrastructure/kill-switch` | Three-tier emergency passthrough; auto-revert status |
| SIEM | `/infrastructure/siem` | SIEM bridge channels and recent forwards |
| Observability Config | `/infrastructure/observability-config` | Metrics and tracing configuration |

## IAM action wiring rule

Every sidebar page declares `allowedActions` in `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`. The backend handler for each page must check exactly the same action string via `iamMW(...)`. Mismatches produce silent 403s on the API call even though the menu item renders. The binding IAM impact review in [Control Plane IAM Model](Control-Plane-IAM-Model) governs any add/move/rename of a route.

---

## Canonical docs

- [`overview.md` (cp-ui features)](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/cp-ui/overview.md) — Overview section pages and workflows
- [`iam.md` (cp-ui features)](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/cp-ui/iam.md) — IAM section pages, seeded managed policies
- [`infrastructure.md` (cp-ui features)](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/cp-ui/infrastructure.md) — Infrastructure section pages
- [`alerts.md` (cp-ui features)](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/cp-ui/alerts.md) — Alerts section pages

**Adjacent wiki pages**: [Control Plane Overview](Control-Plane-Overview) · [Control Plane IAM Model](Control-Plane-IAM-Model) · [Control Plane Authentication](Control-Plane-Authentication) · [Control Plane Infrastructure Pages](Control-Plane-Infrastructure-Pages) · [Control Plane Alerting Rules](Control-Plane-Alerting-Rules)
