---
doc: provider-coverage
area: service
service: ai-gateway
tier: 1
updated: 2026-05-20
---

# Provider Coverage Plan

Status: **rolling tracker for the multi-phase provider onboarding work** (per
`docs/users/product/architecture.md` and `CLAUDE.md` mandatory workflow). Each row in the
tables below maps to one *phase* of the onboarding pipeline; a phase ships an
ai-gateway provider adapter, a `shared/traffic` adapter, a
`provider-templates/<name>.json` catalog, an `index.json` entry, one or more
`InterceptionDomain` builtin seed rows, and full Go test coverage built around
real-format fixtures (no mocked wire bytes).

This document is the **source of truth for "what is onboarded vs still missing"**. Status reflects the live state of `packages/shared/traffic/adapters/`. Once a remaining phase ships it should be ticked off here in the same PR.

## Audience and scope

- **Audience**: Nexus Gateway engineers and reviewers. Not user-facing.
- **Geographic focus**: North America. Globally popular Chinese providers
  (DeepSeek, GLM, Moonshot, MiniMax) are included because enterprise users
  outside China still hit them.
- **Modality focus**: text only — chat, coding, and agent traffic, including
  tool calls and MCP-formatted tool invocations carried inside provider
  responses. Image, video, and audio surfaces are **out of scope** for this
  wave.
- **Surface types**: each vendor that exposes both an *API surface* (programmatic
  access from corporate code) and a *Web/console surface* (browser or desktop
  client) is split into two phases with **distinct adapters**, because the
  wire formats differ and the API adapter cannot parse the web traffic.

## Adapter status legend

| Symbol | Meaning |
|---|---|
| ✅ | Existing adapter in `packages/shared/traffic/adapters/builtins.go` is correct for this surface; only verification + tests needed. |
| 🟡 | `generic-jsonpath` adapter can serve the surface acceptably; a dedicated adapter would improve efficiency and maintainability. |
| ❌ | No adapter exists for this wire format; a new Go package under `packages/shared/traffic/adapters/<name>/` must be written, registered in `builtins.go`, and added to `manifest.json`. |

Tool-use extraction (function-calling, MCP-formatted tool invocations) for
each existing adapter is marked **TBD** until audited as part of its phase.

## Table A — Foreign LLM API surfaces

Direct API access from corporate code or SDKs. These are OpenAI-style RESTful
endpoints; most expose a chat-completions and/or messages endpoint.

| Vendor | Host pattern | Wire | Adapter status | Priority | Notes |
|---|---|---|---|---|---|
| OpenAI | `api.openai.com` | OpenAI native | ✅ `openai-compat` | P0 | Builtin row already seeded. |
| Anthropic | `api.anthropic.com` | Anthropic Messages | ✅ `anthropic` | P0 | |
| Google Gemini | `generativelanguage.googleapis.com` | Gemini native | ✅ `gemini` | P0 | |
| Mistral | `api.mistral.ai` | OpenAI-compat | ✅ `openai-compat` | P1 | |
| DeepSeek | `api.deepseek.com` | OpenAI-compat (DeepSeek extensions for `reasoning_content`) | ✅ `deepseek` | P1 | |
| xAI Grok | `api.x.ai` | OpenAI-compat | ✅ `openai-compat` | P1 | |
| Groq | `api.groq.com` | OpenAI-compat | ✅ `openai-compat` | P1 | Inference-only platform, common in NA. |
| Perplexity | `api.perplexity.ai` | OpenAI-compat (Sonar models) | ✅ `openai-compat` | P1 | |
| Together AI | `api.together.xyz`, `api.together.ai` | OpenAI-compat | ✅ `openai-compat` | P2 | Both hosts must have InterceptionDomain rows. |
| Fireworks AI | `api.fireworks.ai` | OpenAI-compat | ✅ `openai-compat` | P2 | |
| Moonshot Kimi | `api.moonshot.cn`, `api.moonshot.ai` | OpenAI-compat | ✅ `openai-compat` | P2 | International host needs verification at phase time. |
| MiniMax | `api.minimax.chat`, `api.minimax.io` | MiniMax native | ✅ `minimax` | P2 | International host suffix differs. |
| Zhipu GLM | `open.bigmodel.cn` | GLM native | ✅ `glm` | P2 | |
| Cohere | `api.cohere.com` | Cohere native | ✅ `api/cohere` | P2 | Dedicated adapter shipped under `packages/shared/traffic/adapters/api/cohere/`. |
| HuggingFace Inference | `api-inference.huggingface.co`, `*.endpoints.huggingface.cloud` | Multiple | ✅ `api/huggingface` | P2 | Hosted-endpoint subdomains use SUFFIX matching. |
| Replicate | `api.replicate.com` | Replicate native (poll-based) | ✅ `api/replicate` | P3 | Asynchronous prediction model. |

## Table B — Cloud-hosted enterprise LLM endpoints

These are the enterprise on-prem / hyperscaler-hosted variants of the foreign
LLM APIs. Each has its own wire dialect and host pattern; HOST_MATCH_TYPE
must be SUFFIX because the regional/account prefix is customer-specific.

| Vendor | Host pattern | Wire | Adapter status | Priority | Notes |
|---|---|---|---|---|---|
| Azure OpenAI | `*.openai.azure.com` (SUFFIX) | OpenAI variant with deployments | ✅ `azure-openai` | P0 | Required for the majority of large NA enterprises. |
| AWS Bedrock | `bedrock-runtime.*.amazonaws.com` (SUFFIX) | Bedrock multi-model envelope | ✅ `bedrock` | P0 | Must cover Anthropic-on-Bedrock and Llama-on-Bedrock. |
| Google Vertex AI | `*-aiplatform.googleapis.com` (SUFFIX), `aiplatform.googleapis.com` | Vertex multi-model | ✅ `vertex` | P0 | Both regional and global hosts. |

## Table C — Web / console surfaces

Browser-based or desktop-client traffic. **Each surface uses a private,
undocumented wire format** that differs from its API counterpart, so each
needs a dedicated Go adapter. The `streaming/extract/chatgpt_web.go` file
already contains a streaming extractor for ChatGPT web traffic — the new
traffic adapters should follow the same fixture-based reverse-engineering
discipline.

| Vendor | Surface | Host | Adapter status | Priority | Notes |
|---|---|---|---|---|---|
| OpenAI | Consumer chat | `chatgpt.com` | ✅ `web/chatgptweb` | P0 | Adapter package shipped under `packages/shared/traffic/adapters/web/chatgptweb/`. |
| OpenAI | Developer console | `platform.openai.com` | ✅ `web/openaiplatformweb` | P1 | Playground traffic shares this adapter. |
| Anthropic | Consumer chat | `claude.ai` | ✅ `web/claudeweb` | P0 | Internal `/api/organizations/.../completion` SSE endpoint. |
| Anthropic | Developer console | `console.anthropic.com` | ✅ `web/anthropicconsoleweb` | P1 | Workbench traffic. |
| Google | Consumer chat | `gemini.google.com` | ✅ `web/geminiweb` | P0 | Uses Google batchexecute (`f.req=` envelope); decoded by `BatchExecuteDetector`. |
| Google | AI Studio | `aistudio.google.com` | ✅ `web/googleaistudioweb` | P1 | |
| Microsoft | Consumer Copilot | `copilot.microsoft.com` | ✅ `web/copilotmsweb` | P0 | |
| Microsoft | M365 Copilot | `m365.cloud.microsoft`, paths under `*.office.com` | ✅ `web/m365copilotweb` | P1 | Separate adapter — wire differs from consumer Copilot. |
| Perplexity | Web | `www.perplexity.ai`, `perplexity.ai` | ✅ `web/perplexityweb` | P1 | |
| xAI Grok | Web | `grok.com`, embedded inside `x.com` paths | ✅ `web/grokweb` | P1 | Path-level handling for the `x.com` embedding case. |
| Mistral Le Chat | Web | `chat.mistral.ai` | ✅ `web/mistralweb` | P2 | |
| Poe | Web aggregator | `poe.com` | ✅ `web/poeweb` | P2 | Quora-owned, multi-model aggregator. |
| Character.AI | Web | `character.ai`, `c.ai` | ✅ `web/characterweb` | P3 | |
| You.com | Web | `you.com`, `chat.you.com` | ✅ `web/youweb` | P3 | |
| HuggingChat | Web | `huggingface.co/chat` (path subset) | ✅ `web/huggingchatweb` | P3 | |
| GitHub Copilot Chat (web) | Web | `github.com/copilot` (path subset) | ✅ `web/githubcopilotweb` | P1 | |
| DeepSeek | Web | `chat.deepseek.com` | ✅ `web/deepseekweb` | P2 | |
| Moonshot Kimi | Web | `kimi.moonshot.cn`, `www.kimi.com` | ✅ `web/kimiweb` | P2 | |
| Zhipu ChatGLM | Web | `chatglm.cn` | ✅ `web/chatglmweb` | P2 | |

## Table D — Coding tools and IDE agents

These are the highest-value targets for a compliance proxy: corporate
endpoints commonly run Cursor, Copilot, or Codeium without IT visibility.

| Tool | Host pattern | Surface | Adapter status | Priority | Notes |
|---|---|---|---|---|---|
| GitHub Copilot | `api.githubcopilot.com`, `copilot-proxy.githubusercontent.com` | IDE backend | ✅ `ide/githubcopilot` | P0 | Adapter package carries both completion and chat traffic. |
| GitHub Copilot Chat (web) | `github.com/copilot` (path subset) | Web | ✅ `web/githubcopilotweb` | P1 | See Table C row. |
| Cursor | `api2.cursor.sh`, `api3.cursor.sh`, `cursor.com` | IDE backend + account | ✅ `ide/cursor` | P0 | Connect-RPC + protobuf wire; decoded by `ConnectRPCProtobufDetector`. |
| Codeium / Windsurf | `server.codeium.com`, `inference.codeium.com`, `api.codeium.com` | IDE backend + inference | ✅ `ide/codeium` | P0 | |
| Tabnine | `api.tabnine.com` (cloud) | IDE backend | ✅ `ide/tabnine` | P1 | Self-hosted deployments are out of scope. |
| Continue.dev | Underlying provider hosts; Hub `*.continue.dev` | IDE proxy | ✅ `ide/continuedev` | P2 | Adapter shipped; Hub-side traffic is handled in addition to underlying provider adapters. |
| v0.dev | `v0.dev` (Vercel AI backend) | Web codegen | ✅ `web/v0web` | P2 | |
| Bolt.new | `bolt.new`, StackBlitz subdomains | Web codegen | ✅ `web/boltweb` | P2 | |
| Replit AI | `replit.com` paths, `*.replit.dev` | Web IDE + agent | ✅ `ide/replitai` | P3 | |
| Devin | Hosts to be verified at phase time | Web agent | ✅ `web/devinweb` | P3 | Cognition Labs, smaller footprint. |

## Tool-use and MCP coverage requirement

Every adapter shipped under this plan **must extract tool-call content** in
addition to text. The Nexus compliance pipeline relies on the canonical
`{prompt, completion}` view to inspect tool invocations (function calling,
Anthropic `tool_use` blocks, Gemini `functionCall` parts) and any
MCP-formatted tool requests carried inside provider responses. Adapters that
drop tool content silently produce false-negative compliance decisions.

This applies retroactively to existing adapters: each P0/P1 phase audits the
existing adapter and adds a regression test that asserts tool-call content
is captured.

## Provider model catalogs

Every `provider-templates/<name>.json` catalog must be backed by a
**WebFetch-verified** model list pulled from the vendor's official
documentation at phase time. Models hallucinated from training data are
unacceptable. Fetched URLs and the date of fetch should be recorded in the
phase's commit message or SDD doc.

## Phase order

The wave is sequenced so that every successive phase exercises adapter
patterns established in earlier phases. P0 must complete before any P1 phase
starts.

### P0 wave (eight phases — enterprise-critical core)

1. **OpenAI** — API audit + `chatgpt-web` adapter + `chatgpt.com` and
   `platform.openai.com` builtin rows + tool-call test.
2. **Anthropic** — API audit + `claude-web` adapter + `claude.ai` and
   `console.anthropic.com` builtin rows + tool-call test.
3. **Google** — Gemini API audit + `gemini-web` adapter + `gemini.google.com`
   and `aistudio.google.com` builtin rows + tool-call test.
4. **Microsoft** — Azure OpenAI audit + `copilot-ms-web` adapter +
   `*.openai.azure.com` (SUFFIX) and `copilot.microsoft.com` builtin rows +
   tool-call test.
5. **AWS Bedrock** — `bedrock` adapter audit + verified Bedrock model catalog
   (Claude on Bedrock, Llama, Titan) + `bedrock-runtime.*.amazonaws.com`
   (SUFFIX) builtin row + tool-call test.
6. **GitHub Copilot** — new `github-copilot` adapter for both completion and
   chat traffic + `api.githubcopilot.com` and
   `copilot-proxy.githubusercontent.com` builtin rows + provider-templates
   JSON.
7. **Cursor** — new `cursor` adapter + `api2.cursor.sh`, `api3.cursor.sh`,
   `cursor.com` builtin rows + provider-templates JSON.
8. **Codeium / Windsurf** — new `codeium` adapter + `server.codeium.com`,
   `inference.codeium.com`, `api.codeium.com` builtin rows +
   provider-templates JSON.

### P1 wave

Vertex AI, xAI, Perplexity, Groq, Mistral (API + web), `platform.openai.com`,
`console.anthropic.com`, `aistudio.google.com`, Tabnine, M365 Copilot.

### P2 wave

Together, Fireworks, DeepSeek (web), Moonshot, MiniMax, GLM, Cohere upgrade,
HuggingFace upgrade, Continue.dev, v0.dev, Bolt.new, additional web tier.

### P3 wave

Replicate, Character.AI, You.com, HuggingChat path rule, Replit AI, Devin.

## Phase template

Each phase, regardless of provider, follows this checklist:

1. **WebFetch the official model docs** for that vendor and record the URL
   and fetch date.
2. **Audit the existing adapter** (if any) for tool-use coverage; add a
   regression test if missing.
3. **Implement the new adapter** (if any) under
   `packages/shared/traffic/adapters/<name>/`, register it in `builtins.go`,
   and add it to `manifest.json`.
4. **Add fixture-based tests** under
   `packages/shared/traffic/adapters/<name>/<name>_test.go` using
   `testdata/` fixture files captured from real wire bytes (de-credentialed).
5. **Update `provider-templates/<name>.json`** if the catalog needs new models
   or pricing; rebuild `index.json` if a new template is added.
6. **Add `InterceptionDomain` rows** in `tools/db-migrate/seed/seed-builtins.ts`
   for every host pattern relevant to that surface.
7. **Run** `go test -race -count=1 ./packages/shared/traffic/adapters/...` and
   confirm green output before committing.
8. **Commit** with a message of the form
   `feat(provider/<name>): <summary>`.
