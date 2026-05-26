# E31 S3 - Routing no-match passthrough fallback

**Epic:** 31  
**Story:** 3  
**Status:** Draft - 2026-04-26  
**Requirements:** `docs/developers/specs/e31/e31-routing-no-match-passthrough-fallback.md`

## User Story

As an operator, I want AI Gateway to continue serving authorized model requests even when no routing rule matches, so routing policy can be selective without breaking baseline model access.

## Tasks

### T1. Contract alignment

- Document global no-match passthrough semantics in requirements and OpenAPI.
- Clarify that this is a behavior change only; no architecture boundary change.

### T2. Proxy no-match fallback implementation

- In the proxy request pipeline, replace immediate no-target rejection with fallback resolution.
- Resolve requested model via model lookup and synthesize a single passthrough target.
- Keep existing flow unchanged when routing already yields targets.

### T3. Authorization and failure semantics

- Enforce virtual-key allowed-model checks before passthrough dispatch.
- Return explicit failure when model lookup fails.
- Keep upstream/provider failures on normal executor path.

### T4. Observability continuity

- Mark fallback mode in route metadata so audit/debug traces can distinguish fallback from matched-rule routing.

### T5. Unit tests

- Add tests for:
  - no-match + authorized model -> succeeds via passthrough fallback
  - no-match + disallowed model -> rejected with authorization error
  - no-match + unknown model -> rejected with not-found routing error

## Acceptance Criteria

1. A request with no matched routing rule succeeds when its requested model exists and is authorized.
2. A request with no matched routing rule fails with authorization error when model is outside VK allow-list.
3. A request with no matched routing rule fails with routing not-found when model does not exist.
4. Requests with matched routing rules preserve existing behavior.
5. New/updated tests pass locally.
