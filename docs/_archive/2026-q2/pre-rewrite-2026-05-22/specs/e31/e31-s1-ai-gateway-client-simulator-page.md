# E31 Story 1 - AI Gateway client simulator page

**Epic:** 31 - AI Gateway client simulator page  
**Story:** 1  
**Status:** Draft - 2026-04-25  
**Requirements:** `docs/developers/specs/e31/e31-ai-gateway-client-simulator.md`  
**OpenAPI:** `docs/users/api/openapi/ai-gateway/e31-s1-ai-gateway-client-simulator.yaml` (consumer contract for existing AI Gateway routes)

## User Story

As a PM/operator, I want a single simulator page in Control Plane UI where I can paste a VK, pick provider/model, choose SSE or non-SSE, and run chat against AI Gateway client APIs, so I can validate real client behavior quickly.

## Context

- Existing UI already has simulator-style patterns (`IamSimulator`, `DryRunPanel`) but no AI Gateway client chat simulator page.
- Existing AI Gateway OpenAPI already defines required endpoints:
  - `GET /v1/models`
  - `POST /v1/chat/completions`
  - `GET /v1/usage`
- Existing route/nav and i18n patterns require:
  - lazy page export,
  - shell route registration with nav key,
  - locale key additions for `en/zh/es` and `public/locales`.

## Tasks

### T1. Route + nav wiring

**Files**

- `packages/control-plane-ui/src/routes/lazyPages.tsx`
- `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`
- `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/nav.json`
- `packages/control-plane-ui/public/locales/{en,zh,es}/nav.json`

**Changes**

1. Add lazy export for `AIGatewaySimulatorPage`.
2. Register new shell route under System section (super-admin only).
3. Add nav label key for the simulator page.

### T2. AI Gateway client simulator service

**Files**

- `packages/control-plane-ui/src/api/services/aiGatewayClientSimulator.ts` (new)

**Changes**

1. Add typed methods:
   - `listModels(baseUrl, vk)`
   - `createChatCompletion(baseUrl, vk, payload)`
   - `createChatCompletionStream(baseUrl, vk, payload, callbacks, signal)`
   - `getUsage(baseUrl, vk)`
2. Keep auth and base URL handling in one place.
3. Parse SSE `data:` frames with `[DONE]` termination support.
4. Keep payload schema OpenAI-compatible and minimal.

### T3. Simulator page UI implementation

**Files**

- `packages/control-plane-ui/src/pages/tools/ai-gateway-simulator/AIGatewaySimulatorPage.tsx` (new)
- `packages/control-plane-ui/src/pages/tools/ai-gateway-simulator/AIGatewaySimulatorPage.module.css` (new)

**Changes**

1. Build 3-zone UX:
   - connection context (gateway base URL + VK + load models),
   - setup controls (provider/model cascade + stream toggle + tuning),
   - chat + usage/result panels.
2. Derive provider groups from `/v1/models` response (`owned_by`).
3. Implement send behavior for SSE/non-SSE.
4. Implement stop/cancel for active SSE stream.
5. Render response usage + summary usage.
6. Ensure all copy uses i18n keys.

### T4. Locale keys

**Files**

- `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/pages.json`
- `packages/control-plane-ui/public/locales/{en,zh,es}/pages.json`

**Changes**

1. Add `aiGatewaySimulator` page namespace keys (title/subtitle/form labels/buttons/states/errors/usage fields).
2. Keep key parity across `en/zh/es`.

### T5. Unit tests

**Files**

- `packages/control-plane-ui/src/api/services/aiGatewayClientSimulator.test.ts` (new)
- `packages/control-plane-ui/src/pages/tools/ai-gateway-simulator/AIGatewaySimulatorPage.test.tsx` (new)

**Changes**

1. Service tests for endpoint URLs, auth header, and SSE parser behavior.
2. Page tests for:
   - disabled states before VK/models are ready,
   - provider->model cascade reset,
   - non-SSE happy path,
   - usage summary refresh call.

### T6. Docs alignment

**Files**

- `docs/users/api/openapi/ai-gateway/e31-s1-ai-gateway-client-simulator.yaml` (new)
- `docs/users/product/architecture.md`

**Changes**

1. Add simulator consumer contract OpenAPI file referencing existing AI Gateway routes used by UI.
2. Update architecture doc UI section with simulator capability note.

## Acceptance Criteria

| AC | Description |
|---|---|
| AC1 | Route exists and page renders from authenticated shell navigation. |
| AC2 | Page can load models using VK and derive provider/model cascade. |
| AC3 | Non-SSE chat returns assistant message and response usage. |
| AC4 | SSE chat renders incremental tokens and supports stop action. |
| AC5 | Usage summary (`/v1/usage`) renders after a completed chat run. |
| AC6 | New UI strings are fully i18n-based across `en/zh/es` + `public/locales`. |
| AC7 | Vitest tests for service + page pass locally. |

## Risks

- CORS or gateway reachability can block browser direct calls in some environments.
- SSE framing differences could break naive parsers; tests mitigate by covering chunk boundaries and done event semantics.
- Missing `owned_by` on model rows can break provider grouping; fallback grouping to `unknown` is required.

## Out of Scope

- New backend APIs or admin proxy endpoints for simulator.
- Persisted conversation history.
- Embeddings/image/audio simulator modes.
