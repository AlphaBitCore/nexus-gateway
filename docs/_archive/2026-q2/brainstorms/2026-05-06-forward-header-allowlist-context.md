# Forward-header allowlist — Context brief for a fresh session

> Status: Context brief (not a spec)
> Owner: AI Gateway team
> Date: 2026-05-06
> Purpose: Hand-off document so a new Claude Code session can pick up
> this topic from zero and run brainstorm → spec → implementation
> independently. Read this in full before doing anything else.

## 0. Why you are reading this

The repository is `nexus-gateway`, an enterprise AI traffic gateway.
One of its services, `ai-gateway` (path
`packages/ai-gateway/`), proxies user requests to upstream LLM
providers (OpenAI, Anthropic, Gemini, Vertex, Bedrock, GLM, …).

When the gateway forwards a request to an upstream provider, **the
overwhelming majority of the inbound client headers are stripped**.
Only a small allowlist passes through, plus a per-format extension
list declared in code by each provider adapter.

A separate workstream (SSE cache, see
`2026-05-06-sse-cache-design.md` in this folder) had a tangential
need to confirm this safety posture. While doing so we noticed that
**the per-format allowlist is hard-coded in Go**, not configurable
via YAML, DB, or admin API. That raised a question the team
flagged for independent treatment: should it be made
runtime-configurable, and if so how?

This document is the seed for that conversation. It does **not**
prescribe a direction. Your job is to walk the user through a
brainstorm and produce a separate spec for whichever direction the
user picks.

## 1. Hard repository conventions you must follow

These come from `CLAUDE.md` and are binding regardless of the topic
being discussed:

- **English only** for everything you write into the repo (docs,
  code comments, commit messages, UI strings, OpenAPI). Conversation
  in chat may mirror the user's language.
- **Plan-first + todo list** before any substantive change.
- **Mandatory SDD pipeline**: Architecture → Requirements (`docs/requirements/`)
  → SDD (`docs/sdd/`) → OpenAPI (`docs/openapi/`) → Code → Unit
  tests → Verify → ask user about commit. Skipping any step needs
  explicit user waiver.
- **No backward-compat shims**: nexus-gateway is pre-GA, no installed
  user base. If you remove a thing, remove it everywhere in the same
  PR. No `@deprecated` markers, no parallel "legacy" paths.
- **No `git stash`** under any circumstance. Parallel sessions share
  the working tree; stashing can sweep a sibling's work.
- **Real implementation only** in production code. No `TODO` /
  `FIXME` / stub returns / mock production paths.
- **English-only artifacts** for every file under `docs/`,
  `packages/*/`, `.cursor/`, etc.
- **Ask the user** when a choice materially affects behavior, API
  contracts, security, or scope and the answer is not explicit.
  Offer A/B/C options + recommended default + free-text input.

## 2. Quick orientation: where in the codebase

Module layout:

- `packages/ai-gateway/` — Go service that fronts upstream providers
- `packages/ai-gateway/internal/providers/` — adapter framework
- `packages/ai-gateway/internal/providers/spec_adapter.go` — generic
  adapter that composes per-spec components
- `packages/ai-gateway/internal/providers/spec_*/` — per-provider
  packages (`spec_openai`, `spec_anthropic`, `spec_gemini`,
  `spec_vertex`, `spec_bedrock`, `spec_cohere`, `spec_glm`,
  `spec_minimax`, `spec_azure_openai`, plus a few OpenAI-compat
  passthroughs)

Each provider package has a `spec.go` that returns an
`AdapterSpec` struct. `AdapterSpec` is defined in
`packages/ai-gateway/internal/providers/spec.go`.

## 3. What the current allowlist looks like

### 3.1 Base allowlist (shared by all adapters)

`packages/ai-gateway/internal/providers/spec_adapter.go:52`:

```go
var forwardHeaderAllowList = map[string]struct{}{
    "accept":       {},
    "user-agent":   {},
    "content-type": {},
}
```

Lower-case header names. Anything not in this map (and not in the
per-format extension below) is dropped before the outbound request.

`Accept-Encoding` is **deliberately excluded** — there is a
load-bearing comment at `spec_adapter.go:38–51` explaining that
forwarding it causes Go's `net/http.Transport` to disable transparent
gzip decompression, which broke Anthropic streaming SSE in a real
production incident. Do not "fix" this by adding `accept-encoding`
to the list.

### 3.2 Per-format extension

`AdapterSpec` carries a field:

```go
// packages/ai-gateway/internal/providers/spec.go:18
type AdapterSpec struct {
    Format          Format
    Transport       Transport
    SchemaCodec     SchemaCodec
    StreamDecoder   StreamDecoder
    ErrorNormalizer ErrorNormalizer
    PerFormatForwardHeaders map[string]struct{}
}
```

Each provider's `spec.go` populates this with the headers that
provider's API expects to receive verbatim. Current declarations:

| Provider package | Headers extended past the base allowlist |
|---|---|
| `spec_openai/spec.go:25` | `openai-beta`, `openai-organization`, `openai-project` |
| `spec_anthropic/spec.go:24` | `anthropic-beta`, `anthropic-version` |
| `spec_gemini/spec.go:24` | `x-goog-user-project` |
| `spec_vertex/spec.go:25` | `x-goog-user-project` |
| All other `spec_*` | empty / unset (only base allowlist applies) |

### 3.3 The merge-and-filter step

`spec_adapter.go:295`:

```go
func (a *specAdapter) forwardHeaders(dst *http.Request, src http.Header) {
    if len(src) == 0 {
        return
    }
    allowed := make(map[string]struct{},
        len(forwardHeaderAllowList)+len(a.spec.PerFormatForwardHeaders))
    for k := range forwardHeaderAllowList {
        allowed[k] = struct{}{}
    }
    for k := range a.spec.PerFormatForwardHeaders {
        allowed[strings.ToLower(k)] = struct{}{}
    }
    for k, vs := range src {
        lk := strings.ToLower(k)
        if _, ok := allowed[lk]; !ok {
            continue
        }
        for _, v := range vs {
            dst.Header.Add(k, v)
        }
    }
}
```

Authorization (or `x-api-key` / `x-goog-api-key` / `api-key`
depending on the provider) is **set after** this step by
`Transport.ApplyAuth`, using the upstream credential from
`target.APIKey`. Therefore the client's `Authorization` header is
guaranteed never to reach the upstream — even if it were
hypothetically allowlisted, `ApplyAuth` overwrites it.

## 4. The actual question to brainstorm with the user

**Should `PerFormatForwardHeaders` (and the base allowlist) be
runtime-configurable, or stay hard-coded in code?**

That is the entire question. The user did not pre-decide; they
flagged it as needing its own brainstorm.

### 4.1 Why someone might want it configurable

- A provider rolls out a new beta header (`openai-beta`-like) and
  one customer needs it forwarded *today*, not after a release. Code
  change → PR → review → release → deploy is a multi-day path.
- Multi-tenant deployments where one tenant has special headers it
  needs forwarded but other tenants must not.
- Allowing experimentation (e.g. forwarding `openai-prompt-cache`
  to A/B-test prompt caching) without rebuilding.
- Some operators want a single source of truth for "what hits the
  upstream", separate from binary versioning.

### 4.2 Why hard-coding is currently the safer default

- Every header added to the allowlist is a potential PII /
  attribution / audit-trail leak. Code change → PR diff is the
  single best place to catch "wait, why are we forwarding *that*?".
- `git blame` on the allowlist tells you who added each entry and
  why; no equivalent answer for a YAML/DB list edited at runtime.
- The current set is small enough that it has been audited
  exhaustively. Runtime-config introduces an unbounded surface.
- For a compliance gateway, "operators should not be able to weaken
  egress controls without engineering review" is a defensible
  posture.
- Multi-tenant runtime-config means we now need an authorization
  model around the config itself (who can edit, who can approve,
  audit log of changes), which is a non-trivial subproblem.

### 4.3 The middle-ground options to surface

Worth offering the user as A/B/C/D before going deep on any one:

- **A. Stay hard-coded.** Document the policy. Add a unit test that
  flags any new entry in `PerFormatForwardHeaders` so PR review is
  forced. Done.
- **B. YAML-configurable, deployment-time only.** Each `*.dev.yaml`
  / production config carries a `forwardHeaderAllowlist:` map. Reload
  requires service restart. Audit lives in git on the YAML.
- **C. Admin-API configurable, hot-reload via Hub shadow.** Operators
  can update the allowlist through the admin UI; changes propagate
  to running services via the same Hub config-sync mechanism that
  governs other settings (see `docs/dev/service-call-framework.md`).
  Requires authorization model (super_admin only? compliance role?)
  and audit (`audit_log` row per change).
- **D. Per-provider in DB.** Each `Provider` row in the DB carries
  its own allowlist; no global admin path. Operators edit per
  provider via the existing provider admin UI. Reuses existing auth
  + audit infrastructure for provider edits.

Recommended default to lead with: **A**, because the team explicitly
noted that the current posture is the safer default and there is no
operator pain on record (yet). The user may overrule.

If the user picks B/C/D, the spec needs to cover at minimum:

- Authorization (who can edit)
- Audit log (every change recorded; tie into existing `audit_log`)
- Validation rules (case insensitivity; prevent obvious-bad
  headers like `authorization`, `cookie`, `x-forwarded-*` —
  authentication is provider-credential, not client-supplied)
- Hot-reload mechanism (Hub shadow vs file watch vs SIGHUP)
- Testing matrix (multi-tenant isolation if applicable)
- Documentation update (`docs/ops/` runbook)

## 5. Things that MUST come up in your brainstorm

These are non-obvious traps. Ask the user explicitly:

1. **Multi-tenant scope.** Is the allowlist global to the gateway,
   or scoped per VK / per Org? A global change leaks into every
   customer; a per-VK scope multiplies operational surface.
2. **`Authorization` and friends are special.** Even if the allowlist
   is configurable, must the system **refuse** to allowlist
   `authorization`, `cookie`, `x-api-key`, `x-amz-*` (anything that
   would leak credentials or override `ApplyAuth`)?
3. **Compliance trail.** Today, "what got forwarded" is reproducible
   from the binary version. A runtime config breaks that
   reproducibility unless every change is recorded with timestamp +
   actor in audit. Confirm with user how strict their compliance
   requirement is.
4. **Default-deny vs default-allow.** All current code is default-
   deny (drop everything not on the list). If config is added,
   stay default-deny — never introduce a "forward all
   `x-customer-*` prefixes" rule, that is an exfiltration vector.
5. **Test posture.** A unit test that snapshot-asserts the current
   list is in `proxy_test.go`. After this work, the same test
   strategy needs to apply to the runtime-loaded config (e.g.
   "default config matches the historical hard-coded list").
6. **Interaction with cache.** The SSE cache spec
   (`2026-05-06-sse-cache-design.md`) declares that the forward-
   header set is "compile-time constant" and uses that as part of
   the cache equivalence proof. If you make the allowlist
   runtime-config, the cache spec needs a small amendment (add
   header-allowlist version to the cache key, or re-prove
   equivalence under runtime change). **Coordinate with whoever is
   implementing the SSE cache spec before changing the allowlist
   surface.**

## 6. Existing test you must not break

`packages/ai-gateway/internal/handler/proxy_test.go:43` references
the per-format allowlist behavior; treat as snapshot test. If your
change makes the list dynamic, port that test to assert the
default-loaded list matches the historical hard-coded list, so
regressions are caught immediately.

`packages/ai-gateway/internal/providers/spec_adapter_forward_test.go`
exercises the merge-and-filter logic on a fixture spec — that test
should keep passing under any solution.

## 7. Suggested workflow

1. Read this document (you are here).
2. Read `CLAUDE.md` at repo root.
3. Read `spec_adapter.go:38–315` to internalize the current code.
4. Read each `spec_*/spec.go` to see the current per-format
   declarations (paths in §3.2).
5. Skim the SSE cache spec to understand the coupling
   (`2026-05-06-sse-cache-design.md`, §7.6).
6. Run the brainstorming skill (`brainstorming`) with the user.
   Lead with the A/B/C/D framing in §4.3. The user wants A/B/C/D
   options + your recommendation + free-text input — that is
   project policy.
7. Once a direction is chosen, follow the SDD pipeline:
   architecture (note any change in `docs/dev/architecture.md`)
   → requirements (`docs/requirements/`) → SDD (`docs/sdd/`) →
   OpenAPI (only if option C, where admin API surfaces are added)
   → code → tests → verify.
8. End by asking the user about commit (project policy).

## 8. What you must NOT do

- Do not change the SSE cache spec without coordinating with that
  spec's owner; the two designs are currently consistent and that
  consistency is load-bearing.
- Do not add `Authorization`, `Cookie`, `Set-Cookie`, `X-Forwarded-*`,
  or any provider-credential header (`x-api-key`, `x-goog-api-key`,
  `api-key`, `x-amz-*`) to any allowlist proposal. Even if the user
  asks, push back hard — these are credential-leak vectors and
  `ApplyAuth` would overwrite them anyway.
- Do not implement before brainstorming. The user explicitly wants a
  brainstorm-first approach on this topic.
- Do not skip the SDD pipeline (Architecture → Requirements → SDD →
  OpenAPI → Code → Tests → Verify). It is binding project policy.
- Do not write Chinese or any non-English content into repository
  files. Chat with the user can mirror their language; files cannot.

## 9. Useful greps to orient

```bash
# Current allowlist definitions
grep -rn "PerFormatForwardHeaders" packages/ai-gateway/

# The merge-and-filter site
grep -n "forwardHeaders\b" packages/ai-gateway/internal/providers/spec_adapter.go

# Where ApplyAuth lives per provider (for context on "what gets
# overwritten anyway")
grep -rn "func.*ApplyAuth" packages/ai-gateway/internal/providers/spec_*/

# Existing tests that touch the allowlist
grep -rn "forwardHeaderAllowList\|PerFormatForwardHeaders\|forwardHeaders" \
    packages/ai-gateway/ --include "*_test.go"
```

End of context brief.
