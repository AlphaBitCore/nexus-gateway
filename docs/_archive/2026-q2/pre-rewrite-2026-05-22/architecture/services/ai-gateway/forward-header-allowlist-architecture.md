---
doc: forward-header-allowlist-architecture
area: service
service: ai-gateway
tier: 2
updated: 2026-05-21
---

# Forward Header Allowlist Architecture (E36)

> **Tier 2 architecture doc.** Read when changing how the AI Gateway forwards inbound HTTP headers to upstream providers, or when adding a new adapter type. Lives in `packages/ai-gateway/internal/execution/forwardheader/`.

By default, the AI Gateway **strips** all inbound headers from the application and constructs the upstream request fresh (with the provider credential, the canonical body, a fresh `User-Agent`, etc.). The forward-header allowlist is the controlled escape hatch: a **YAML-only**, **boot-fixed**, **per-adapter-type** list of headers that pass through verbatim (request side) or are echoed back to the client (response side).

There is no DB table, no per-route plumbing, no per-VK plumbing, no admin CRUD. The config is loaded once at startup and swapped into an `atomic.Pointer` for lock-free read on the hot path.

---

## 1. Why "strip by default"

Three reasons to strip:

1. **Credential isolation** — the application's `Authorization` header is the Virtual Key, NOT the provider key. Forwarding it would leak the VK upstream (a Nexus secret) and would also overwrite the credential the executor injects.
2. **Trust boundary** — the application is on the customer's side of the trust boundary; passing arbitrary headers upstream could be used to smuggle data or to influence provider behaviour in unaudited ways.
3. **Predictability** — providers vary in how they handle unknown headers. Stripping keeps behaviour deterministic.

## 2. Why an allowlist exists

Some headers are legitimately needed:

- **Tracing**: `traceparent`, `tracestate` (W3C trace context for end-to-end correlation).
- **Idempotency**: `idempotency-key` for retry-safety on operations that support it.
- **Provider-specific opt-ins**: `anthropic-beta: prompt-caching-2024-07-31`, `openai-organization`, etc.

The allowlist is configured per **adapter type** (the `Provider.adapter_type` slug — `openai-compat`, `anthropic`, `gemini`, …), not per route and not per VK. Less is more (CLAUDE.md "less is more / delete instead of add"): per-route / per-VK overrides would multiply the surface for a feature that real operators never use across more than a handful of headers.

## 3. Configuration shape

The block lives at the top level of `ai-gateway.{dev,prod}.yaml` and parses into `forwardheader.Config` (see `packages/ai-gateway/internal/execution/forwardheader/forwardheader.go:35`):

```yaml
forwardHeaders:
  request:
    # Universal request-side allowlist applied to every adapter type.
    base:
      - traceparent
      - tracestate
      - idempotency-key
    # Per-adapter-type extension. Keys must be a known adapter slug
    # (providers.AllFormats() — unknown keys cause a fatal startup
    # error in Resolve).
    perAdapterType:
      anthropic:
        headers:
          - anthropic-beta
          - anthropic-version
      openai-compat:
        headers:
          - openai-organization
          - openai-project

  response:
    # Response-side base splits into two arms:
    #   - static: cacheable headers that travel with the response on
    #     both cache-miss and cache-hit.
    #   - perRequest: headers stripped on cache hit because they're
    #     bound to the original upstream call (rate-limit budgets,
    #     request-scoped IDs, etc.).
    base:
      static:
        - content-type
        - cache-control
      perRequest:
        - x-request-id
    perAdapterType:
      anthropic:
        static:
          - anthropic-ratelimit-requests-limit
        perRequest:
          - anthropic-ratelimit-requests-remaining
          - anthropic-ratelimit-requests-reset
      openai-compat:
        static:
          - openai-organization
        perRequest:
          - x-ratelimit-limit-requests
          - x-ratelimit-remaining-requests
          - x-ratelimit-reset-requests
```

Each entry is just a header name (lower-cased internally; case-insensitive). There is no `direction` field (direction is implied by `request:` vs `response:`), no `transform.rename` field (renames would let an operator collide with an internal Nexus header by accident — design refused the feature), and no `applies_to.providers` field (the perAdapterType key already scopes it).

## 4. Resolution order

`forwardheader.Resolve` precomputes a snapshot at startup:

1. Lower-case + validate every header name against the hard denylist (§6).
2. Validate every `perAdapterType` key against `validFormats` (the closed adapter-slug set).
3. For every known adapter slug `f`:
   - **Request set** = `request.base ∪ request.perAdapterType[f].headers`.
   - **Response Static set** = `response.base.static ∪ response.perAdapterType[f].static`.
   - **Response PerRequest set** = `response.base.perRequest ∪ response.perAdapterType[f].perRequest`.
4. Forbid any single header from appearing in both Static and PerRequest for the same adapter (raises a startup error).

`forwardheader.SetActive(resolved)` stores the resolved snapshot in an `atomic.Pointer[Resolved]` (`activeResolved`); the request path calls `Resolved.Request(formatSlug)` / `Resolved.Response(formatSlug)` without locks. There is no route / VK layering — once the snapshot is set, every request with the same adapter slug gets the same allowlist.

## 5. Cache-hit response semantics

For a cache hit, the Response **Static** set is replayed from the cached headers (same bytes the upstream returned the first time); the Response **PerRequest** set is stripped because those values were bound to the original upstream call and would mislead the client (`x-request-id` doesn't change behavior but `anthropic-ratelimit-requests-remaining` does — replaying it would surface a stale rate-limit budget that may no longer hold).

This is the only place the request/response asymmetry matters; on the request side every header is treated identically (always forwarded if in the allowlist, always stripped if not).

## 6. Hard denylist

Some headers are **always** stripped — they cannot be added to any allowlist. From `packages/ai-gateway/internal/execution/forwardheader/forwardheader.go:395` (kept up to date as the canonical list):

**Exact-match denylist (case-insensitive):**

| Header | Reason |
|---|---|
| `authorization` | Leaks the VK upstream + would overwrite the executor-injected provider credential. |
| `cookie` | Application cookies never cross the trust boundary. |
| `set-cookie` | Same as above, response side. |
| `x-api-key` | Provider credential lookalike — strip to avoid client-side smuggling. |
| `x-goog-api-key` | Google API key lookalike. |
| `api-key` | Generic credential lookalike. |
| `proxy-authorization` | Intermediate-proxy credential — never forwarded. |
| `x-real-ip` | Source IP would leak the Nexus subnet to the upstream. |
| `www-authenticate` | Response credential challenge — strip to avoid client-side credential prompts. |
| `strict-transport-security` | Provider HSTS would override Nexus HSTS on the client connection. |
| `content-security-policy` | Same — provider CSP would override Nexus CSP. |
| `x-frame-options` | Provider framing policy bleeds into Nexus UI. |
| `server` | Identifies the upstream — strips fingerprintable info. |
| `via` | Proxy attribution — strip to avoid leaking the chain. |
| `x-served-by` | Server fingerprint. |
| `cf-ray` | Cloudflare attribution — strip to avoid leaking CDN topology. |
| `content-length` | Framing header; Nexus recomputes after body translation. |
| `transfer-encoding` | Framing header; Nexus owns chunked/identity decisions. |
| `connection` | Hop-by-hop header per RFC 7230. |
| `accept-encoding` | **Load-bearing** — see callout below. |

**Prefix-match denylist (case-insensitive):**

| Prefix | Reason |
|---|---|
| `x-amz-` | AWS-attribution headers — strip to avoid leaking AWS infrastructure. |
| `x-forwarded-` | Proxy-chain attribution; Nexus injects its own where needed. |
| `x-nexus-` | Nexus-internal namespace; operators can't add headers that collide with internal contracts. |
| `access-control-` | CORS headers belong to the Nexus front, not the upstream. |

> **`accept-encoding` is denied, not allowed.** Forwarding `accept-encoding` from the client through to the upstream disables Go `net/http.Transport`'s transparent gzip decompression, which broke Anthropic SSE in production once already. Do not move it to the allowlist without re-reading the load-bearing comment at `packages/ai-gateway/internal/providers/spec_adapter.go:38-51` and `forwardheader.go:415-420`.

> **`host` is not on the deny list** (the executor sets the upstream host from `Provider.baseUrl`; the inbound `Host` header is irrelevant by then). Listing `host` here would be wrong.

Attempting to put any denylisted header into the YAML triggers a fatal validation error from `Resolve`; the gateway refuses to start.

## 7. Auditing

There is no admin write path for the allowlist, so there is no admin-audit action. The active snapshot (and its content hash) is logged at startup; operators see "forward-header config loaded" with the hash in the gateway boot log. The `traffic_event` row does not enumerate the forwarded header names (would multiply the row size for no operational value); when investigators need to reconstruct what the upstream saw, the gateway-side body-debug log captures the full outbound `http.Header` map at the point of dispatch.

## 8. Sources

- `packages/ai-gateway/internal/execution/forwardheader/forwardheader.go` — `Config` struct, `Resolve`, `SetActive`, hard denylists.
- `packages/ai-gateway/internal/execution/forwardheader/defaults.yaml` — built-in default allowlist (used when `forwardHeaders:` is absent in the YAML).
- `packages/ai-gateway/cmd/ai-gateway/main.go` — boot wiring: load YAML → `Resolve` → `SetActive`.
- `docs/developers/specs/e36/e36-s1-forward-header-yaml-request.md` and `e36-s2-forward-header-yaml-response.md` — design specs.

<!-- 💡 harvest: nothing new. Hard denylist is enforced at boot; no lint candidate. -->

## 9. Cross-references

- `provider-adapter-architecture.md` — adapter framework that wraps the upstream call.
- `trace-id-propagation-architecture.md` — `traceparent` / `tracestate` propagation.
- `routing-architecture.md` — adjacent (but separate) route-level config.
- `credentials-architecture.md` — `authorization` strip rationale.
