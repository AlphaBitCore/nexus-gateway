# E28 — Hub canonical routing (cross-ingress chat)

## Functional requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-H1 | For `chat_completions`, the AI Gateway SHALL accept routing from Anthropic Messages ingress to any provider whose `SchemaCodec` can encode from canonical OpenAI chat JSON. | Must |
| FR-H2 | For `chat_completions`, the AI Gateway SHALL accept routing from Gemini `generateContent` ingress (including Vertex-native wire, same JSON shape) to any such provider. | Must |
| FR-H3 | Successful **non-streaming** responses SHALL be returned in the same ingress wire shape the client used (including Anthropic and Gemini native envelopes). | Must |
| FR-H4 | OpenAI-shaped ingresses (`openai`, `deepseek`, `glm`, `azure-openai`) SHALL continue to support routing to all registered provider formats as today. | Must |
| FR-H5 | **Streaming** requests where ingress SSE framing is incompatible with the upstream target without a transcoder SHALL fail fast with HTTP 400 and error type `cross_format_stream_unsupported`. | Must |
| FR-H6 | **Embeddings** SHALL allow routing only when ingress wire format equals the target provider format (same-format only) until an embeddings codec re-opens cross-format pairs (SDD `docs/developers/specs/e28/e28-s6-canonical-hub-completeness.md`, T-EMBED-1 / optional T-EMBED-2). **Model-list** routes SHALL retain the legacy rule (same format or OpenAI ingress only). | Must |

## Translation surfaces (S1–S6)

The six chat translation surfaces, task references, and streaming policy are defined in SDD **`docs/developers/specs/e28/e28-s6-canonical-hub-completeness.md`** Section 2.2 and Section 4 (tasks T-CANON-SUBSET through T-DOC-CROSS-REFS). Implementations live under `packages/ai-gateway/internal/execution/canonicalbridge` and per-provider `SchemaCodec` / `StreamDecoder` packages.

| Surface | Summary |
|---------|---------|
| S1 | Native ingress request JSON → canonical OpenAI chat JSON |
| S2 | Canonical request JSON → provider-native request JSON (`SchemaCodec.EncodeRequest`) |
| S3 | Provider-native response JSON → canonical (`SchemaCodec.DecodeResponse`) |
| S4 | Canonical response JSON → native ingress response JSON |
| S5 | Provider SSE / stream frames → `providers.Chunk` (`StreamDecoder`) |
| S6 | Canonical chunk → ingress SSE (transcoder interface; implementations deferred) |

## Non-functional

- **NFR-H1** — Hub translation errors MUST surface as 502/400 with structured JSON; they MUST NOT leak upstream secrets.
- **NFR-H2** — Routing-simulate MUST expose `schemaMode` consistent with live compatibility for chat when the canonical bridge is wired.

## Constraints

- Hub request/response mappers intentionally support the **same text-first subset** as the existing Anthropic and Gemini codecs (tools best-effort where mirrored).
- MiniMax native `chatcompletion_pro` and Bedrock/Vertex native ingress are **out of scope** for hub ingress until explicit parsers exist.

## Glossary

- **Canonical (hub)** — Internal OpenAI `chat.completions` JSON for the chat endpoint.
- **Ingress format** — Wire shape implied by the HTTP path (and optional `X-Nexus-Body-Format` on OpenAI-compat routes).
