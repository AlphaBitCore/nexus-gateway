---
name: spec-writing
description: >
  Spec-writing for Spec-Driven Development (SDD) skill for writing requirement specifications
  before implementation. Use when starting a new feature, planning work,
  writing requirements, or designing a component. Trigger keywords: spec,
  specification, SDD, plan feature, write requirements, design feature,
  spec-driven, new feature, what to build.
user-invocable: true
---

# Spec-Driven Development (SDD)

SDD is a development discipline where **specifications are written and agreed upon before any implementation begins**. Specs serve as the control document for both humans and AI agents: they define what must be built, what must not happen, and how success is judged.

**Core principle:** Write for two readers simultaneously — a software practitioner and an AI coding agent. If either has to guess, the spec is not ready.

---

## Invocation Protocol

When this skill is triggered, follow these steps in order. **Do not write code or architecture before completing Phase 1.**

### Step 1 — Clarify intent
Ask the user:
1. What is the feature or change in one sentence?
2. Is this a new feature, a change to existing behavior, or a bug fix?
3. Is there a ticket ID, Jira/Linear issue, or slug to use for file naming?
4. Is there an existing spec or plan already started? (Check `.plans/` directory)

Once you have a ticket ID or slug, scaffold the plan directory:
```
.claude/skills/spec-writing/scripts/new.sh <ticket-id>
```
This copies blank templates into `.plans/<ticket-id>/` and ensures `.plans/` is in `.gitignore`.

### Step 2 — Gather context (brownfield projects)
Before writing a single requirement, read the codebase:
- Identify files, modules, and interfaces the feature will touch
- Note architectural patterns, naming conventions, and constraints already in use
- Read any existing relevant specs, plans, or ADRs
- Summarize findings to the user before proceeding

### Step 3 — Ask elicitation questions
Use the **Key Questions** bank below. Pick the most relevant 4–6; do not ask all at once. Present as a numbered list and wait for answers before drafting the spec.

### Step 4 — Draft the spec
Write `A1-spec.md` using the Specification Template. Present the full draft to the user.

### Step 5 — Approval gate
**Stop. Do not proceed to Phase 2 until the user explicitly approves the spec.**
Say: _"Spec draft complete. Please review A1-spec.md and say 'approved' or provide feedback."_

Repeat Steps 4–5 until the spec is approved.

---

## Key Questions Bank

Use these to elicit missing requirements. Choose the most relevant for the feature:

**Scope**
- Who are the actors? (end user, admin, system, external service)
- What triggers the feature? (user action, event, schedule, API call)
- What is the expected output or side effect?

**Edge cases**
- What happens on invalid or missing input?
- What happens when a dependency (DB, API, service) is unavailable?
- What happens on partial success or timeout?

**Non-functional**
- What are the performance targets? (latency, throughput, SLA)
- Are there security or compliance constraints? (auth, PII, audit log)
- What observability is required? (metrics, logs, alerts)

**Boundaries**
- What explicitly should NOT change?
- What deferred work should be listed as out of scope?
- Are there existing interfaces or data contracts that must not break?

**AI/agent-specific** (if applicable)
- What data must not be sent to external models?
- Are there human review gates before applying outputs?
- What is the fallback if the model is unavailable or returns bad output?

---

## Workflow Phases

Progress through five phases in order. **Each phase requires explicit user approval before advancing.**

| Phase | Artifact | Gate |
|-------|----------|------|
| 0. Context Gathering | Notes / summary | User confirms understanding |
| 1. Specification | `A1-spec.md` | User approves spec |
| 2. Planning | `A2-plan.md` | User approves plan |
| 3. Task Breakdown | `A3-tasks.md` | User approves tasks |
| 4. Implementation | Code + tests | PR / review |

### Scripts

| Script | When to use |
|--------|-------------|
| `scripts/new.sh <ticket>` | Start of Phase 0 — scaffolds `.plans/<ticket>/` with blank templates |
| `scripts/validate.sh <spec>` | Before Phase 1 approval gate — lints spec for completeness |
| `scripts/status.sh` | Any time — shows phase status of all active plans |

Run scripts from the project root. Templates are in `templates/`.

### File Conventions

Store all phase artifacts in `.plans/[TICKET-ID]/` (add to `.gitignore` if not already).

```
.plans/
  AUTH-42/
    A1-spec.md        ← specification
    A2-plan.md        ← architecture and approach
    A3-tasks.md       ← atomic tasks with dependencies
```

If no ticket ID exists, use a short slug: `.plans/user-export/`.

Check for an existing `.plans/` directory before creating one. If a partial spec exists, continue from where it left off rather than starting fresh.

---

## Phase 1: Write the Specification

Use the template below. Fill every section; mark sections `N/A` only if genuinely not applicable. Incomplete specs produce poor plans and broken implementations.

---

### Specification Template

```markdown
# [Feature Name] — Specification

**Ticket:** [ID or slug]
**Status:** Draft | Review | Approved
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

Explicit list of what this feature does NOT include. Prevents AI agents from inventing scope.

- [Feature or behavior] is not part of this change.
- ...

## Open Questions

Unresolved items that could block design or implementation.

- [ ] [Question] — blocked by [dependency or person]
- ...

## AI-Specific Requirements

(Include when the feature involves AI models, agents, or automated decisions.)

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
```

---

## Phase 2: Create the Plan

**Prerequisite:** `A1-spec.md` must be approved before starting this phase.

Create `A2-plan.md`. The plan answers *how* the spec will be built. Every decision in the plan must trace back to a requirement in the spec — do not introduce new behaviors here.

After drafting, present the full plan and say: _"Plan draft complete. Please review A2-plan.md and say 'approved' or provide feedback."_ Do not begin task breakdown until approved.

Use the template below. Fill every section; mark sections `N/A` only if genuinely not applicable.

---

### Plan Template

```markdown
# [Feature Name] — Plan

**Ticket:** [ID or slug]
**Spec:** A1-spec.md
**Status:** Draft | Review | Approved
**Author:** [name or agent]
**Date:** [YYYY-MM-DD]

---

## Architecture Overview

Which components are added or modified? Describe their relationships.

- [Component A] — [role and change]
- [Component B] — [role and change]

Include a diagram if the interaction is non-trivial (ASCII or Mermaid).

## Technical Decisions

| Decision | Choice | Rationale | Alternatives Rejected |
|----------|--------|-----------|-----------------------|
| [e.g. state management] | [choice] | [why] | [what else was considered] |

## Data Model Changes

Schema additions, modifications, or migrations required. Use `N/A` if none.

```sql
-- Example: new table or column
ALTER TABLE users ADD COLUMN export_token TEXT;
```

Migration strategy: [how existing data is handled]

## API Contract

Endpoint or interface definitions. Include method, path, inputs, outputs, and error cases.

```
POST /api/[resource]
Request:  { field: type, ... }
Response: { field: type, ... }
Errors:   400 [condition], 401 [condition], 500 [condition]
```

Use `N/A` if no new interfaces are introduced.

## Dependencies

| Type | Name | Purpose | Already in project? |
|------|------|---------|---------------------|
| Library | [name] | [why needed] | Yes / No |
| Service | [name] | [why needed] | Yes / No |
| Internal | [module] | [why needed] | Yes / No |

## Security Considerations

- Authentication: [how identity is established]
- Authorization: [what gates access]
- Input validation: [where and how inputs are sanitized]
- Secrets handling: [how credentials or tokens are managed]
- Data exposure: [what data could leak and mitigations]

## Testing Strategy

| Layer | Scope | Tool/Approach |
|-------|-------|---------------|
| Unit | [what] | [framework] |
| Integration | [what] | [framework] |
| E2E | [what] | [framework] |

Note which ACs each layer covers.
```

---

## Phase 3: Break Down into Tasks

**Prerequisite:** `A2-plan.md` must be approved before starting this phase.

Create `A3-tasks.md`. Each task must be:
- **Atomic** — completable in one focused session
- **Independently testable** — can be verified in isolation
- **Scoped** — touches the fewest files necessary
- **Traced** — references the FR-N and AC-N it satisfies

Task format:
```markdown
## Tasks

- [ ] TASK-1: [What to implement] — satisfies FR-1, FR-2
  - Files: [list files to create or modify]
  - Acceptance: [specific, observable check matching AC-N]
  - Depends on: none

- [ ] TASK-2: [What to implement] — satisfies FR-3
  - Files: [list files to create or modify]
  - Acceptance: [specific, observable check matching AC-N]
  - Depends on: TASK-1
```

Declare dependencies explicitly. Do not start a task before its dependencies are complete.

After drafting, present the full task list and say: _"Task breakdown complete. Please review A3-tasks.md and say 'approved' to begin implementation."_

---

## Phase 4: Implement

**Prerequisite:** `A3-tasks.md` must be approved before writing any code.

Execute tasks in dependency order. For each task:

1. **Read before writing** — read every file the task will touch before making changes
2. **Check the FR and AC** — confirm you understand what success looks like
3. **Write tests first or alongside** — do not leave tests to the end
4. **Implement minimally** — only change what the task requires; do not refactor adjacent code
5. **Verify the AC** — run the test or manually confirm the observable outcome
6. **Mark done** — update the checkbox in `A3-tasks.md` and note the commit or PR

After all tasks are complete, run the **Review Checklist** against the implementation to confirm all ACs are satisfied. Reference FR-N IDs in commit messages and PR descriptions.

---

## Core Spec-Writing Rules

### Rule 1: Write outcomes, not implementation guesses
Describe **what** the system must do, not **how** to build it.

| Good | Bad |
|------|-----|
| The system shall allow users to export reports in CSV format. | The system shall add a React button that calls `/api/export`. |

### Rule 2: Use precise, testable language
Replace vague terms with measurable expectations.

Vague (avoid): `fast`, `user-friendly`, `robust`, `seamless`, `intelligent`, `appropriate`

| Good | Bad |
|------|-----|
| The system shall return results within 2s for 95% of requests. | The system shall provide fast search. |

### Rule 3: One requirement per statement
Never bundle multiple conditions into one sentence.

| Bad | Better |
|-----|--------|
| Validate emails, log failed attempts, and notify admins. | FR-1. Validate email format. FR-2. Log failed attempts. FR-3. Notify admins when threshold exceeded. |

### Rule 4: Eliminate ambiguous verbs
Avoid: `support`, `handle`, `optimize`, `minimize`, `manage`, `etc.`

Be explicit about: actor, trigger, input, output, limits, exceptions.

### Rule 5: Separate constraints from behavior
Business, legal, technical, and policy constraints belong in the **Constraints** section — not embedded inside FRs.

### Rule 6: Every requirement needs acceptance criteria
If it cannot be checked, it is incomplete.

### Rule 7: Name what is out of scope
AI agents fill gaps aggressively. Silence implies permission. Explicitly list non-goals, excluded behaviors, and deferred work.

---

## Review Checklist

Run before handing the spec to planning or implementation:

- [ ] Every FR is testable (observable pass/fail)
- [ ] Each FR expresses exactly one behavior
- [ ] User scenarios are concrete and realistic
- [ ] Performance, security, and reliability are covered in NFRs
- [ ] AI-specific boundaries, oversight, and fallback rules are defined (if applicable)
- [ ] Constraints are separate from functional requirements
- [ ] Out-of-scope items are explicit
- [ ] Edge cases (invalid input, empty states, timeouts, partial success, permissions) are covered
- [ ] A new engineer or AI agent could execute from this spec without guessing

If any answer is no, revise the spec before proceeding.

---

## Common Mistakes

**1. Writing prompts instead of requirements**
"Build a dashboard for admins" is a request, not a spec. Expand it into FRs and ACs.

**2. Embedding design too early**
Do not lock in implementation details (frameworks, file names, API paths) unless they are intentional constraints.

**3. Leaving edge cases implicit**
Always state behavior for: invalid input, empty states, retries, timeouts, partial success, and permission failures.

**4. Ignoring non-functional requirements**
Many production failures trace back to specs that defined features but not limits, security, observability, or reliability.

**5. Letting AI infer policy**
Do not assume the agent knows your compliance rules, privacy policies, or review gates. Write them down explicitly.

**6. Optimizing for completeness over clarity**
A good spec is complete enough to guide action but short enough to be read. Remove repetition. Keep sentences direct.

---

## Brownfield Projects

When adding a feature to an existing codebase, Phase 0 (context gathering) is mandatory. Do not write any spec content before completing these steps:

1. **Read the code, not just the docs** — grep for entry points, interfaces, and patterns relevant to the feature
2. **Identify integration points** — existing middleware, data contracts, and interfaces the feature must respect
3. **Write a project constitution** if one does not exist (`.plans/CONSTITUTION.md`): list architectural decisions, naming conventions, patterns in use, and things that must not change
4. **Specify only the delta** — requirements should describe what is new or changing, not restate existing behavior
5. **Validate the spec against the constitution** — new requirements must not contradict existing architectural constraints

If a constitution already exists, read it before drafting the spec and call out any tension between new requirements and existing constraints.

---

## Quick Reference

| Element | Format |
|---------|--------|
| Functional requirement | `FR-N. The system shall [verb] [object] when [condition].` |
| Non-functional requirement | `NFR-N. The system shall [metric] under [conditions].` |
| Acceptance criterion | `AC-N. [Observable outcome that proves FR-N is satisfied.]` |
| Task | `TASK-N: [What] — satisfies FR-N, AC-N. Files: [...]. Depends on: TASK-M.` |
| Constraint | Plain statement. No FR- prefix. |
| Out of scope | Plain statement. Starts with what is excluded. |
| Phase gate | Say: _"[Phase] complete. Please review [artifact] and say 'approved' or provide feedback."_ |

### Phase Gate Checklist

Before advancing from each phase, confirm:

- **Spec → Plan:** All FRs are testable, ACs are observable, out-of-scope items are explicit, user has said "approved"
- **Plan → Tasks:** All decisions trace to FRs, no new behaviors introduced, security and data model addressed, user has said "approved"
- **Tasks → Implementation:** All tasks are atomic, dependencies declared, files named, ACs linked, user has said "approved"
- **Implementation → Done:** All ACs verified, tasks checked off in A3-tasks.md, Review Checklist passed
