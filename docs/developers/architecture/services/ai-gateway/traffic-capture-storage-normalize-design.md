# Traffic Capture / Storage / View-Time Normalize — Storage-Frame Design

Scope: AI Gateway audit capture · `traffic_event` storage · Control-Plane view-time normalize (Traffic drawer)
Related: [normalization-architecture.md](./normalization-architecture.md) · [hook-architecture.md](./hook-architecture.md) · [provider-adapter-architecture.md](./provider-adapter-architecture.md) · [audit-pipeline-architecture.md](../../cross-cutting/observability/audit-pipeline-architecture.md)

> **Status: implemented.** The design below is shipped. `ingress_format` is persisted on `traffic_event` (the AI Gateway stamps `rec.IngressFormat` onto the audit message → Hub insert; agent / compliance-proxy rows carry the domain-matched adapter id). The `traffic_event_normalized` sidecar is **not written on the audit write path** (write-frozen — retained for historical rows and older-agent uploads), and the Control Plane recomputes the normalized projection at view time from the stored (already-redacted) body, keyed on `ingress_format` (empty ⇒ path/sniff fallback). See [audit-pipeline-architecture.md](../../cross-cutting/observability/audit-pipeline-architecture.md) §10.2.

## 1. Problem

The Traffic drawer's normalized view is recomputed **at view time** from the captured request/response bodies (the `traffic_event_normalized` sidecar is not written on the write path — see [normalization-architecture.md](./normalization-architecture.md) §5.2 and [audit-pipeline-architecture.md](../../cross-cutting/observability/audit-pipeline-architecture.md) §10.2). The problem this design solved: on a fresh prod data set before the fix, **~294 / 588 (50%) of 200-status events rendered raw JSON with the wrong detected protocol** (`generic-http`) instead of the canonical `ai-chat` projection.

The break is **exactly** the set of events where the **ingress wire protocol differs from the upstream provider's adapter** — i.e. cross-protocol transcoding (client speaks Anthropic `/v1/messages`, routed to an OpenAI provider; client speaks Gemini `/v1beta`, routed to Anthropic; etc.).

## 2. Evidence (ground truth from prod)

**Smoking gun — identical body, different adapter, opposite result.** Two Gemini-ingress requests with byte-identical Gemini wire bodies:

| stored request body | `adapter_type` | `target_path` | detected |
|---|---|---|---|
| `{"contents":[{"parts":[{"text":…}]}]}` | openai | /v1/chat/completions | ✅ `gemini-generate` |
| `{"contents":[{"parts":[{"text":…}]}]}` | anthropic | /v1/messages | ❌ `generic-http` |

The only difference is the **meta** the view-time normalize is fed. The body is the same Gemini ingress format in both.

**The stored bodies are in the INGRESS (client-facing) wire frame**, both directions:
- Gemini ingress → request stored as `{"contents":[{"parts"…}]}` (Gemini format).
- Anthropic ingress → response stored as `{"content":[{"text"…}]}` (Anthropic Messages response format).

**Cross-product scan (588 events):** 13 / 22 (ingress × adapter) combos broken; broken iff ingress wire ≠ upstream adapter. Same-frame combos (openai→openai, `/v1/messages`→anthropic) are clean.

## 3. Root cause

`GetTrafficEventForNormalize` (CP store) derives the normalize meta from the **upstream provider**:

```sql
SELECT COALESCE(pr.adapter_type, ''),               -- upstream provider adapter
       COALESCE(a.target_path, a.path, ''),          -- UPSTREAM path
       …
FROM traffic_event a
LEFT JOIN "Provider" pr ON pr.id = COALESCE(a.routed_provider_id, a.provider_id)
```

The registry resolves a normalizer by `AdapterType` + `EndpointPath` (Tier-1 keyed lookup). The stored bodies are **ingress-framed**, so when ingress ≠ upstream the wrong codec is selected; it either mis-claims or declines the body in a way that also defeats the Tier-1.5 content sniffers, and resolution falls through to the Tier-3 `*:*:*` generic-http catch-all. `generic-http` is a *successful* fallback (status `ok`), so it is not caught by any status assertion — it silently renders raw JSON.

**The audit-time writer uses the correct ingress meta** (it knows the ingress at request time); substituting upstream provider context at view time would break the documented "view-time recompute is byte-identical to what the writer would have stored" guarantee for cross-protocol ingress.

## 4. The unifying principle — everything we store is the INGRESS frame

This is an invariant in the data plane, not a coincidence:

- **Success response** — stored after upstream→canonical→ingress transcoding = exactly what the client received.
- **Error response** — `writeIngressError` **explicitly reshapes the error to the ingress wire shape** before storing (anthropic→`{"type":"error"}`, gemini→`{"error":{code}}`, responses→Responses error, openai→proxy_error) using `rec.IngressFormat`.
- **Redact** — `RewriteRequestBody`/`RewriteResponseBody` rewrite **in place**, masking only matched sensitive spans and **preserving the JSON wire structure** → still valid ingress wire.
- **Block** — same rewrite path; stores the rewritten ingress body + the block decision.
- **Request** — captured at admission *before* upstream translation = the ingress request.

**Therefore the normalize must always use the INGRESS wire spec.** The provider `adapter_type` is the right hint for "what hit the upstream", but we never store that body — we store the client-facing one.

### Why ingress is the correct audit truth (cross-protocol)

1. The gateway's purpose is **compliance governance of what users send/receive**. The audit record should reflect the user's actual interaction in their protocol.
2. Storing ingress **exposes ingress-side transcoding bugs**; storing upstream would hide them.
3. **Cost/tokens are unaffected** — they are extracted into dedicated columns from usage fields, not re-derived from the body.
4. The **canonical projection is provider-agnostic** — normalizing the ingress body yields the same OpenAI-shape canonical regardless of ingress protocol, so the drawer renders consistently.

## 5. The enabler — `rec.IngressFormat` (now persisted)

`rec.IngressFormat` (set from `resolved.BodyFormat`) carries the **authoritative ingress wire format** — `openai` / `openai-responses` / `anthropic` / `gemini` / … — and `writeIngressError` relies on it. It is now **persisted to `traffic_event.ingress_format`** (the fix below); before that, the view-time path was forced to re-derive (wrongly) from the provider adapter.

`resolved.BodyFormat` is known from **route registration** (which ingress endpoint was hit) + the optional `x-nexus-aigw-body-format` override — available at `newProxyState`, *before* VK auth. `rec.IngressFormat` is **already stamped there** (in the Record literal built by `newProxyState` before any stage runs), so even early-rejection rows (VK-invalid, rate-limited, malformed body) carry it. The original gap was purely that the field was never persisted to `traffic_event`; persisting it (carried through the audit→mq message and the Hub insert) is what lets the view-time recompute read it.

## 6. Pipeline-position determines what is stored

Admission order: **① VK auth (headers only, before body read) → ② rate limit → ③ body read + model extract → ④ stamp RequestBody → ⑤ build canonical context (stamps `rec.IngressFormat`)**. `defer finalizeAudit()` covers every exit, so **every rejection still writes a `traffic_event`**.

| case | request body | response body | `ingress_format` | normalize / drawer |
|---|---|---|---|---|
| Normal | ingress request | ingress response (post-transcode) | ✓ | use it → `ai-chat` ✅ |
| Redact | ingress request, spans masked, structure intact | same | ✓ | normalizes normally; spans stored separately |
| Block | rewritten/masked offending content + decision | block envelope (ingress error shape) | ✓ | request normal; response is error envelope |
| Upstream error | ingress request | ingress error envelope (reshaped) | ✓ | non-200 → error view |
| Internal error | ingress request | ingress error envelope | ✓ | non-200 → error view |
| **Model authz fail** (our side) | ingress request | ingress error envelope | ✓ | request normal; response is error |
| **VK invalid** | **empty (by security design)** | auth error envelope | ✓ (after early stamp) | no body → nothing to normalize |

**VK-invalid stores no body by design** — auth runs before body read precisely so an unauthenticated caller cannot force a full body read or get attacker-controlled bytes persisted. This is correct; the row is an auth-failure record (status 401 + error code + key fingerprint), not a normalize target.

**Block (response direction)** is the one genuine policy choice: the upstream returned content that a hook blocked from the client. The **blocked content is stored masked** (compliance's value is auditing *what* was blocked) plus the decision; the client-facing block envelope is reconstructable from the decision columns.

## 7. Design

Because this is a **fresh-dev deployment, no backfill of old rows is required** (the current smoke events are throwaway traffic; re-issue traffic after the fix).

1. **Data plane**
   - `rec.IngressFormat` is already stamped at `newProxyState` (no code move needed). **Carry it through to persistence**: add `ingress_format` to the audit→mq message and the hub insert.
   - **Persist `ingress_format`** as a small string column on `traffic_event`, `@default("")` (≈10 bytes; this is a format tag, not the full normalized payload, so it does not reintroduce the sidecar's space/CPU cost).
2. **Control Plane read path**
   - `GetTrafficEventForNormalize` feeds the normalizer `AdapterType = ingress_format` and `EndpointPath = a.path` (the ingress path), for **both** directions. Drop the `Provider.adapter_type` join and the `target_path` preference. (Source-agnostic: cp/agent rows have empty `ingress_format` → the same path-only fallback — behavior-preserving.)
3. **Drawer / normalize semantics**
   - Branch on `status_code`: 200 expects a chat/embedding canonical; non-200 (401/403/429/4xx/5xx) renders a **typed Error projection** (code/message/type) with a plain-language, i18n-keyed badge ("Gateway/Provider error — not chat content"), visually distinct from chat bubbles and the neutral Structural badge.
4. **Registry robustness** — add explicit `openai-responses::/v1/responses` and `gemini::/v1beta/…:generateContent`-class keys, or document the deliberate reliance on the adapter-only / path-only fallbacks so a future tightening can't silently regress these two ingresses.
5. **Smoke** — already lazy-normalize-aware; after the fix the view-time sample shows the ingress-correct protocol (`anthropic-messages` / `gemini-generate` / `openai-responses`) instead of `generic-http`, and non-200 rows are skipped.

### Registry mapping that makes this work

The registry already keys ingress specs by `<format>::<ingress-path>`: `anthropic::/v1/messages`, `gemini` / `gemini::…generateContent`, `openai::/v1/chat/completions`, `…::/v1/responses`. Feeding `AdapterType = ingress_format` + `EndpointPath = a.path` selects the correct ingress codec; `meta.Direction` disambiguates request vs response within one codec.

## 8. Scope of change

- `packages/ai-gateway/internal/ingress/proxy/proxy.go` (or `stage_context.go`) — early `IngressFormat` stamp.
- audit record → mq message → hub insert — carry `ingress_format` through to the `traffic_event` column.
- `tools/db-migrate/schema/traffic.prisma` — new `ingress_format` column (+ `schema-extras` if needed).
- `packages/control-plane/internal/traffic/store/trafficstore/traffic_event_normalized.go` — read `ingress_format` + `a.path`; drop the provider join.
- Drawer/handler — status-based branch (may already hold).
- Docs lockstep: this doc + normalization-architecture.md; smoke note.

## 9. Risks / open questions

- **R1** Does any ingress legitimately store the response in **upstream** frame (e.g. passthrough / raw proxy mode, or `/v1/responses` cross-format rejection)? If so, those need their own `ingress_format` value or a per-direction tag. (Believed no — passthrough still returns the ingress shape — but to be verified by the architecture review.)
- **R2** Compliance-proxy and Agent paths share the same normalize registry but capture differently (the `Provider` field can carry a host/tool name). Does persisting `ingress_format` on the AI-gateway path interact with those? (These do not flow through `ServeProxy`; out of scope here, but confirm no shared-column assumptions.)
- **R3** Streaming (SSE) responses: the stored response body is the reassembled ingress SSE; confirm `ingress_format` + `stream=true` selects the SSE codec correctly for every ingress.
- **R4** Embeddings: `/v1/embeddings` (openai) and `/v1beta/…:embedContent` (gemini) — confirm `ingress_format` + path resolves the embedding normalizers (path-keyed entries are more specific and should still win).
- **R5** The new column is a shipped-contract addition to `traffic_event` — additive only, no migration of old rows (fresh dev), but the mq message + hub insert must stay backward compatible if a stale binary writes without it (empty → falls through, non-200/error semantics still hold).
