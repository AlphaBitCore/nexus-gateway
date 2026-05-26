# add-provider-adapter

Walk the multi-step procedure when adding a new provider adapter (vendor / wire-format / consumer surface).

Use this skill when:

- A new vendor / model family is being onboarded.
- A new wire format (binary protocol, custom SSE flavour, gRPC variant) is being captured.

The architectural binding is `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` §3a (**7 binding rules**). The Cursor rule `provider-adapter-canonical-openai.mdc` enforces them in the IDE; this skill is the canonical **procedure**. To audit an existing adapter against the rules, use `.claude/skills/adapter-conformance-check/` instead.

---

## Step 0 — Read the binding before writing code

Read `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md`, **especially §3a**. The 7 rules:

1. Canonical format = OpenAI chat-completions shape. New canonical fields need an architecture-doc PR.
2. Non-OpenAI adapters own their bidirectional translation (`SchemaCodec.EncodeRequest` / `DecodeResponse`). OpenAI side is the identity codec.
3. Per-model wire quirks live in the adapter that talks to that wire — never in cross-adapter case-statements in shared helpers. Wire them via `AdapterSpec.PassthroughRewrite` rather than editing the generic dispatcher.
4. `nexus.ext.<provider>.<key>` is the canonical extension namespace for fields with no clean OpenAI mapping.
5. `SchemaCodec.EncodeRequest` accepts canonical-or-empty only. Ingress-format bodies MUST be canonicalised first via `canonicalbridge.IngressChatToCanonical`.
6. Streaming + non-streaming are both in scope. A codec rule that applies to one must apply to the other.
7. Every prefix-list (model-X-rejects-param-Y) must be backed by **empirical 400 evidence** — observed trace_id or direct test call — documented in the comment above the list.

---

## The 9-step procedure

### 1. Decide the wire family

Most adapters reuse one of three families:

- **OpenAI-shape** — DeepSeek / Moonshot / GLM / MiniMax / etc.
- **Anthropic-shape** — Anthropic + downstream resellers.
- **Gemini-shape** — Google Gemini + consumer Gemini-web.

If your provider doesn't fit any: it needs a fresh codec from scratch (rare). Pause and discuss.

### 2. Implement the `SchemaCodec` (canonical → wire, wire → canonical)

For non-OpenAI adapters, create `packages/ai-gateway/internal/providers/spec_<provider>/codec.go`:

```go
type Codec struct{}

func (Codec) EncodeRequest(ctx context.Context, canonical CanonicalChatRequest) ([]byte, error) {
    // canonical → vendor wire shape
    // Use canonicalext.Get for provider-specific extension fields.
    // Apply Rule 3 quirks here (per-model wire deprecations).
}

func (Codec) DecodeResponse(ctx context.Context, raw []byte) (CanonicalChatResponse, error) {
    // vendor wire shape → canonical
    // Use canonicalext.Set for fields with no canonical mapping.
    // canonicalext.WarnOnce for unrecognised canonical fields the wire doesn't carry.
}
```

The OpenAI codec is the identity codec — it pass-throughs canonical bytes verbatim. **Never add `if provider == X` branches in the OpenAI codec.**

### 3. Implement the high-level `Adapter` interface (registry-facing)

`packages/shared/traffic/adapters/<provider>/`:

```go
type Adapter struct {
    ProviderID string
    codec      Codec
}

func (a *Adapter) Manifest() Manifest { ... }
func (a *Adapter) ExtractText(raw []byte) (Extracted, error) { ... }  // text-first hook eval
```

`ExtractText` is the cheap text-only extractor for hook evaluation (cross-ref `text-first-normalizer.mdc`). It does NOT have to produce structured usage — text suffices.

### 4. Wire the codec into `PrepareBody`

`PrepareBody` is the dispatch entry. Callers MUST canonicalise ingress-format bodies first:

```go
// Caller (AI-Gateway middleware) when the ingress is e.g. Anthropic /v1/messages:
canonical, err := canonicalbridge.IngressChatToCanonical(ingress, body, target)

// Then PrepareBody invokes SchemaCodec.EncodeRequest(canonical, ...).
```

This is Rule 5. Skipping canonicalisation causes the OpenAI identity codec to forward the ingress body verbatim, and the upstream returns 400.

### 5. Register adapter + provider id

```go
// packages/shared/traffic/adapters/<provider>/register.go
func init() {
    adapters.Register(&Adapter{ProviderID: "<provider-id>", ...})
}
```

Add the constant to `packages/shared/traffic/adapters/providers.go`. **Don't call `Lookup` with a typo-prone string literal.**

### 6. Seed provider + initial models

`tools/db-migrate/seed/seed.ts` — add a `Provider` row + per-`Model` entries with pricing + capabilities. If your provider is in the prod-data baseline, also update `tools/db-migrate/seed/prod-data.sql`.

### 7. Map provider errors to `ErrorClass`

`packages/ai-gateway/internal/execution/executor/classify.go` (or per-adapter classifier):

```go
case strings.Contains(body, "rate_limit"):
    return ErrorClassRate429
case strings.Contains(body, "content_filter"):
    return ErrorClassContentFiltered
```

Cross-ref `docs/developers/architecture/cross-cutting/safety/error-taxonomy-architecture.md`.

### 8. Token-field stamp sweep (binding, 5 sites)

If the provider returns new usage fields (e.g., Anthropic's `cache_creation_input_tokens`):

1. Add to `Usage` struct in `providers/types.go`.
2. Add column to `traffic_event` schema.
3. Stamp in all 5 sites: `handleNonStream`, `handleStream`, `cacheStoreNonStream`, `cacheStoreStream`, `cacheRead*`.

Cross-ref `token-field-stamp-sweep.mdc`.

### 9. Smoke test (streaming + non-streaming, per Rule 6)

```bash
# Non-stream:
curl -H "Authorization: Bearer vk-..." https://nexus.local/v1/chat/completions \
  -d '{"model":"<new-model>","messages":[{"role":"user","content":"hi"}]}'

# Stream:
curl ... -d '{"model":"<new-model>","stream":true,"messages":[...]}'

# Mixed-ingress: if your model supports Anthropic-ingress, also test:
curl ... -H "Anthropic-Version: 2023-06-01" /v1/messages \
  -d '{"model":"<new-model>", "messages":[...]}'

# Confirm Postgres rows:
docker exec postgres psql -U postgres -d nexus_gateway \
  -c "SELECT provider, model, error_class FROM traffic_event ORDER BY emitted_at DESC LIMIT 3"
```

Both stream + non-stream rows must show non-null usage. Run a known wire-quirk scenario (e.g., for Anthropic models that deprecate `temperature`, send a request **with** `temperature` and verify the codec strips it cleanly on both stream and non-stream).

---

## Verification before merge

```bash
go test -race -count=1 ./packages/shared/traffic/adapters/<provider>/...
go test -race -count=1 ./packages/ai-gateway/...
npm run check:arch-doc-triggers
```

Audit the 7 binding rules (Rule 1-7 in `provider-adapter-canonical-openai.mdc`). For an automated rule sweep, invoke the `adapter-conformance-check` skill.

---

## Output (PR description)

```
Provider adapter added:
- Provider id: <id>
- Display name: <Display Name>
- Wire family: OpenAI | Anthropic | Gemini | custom
- §3a binding rules:
  * Rule 1 (canonical = OpenAI; no unilateral canonical fields): ✓
  * Rule 2 (own bidirectional codec; OpenAI side untouched): ✓
  * Rule 3 (per-model quirks in own adapter; wired via PassthroughRewrite): ✓
  * Rule 4 (nexus.ext.<provider>.* used for non-canonical fields): ✓ (used | n/a)
  * Rule 5 (IngressChatToCanonical called by all ingress paths): ✓
  * Rule 6 (stream + non-stream both covered): ✓
  * Rule 7 (empirical evidence in every prefix-list comment): ✓ (or n/a if no prefix-list added)
- adapter-conformance-check run: PASS
- Token-field sweep (5 sites): ✓
- Error class mapping: ✓
- Smoke test (stream + non-stream + mixed-ingress): 3/3 PASS
- Seed: provider + N models added
```
