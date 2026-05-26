# [Feature Name] — Specification

**Ticket:** [ID or slug]
**Status:** Draft
**Author:** [name or agent]
**Date:** [YYYY-MM-DD]

---

## Feature

One sentence: what capability does this add or change?

## Problem

Why is this needed? What breaks or is missing without it?

## User Scenarios

Concrete flows. Use numbered steps. Include the actor, trigger, action, and outcome.

1. [Actor] does [action] when [condition] → [expected outcome]
2. ...

## Functional Requirements

Use `FR-N` numbering. One behavior per statement. Write outcomes, not implementation.

- FR-1. The system shall [verb] [object] when [condition].
- FR-2. ...

## Non-Functional Requirements

Performance, security, reliability, observability, accessibility, compliance.

- NFR-1. The system shall [metric] under [conditions].
- NFR-2. ...

## Constraints

Technology, policy, architecture, data, and operational limits. Keep separate from FRs.

- Must use existing [auth / middleware / infrastructure].
- Must not introduce new paid external dependencies.
- Must run within the current cloud environment.

## Acceptance Criteria

Observable pass/fail checks. Each maps to one or more FRs.

- AC-1. [Observable outcome that proves FR-N is satisfied]
- AC-2. ...

## Out of Scope

Explicit list of what this feature does NOT include.

- [Feature or behavior] is not part of this change.
- ...

## Open Questions

Unresolved items that could block design or implementation.

- [ ] [Question] — blocked by [dependency or person]
- ...

## AI-Specific Requirements

<!-- Remove this section if the feature does not involve AI models, agents, or automated decisions. -->

### Data Boundaries
- The system shall not send [data type] to [external service].
- [Log / prompt / response] data shall be retained for [N] days.

### Model Behavior
- The agent shall [cite source / refuse request / ask for clarification] when [condition].
- The agent shall not [action] without [human approval / explicit instruction].

### Human Oversight Points
- All [generated code / config changes / emails] must be reviewed before [merge / send / apply].

### Failure and Fallback
- If [model / service] is unavailable, the system shall [fallback action].
- If output fails schema validation, the system shall retry once then return a structured error.

### Traceability
- Each implementation task shall reference the FR-N IDs it satisfies.
- Test cases shall map to AC-N criteria.
