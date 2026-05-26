# Compliance section — CP-UI feature doc

> Audience: compliance officers and security admins. This section configures hook policies, rule packs, interception domains, and exemptions.

## Pages in this section

Routes sourced from `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`
(sectionKey `compliance`, orders 0-9).

| Order | Page | Path | IAM action | Purpose |
|---|---|---|---|---|
| 0 | Compliance Overview | `/compliance/overview` | `admin:compliance-report.read` | Rejection rate trends, classification breakdown, top blocking rules. |
| 1 | Hooks & Policies | `/compliance/hooks` | `admin:hook.read` | HookConfig CRUD: enable/disable, scope, `onMatch.action`, `applicableIngress`. |
| 2 | Rule Packs | `/compliance/rule-packs` | `admin:rule-pack.read` | Versioned bundles of pattern rules reusable across hooks. |
| 3 | Interception Domains | `/compliance/interception-domains` | `admin:interception-domain.read` | Per-domain interception ruleset (compliance proxy + agent). |
| 4 | Exemptions | `/compliance/exemptions` | `admin:compliance-exemption.read` | Domain / device / org-scoped exemptions (manual + auto from pinning detector). |
| 5 | AI Guard Backend | `/compliance/ai-guard` | `admin:ai-guard-config.read` | Configure the upstream AI-guard scoring backend used by `content-safety` and `quality-checker`. |
| 6 | Streaming Compliance | `/compliance/streaming` | `admin:settings.read` | Streaming-mode policy: `chunked_async` vs `buffer_full_block`, per-route streaming caps. |
| 7 | Payload Capture | `/compliance/payload-capture` | `admin:payload-capture.read` | Fleet-wide opt-in for storing request/response bytes (ai-gateway + compliance-proxy + agent). Privacy / retention gate. |
| 8 | Audit Logs | `/compliance/audit-logs` | `admin:audit-log.read` | Admin operation audit trail (`AdminAuditLog`) — who changed what, when. |
| 9 | DSAR | `/compliance/dsar` | `admin:dsar.read` | Data Subject Access Request intake + export workflow. |
| — | Compliance Report | `/compliance/compliance-report` | `admin:compliance-report.read` | Per-period compliance reports (no nav entry; deep-linked from Overview). |

## Common workflows

- **Enable a built-in hook** — Hooks → toggle on → select scope (org / project / route) → set `applicableIngress: [aiGateway, complianceProxy, agent]` → confirm. Hub propagates via change-signal; data-plane services hot-swap. Cross-ref `hook-architecture.md`.
- **Tune `onMatch` action** — Hooks → select hook → edit `onMatch.action` (`block-hard`, `block-soft`, `redact`, `flag`, `log-only`). **Validate against expected semantics** — the PII scanner saga (2026-05-13) shipped `block-hard` instead of `redact` and silently broke flows.
- **Author a rule pack** — Rule Packs → new → define pattern rules → assign to one or more hooks. Agent consumes installed rule packs via the `installed_rule_packs` Cat B shadow key (see `agent-policy-eval-architecture.md` §5).
- **Add an interception domain** — Interception Domains → new → glob pattern + traffic adapter → optional path filter → save. Compliance proxy + agent pull the new ruleset on change-signal.
- **Manage exemptions** — Exemptions surface mixes manual exemptions (admin entered) and auto exemptions (compliance proxy pinning detector adds). Investigators see who created what. Agent consumes exemptions locally via the `agent_exempt_hosts` Cat B shadow key (see `agent-exemption-grants-architecture.md` §5).

## Key API endpoints

```
/api/admin/compliance/overview      [GET]
/api/admin/hooks                    [GET/POST/PUT/DELETE]
/api/admin/rule-packs               [GET/POST/PUT/DELETE]
/api/admin/interception-domains     [GET/POST/PUT/DELETE]
/api/admin/exemptions               [GET/POST/PUT/DELETE]
```

## Failure modes & gotchas

- **Agent enforcement coverage** — agent runs hooks locally and consumes both exemptions and rule packs (see `agent-policy-eval-architecture.md` §5 and `agent-exemption-grants-architecture.md` §5). The `auto_exempt_cert_pinned` knob is parsed but not yet consumed at runtime — auto-exemption today comes from the TLS handshake-failure tracker, not from a cert-pin signal off the wire. Cross-ref `agent-forwarder-architecture.md` §7.
- **`block-hard` blast radius** — a misconfigured hook with `onMatch.action: block-hard` and a broad `match` filter will reject substantial traffic. Recommend staging on a single project first.
- **Streaming compliance mode mismatch** — a hook configured for `chunked_async` cannot stop already-sent bytes; admins sometimes confuse this with `buffer_full_block` blocking ability. UI explains the trade-off inline.
- **HookConfig migration sensitivity** — adding a new field to the canonical `onMatch` schema requires migration + Hub change-signal; cross-ref `thing-config-sync-architecture.md` §7 (three-path audit).

## Architecture references

- `docs/developers/architecture/services/ai-gateway/hook-architecture.md` — hook framework + onMatch semantics
- `docs/developers/architecture/services/compliance-proxy/compliance-pipeline-architecture.md` — compliance-proxy invocation
- `docs/developers/architecture/services/agent/agent-forwarder-architecture.md` — agent invocation + reality gap
- `docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md` — hook decisions land in traffic_event
