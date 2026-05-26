# Feature Hooks Framework

The Hooks Framework is the cross-cutting enforcement mechanism in Nexus Gateway. The same Go code runs in all three traffic paths — AI Gateway, Compliance Proxy, and Desktop Agent — driven by the same `HookConfig` shape delivered via the Hub's config sync. A hook receives a canonicalised view of the request or response (not raw provider JSON), returns a verdict, and optionally modifies the body. Decisions from multiple hooks are aggregated per stage before forwarding proceeds.

---

## What Nexus does

### The three stages

Every traffic interaction flows through up to three hook stages:

| Stage | Runs at | Input |
|---|---|---|
| `request` | Before forwarding to the upstream provider | Canonical request: normalised messages, tools, metadata |
| `response` | After receiving the upstream response (or per chunk during streaming) | Canonical response: normalised completion, usage |
| `connection` | Once per upstream connection, before any request flows | TLS context: SNI, client-cert fingerprint, target host; no body |

Hooks never receive raw provider JSON. The `HookInput` carries the `NormalizedPayload` produced by `shared/transport/normalize` — the same provider-agnostic structure used by routing and cost tracking. Hooks added to one traffic path apply consistently across all paths via the shared `HookConfig`.

### Verdict vocabulary

Each hook returns one verdict:

| Verdict | Effect |
|---|---|
| `block-hard` | Pipeline short-circuits. Client receives HTTP 451 (request stage) or a stream termination (response stage). |
| `block-soft` | Pipeline continues. The soft reject is recorded in the audit trail. Upstream call still proceeds. Used for "alert but don't block" policies. |
| `redact` | Body is modified in-flight (matched spans replaced by `replacement`). Request continues with redacted content. |
| `approve` | Request passes through unchanged. |
| `abstain` | Hook has no opinion. Pipeline continues. |

### `inflightAction` vs `storageAction`

The hook's `onMatch` config has two independent settings:

```yaml
onMatch:
  inflightAction: block-hard | block-soft | redact | approve
  storageAction:  keep | redact | drop-content
  replacement:    "***"
```

This split allows policies like "block the request but retain the original in audit for investigation" (`inflightAction=block-hard, storageAction=keep`), or "allow forwarding but store only the redacted form" (`inflightAction=approve, storageAction=redact`).

### Pipeline aggregation

Multiple hooks run in priority order. The aggregated outcome:
- If any hook returns `block-hard`: final decision is `block-hard`; subsequent hooks are skipped; attribution points to the first hard reject.
- If no hard reject but any hook returns `block-soft`: final decision is `block-soft`; all hooks ran; attribution points to the first soft reject.
- Otherwise: final decision is `approve`.

Body modifications from `redact`-verdict hooks are composed and applied together. The final decision plus `blocking_rule` attribution is stamped on the traffic event.

## Built-in hooks

| Hook | Description | Available paths |
|---|---|---|
| PII Detector | Pattern + checksum detection for emails, phones, credit cards, SSNs, IBANs, API keys, JWTs, private keys | All paths |
| Keyword Filter | Configurable exact-match and regex patterns | All paths |
| Content Safety | Policy-based safety evaluation | All paths |
| Rate Limiter | Per-source / per-VK / per-org limits (Redis or local counters) | All paths |
| Request Size Validator | Body-size limits | All paths |
| IP Access Filter | Source IP allowlist / denylist | All paths |
| Webhook Forward | Forward to admin-configured webhook for custom evaluation | AI Gateway only |
| Quality Checker | Response quality evaluation against criteria | AI Gateway only |
| AI Guard | Semantic classification via embeddings | AI Gateway only |

Custom hooks and Rule Packs extend the built-in set without code changes.

## Streaming compliance modes

For streaming responses (Server-Sent Events), three modes are available per provider/host:

| Mode | Behavior |
|---|---|
| `passthrough` | Relay only — no hook execution, no body capture. Use for non-AI traffic that should pass uninspected. |
| `buffer_full_block` | Buffer the full response before forwarding any byte. Response-stage hook runs once at stream end. Hard reject prevents the upstream body from reaching the client. |
| `chunked_async` | Relay bytes to the client in real time. Response-stage hook runs per chunk and once at stream end. Cannot stop bytes already sent, but produces a full audit trail and triggers post-hoc alerting. |

Per-scope `fail_behavior` (`fail_open` or `fail_close`) determines what happens on hook timeout or error.

## Where it sits

- Hook interface and verdict types: `packages/shared/policy/hooks/core/types.go`
- Built-in hook implementations: `packages/shared/policy/hooks/<hook_name>/`
- Registry and dispatcher: `packages/shared/policy/hooks/` (global registry; per-stage dispatcher per service)
- Hook config schema and aggregation: `packages/shared/policy/hooks/`

Config changes propagate via Hub config sync — the dispatcher performs an atomic swap of its config snapshot when a new `HookConfig` arrives; in-flight transactions complete on the old config.

## How to enable and configure

Hooks are managed from **Compliance → Hooks** in the Control Plane UI:

1. Select **New Hook** and choose a built-in type (or import a Rule Pack).
2. Set the stage (`request`, `response`, or `connection`), priority, and applicable ingress (`ALL`, `AI_GATEWAY`, `COMPLIANCE_PROXY`, or `AGENT`).
3. Configure the hook-specific settings and the `onMatch` block (`inflightAction`, `storageAction`, `replacement`).
4. Save. The config propagates to all connected nodes within seconds via config sync.

Rule Packs bundle multiple hook configurations for common compliance scenarios (HIPAA, PCI-DSS, custom keyword sets) and can be imported from the Hooks page.

---

## Canonical docs

- [`hook-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/hook-architecture.md) — Hook interface, registry/dispatcher, three stages, onMatch schema, pipeline aggregation, streaming compliance modes, built-in hooks, body capture

**Adjacent wiki pages**: [Feature PII Redaction](Feature-PII-Redaction) · [Feature Audit And SIEM](Feature-Audit-And-SIEM) · [AI Gateway Hooks](AI-Gateway-Hooks) · [Compliance Proxy Overview](Compliance-Proxy-Overview) · [Feature Desktop Agent](Feature-Desktop-Agent) · [Features Index](Features-Index)
