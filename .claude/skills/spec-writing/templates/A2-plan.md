# [Feature Name] — Plan

**Ticket:** [ID or slug]
**Spec:** A1-spec.md
**Status:** Draft
**Author:** [name or agent]
**Date:** [YYYY-MM-DD]

---

## Architecture Overview

Which components are added or modified? Describe their relationships.

- [Component A] — [role and change]
- [Component B] — [role and change]

<!-- Include a diagram if the interaction is non-trivial (ASCII or Mermaid). -->

## Technical Decisions

| Decision | Choice | Rationale | Alternatives Rejected |
|----------|--------|-----------|-----------------------|
| [e.g. state management] | [choice] | [why] | [what else was considered] |

## Data Model Changes

<!-- Schema additions, modifications, or migrations required. Use N/A if none. -->

```sql
-- Example: new table or column
ALTER TABLE users ADD COLUMN export_token TEXT;
```

Migration strategy: [how existing data is handled]

## API Contract

<!-- Endpoint or interface definitions. Use N/A if no new interfaces are introduced. -->

```
POST /api/[resource]
Request:  { field: type, ... }
Response: { field: type, ... }
Errors:   400 [condition], 401 [condition], 500 [condition]
```

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

| Layer | Scope | Tool/Approach | ACs Covered |
|-------|-------|---------------|-------------|
| Unit | [what] | [framework] | AC-N |
| Integration | [what] | [framework] | AC-N |
| E2E | [what] | [framework] | AC-N |
