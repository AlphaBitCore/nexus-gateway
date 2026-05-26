# E31 - AIGuard compliance-webhook integration

## Background

Operators want to use AIGuard as a first-class webhook target in hook configuration without manual endpoint lookup and contract mismatch handling. Today, webhook-forward accepts webhook-style decisions while AIGuard classify responses are designed for classifier callers, which increases setup friction.

## Functional Requirements

| ID | Requirement | Priority |
|---|---|---|
| FR-1 | The AI Guard settings page must show a copyable runtime webhook URL that can be used as a webhook-forward endpoint. | Must |
| FR-2 | Hook configuration for webhook rows must provide an explicit `AIGuard` target option that prefills the endpoint with the runtime webhook URL. | Must |
| FR-3 | Operators must still be able to select a manual/custom webhook endpoint instead of AIGuard. | Must |
| FR-4 | AI Gateway must expose a webhook-compatible AIGuard endpoint that accepts webhook-forward payload shape and returns webhook-forward decision shape. | Must |
| FR-5 | The webhook-compatible AIGuard endpoint must map AIGuard internal decisions to webhook decision tokens: `APPROVE`, `REJECT_HARD`, `REJECT_SOFT`, `MODIFY`, with `ABSTAIN` supported as pass-through when present. | Must |
| FR-6 | Existing `/v1/ai-guard/classify` behavior for current internal callers must remain available. | Must |

## Non-Functional Requirements

| ID | Requirement | Priority |
|---|---|---|
| NFR-1 | The integration must preserve deterministic behavior under empty or partial webhook payloads, using a stable fallback content synthesis strategy. | Must |
| NFR-2 | UI text for new controls must be fully i18n-backed across `en`, `es`, and `zh` locale files (including public mirrors). | Must |
| NFR-3 | Unit tests must cover decision mapping, endpoint contract shape, and UI prefill/copy behavior. | Must |

## User Roles and Personas

- **Super Admin / Compliance Admin**: configures hooks and AI Guard backend, needs low-friction integration.
- **Platform Operator**: validates runtime behavior and copies endpoint values for environment setup.

## Constraints and Assumptions

- No new service is introduced; implementation must reuse existing AI Gateway and Control Plane UI surfaces.
- Webhook-forward remains the execution path for webhook rows.
- Runtime webhook URL defaults to local development base `http://localhost:3050` unless overridden by user input in the UI.

## Glossary

- **AIGuard**: internal classifier service in AI Gateway.
- **compliance-webhook**: webhook hook row configured via `webhook-forward`.
- **webhook-forward contract**: response shape with decision tokens such as `REJECT_HARD`.

## Success Criteria

- An operator can configure a webhook hook row by selecting `AIGuard` and saving without manual URL assembly.
- Requests reaching `webhook-forward` and forwarded to AIGuard receive webhook-compatible decision responses.
