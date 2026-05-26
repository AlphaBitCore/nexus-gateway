# E31 - Routing no-match passthrough fallback

## Background

When operators disable a catch-all routing rule, AI Gateway currently returns `ROUTING_NO_MATCH` for requests whose model does not match any enabled stage-1 rule. This creates an unnecessary hard dependency on exhaustive routing rules and blocks valid traffic even when the requested model exists and the caller is authorized.

## Functional Requirements

| ID | Requirement | Priority |
|---|---|---|
| FR-1 | When routing resolves with zero targets, AI Gateway must attempt a global passthrough fallback using the client-requested model. | Must |
| FR-2 | Passthrough fallback must look up an enabled model by ID or name and route directly to that model's provider/model pair. | Must |
| FR-3 | If the requested model is outside the virtual key allow-list, AI Gateway must return an authorization error and must not dispatch upstream. | Must |
| FR-4 | If no enabled model exists for the requested identifier, AI Gateway must return a not-found routing error. | Must |
| FR-5 | Existing matched-routing behavior (when stage-1 produces targets) must remain unchanged. | Must |
| FR-6 | Routing telemetry must distinguish fallback mode from matched-rule mode for audit/debugging. | Should |

## Non-Functional Requirements

| ID | Requirement | Priority |
|---|---|---|
| NFR-1 | The fallback decision path must execute in-request without introducing new network calls beyond existing model lookup paths. | Must |
| NFR-2 | The change must preserve current cross-format compatibility checks and upstream dispatch guarantees. | Must |
| NFR-3 | Unit tests must cover no-match success fallback, unauthorized fallback rejection, and model-not-found rejection. | Must |

## User Roles and Personas

- **Platform Operator**: manages routing rules and expects requests to keep working when rules are intentionally narrow.
- **Application Developer**: calls AI Gateway with a permitted model and expects deterministic success/failure semantics independent of default-rule presence.

## Constraints and Assumptions

- Routing rules remain DB-backed Category-B config; no control-plane config payload schema change is introduced.
- Passthrough fallback is evaluated only after routing returns no targets.
- Virtual key allow-list rules remain authoritative for model authorization.

## Glossary

- **No-match routing**: routing resolution result with zero targets.
- **Passthrough fallback**: direct model/provider resolution using requested model identity rather than a matched routing rule.
- **Catch-all rule**: a routing rule whose match conditions effectively match all requests in a stage.

## Success Criteria

- Disabling a catch-all/default rule no longer causes valid, authorized model requests to fail with `ROUTING_NO_MATCH`.
- Requests still fail with explicit errors for unauthorized or unknown models.
- Regression tests prove matched routing remains unchanged.
