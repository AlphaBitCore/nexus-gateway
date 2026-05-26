# E31 - AI Gateway Client Simulator Page

**Status:** Draft - 2026-04-25  
**Epic:** 31  
**Depends on:** `docs/users/api/openapi/ai-gateway/ai-gateway-v1.yaml`, `packages/control-plane-ui` routing/i18n/runtime API conventions.

## 1. Business Goal

Operators and PMs need a fast, user-friendly way to validate AI Gateway behavior from the client perspective before or during rollout checks. The simulator must let a user paste a virtual key (VK), discover available providers/models, run chat in SSE or non-SSE mode, and inspect usage feedback without using admin-only APIs.

The page acts as a deterministic troubleshooting and demo surface for:

- VK auth validity,
- model availability by provider,
- streamed vs non-streamed response behavior,
- token/cost usage visibility.

## 2. Scope

### In scope

- Add one new page in `control-plane-ui` reachable from authenticated shell routes.
- All runtime requests on that page use AI Gateway client-facing endpoints only:
  - `GET /v1/models`
  - `POST /v1/chat/completions`
  - `GET /v1/usage`
- Users can:
  - input VK,
  - select provider and model (cascade),
  - toggle streaming mode,
  - chat with normal text input,
  - see response metadata and usage.
- Provider/model options are derived from `/v1/models` (`owned_by` => provider grouping).
- SSE parser handles incremental chunks and `[DONE]` completion.

### Out of scope

- Any new AI Gateway backend endpoint.
- Any admin API/BFF proxy endpoint for simulator runtime calls.
- Multi-turn persistent storage (history is in-memory only for current page session).
- Embeddings/image/audio simulation (chat only in this story).

## 3. User Roles & Personas

| Role | Need met by this epic |
|---|---|
| **PM / Product Owner** | Quickly validate whether a VK can run real chat with expected provider/model and stream behavior. |
| **Platform Operator** | Reproduce customer-side API behavior without writing curl scripts. |
| **QA Engineer** | Run deterministic UI checks for model catalog, stream mode, and usage rendering. |

## 4. Functional Requirements

### F1 - VK connection context (MUST)

The page MUST provide a VK input and a "load models" action. The call to `GET /v1/models` MUST use the same client header strategy used by chat and usage calls so the page reflects real client-side auth behavior.

### F2 - Provider/model cascading selector (MUST)

After successful model fetch, the page MUST group models by provider (`owned_by`) and expose:

- provider dropdown,
- model dropdown constrained by selected provider.

Changing provider MUST clear model selection to avoid invalid pairs.

### F3 - Chat request modes (MUST)

The page MUST support:

- **non-SSE mode**: `POST /v1/chat/completions` with `stream=false`,
- **SSE mode**: `POST /v1/chat/completions` with `stream=true` and progressive rendering.

Both modes MUST use OpenAI-compatible request structure (`model`, `messages`, optional tuning fields).

### F4 - Usage visibility (MUST)

After each completed chat turn, the page MUST surface usage from:

- completion response `usage` (when present),
- `GET /v1/usage` summary (same VK context).

### F5 - Error handling and UX safety (MUST)

The page MUST provide clear user feedback for at least:

- unauthorized VK (`401`),
- quota/rate limit (`429`),
- upstream failure (`502`),
- stream interruption / parser failure.

Send actions MUST be disabled while a request is in-flight. SSE mode MUST support stop/cancel.

### F6 - Internationalized UI text (MUST)

All user-visible copy MUST use i18n keys; no hardcoded strings in JSX. New keys must be present in `en`, `zh`, and `es` locale files and synchronized to `public/locales`.

## 5. Non-Functional Requirements

### NF1 - Runtime isolation

Simulator logic should be isolated in a dedicated service module and page-level helpers so route wiring and API behavior can be tested independently.

### NF2 - Test coverage

Vitest coverage MUST include:

- service endpoint calls and payload shapes,
- provider/model grouping and reset behavior,
- SSE and non-SSE chat flow states,
- usage panel rendering.

### NF3 - Deterministic behavior

The simulator must avoid hidden mutable globals and rely on explicit page state so repeated runs produce consistent results.

## 6. Constraints & Assumptions

- Control Plane UI is the only implementation surface for this epic.
- Direct browser-to-AI-Gateway calls may require reachable gateway URL and acceptable CORS policy in dev/runtime.
- No backward compatibility layer is needed (pre-GA policy).
- **Architecture impact:** no new service/component boundary is introduced; this is a UI capability extension over existing `/v1/*` contracts.

## 7. Glossary

- **VK (Virtual Key):** Client credential accepted by AI Gateway for `/v1/*` APIs.
- **SSE:** Server-Sent Events stream returned by `stream=true` chat completions.
- **Provider/model cascade:** Two-level selection where model options depend on chosen provider.
- **Usage summary:** Current billing-period usage from `GET /v1/usage`.

## 8. Priority (MoSCoW)

| Requirement | Priority |
|---|---|
| F1 VK context | Must |
| F2 provider/model cascade | Must |
| F3 SSE + non-SSE chat | Must |
| F4 usage visibility | Must |
| F5 error handling + safety | Must |
| F6 i18n completeness | Must |
| NF1 runtime isolation | Should |
| NF2 tests | Must |
| NF3 deterministic behavior | Should |
