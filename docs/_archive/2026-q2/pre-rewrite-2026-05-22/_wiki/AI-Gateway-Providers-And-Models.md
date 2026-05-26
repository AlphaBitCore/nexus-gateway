# AI Gateway Providers And Models

The AI Gateway ships with a catalog of 50+ provider adapter registrations covering API surfaces, web/browser interfaces, and IDE coding tools. Every request is resolved against the catalog — routing rules reference provider and model rows by UUID, and the gateway uses the model row's price fields as the single source of truth for cost stamping. This page describes what is covered, how capability flags work, and how the admin catalog is managed.

---

## Provider families

Providers are grouped by surface type. Each adapter under a family handles one wire format.

### API surfaces

Programmatic API access via REST or HTTP/2. Most vendors expose an OpenAI-compatible chat-completions endpoint; those that don't have dedicated adapters.

| Vendor | Adapter | Wire format | Notes |
|---|---|---|---|
| OpenAI | `openai` | OpenAI native | `/v1/chat/completions`, `/v1/responses`, `/v1/embeddings` |
| Anthropic | `anthropic` | Anthropic Messages | `/v1/messages`; `thinking` via `nexus.ext.anthropic.thinking` |
| Google Gemini | `gemini` | Gemini native | `:generateContent`, `:batchEmbedContents` |
| Azure OpenAI | `azure-openai` | OpenAI variant with deployments | SUFFIX host match `*.openai.azure.com` |
| AWS Bedrock | `bedrock` | Bedrock multi-model envelope | Covers Anthropic-on-Bedrock + Llama-on-Bedrock |
| Google Vertex AI | `vertex` | Vertex multi-model | Regional + global hosts |
| DeepSeek | `deepseek` | OpenAI-compat + `reasoning_content` extension | |
| Mistral | `mistral` | OpenAI-compat | |
| xAI Grok | `xai` | OpenAI-compat | |
| Groq | `groq` | OpenAI-compat | Inference-only platform |
| Perplexity | `perplexity` | OpenAI-compat (Sonar models) | |
| Together AI | `together` | OpenAI-compat | Both `api.together.xyz` + `api.together.ai` |
| Fireworks AI | `fireworks` | OpenAI-compat | |
| Moonshot Kimi | `moonshot` | OpenAI-compat | Fixed-temp models handled via `PassthroughRewrite` |
| MiniMax | `minimax` | MiniMax native | |
| Zhipu GLM | `glm` | GLM native | |
| Cohere | `cohere` | Cohere native | |
| HuggingFace Inference | `huggingface` | Multiple | Hosted-endpoint subdomains use SUFFIX matching |
| Replicate | `replicate` | Replicate native (poll-based) | Asynchronous prediction model |

### Web and browser surfaces

Browser-based or desktop-client traffic uses private wire formats distinct from each vendor's API surface.

| Surface | Adapter | Host |
|---|---|---|
| ChatGPT | `chatgptweb` | `chatgpt.com` |
| Claude.ai | `claudeweb` | `claude.ai` |
| Gemini | `geminiweb` | `gemini.google.com` (batchexecute envelope) |
| OpenAI Platform | `openaiplatformweb` | `platform.openai.com` |
| Anthropic Console | `anthropicconsoleweb` | `console.anthropic.com` |
| Google AI Studio | `googleaistudioweb` | `aistudio.google.com` |
| Microsoft Copilot | `copilotmsweb` | `copilot.microsoft.com` |
| M365 Copilot | `m365copilotweb` | `m365.cloud.microsoft` |
| Perplexity Web | `perplexityweb` | `www.perplexity.ai` |
| xAI Grok Web | `grokweb` | `grok.com` + `x.com` paths |
| DeepSeek Web | `deepseekweb` | `chat.deepseek.com` |
| Moonshot Kimi Web | `kimiweb` | `kimi.moonshot.cn` |
| Mistral Le Chat | `mistralweb` | `chat.mistral.ai` |
| Poe | `poeweb` | `poe.com` |
| GitHub Copilot Web | `githubcopilotweb` | `github.com/copilot` |

### IDE and coding-agent surfaces

| Tool | Adapter | Host |
|---|---|---|
| Cursor | `cursor` | `api2.cursor.sh`, `api3.cursor.sh` (Connect-RPC + protobuf) |
| GitHub Copilot | `githubcopilot` | `api.githubcopilot.com` |
| Codeium / Windsurf | `codeium` | `server.codeium.com`, `inference.codeium.com` |
| Tabnine | `tabnine` | `api.tabnine.com` |
| Continue.dev | `continuedev` | `*.continue.dev` |

## Model catalog

Every provider-model combination that the gateway can route to is represented as a `Model` row in the database. The row carries:

- `modelCode` — the customer-facing identifier (e.g., `gpt-4o`, `claude-sonnet-4-6`). This is what callers put in the `model` field.
- `providerModelID` — the exact string sent to the upstream (may differ from `modelCode` after routing rewrite).
- `inputPricePerMillion`, `outputPricePerMillion` — token prices in USD per million tokens, stamped onto every `traffic_event` as `estimated_cost_usd`.
- `cachedInputReadPricePerMillion`, `cachedInputWritePricePerMillion` — provider prompt-cache token prices where applicable.
- `maxContextTokens`, `maxOutputTokens` — capacity caps consumed by smart routing candidate filtering.
- `features` — capability tags: `chat`, `embedding`, `vision`, `tool-calling`, `reasoning`, `long-context`, `fast`, `cheap`.
- `enabled` — only enabled models are offered via `/v1/models` and eligible in routing rules.

The `Model` row is the **single source of truth** for cost. Prices are not fetched at request time; the gateway reads the current row price at the moment it stamps the traffic event. Updating a model's price takes effect for all future requests without a restart.

## Capability matrix

The gateway enforces capability pre-filtering before routing dispatch. A routing rule that targets a model whose capability flags don't match the request type (for example, routing an embedding request to a chat-only model) is caught by the admin guard at rule creation time and rejected.

| Capability flag | What it gates |
|---|---|
| `chat` | Eligible for `/v1/chat/completions`, `/v1/messages`, `/v1/responses` |
| `embedding` | Eligible for `/v1/embeddings` |
| `tool-calling` | Eligible when request carries `tools[]` array |
| `reasoning` | Eligible when request targets an o-series / thinking model |
| `vision` | Eligible for multimodal input (image content blocks) |
| `long-context` | Surfaced in smart routing candidate scoring for long prompts |

Smart routing uses these tags plus token prices and context-window limits to rank candidates. See [AI Gateway Smart Routing](AI-Gateway-Smart-Routing) for how the catalog feeds the dispatch call.

## Admin management

The Providers & Models page at `/ai-gateway/providers` (`admin:provider.read` IAM action) shows the full catalog. The onboard workflow:

1. Create a provider entry (name, base URL, adapter type).
2. Add a credential for that provider (API key, encrypted at rest via AES-256-GCM).
3. Use the "Test connection" button — the gateway calls the provider's health endpoint and surfaces the exact error on failure.
4. Add model rows with pricing and capability flags.
5. Enable the models; they appear in `/v1/models` and become eligible in routing rules.

Provider model catalogs are managed via `provider-templates/<name>.json` files (JSON templates seeded at bootstrap). Model prices are updated via the admin API or directly in the seed when catalog data changes. Every admin mutation emits an audit event.

---

## Canonical docs

- [`provider-coverage.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/provider-coverage.md) — Full provider/adapter matrix across API, web, and IDE surfaces with priority tiers
- [`provider-adapter-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md) — §3a Rules 1-8 governing every adapter; codec + stream + error normalizer structure
- [`cost-estimation-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md) — How Model row prices are stamped at five sites in the proxy

**Adjacent wiki pages**: [AI Gateway Overview](AI-Gateway-Overview) · [AI Gateway Provider Adapters](AI-Gateway-Provider-Adapters) · [AI Gateway Routing Rules](AI-Gateway-Routing-Rules) · [AI Gateway Smart Routing](AI-Gateway-Smart-Routing) · [Canonical Vs Wire Format](Canonical-Vs-Wire-Format)
