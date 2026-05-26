# E36-S1 — Forward-Header YAML Allowlist (Request Side)

## 0. References

- Requirements: `docs/developers/specs/e36/e36-forward-header-allowlist.md`
- Hand-off context brief: `docs/_archive/2026-q2/brainstorms/2026-05-06-forward-header-allowlist-context.md`
- Architecture diff: `docs/users/product/architecture.md` §"AI Gateway Provider Adapters" — "Forward-header allowlist (request + response)"
- Companion story: `docs/developers/specs/e36/e36-s2-forward-header-yaml-response.md`
- Coupling target (deferred amendment): `docs/developers/specs/e35/e35-s1-sse-cache.md`

## 1. User story

> *"As a gateway operator, when an upstream provider rolls out a new
> beta header — e.g. OpenAI adds `openai-prompt-cache-experiment` —
> I want to enable forwarding of that header by editing
> `ai-gateway.dev.yaml` and restarting the gateway, instead of
> waiting for a code change → PR → review → release → deploy. The
> change must be a YAML diff visible in git, not a runtime mutation."*

## 2. Scope

### 2.1 In scope

- A new `forwardHeaders.request` block in
  `packages/ai-gateway/ai-gateway.dev.yaml` (and equivalent prod
  config files) carrying `base` and `perAdapterType` lists.
- A startup-time loader + validator
  (`packages/ai-gateway/internal/config/forward_header.go`) that
  reads the YAML, applies the hard denylist, validates
  `perAdapterType` keys against `providers.AllFormats()`, and
  builds a precomputed per-Format effective set.
- Refactor of `packages/ai-gateway/internal/providers/spec_adapter.go`
  `forwardHeaders()` to consult the loaded config rather than the
  package-level `forwardHeaderAllowList` constant + each spec's
  `PerFormatForwardHeaders` field.
- Removal of `AdapterSpec.PerFormatForwardHeaders` and the
  per-spec assignments in `spec_openai`, `spec_anthropic`,
  `spec_gemini`, `spec_vertex` (the four currently non-empty
  declarations). Greenfield deletion per CLAUDE.md
  development-phase policy — the YAML defaults reproduce the
  same set, no compatibility layer.
- A Prometheus counter
  `ai_gateway_forward_header_dropped_total{header,direction="request",adapter_type}`
  incremented when an inbound header is filtered out.
- Default YAML values that, when loaded, produce an effective
  set identical to today's compile-time set.
- Unit-test coverage for: default-equals-historical-hardcoded,
  hard denylist fail-fast, invalid `perAdapterType` key fail-fast,
  per-adapter-type isolation, metrics counter increment.

### 2.2 Out of scope (covered elsewhere)

- Response-side allowlist — `docs/developers/specs/e36/e36-s2-forward-header-yaml-response.md`.
- The actual cache-key amendment in E35 — see §6.
- Per-Provider override layer — see Requirements FR-FH10.
- Hot reload / admin API.

## 3. YAML schema

```yaml
forwardHeaders:
  request:
    base:
      - accept
      - user-agent
      - content-type
    perAdapterType:
      openai:
        - openai-beta
        - openai-organization
        - openai-project
      anthropic:
        - anthropic-beta
        - anthropic-version
      gemini:
        - x-goog-user-project
      vertex:
        - x-goog-user-project
      # All other adapter types are absent → empty extension.
```

Rules:

- All header names are lower-cased before set membership
  comparisons.
- Order within a list does not matter (deduplicated at load).
- A missing `forwardHeaders.request` block falls back to the
  same defaults that ship in the binary's `embed.FS` baseline
  (see §4.4) — so a YAML with the block omitted reproduces
  today's behavior exactly.

## 4. Tasks

### 4.1 T-CONFIG-TYPES — Define config types in `internal/config/`

**Files**: `packages/ai-gateway/internal/config/forward_header.go` (new).

```go
type ForwardHeaderConfig struct {
    Request  ForwardHeaderDirection `yaml:"request"`
    Response ForwardHeaderDirection `yaml:"response"` // populated by S2
}

type ForwardHeaderDirection struct {
    Base           []string                       `yaml:"base"`
    PerAdapterType map[string]ForwardHeaderEntry  `yaml:"perAdapterType"`
}

// ForwardHeaderEntry: request side uses Headers slice only;
// response side (S2) extends with Static + PerRequest.
type ForwardHeaderEntry struct {
    Headers    []string `yaml:"headers,omitempty"`    // request side
    Static     []string `yaml:"static,omitempty"`     // response side (S2)
    PerRequest []string `yaml:"perRequest,omitempty"` // response side (S2)
}
```

Deliberate split: S1 uses `Headers`, S2 uses `Static` +
`PerRequest`. `ForwardHeaderEntry` carries all three so the same
YAML layer covers both directions without two parallel struct
hierarchies.

### 4.2 T-CONFIG-VALIDATE — Hard denylist + adapter-type check

**Files**: same as T-CONFIG-TYPES.

Validator behavior:

1. Concatenate every header name across `request.base`,
   `request.perAdapterType[*].headers`, and (in S2)
   `response.*` lists.
2. Lower-case each name. Reject if it matches the hard denylist
   (exact match for fixed names; prefix match for `x-amz-*`,
   `x-forwarded-*`, `x-nexus-*`, `access-control-*`).
3. For request side, also reject `accept-encoding` (permanent —
   tied to the SSE incident at `spec_adapter.go:38-51`).
4. Reject any `perAdapterType` key not in `providers.AllFormats()`
   (case-sensitive; lowercase canonical form).
5. On any rejection, return an error that names the offending
   header / key and the YAML file path. Caller (main.go) treats
   the error as fatal.

### 4.3 T-CONFIG-PRECOMPUTE — Build per-Format effective set

**Files**: `internal/config/forward_header.go`.

After validation, compute and freeze:

```go
type ResolvedAllowlist struct {
    request  map[providers.Format]map[string]struct{} // F → effective request set
    response map[providers.Format]ResolvedResponseSet // populated by S2
}

func (r *ResolvedAllowlist) Request(f providers.Format) map[string]struct{}
```

`Request(f)` returns the precomputed `base ∪ perAdapterType[f].headers`
for Format `f`, or just `base` when `f` has no entry. Returned map is
read-only — callers never mutate. The map is built once at startup;
NFR-FH3 requires the hot path not allocate per request.

### 4.4 T-CONFIG-DEFAULTS — Embed default YAML

**Files**: `packages/ai-gateway/internal/config/forward_header_defaults.yaml` (new),
embedded via `//go:embed`.

The default file contains exactly the historical hard-coded
values (see Requirements FR-FH3). Loading order:

1. Parse the embedded defaults into the resolved structure.
2. If `ai-gateway.dev.yaml` (or production equivalent) sets a
   `forwardHeaders.request` block, **replace** the loaded
   defaults at the block level. (Whole-block replace, not
   merge — operators read one file, not two; matches the
   "real implementation only" rule.)
3. Run validators on the final structure.

### 4.5 T-WIRE-CONFIG — Plumb the resolved allowlist into adapters

**Files**:
- `packages/ai-gateway/internal/providers/spec.go` — drop
  `PerFormatForwardHeaders` field from `AdapterSpec`.
- `packages/ai-gateway/internal/providers/spec_adapter.go` —
  rewrite `forwardHeaders()` to read the runtime config;
  delete the package-level `forwardHeaderAllowList` constant.
- `packages/ai-gateway/internal/providers/adapter_registry.go` —
  add `Registry.SetAllowlist(*config.ResolvedAllowlist)` called
  from `cmd/ai-gateway/main.go` before `Freeze()`. Adapters
  resolve the allowlist via the registry rather than each
  carrying a copy, so a single source of truth is shared across
  every Format.
- `packages/ai-gateway/internal/providers/spec_openai/spec.go`,
  `spec_anthropic/spec.go`, `spec_gemini/spec.go`,
  `spec_vertex/spec.go` — remove the
  `PerFormatForwardHeaders: map[string]struct{}{...}` lines.

### 4.6 T-METRICS — Drop counter

**Files**: `packages/ai-gateway/internal/observability/metrics/forward_header.go`
(new). Register `ai_gateway_forward_header_dropped_total` with
labels `header`, `direction`, `adapter_type`. Header label is
lower-cased; unknown headers (not on any allowlist or denylist)
bucket as `header="other"` (NFR-FH4).

The counter increments inside `forwardHeaders()` when a
client-supplied header is dropped. Counts are bounded by the
denylist size + the union of all allowlists + a constant `other`
bucket — well below cardinality limits.

### 4.7 T-RESP-VERSION-HEADER — Allowlist version stamp (FR-FH9, Could)

**Files**: `packages/ai-gateway/internal/handler/proxy.go`
`setResponseHeaders` / `setResponseHeadersStream`.

Add `w.Header().Set("x-nexus-aigw-allowlist-version", h.allowlistHash)`
where `allowlistHash` is `SHA256(canonicalize(resolved struct))[:8]`
computed once at startup and held on `Handler.deps`.

This task is `Could` priority — fold into the same change because
it costs almost nothing and is the simplest rollout-confirmation
signal.

### 4.8 T-SDD-COUPLE — Document the cache coupling

**Files**: this SDD §6, plus the response-side companion. **No**
edits to `docs/developers/specs/e35/e35-s1-sse-cache.md` from this story (per the
deferral note in §6).

## 5. Acceptance criteria

### 5.1 Functional

- **AC-FH-S1-01** Loading the embedded defaults (no
  `forwardHeaders` block in `ai-gateway.dev.yaml`) produces a
  resolved request allowlist that, for every Format `F`, equals
  the union of the historical `forwardHeaderAllowList` constant
  and the historical `spec_F.PerFormatForwardHeaders`. Asserted
  by a snapshot test.

- **AC-FH-S1-02** A YAML file declaring
  `forwardHeaders.request.perAdapterType.openai.headers: [openai-x]`
  results in `ResolvedAllowlist.Request(FormatOpenAI)` containing
  `openai-x` and **not** containing it for any other Format
  (per-adapter-type isolation).

- **AC-FH-S1-03** A YAML containing any of the hard-denylist
  entries (`authorization`, `cookie`, `x-api-key`,
  `x-goog-api-key`, `api-key`, `proxy-authorization`,
  `x-real-ip`, `accept-encoding`, or any prefix match for
  `x-amz-*`, `x-forwarded-*`, `x-nexus-*`, `access-control-*`)
  causes the loader to return an error naming the header and
  YAML file. `cmd/ai-gateway/main.go` treats this as fatal.

- **AC-FH-S1-04** A YAML containing a `perAdapterType` key not
  in `providers.AllFormats()` (e.g. `open-ai`, `OpenAi`,
  `vllm`) causes the loader to return an error naming the key
  and the closest valid Format.

- **AC-FH-S1-05** At request time, when `req.Target.Format ==
  FormatGroq` and the inbound request carries an `openai-beta`
  header, the outbound request to Groq's upstream **does not**
  contain `openai-beta`. (Per-adapter-type isolation, even
  though Groq reuses `*spec_openai.Transport`.)

- **AC-FH-S1-06** When a client sends a header not on the
  effective allowlist for that request's Format,
  `ai_gateway_forward_header_dropped_total{direction="request",
  adapter_type=<F>, header=<h>}` increments by exactly 1.
  Unknown / off-list headers bucket under `header="other"`.

- **AC-FH-S1-07** Every gateway response carries
  `x-nexus-aigw-allowlist-version: <8-hex-chars>`. The value is
  stable across the process lifetime of one config and changes
  on the next restart with a different YAML.

### 5.2 Behavioral preservation

- **AC-FH-S1-08** The existing snapshot test
  `packages/ai-gateway/internal/handler/proxy_test.go` keeps
  passing with default config loaded.

- **AC-FH-S1-09** `packages/ai-gateway/internal/providers/spec_adapter_forward_test.go`
  keeps passing; if its fixture spec used
  `PerFormatForwardHeaders`, the test is rewritten to inject a
  `*config.ResolvedAllowlist` instead, asserting the same
  filter behavior. No semantic regression.

- **AC-FH-S1-10** `Authorization` from a client request never
  reaches the upstream regardless of YAML content. (Verified by
  unit test that sets a YAML
  `request.base: [authorization]` — load-time error — **and**
  by a runtime test that puts an `Authorization` header on the
  inbound request and confirms it is absent from the outbound
  before `ApplyAuth` runs.)

### 5.3 Observability

- **AC-FH-S1-11** Prometheus scrape exposes the new counter
  with all three labels populated. Verified by
  `internal/metrics` package unit test.

- **AC-FH-S1-12** Startup log line at INFO level prints the
  resolved allowlist summary: `forward header allowlist: request
  base=[…], formats=[openai:n, anthropic:n, …]` (header names
  hashed in the log to avoid leaking custom names into logs;
  hash same as the version header).

## 6. Coordination required (E35 SSE cache, deferred)

The forward-header allowlist is part of the cache-equivalence
proof in `docs/_archive/2026-q2/brainstorms/2026-05-06-sse-cache-design.md` §7.6
(referenced from `docs/developers/specs/e35/e35-s1-sse-cache.md`). Today's spec
treats it as a compile-time constant. After E36-S1 lands, the
cache must:

1. **Include the effective request allowlist hash in the cache
   key**. Same `allowlistHash` computed for FR-FH9 is reused.
   Two requests that arrived under different config versions
   must not collide. **Insertion point** in
   `docs/developers/specs/e35/e35-s1-sse-cache.md` §"T-CACHEKEY-V2": amend the
   formula to
   `SHA256(provider + ProviderModelID + canonicalize(prepareBody output) + allowlistHash)`.

2. **Strip per-request response headers on cache hit** — see
   companion story `docs/developers/specs/e36/e36-s2-forward-header-yaml-response.md`
   §6 for the response-side details.

**This SDD does not edit `docs/developers/specs/e35/e35-s1-sse-cache.md`.** Per
the in-flight session arrangement, the E35 owner applies the
amendment when their work converges. The exact text and line
location proposed above is a hand-off; if the E35 spec
restructures `T-CACHEKEY-V2` before then, the amendment moves
with it.

The contract E36 commits to:

- `cmd/ai-gateway/main.go` exposes `Handler.AllowlistHash()` as a
  public method returning the 8-char hex hash.
- The cache layer reads it via the existing `Handler.deps`
  injection and includes it in key derivation.
- Whether to invalidate existing entries on hash change vs let
  TTL drain is an E35 decision; the contract only requires the
  hash be *available*.

## 7. Out of scope (this story)

- Response-side allowlist (S2).
- Hot reload, SIGHUP, Hub-shadow propagation.
- Per-Provider override layer (Requirements FR-FH10).
- Admin REST/UI surface for the allowlist.
- The actual edit to `docs/developers/specs/e35/e35-s1-sse-cache.md` (deferred).
- Touching `packages/ai-gateway/internal/cache/*` code (owned
  by E35).

## 8. Risks / open questions

- **R1**: A future operator could set `request.base: []`,
  removing `content-type`. The gateway then sends bodyless
  POSTs to upstream. Mitigation: add `content-type` to a
  *required* set the validator enforces (i.e. base must be a
  superset of `[content-type]`). Open question — flag if you
  want this elevated to a Must.
- **R2**: `spec_adapter_forward_test.go` may need a non-trivial
  rewrite to inject the resolved allowlist. Bounded — the
  fixture is small and the test is white-box.
- **R3**: The version-header (`x-nexus-aigw-allowlist-version`)
  exposes a fingerprint of the allowlist to clients. Low risk
  (8-hex-char hash, content not recoverable), but worth
  flagging for the security review pass.
