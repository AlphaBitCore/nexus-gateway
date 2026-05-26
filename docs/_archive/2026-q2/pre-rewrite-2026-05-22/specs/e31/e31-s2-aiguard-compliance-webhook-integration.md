# E31 S2 - AIGuard compliance-webhook integration

**Epic:** 31  
**Story:** 2  
**Status:** Draft - 2026-04-25  
**Requirements:** `docs/developers/specs/e31/e31-aiguard-compliance-webhook-integration.md`

## User Story

As an operator, I want compliance webhook configuration to select AIGuard directly and receive webhook-compatible decisions so I can enable ML classification without manual URL stitching or contract translation.

## Tasks

### T1. Documentation alignment

- Add requirements for URL copy + AIGuard target option + contract mapping.
- Add OpenAPI contract for webhook-compatible endpoint.
- Update architecture note for new UI integration path.

### T2. AI Gateway webhook-compatible endpoint

- Add a dedicated handler entry for `/v1/ai-guard/compliance-webhook`.
- Accept webhook-forward payload shape.
- Map payload into AIGuard classify request:
  - `detector_type`: fixed `compliance_webhook`
  - `content`: join `normalizedContent` or synthesize deterministic fallback text
  - `context.ingress`: from `ingressType` when available
  - `context.target_model`: from `model`
- Translate AIGuard response decision tokens to webhook-forward tokens.

### T3. Control Plane UI integration

- In AI Guard settings page, render read-only runtime webhook URL with copy action.
- In hook form webhook flow, add selector:
  - `AIGuard` (prefills endpoint with runtime URL)
  - `Custom` (manual endpoint editing remains)

### T4. i18n

- Add translation keys for new AI Guard URL copy UI and webhook target selector across `en/es/zh` and public mirrors.

### T5. Tests

- Go: handler tests for webhook endpoint request mapping + response mapping.
- UI: hook form prefill behavior and AI Guard URL copy rendering/interaction.

## Acceptance Criteria

1. `/v1/ai-guard/compliance-webhook` accepts webhook-forward payload and returns webhook-forward decision contract.
2. AI Guard settings page shows copyable runtime webhook URL.
3. Hook form webhook rows can select `AIGuard` and endpoint is prefilled.
4. Existing custom/manual webhook endpoint flow remains available.
5. New i18n keys are present in all required locale files.
6. New/updated tests pass locally.
