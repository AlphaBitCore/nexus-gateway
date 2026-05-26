# E36 — Forward-Header Allowlist (YAML-configurable, request + response)

## Background

The AI Gateway proxies user traffic to upstream LLM providers. Today the
set of HTTP headers it forwards is **hard-coded in Go**, in two places:

- `packages/ai-gateway/internal/providers/spec_adapter.go:52` defines a
  three-entry base allowlist (`accept`, `user-agent`, `content-type`)
  shared by every adapter.
- Each provider's `spec.go` populates an `AdapterSpec.PerFormatForwardHeaders`
  map with vendor-specific extensions (`openai-beta`,
  `anthropic-version`, `x-goog-user-project`, …).

Two operational pains motivate moving this configuration to YAML:

1. **Upstream churn**: when a provider rolls out a new beta header
   (`openai-beta`-style) and a customer needs it forwarded, the
   current path is code change → PR → review → release → deploy
   (multi-day). Operators want the same change to be a YAML edit
   plus restart.
2. **Response side has the same dynamic and is currently uncovered**:
   upstream providers add response headers regularly
   (`x-request-id`, token-rate `x-ratelimit-*-tokens`, prompt-cache
   breakdown headers) that customer SDKs and customer support both
   want to see. Today the gateway clones upstream response headers
   into `Response.Headers` but the handler never reads them — a dead
   field that masks a missing feature. Closing this gap requires
   the same allowlist mechanism on the response path.

This epic introduces a YAML configuration source that owns **both**
directions, preserves the current security posture as the default,
and adds a hard denylist enforced at config load so operator typos
cannot weaken egress controls.

A hand-off context brief was drafted earlier and is preserved at
`docs/_archive/2026-q2/brainstorms/2026-05-06-forward-header-allowlist-context.md`;
the conclusions of the brainstorm in that thread are recorded in this
document.

## Functional Requirements

- **FR-FH1**: The set of headers forwarded from the client to the
  upstream provider must be configurable via a YAML block in
  `ai-gateway.dev.yaml` (and equivalent production config files).
  The block must support a global `base` list and a `perAdapterType`
  map keyed by `Provider.adapter_type`. **(Must)**

- **FR-FH2**: The set of headers forwarded from the upstream
  provider response to the client must be configurable in the same
  YAML block, split into `static` (cacheable) and `perRequest`
  (must be regenerated or stripped on cache hit) sub-lists.
  **(Must)**

- **FR-FH3**: Default values shipped with the binary must reproduce
  today's hard-coded request-side behavior exactly:
  `base = [accept, user-agent, content-type]`,
  `perAdapterType.openai = [openai-beta, openai-organization, openai-project]`,
  `perAdapterType.anthropic = [anthropic-beta, anthropic-version]`,
  `perAdapterType.gemini = [x-goog-user-project]`,
  `perAdapterType.vertex = [x-goog-user-project]`. All other
  adapter types default to empty extensions. Response side defaults
  to empty (preserves zero-passthrough posture). **(Must)**

- **FR-FH4**: A hard denylist must be enforced at config load. The
  following header names (case-insensitive) cause a fatal startup
  error if they appear anywhere in the request or response YAML:
  `authorization`, `cookie`, `set-cookie`, `x-api-key`,
  `x-goog-api-key`, `api-key`, `x-amz-*` (prefix match),
  `proxy-authorization`, `x-forwarded-*` (prefix match), `x-real-ip`,
  `x-nexus-*` (prefix match), `www-authenticate`,
  `strict-transport-security`, `content-security-policy`,
  `x-frame-options`, `server`, `via`, `x-served-by`, `cf-ray`,
  `access-control-*` (prefix match), `content-length`,
  `transfer-encoding`, `connection`. **(Must)**

- **FR-FH5**: Any `perAdapterType` key that is not a member of
  `providers.AllFormats()` (e.g. operator typo `open-ai`,
  `OpenAi`) must cause a fatal startup error. **(Must)**

- **FR-FH6**: The merge function used at request time must compute
  the effective allowlist as `base ∪ perAdapterType[req.Target.Format]`
  with no fallthrough across adapter types. Even when two
  adapter types share Go transport code (e.g. all OpenAI-compat
  siblings reuse `*spec_openai.Transport`), each adapter's forward
  decision must consult only its own configuration. **(Must)**

- **FR-FH7**: The handler must wire the response-side allowlist into
  the actual response written to the client. Today `Response.Headers`
  (cloned by the adapter) is dead — no consumer reads it. After this
  work, allowlisted upstream response headers must appear on the
  client's response, with Nexus's own `x-nexus-aigw-*` headers
  winning on conflict. **(Must)**

- **FR-FH8**: A Prometheus counter
  `ai_gateway_forward_header_dropped_total{header,direction,adapter_type}`
  must increment whenever a header arrives but is not on the
  effective allowlist. Counters carry the lower-cased header name
  (cardinality is bounded by the closed denylist + a small "other"
  bucket for unknown headers — see NFR-FH4). **(Should)**

- **FR-FH9**: An `x-nexus-aigw-allowlist-version` response header
  containing a short hash of the effective allowlist must be set on
  every gateway response. This gives operators a way to confirm
  rollout without parsing a config file. **(Could)**

- **FR-FH10**: Per-Provider override is **explicitly out of scope**.
  When two `Provider` rows share the same `adapter_type` they share
  the same forward-header configuration. The accepted pattern when
  a real divergence emerges is to introduce a new `Format` /
  `adapter_type` (mirroring how Groq, DeepSeek, Together, Fireworks
  are already structured today), not to add a per-Provider layer.
  **(Won't, this epic)**

## Non-Functional Requirements

- **NFR-FH1**: Config is loaded at process start. There is no hot
  reload, no SIGHUP, no Hub-shadow path, and no admin API for this
  setting. Changing the allowlist requires a service restart. This
  is intentional — egress controls should not be mutable through
  the same surfaces that operators use for routine ops.

- **NFR-FH2**: Failing the hard denylist or the adapter-type
  validator must abort startup with a clear error message naming
  the offending header / key and the YAML file. Silent drop is
  forbidden — fail fast or fail loud.

- **NFR-FH3**: The merge cost per request must be O(|effective set|)
  and must not allocate per-request maps in the hot path. The
  current `forwardHeaders` function allocates a fresh `allowed` map
  on every call (`spec_adapter.go:303`); this is acceptable today
  but the YAML refactor must not regress it. The effective set per
  Format may be precomputed at config load and cached on the
  `AdapterSpec`.

- **NFR-FH4**: The metrics counter cardinality must be bounded.
  Header names not on any allowlist or denylist are bucketed under
  `header="other"` to prevent unbounded series growth from
  malicious or noisy clients.

- **NFR-FH5**: The change must not regress the existing snapshot
  test `packages/ai-gateway/internal/handler/proxy_test.go` (which
  pins per-format allowlist behavior) nor
  `packages/ai-gateway/internal/providers/spec_adapter_forward_test.go`.

- **NFR-FH6**: English-only artifacts (per CLAUDE.md). All YAML
  comments, error messages, doc strings, and commit messages stay
  English.

## User Roles & Personas

- **AI gateway operators** — own the `ai-gateway.dev.yaml` /
  production config file; edit the `forwardHeaders` block; restart
  the service. Read the `ai_gateway_forward_header_dropped_total`
  metric and the `x-nexus-aigw-allowlist-version` header to confirm
  rollout.

- **Compliance officers** — rely on the hard denylist as a
  binding-by-construction guarantee that operators cannot
  weaken egress controls (no credential header, no debug header
  that leaks backend identity, no CORS / CSP / framing header that
  reshapes the security context). Audit the YAML diff in git on
  every change.

- **Customer SDK clients** — consumers of `/v1/*`. Benefit from
  receiving response headers their SDK already knows how to parse
  (`x-request-id` for support tickets, `x-ratelimit-*-tokens` for
  client-side throttling) without API changes.

- **Engineers shipping a new wire-protocol variant** — when an
  operator needs forward-header behavior that diverges within a
  single adapter type (the "internal vLLM via openai format"
  hypothetical), the answer is to register a new `Format` and
  `spec_*` package, not to extend the YAML schema. This keeps
  audit boundaries on engineering review rather than YAML edit.

## Constraints & Assumptions

- Pre-GA: no installed user base. Schema and defaults can change
  freely between this work and the first GA release; no
  backward-compat shims are needed (per CLAUDE.md
  development-phase policy).

- The `Provider.adapter_type` value space is enum-like and
  validated at the Control Plane handler layer (per
  `tools/db-migrate/schema.prisma:16-24`). The set is not closed
  by a SQL CHECK constraint, but the Control Plane handler and
  the OpenAPI spec enforce membership. The YAML validator
  re-checks against `providers.AllFormats()` at gateway startup
  to defend against drift between the two enums.

- The gateway today registers one `AdapterSpec` per Format
  (`packages/ai-gateway/internal/providers/adapter_registry.go:12`).
  OpenAI-compatible siblings (Groq, DeepSeek, Together, Fireworks,
  XAI, Moonshot, Perplexity, Mistral, HuggingFace, Replicate)
  each have their own Format and their own `spec.go`, even when
  they reuse `spec_openai`'s Transport / SchemaCodec / StreamDecoder
  components. The forward-header configuration mirrors this layout.

- The SSE / non-stream response cache (E35,
  `docs/developers/specs/e35/e35-s1-sse-cache.md`) is in flight in a separate
  session. The cache key formula and broker model in that work
  must include the effective forward-header allowlist hash so a
  config change invalidates the right cache entries, and cache
  hits must regenerate / strip `perRequest` response headers.
  E36 commits to the contract; the actual amendment to the E35
  spec / code is owned by E35 and is deferred to avoid merge
  conflict with the in-flight session. The SDD documents the
  exact insertion points.

- `Accept-Encoding` is permanently excluded from the request-side
  allowlist (see `spec_adapter.go:38-51` for the production
  incident comment). The YAML validator must reject any attempt
  to add it (treated as a hard denylist entry).

## Glossary

- **AdapterType** — DB column `Provider.adapter_type`, one of the
  Format values (`openai`, `anthropic`, `gemini`, `vertex`,
  `bedrock`, `cohere`, `azure-openai`, `glm`, `minimax`,
  `deepseek`, `fireworks`, `groq`, `huggingface`, `mistral`,
  `moonshot`, `perplexity`, `replicate`, `together`, `xai`).
  Synonymous with `providers.Format` at the Go layer.

- **Format** — Go enum `providers.Format` (one-to-one with
  AdapterType).

- **Forward-header allowlist** — the closed set of HTTP header
  names safe to forward in a given direction (request or
  response) for a given adapter type. Default-deny semantics:
  anything not on the list is dropped.

- **Hard denylist** — header names that the YAML validator
  refuses to accept anywhere, regardless of which list the
  operator placed them in. Enforced at config load (fail-fast).

- **Static response header** — a response header whose value is
  expected to be stable across calls to the same provider /
  model (e.g. `openai-version`). Safe to replay from cache.

- **Per-request response header** — a response header whose
  value is unique to the call it was issued in (e.g.
  `x-request-id`, `*-ratelimit-remaining-*`,
  `openai-processing-ms`). Must be stripped on cache hit;
  replaying a stale value is worse than not returning it.

- **Effective allowlist** — `base ∪ perAdapterType[F]` for a
  given direction and Format `F`, after the denylist is applied.

## Priority Summary (MoSCoW)

- **Must**: FR-FH1, FR-FH2, FR-FH3, FR-FH4, FR-FH5, FR-FH6, FR-FH7;
  NFR-FH1, NFR-FH2, NFR-FH5.
- **Should**: FR-FH8; NFR-FH3, NFR-FH4, NFR-FH6.
- **Could**: FR-FH9.
- **Won't (this epic)**: FR-FH10 (per-Provider override layer);
  hot reload; admin API surface for the allowlist; cross-tenant
  scoping.

## Out of Scope

- Per-Provider override (handled by introducing new Format when
  needed; see FR-FH10).
- Hot reload / SIGHUP / Hub-shadow propagation of the allowlist.
- An admin REST/UI surface to mutate the allowlist.
- Multi-tenant (per-VK / per-Org) scoping.
- Coordinating BYOK vs platform-key response-header forwarding
  policy. (Default empty response set sidesteps the question;
  if a future epic opens up response forwarding, that work owns
  the BYOK / platform-key axis.)
- Forwarding `Accept-Encoding` (permanently excluded).
- Implementing the actual amendment to E35's SSE cache spec /
  code. The contract is documented in the E36 SDD; the change
  to E35 artifacts is owned by the E35 session.
