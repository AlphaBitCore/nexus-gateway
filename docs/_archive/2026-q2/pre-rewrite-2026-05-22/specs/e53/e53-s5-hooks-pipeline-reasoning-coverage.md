# E53-S5 — Expose IncludeReasoning toggle on hook rules

**Epic:** E53 Reasoning Content Passthrough
**Type:** Feature (operator-facing toggle on existing architecture
primitive)
**Owner:** nexus
**Depends on:** none (parallel to s1-s4)

## Pre-SDD audit (settled before writing this story)

The hooks pipeline does **not** scan reasoning content today, but this is
**not a bug** — it is a deliberate architectural choice documented at
`packages/shared/transport/normalize/projection.go:32-37`:

```go
type TextProjectionOptions struct {
    // IncludeReasoning, when true, adds ContentReasoning blocks to the
    // projection. Default false: reasoning is informational metadata,
    // not user-spoken content.
    IncludeReasoning bool
}
```

The infrastructure for opting in is already in place
(`TextProjectionWith(opts)` at projection.go:41). Hooks invoke
`TextProjection()` (the no-opts form, line 24) so they get the default
`IncludeReasoning: false`.

The story is therefore to **expose this toggle to operators**, not to
fix anything in the hooks pipeline itself.

## User story

> As a compliance operator, I want to opt specific hook rules in to
> scanning the model's reasoning content (chain-of-thought), so that
> for regulated workloads where PII may appear in the model's internal
> reasoning the rule still fires and redacts. For non-regulated
> workloads I want to keep today's behavior (reasoning content is
> "informational metadata" and bypasses scan), so I do not pay the
> extra regex-match cost on every request.

## Architecture decision

- **Default remains `IncludeReasoning: false`.** No effective behavior
  change for existing rule configs.
- **Opt-in is per-hook-rule**, not per-pipeline or per-tenant. Different
  hooks may have different compliance requirements (PII detector =
  yes, sensitive-word block = maybe, rate limiter = irrelevant).
- **Where the toggle lives**: in the hook rule's JSON config blob (the
  per-rule schema stored in the DB), under a new field `scope`.
- **Toggle values**: `"default"` (no reasoning scan, today's behavior)
  or `"include_reasoning"` (also scan reasoning blocks). Omitting the
  field is treated as `"default"`.

Future toggle values can extend the enum without a schema change:
e.g. `"include_tool_results"` later if HTTP request bodies need a
similar opt-out. The string-enum shape is forward-compatible.

## Tasks

### T5.1 — Hook input scope plumbing

**File:** `packages/shared/policy/hooks/types.go`

`HookInput.TextSegments()` at line 102 currently calls
`TextProjection()` without options. Replace with a version that reads
the rule's `Scope` field:

```go
func (i *HookInput) TextSegments() []string {
    return i.TextSegmentsWith(i.Rule.Scope)
}

func (i *HookInput) TextSegmentsWith(scope string) []string {
    if i.Normalized == nil {
        return nil
    }
    opts := normalize.TextProjectionOptions{}
    if scope == "include_reasoning" {
        opts.IncludeReasoning = true
    }
    return i.Normalized.TextProjectionWith(opts)
}
```

Each hook implementation (`pii_detector.go`, `keyword_filter.go`, etc.)
already calls `input.TextSegments()` — no change needed in individual
hooks. The plumbing is purely in `HookInput`.

`HookInput.Rule` needs a `Scope string` field. Audit where `HookInput`
is constructed (likely in the hook registry / dispatcher) and pass the
rule's `Scope` from its DB config.

### T5.2 — SpansFromModifiedContent iteration scope

**File:** `packages/shared/policy/hooks/types.go:299`

The current line filters spans to ContentText / ContentToolResult only.
When `scope == "include_reasoning"`, also accept ContentReasoning:

```go
allowedTypes := map[normalize.ContentType]bool{
    normalize.ContentText:       true,
    normalize.ContentToolResult: true,
}
if input.Rule.Scope == "include_reasoning" {
    allowedTypes[normalize.ContentReasoning] = true
}
// ... iterate ...
if !allowedTypes[b.Type] { continue }
```

This ensures redact spans land on the right blocks when the rule scans
reasoning.

### T5.3 — Hook rule schema

**Decide at implementation:** the hook rule config is stored as JSONB
in the `HookConfig` table. The change is additive — add a `scope`
string field with no DB migration needed (JSONB tolerates new fields).

Document the new field in:
- `packages/shared/schemas/configtypes/hook_config.go` (the canonical Go struct
  for the JSONB shape)
- OpenAPI spec for the admin hook editor endpoint
- The hook rule's JSON schema used by the admin UI form

### T5.4 — Admin UI

**File:** `packages/control-plane-ui/src/pages/compliance/hooks/HookDetail.tsx`
(per memory `feedback_thing_config_template_audit_paths.md`-style
audit of where hook config form lives)

Add a checkbox: "Scan model reasoning content (in addition to visible
text)" with a tooltip explaining the trade-off:

> When enabled, this rule also scans the model's chain-of-thought /
> reasoning text. Useful for compliance scenarios where PII may appear
> in the model's internal reasoning. Off by default — reasoning is
> "informational metadata" not user-visible output, and scanning adds
> regex match cost on every request.

i18n keys:
- `pages:hooks.scope.includeReasoning.label`
- `pages:hooks.scope.includeReasoning.tooltip`

### T5.5 — Tests

- `packages/shared/policy/hooks/types_test.go`: case where `Rule.Scope =
  "include_reasoning"` and payload contains both text + reasoning →
  TextSegments returns both.
- `packages/shared/policy/hooks/pii_detector_test.go`: case where rule with
  reasoning scope detects PII embedded in reasoning content.
- `packages/control-plane-ui/.../HookDetail.test.tsx`: checkbox round-
  trips correctly through save/load.

### T5.6 — Documentation

- Update `docs/developers/architecture/services/ai-gateway/hook-architecture.md` with a section on rule scope.
- Update the hook config OpenAPI spec.
- Operator release note: "New per-rule toggle to scan model reasoning
  content. Default off — existing rules unchanged."

## Acceptance criteria

- AC-5.1: A hook rule with `scope: "include_reasoning"` and a PII
  pattern matching SSN-like strings fires on a request where the SSN
  appears only in the model's reasoning content (not in visible text).
- AC-5.2: The same rule with `scope: "default"` (or omitted) does not
  fire on the same request — today's behavior.
- AC-5.3: Admin UI checkbox round-trips through save/load and matches
  the stored DB value.
- AC-5.4: No regression in any existing hook rule (all current rules
  default to `scope: "default"` which preserves today's behavior).
- AC-5.5: Tooltip surfaces the trade-off in en/zh/es locales.

## Verification

- `go test ./packages/shared/policy/hooks/... -race -count=1`
- `npm test -w packages/control-plane-ui` (HookDetail test)
- Live test: enable `include_reasoning` on a PII rule, send a request
  with SSN in user prompt + verify the PII detector also catches a
  separate SSN injected into Claude's thinking response. Confirm
  `traffic_event.request_hook_decision` or `response_hook_decision`
  reports the rule fired.

## Risks

- **R-5.1**: HookConfig JSONB shape drift. The new `scope` field must
  be tolerated by older readers (default to `""` / `"default"`). Use
  the `omitempty` JSON tag for forward / backward compat. Confirmed
  by existing pattern in `packages/shared/schemas/configtypes/`.
- **R-5.2**: Operators forget the trade-off when enabling the toggle
  and complain about increased latency on hot-path. Mitigation: the
  tooltip is explicit; the admin UI also surfaces "this rule will
  scan reasoning content (additional CPU)" badge on the rule row
  after save. Out of scope for this story to add the badge — separate
  follow-up.
- **R-5.3**: i18n string keys must be added to en/zh/es per CLAUDE.md
  binding rule. Standard checklist applies; no architectural concern.
