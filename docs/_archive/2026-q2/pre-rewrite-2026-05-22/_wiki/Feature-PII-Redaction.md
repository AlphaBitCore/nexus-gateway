# Feature PII Redaction

Nexus Gateway applies PII redaction as a defence-in-depth across multiple layers: the hook pipeline's PII Detector scrubs request and response bodies; the audit emit step scrubs known-sensitive header and body fields; the SIEM bridge applies a field denylist at dispatch time. The PII Detector is the primary enforcement layer — it runs in all three traffic paths (AI Gateway, Compliance Proxy, Desktop Agent) using the same Go code driven by a shared `HookConfig`.

---

## What Nexus does

The PII Detector hook scans request and response bodies for personally identifiable information using pattern matching with checksum validation. Built-in categories:

| Category | Detection | Default strategy |
|---|---|---|
| Email address | Regex `[\w.+-]+@[\w-]+\.[\w.-]+` | Token (stable per-tenant opaque ID for analytics) |
| Phone number | Multiple country formats, locale-aware | Mask |
| Credit card | Regex + Luhn validation | Mask, keep last 4 (`****1234`) |
| SSN (US) | `\d{3}-\d{2}-\d{4}` | Mask |
| IBAN | Country-prefix + checksum validation | Mask |
| API keys | `sk-...`, `vk-...`, `pk_live_...` and similar shapes | Mask |
| JWTs | `eyJ\w+\.\w+\.\w+` | Mask |
| Private-key blocks | `-----BEGIN ... PRIVATE KEY-----` | Mask |

Custom categories (beyond the built-in set) are added via Rule Packs in the Control Plane without code changes.

## Three redaction primitives

| Primitive | Output | When to use |
|---|---|---|
| **Mask** | `***` or `<PII redacted>` | When the value should disappear entirely. Irreversible. |
| **Token** | Stable per-tenant opaque string (`tok_abc123`) | When analytics needs "same user" detection without exposing the value. Same input produces the same token within a tenant. |
| **Hash** | SHA-256 (or HMAC-SHA-256 with a per-tenant secret) | When investigators need to verify "is this the same value as another row?" One-way, but preserves duplicate detection. |

## `inflightAction` vs `storageAction`

The hook's `onMatch` config has two independent settings that control enforcement and audit separately:

```yaml
onMatch:
  inflightAction: block-hard | block-soft | redact | approve
  storageAction:  keep | redact | drop-content
  replacement:    "***"
```

This split allows nuanced policies:

- **Block forwarding, keep original in audit** (`inflightAction=block-hard, storageAction=keep`): request is rejected; the full original content is retained in audit for investigation.
- **Allow forwarding, redact in storage** (`inflightAction=approve, storageAction=redact`): request passes through with original content; only the redacted form lands in the audit trail.
- **Redact both** (`inflightAction=redact, storageAction=redact`): the request is forwarded with PII replaced by `replacement`; audit stores the same redacted form.

## Where it sits

- PII Detector hook: `packages/shared/policy/hooks/validators/pii_detector.go`
- Rule pack engine (handles `_rulePackInstalls` config): `packages/shared/policy/hooks/validators/rulepack_engine.go`
- Hook registration: `packages/shared/policy/hooks/builtins/builtins.go`
- Shared `onMatch` types: `packages/shared/policy/hooks/core/types.go`

The PII Detector runs in all three traffic paths because the hook framework runs in all three. The same `HookConfig` shape, delivered via the Hub's config sync, governs behaviour uniformly across AI Gateway, Compliance Proxy, and Desktop Agent.

Field-level scanning targets specific JSON paths (`$.messages[*].content`); body-level scanning catches PII in free-form text where position is unknown. Both run: field-level first, body-level catches anything missed.

## Observability

Every redaction produces observable signal:

- `traffic_event` hook-decision rows carry the matched hook config and outcome (`block-hard | block-soft | redact | approve`), plus `blocking_rule` attribution when a rule pack made the call.
- Counter metric `pii_redactions_total{category, strategy}` tracks frequency per category and strategy.
- Alert rules can fire on unusual redaction-rate spikes — a sudden increase may indicate an upstream system is sending more PII than expected.

There is no separate `traffic_event.redactions_applied` column; the category-level breakdown is reconstructed from the hook-decision rows joined to the PII detector's pattern IDs.

## How to enable and configure

PII Detector hooks are managed from **Compliance → Hooks** in the Control Plane UI:

1. Select **New Hook** and choose **PII Detector**.
2. Set the stage (`request`, `response`, or both), priority, and applicable ingress.
3. Configure `onMatch.inflightAction` and `onMatch.storageAction` per the required policy.
4. To override the default redaction strategy for a specific category (e.g., use `mask` instead of `token` for emails), set `redactStrategy` in the hook's category config.
5. Save. The config propagates to all connected nodes within seconds.

**Compliance presets** for common regulatory requirements:
- **HIPAA**: set `storageAction = drop-content` on all body-capturing categories; no token tables.
- **GDPR**: use `Token` strategy with an optional reversal table for right-to-delete fulfillment; set token-table TTL to match retention policy.
- **PCI-DSS**: credit cards must be masked; the built-in default (mask + keep last 4) satisfies standard requirements.

Per-route exemptions are available for routes that legitimately carry email-shaped IDs, code samples with JWT-like strings, or other false-positive-prone patterns.

---

## Canonical docs

- [`pii-redaction-policy-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/pii-redaction-policy-architecture.md) — three primitives, built-in categories, onMatch config, always-on audit redactions, field-level vs body-level, compliance posture
- [`hook-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/hook-architecture.md) — pipeline aggregation, streaming compliance modes, body capture

**Adjacent wiki pages**: [Feature Hooks Framework](Feature-Hooks-Framework) · [Feature Audit And SIEM](Feature-Audit-And-SIEM) · [Compliance Proxy PII Redaction](Compliance-Proxy-PII-Redaction) · [AI Gateway Hooks](AI-Gateway-Hooks) · [Features Index](Features-Index)
