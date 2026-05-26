# Compliance Proxy Traffic Event Taxonomy

*Audience: contributors querying or extending `traffic_event` rows from the Compliance Proxy path.*

Every request handled by any of the three traffic paths produces a `traffic_event` row in PostgreSQL. The `source` column on that row identifies which path produced it: `'compliance-proxy'` for the Compliance Proxy, `'ai-gateway'` for the AI Gateway, and `'agent'` for the Desktop Agent. Within the compliance-proxy rows, two further columns — `kind` and `endpoint_type` — describe what type of AI traffic was detected. This page explains the source/kind/endpoint_type taxonomy, how Tier-1 and Tier-2 detection set those values, and what fields are populated in each case.

---

## Source column — which path produced the row

The `source` column is the primary discriminator for which traffic path captured a request:

| `source` value | Set by | Populated when |
|---|---|---|
| `'ai-gateway'` | AI Gateway handler | Request came through `/v1/*` with a virtual key |
| `'compliance-proxy'` | Compliance Proxy audit producer | Request was captured via CONNECT + TLS bump |
| `'agent'` | Desktop Agent audit producer | Request was intercepted at OS network layer |

These are mutually exclusive — the same request appears under exactly one source. When an org uses both AI Gateway and Compliance Proxy, Nexus deduplicates via `trace_id` + `external_request_id` rather than source column, because a single SDK call directed at the AI Gateway never also passes through the Compliance Proxy.

## Kind column — what type of AI content was detected

`kind` (stored as `NormalizedPayload.Kind`) describes the wire format the normalizer identified. The Compliance Proxy sets this during content extraction.

| `kind` value | Description | How set |
|---|---|---|
| `ai-chat` | Standard AI chat completions — OpenAI, Anthropic, Gemini API shape, or consumer-surface SSE chat recognized by pattern probe | Tier-1 normalizer for API traffic; Tier-2 pattern probe for consumer surfaces |
| `ai-embeddings` | Embedding request/response | Tier-1 normalizer; URL-pattern classifier picks the endpoint type before normalization |
| `http-text` | Plain text body (non-AI or unrecognized) | Tier-3 GenericHTTPNormalizer catch-all |
| `http-json` | JSON body that didn't match any AI spec | Tier-3 catch-all |
| `http-form` | `application/x-www-form-urlencoded` | Tier-3 catch-all |
| `http-binary` | Binary body | Tier-3 catch-all |

Consumer-surface traffic typically arrives as `ai-chat` after Tier-2 detection, with `protocol = "pattern-extract"` and `detectedSpec` set to a spec-specific ID (e.g., `"pattern:openai-chat-sse"`, `"cursor"`, `"gemini-web"`).

## Endpoint type column — AI endpoint classification

`endpoint_type` tells downstream components (hooks, cost estimator, analytics) which AI endpoint category was in use. The Compliance Proxy infers endpoint type from the intercepted URL + method + content-type via the shared endpoint classifier at `packages/shared/traffic/classify/`.

| `endpoint_type` | Typical URL pattern | Required `traffic_event` fields |
|---|---|---|
| `chat` | `/v1/chat/completions`, `/v1/messages`, consumer AI chat | `prompt_tokens`, `completion_tokens`, `total_tokens`, `estimated_cost_usd` |
| `embeddings` | `/v1/embeddings`, `*/embedContent*` | `prompt_tokens`, `total_tokens`, `estimated_cost_usd` |
| `image_generation` | `/v1/images/generations` | `estimated_cost_usd`; image metadata in `metadata` JSONB |
| `tts` | `/v1/audio/speech` | `prompt_tokens` (input chars), `estimated_cost_usd` |
| `stt` | `/v1/audio/transcriptions` | `completion_tokens` (transcript equivalent), `estimated_cost_usd` |
| (unclassified) | Any unrecognized host/path | `kind=http-*`; no token/cost fields |

## Tier-1 vs Tier-2 detection and its effect on field population

Tier classification determines how fully the `traffic_event_normalized` sidecar is populated. The proxy runs the same three-tier normalizer pipeline as the AI Gateway and Hub:

### Tier-1 (per-adapter precision)

Applies to standard JSON API traffic from known providers. Full `NormalizedPayload` extraction: `Messages[]` with roles and content, `Model`, `FinishReason`, `Tools[]`, and complete `Usage` (prompt tokens, completion tokens, cache tokens).

Tier-1 normalizers registered by the Compliance Proxy:

| Adapter / format | Key | When matched |
|---|---|---|
| OpenAI Chat | `openai` | `api.openai.com` API traffic |
| Anthropic Messages | `anthropic` | `api.anthropic.com` API traffic |
| Gemini generateContent | `gemini` | `*.googleapis.com` API traffic |
| OpenAI-compatible | `deepseek`, `groq`, `moonshot`, … | 14 registered aliases |
| ChatGPT web | `chatgpt-web` | `chatgpt.com` consumer traffic |
| Claude web | `claude-web` | `claude.ai` consumer traffic |
| Gemini web | `gemini-web` | `gemini.google.com` consumer traffic |
| Cursor IDE | `cursor` | `api2.cursor.sh` Connect-RPC protobuf traffic |

### Tier-2 (pattern probe + NonJSONDetector framework)

Applies when Tier-1 has no registered entry for an adapter ID, or when Tier-1 returns low confidence. Tier-2 runs in two passes:

**Pass A — JSON multi-spec probe.** Byte-sniffs the body (ignoring the Content-Type header, which is often wrong for consumer traffic), then iterates 7 known chat-request specs and 7 response specs (OpenAI/Anthropic/Gemini API + ChatGPT/Claude/completions-legacy). Picks the highest-confidence match.

**Pass B — NonJSONDetector chain.** For binary or non-JSON wire formats, iterates the `NonJSONDetectors` registry in `packages/shared/transport/normalize/extract/detector.go`:

| Detector | Wire format | Matched hosts |
|---|---|---|
| `ConnectRPCProtobufDetector` | 5-byte Connect-RPC envelope + protobuf | `cursor.com` (Cursor IDE streaming chat) |
| `BatchExecuteDetector` | `f.req=` form-urlencoded + `)]}'`-prefixed JSON | `gemini.google.com`, other Google web AI surfaces |

For Tier-2 matches, `kind=ai-chat` and `detectedSpec="pattern:<spec-id>"`. Token counts and usage are populated when detectable from the wire shape; for consumer surfaces (ChatGPT web, Claude.ai), readable text is the primary output and token counts may be absent. This is by design — the text-first normalizer policy means the compliance hook pipeline receives readable prompt + response text, which is sufficient for PII detection and keyword filtering.

### Tier-3 (GenericHTTPNormalizer catch-all)

Fires when neither Tier-1 nor Tier-2 claims the body with sufficient confidence. Produces `kind = http-text | http-json | http-form | http-binary`. The text view is always populated; token fields are empty.

## Source-distinct rows and deduplication

Because all three traffic paths write to the same `traffic_event` table, queries typically filter by `source`:

```sql
-- Compliance-proxy captured rows in the last hour
SELECT id, target_host, model_name, prompt_tokens, hook_decision
FROM traffic_event
WHERE source = 'compliance-proxy'
  AND timestamp >= now() - interval '1 hour'
ORDER BY timestamp DESC;
```

The `target_host` column (populated from the CONNECT SNI) is the primary join key for compliance-proxy rows, as there is no `provider_id` foreign key in the proxy path (the proxy does not route to pre-configured providers — it forwards wherever the application was already going).

---

## Canonical docs

- [`endpoint-typology-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/foundation/endpoint-typology-architecture.md) — five wire classes, endpoint_type discriminator, traffic_event polymorphism
- [`compliance-proxy-details-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/compliance-proxy/compliance-proxy-details-architecture.md) — subsystem map, audit emission path
- [`traffic-event-lifecycle.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/flows/traffic-event-lifecycle.md) — how traffic_event rows travel from emitter to Postgres

**Adjacent wiki pages**: [Compliance Proxy Normalization](Compliance-Proxy-Normalization) · [Compliance Proxy Overview](Compliance-Proxy-Overview) · [AI Gateway Error Taxonomy](AI-Gateway-Error-Taxonomy) · [Three Traffic Paths](Three-Traffic-Paths)
