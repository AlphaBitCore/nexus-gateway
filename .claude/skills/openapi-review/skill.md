---
name: openapi-review
description: Audit and enrich a generated Control Plane OpenAPI spec file claim-by-claim against the handler code. Pre-commit gate for the `openapi-gen` generator output — every per-kind spec under docs/users/api/openapi/control-plane/ must pass this audit before commit. The generator produces the structural draft (paths, verbs, types, status codes, IAM tiers); this skill verifies that draft against the handlers and fills the semantic layer the generator cannot recover: accurate field/operation descriptions, request/response examples, enum value sets, and the true required-field set. Trigger keywords: openapi review, openapi audit, review schema, audit api spec, enrich openapi, /openapi-review.
---

# openapi-review

Audit one generated OpenAPI spec file claim-by-claim against the control-plane handlers, then enrich it so an AI (or a partner) can understand and call the API correctly. Pre-commit twin of the `openapi-gen` generator.

Use this skill when:

- About to commit freshly generated specs under `docs/users/api/openapi/control-plane/` (binding gate for `openapi-gen`).
- Retroactively auditing a spec suspected of drift from its handler.
- Re-enriching a spec after a regeneration overwrote structural fields.

The generator is **structural**; this skill is **semantic**. The generator emits what the AST proves (paths, verbs, field names/types, optionality, status codes, IAM action + tier). It cannot recover what the handlers enforce imperatively — enum value sets held in `validXxx` maps, "field is required" checks, cross-field rules — nor human-readable descriptions or examples. That gap is this skill's job.

---

## Division of truth (binding)

- **Generator owns structure.** Paths, HTTP verbs, request/response field names + types, path parameters, status codes, `x-nexus-iam-action`, `x-nexus-tier`. If any of these is *wrong*, the fix is in the generator (`packages/nexus-cli/internal/openapigen/`), NOT a hand-edit — a hand-edit is clobbered on the next regen. Report the structural defect; do not paper over it.
- **This skill owns semantics.** `summary`, `description` (operation + per-field), `examples`, `enum`, the corrected `required` set, and any `x-nexus-*` clarification. These are enriched in place.

Because regeneration overwrites structural fields and the generated `summary`/`description`, enrichment is re-applied by re-running this skill after each regen. The handler code is the single source of truth for both passes.

---

## Hard rules

A spec file is CLEAN only if it satisfies ALL of:

1. **Code-anchored.** Every path, verb, field, enum value, status code, and required-flag traces to handler code that exists on disk right now (`packages/control-plane/**`). Cite the handler for each.
2. **No fabricated fields or enums.** An `enum` may only list values the handler actually accepts — found in a `validXxx` map, a `switch`, or a `oneof`/membership check. If you cannot find the value set in code, do NOT invent one; leave the field free and note it.
3. **`required` reflects handler validation, not the pointer heuristic.** The generator marks every non-pointer field required. Replace this with the set the handler actually rejects when absent (the `"X is required"` checks). Remove over-marked entries; add any required pointer field the handler demands.
4. **IAM extensions are immutable here.** Never alter `x-nexus-iam-action` / `x-nexus-tier` — they are code-derived. If one looks wrong, report it as a generator/handler issue.
5. **No `x-nexus-unresolved-*` left behind.** Those mark a type the generator could not map. Resolve to a real schema by reading the type, or report it.
6. **English-only**, timeless prose. No dates, no Epic/SDD/bug/PR references, no line numbers in descriptions.
7. **Surfaces system issues, not just spec issues.** If per-operation verification reveals a handler problem — a route with no validation, a response shape that contradicts the model, a dead field the UI anticipates — output it with: (a) finding, (b) code evidence, (c) recommended fix, (d) next action. The audit is the cheapest checkpoint.

Any violation = DRIFTED or FABRICATION verdict on that operation.

---

## The 5-pass audit

Run per spec file. `SPEC=docs/users/api/openapi/control-plane/<kind>.yaml`.

### Pass 1 — Mechanical sweep

```bash
SPEC=<spec-path>

# Valid YAML + OpenAPI 3.1 shell
python3 -c "import yaml,sys; d=yaml.safe_load(open('$SPEC')); assert d['openapi']=='3.1.0'; assert 'paths' in d" \
  || echo "DRIFT: not a valid 3.1 document"

# Generator gap markers must be gone
grep -nE 'x-nexus-unresolved-(type|basic)' "$SPEC" && echo "DRIFT: unresolved type markers remain"

# Every $ref resolves inside components.schemas
grep -oE '#/components/schemas/[A-Za-z0-9_]+' "$SPEC" | sort -u | while read r; do
  name=${r##*/}
  grep -qE "^        $name:" "$SPEC" || echo "MISSING component: $name"
done

# No dates / program refs / line numbers in prose
grep -nE '20[0-9]{2}-[0-9]{2}|\bE[0-9]+(-S[0-9]+)?\b|\bPR #?[0-9]+|\.(go|ts):[0-9]+' "$SPEC" \
  && echo "DRIFT: dates/program-refs/line-numbers found"

# Every operation still carries its code-derived extensions
python3 - "$SPEC" <<'PY'
import yaml,sys
d=yaml.safe_load(open(sys.argv[1]))
for p,item in d.get('paths',{}).items():
    for m,op in item.items():
        if 'x-nexus-tier' not in op: print(f"DRIFT: {m.upper()} {p} missing x-nexus-tier")
PY
```

Cross-check the file's operation set against `_index.yaml` for the same kind — a path/verb present in one but not the other means the file was hand-edited out of sync with a regen.

### Pass 2 — Per-operation audit (independent sub-agent)

Dispatch a `general-purpose` sub-agent (Read + grep). Feed it the spec file, the mechanical-pass output, and the kind. It resolves each operation's handler via the `operationId` (which is the Go handler name, e.g. `createQuotaPolicy` → `CreateQuotaPolicy`) and verifies + drafts enrichment.

**Required sub-agent prompt structure:**

```
You are a strict code-anchored OpenAPI auditor. Worktree: <path>.

Audit <spec-path> operation-by-operation against the control-plane
handlers under packages/control-plane/**. For EACH operation:

1. Find the handler. operationId is the Go handler name; grep for it
   (e.g. `func (h *Handler) CreateQuotaPolicy`). Cite file:line in your
   report (NOT in the spec).
2. Verify structure against the handler:
   - request body fields == the struct passed to c.Bind (names, types,
     optionality).
   - response status codes == the codes in c.JSON calls.
   - the success-response schema == the type returned on 200/201.
   Verdict each: VERIFIED / DRIFTED / FABRICATION. Structural drift is a
   GENERATOR bug — report it, do not propose a hand-edit.
3. Draft semantic enrichment grounded in the handler:
   - operation summary + description (what it does, from the handler).
   - per-field description for non-obvious fields.
   - enum: the exact value set the handler validates against. Quote the
     validXxx map / switch / membership check. NO invented values.
   - required: the fields the handler rejects when absent ("X is
     required" checks). This REPLACES the generator's pointer heuristic.
   - one realistic request example + one response example, values
     consistent with the field types and enums.

Hard rules: enum/required must cite handler code; never touch
x-nexus-iam-action / x-nexus-tier; English-only; no dates/line-numbers
in spec prose.

Report under 900 words: per-operation verdict + the concrete enrichment
YAML to splice in. End with CLEAN or N issues.
```

### Pass 3 — Apply enrichment + triage

- **FABRICATION / structural DRIFT** → it is a generator defect. Record it for a generator fix; do not mask it in the file.
- **Semantic enrichment** → splice the sub-agent's drafted `summary` / `description` / `enum` / corrected `required` / `examples` into the spec, preserving the generated structural fields and the `x-nexus-*` extensions.
- Verify each enum/required edit cites a handler check; drop any the sub-agent could not ground in code.

### Pass 4 — Re-run if substantial

If you applied enrichment to more than ~3 operations or removed any fabrication, re-dispatch Pass 2 on the edited file. Enrichment edits can introduce their own drift (a wrong enum value, an example that violates the schema).

### Pass 5 — Verdict + handoff

```
openapi-review: <spec-path>
  Pass 1 (mechanical): N hits → fixed
  Pass 2 (per-operation): N ops verified, N enriched, N structural defects → logged for generator
  Pass 4 (re-audit): CLEAN
  Verdict: CLEAN, ready to commit
```

A non-CLEAN verdict blocks commit of that spec. Structural defects logged here become generator fixes (`internal/openapigen/`) + a regen, not file hand-edits.

---

## Anti-patterns to refuse

- Inventing enum values or examples that "look right" — every enum traces to a `validXxx` map / membership check, or it does not go in.
- Trusting the generator's `required` list — it is a pointer heuristic and is usually wrong for create/update bodies. Always re-derive from handler validation.
- Hand-editing paths/verbs/types to fix structural drift — that is clobbered on regen. Fix the generator.
- Touching `x-nexus-iam-action` / `x-nexus-tier`. Code-derived; immutable in this skill.
- Declaring CLEAN after only the mechanical pass. The per-operation handler cross-check is where real drift and the enum/required corrections surface.
- Enriching a field with a description that restates its name ("the name field"). Describe its role and constraints from the handler, or leave it.
