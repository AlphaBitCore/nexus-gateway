# Compliance Proxy PII Redaction

*Audience: compliance leads and operators configuring PII policies on the Compliance Proxy path.*

PII redaction in Nexus Gateway is a layered defence: the hook pipeline scrubs request and response bodies before forwarding and before storing; the audit emit step scrubs known-sensitive HTTP headers; and SIEM forwarding applies a configurable denylist at marshal time. The Compliance Proxy participates in all three layers. The primary surface for operators is the hook pipeline — admins configure a PII detector hook per interception domain, choosing which categories to detect, which redaction strategy to apply (mask, token, or hash), and what to do on a match (redact in flight, redact in storage, block the request). Per-route policy overrides are available for routes that require stricter or more lenient treatment than the default.

---

## Three redaction primitives

| Primitive | Output example | Use case |
|---|---|---|
| **Mask** | `<PII redacted>` or `***` | Maximum protection; value is unrecoverable |
| **Token** | `tok_abc123` (stable per-tenant opaque ID) | Analytics that needs to detect repeated occurrences without seeing the value |
| **Hash** | `sha256:a4f2e...` (HMAC-SHA-256 with per-tenant secret) | Investigators need to verify "same value as another row?" without seeing the value |

Hash uses a per-tenant HMAC secret (rotated periodically). Token uses an optional tenant-controlled reversal table (stored encrypted; default: OFF). Mask is irreversible.

## Built-in PII categories

The PII detector hook ships with the following built-in categories:

| Category | Detection method | Default strategy |
|---|---|---|
| Email address | Regex `[\w.+-]+@[\w-]+\.[\w.-]+` | Token (analytics often needs "same user" detection) |
| Phone number | Multi-country locale-aware regex | Mask |
| Credit card | Regex + Luhn validation | Mask + keep last 4 (`****1234`, PCI convention) |
| SSN (US) | Regex `\d{3}-\d{2}-\d{4}` | Mask |
| IBAN | Country-prefix + checksum validation | Mask |
| API key shapes | `sk-...`, `vk-...`, `pk_live_...` | Mask |
| JWT | `eyJ\w+\.\w+\.\w+` | Mask |
| Private key block | `-----BEGIN ... PRIVATE KEY-----` | Mask |

Custom categories can be added via Rule Packs in the Control Plane UI. Each custom category is a regex plus an optional validator function and a default strategy.

The detector uses regexes tuned for low false negatives (prefer masking a non-PII value over leaking a real one). Luhn validation eliminates most pure-numeric false positives for credit cards; IBAN checksum validation catches most length-only matches. Random base64-looking strings may occasionally trigger JWT detection.

## Redaction strategies and on-match actions

`HookConfig.onMatch` has two independent axes: what happens to the in-flight request/response (forwarding action) and what happens to the stored audit copy:

| `inflightAction` | Effect on forwarding |
|---|---|
| `redact` | Replace matched PII in the body before forwarding upstream (request stage) or before relaying to client (response stage) |
| `approve` | Forward without modification; PII reaches the upstream |
| `block-soft` | Return HTTP 451; log the block; alert |
| `block-hard` | Return HTTP 451; log the block; do not alert (silent reject) |

| `storageAction` | Effect on audit copy |
|---|---|
| `redact` | Replace matched PII in the `traffic_event_normalized` stored payload |
| `approve` | Store the raw extracted text (PII visible to investigators) |

The two can differ: `inflightAction=approve, storageAction=redact` passes the request through unchanged while storing a cleaned audit copy. `inflightAction=redact, storageAction=redact` cleans both.

## Where redaction runs in the pipeline

Redaction happens before forwarding and before storage — not after:

```
Extract content (text-first normalizer)
    ↓
Request-stage hook pipeline
    ├─ PII detector hook
    │   ├─ Field-level scan: check specific JSON fields by path
    │   └─ Body-level scan: regex over full extracted text
    │      → match → apply inflightAction
    ↓
Upstream forwarding (with possibly-redacted body)
    ↓
Response-stage hook pipeline
    ├─ PII detector hook (on response body)
    ↓
Relay to client (with possibly-redacted response body)
    ↓
Audit emission (storageAction applied)
    ↓
traffic_event_normalized written to DB (audit copy)
```

Field-level scan runs first: it checks specific JSON paths (`$.messages[*].content`, etc.) for structured requests where PII position is known. Body-level scan catches unstructured text and anything the field scan missed.

## Always-on audit redactions

Regardless of hook configuration, every audit emit path scrubs:

- `Authorization` header value → `Bearer [redacted]`
- `Cookie` header value → `[redacted]`
- `X-API-Key`, `X-Auth-Token`, `Api-Key` → `[redacted]`
- Request body fields named: `password`, `api_key`, `client_secret`, `refresh_token`, `access_token`, `private_key`, `secret`

These scrubs apply before any PII hook runs and before any `traffic_event` row is written. They protect against accidentally logging provider API keys, session cookies, and OAuth tokens that appear in the raw HTTP exchange.

## Per-route policy overrides

The default PII policy applies to the whole proxy instance. Admins can override it per interception domain via `HookConfig.onMatch.redactStrategy`:

- Set `strategy=mask` for a route that carries medical data (stricter than the default).
- Set `strategy=approve` for a route that legitimately carries email-shaped IDs that are not PII (e.g., a developer-tool route where content is code, not user data).
- Add per-route exemptions for specific pattern IDs if the default detector generates too many false positives for a particular host.

## Observability of redactions

Every redaction produces observable signal:

- `hook_decision` column on `traffic_event` carries the matched hook config ID and outcome (`redact`, `block-hard`, `block-soft`, `approve`).
- `pii_redactions_total{category, strategy}` Prometheus counter increments per match.
- `response_redaction_spans` JSONB column on `traffic_event_normalized` records each applied redaction span with `(rule_id, start, end, replacement)`.

Operators can query for unusual redaction spikes with:

```sql
SELECT
  date_trunc('hour', timestamp) AS hour,
  count(*) AS redaction_events
FROM traffic_event
WHERE source = 'compliance-proxy'
  AND response_hook_decision = 'redact'
  AND timestamp >= now() - interval '24 hours'
GROUP BY 1 ORDER BY 1;
```

Alerting rules that fire on unusual `pii_redactions_total` rate spikes are available via the built-in alert catalog.

## Compliance posture by jurisdiction

| Regulation | Recommended configuration |
|---|---|
| HIPAA | `inflightAction=redact` + `storageAction=redact`; `strategy=mask` for all categories; custom detector for PHI patterns |
| GDPR | `strategy=token` for email (supports right-to-delete via token table purge); `block-soft` on private-key patterns |
| PCI-DSS | Credit card must use `strategy=mask` with last-4 keep; `inflightAction=redact` to prevent PAN reaching log infrastructure |

---

## Canonical docs

- [`pii-redaction-policy-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/pii-redaction-policy-architecture.md) — three primitives, built-in categories, always-on denylist, compliance posture
- [`compliance-pipeline-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/compliance-proxy/compliance-pipeline-architecture.md) — §7 (hook pipeline invocation)

**Adjacent wiki pages**: [Compliance Proxy Overview](Compliance-Proxy-Overview) · [AI Gateway Hooks](AI-Gateway-Hooks) · [Feature PII Redaction](Feature-PII-Redaction) · [Compliance Proxy Normalization](Compliance-Proxy-Normalization)
