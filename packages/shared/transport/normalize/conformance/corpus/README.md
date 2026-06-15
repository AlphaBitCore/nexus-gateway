# Normalize conformance corpus

Golden cases that pin the exact `NormalizedPayload` the production
normalize registry emits for captured wire bodies. `TestConformanceCorpus`
(in `../harness_test.go`) re-normalizes every case through the same
registry assembly the services use (`normalize.BuildRegistry()`: Tier 1
AI builtins + Tier 1 per-host adapters + Tier 2 pattern probe + Tier 3
verbatim fallback) and fails on any output drift. Decoder refactors use
this corpus to prove output stability against real traffic shapes.

## Case layout

Each immediate subdirectory of `corpus/` is one case:

```
corpus/<case-name>/
  wire            raw captured bytes, exactly as seen on the wire
                  (JSON body, SSE stream, protobuf frame, ...)
  meta.json       adapter context handed to Registry.Normalize
  expected.json   golden NormalizedPayload (canonical 2-space JSON)
  BASELINE-WRONG.md   optional, see below
```

`meta.json` fields (all map 1:1 onto `core.Meta`; unknown keys are
rejected so misspelled keys fail loudly). The harness canonicalizes the
decoded meta exactly like the production entry point (`core.BuildAuditFn`):
`adapterType` is lowercased and Content-Type parameters (`; charset=...`)
are stripped, so a case can never exercise a meta shape the registry would
not see in production:

| field          | type   | notes                                              |
|----------------|--------|----------------------------------------------------|
| `adapterType`  | string | registry lookup key, e.g. `"openai"`, `"anthropic"` |
| `model`        | string | optional model hint                                |
| `contentType`  | string | Content-Type without parameters                    |
| `direction`    | string | required: `"request"` or `"response"`              |
| `endpointPath` | string | captured HTTP path, e.g. `"/v1/chat/completions"`  |
| `stream`       | bool   | true for SSE / chunked streaming captures          |

Case naming: `<adapter>-<shape>-<variant>`, e.g.
`openai-chat-nonstream-basic`, `anthropic-sse-tooluse-only`.

Wire files exported from production MUST be manually scrubbed (synthetic
same-shape replacements for account/org UUIDs, emails, session tokens)
before they are committed — see `tests/scripts/export-normalize-corpus.sh`.

## Adding a case / regenerating goldens

1. Create `corpus/<case-name>/` with `wire` and `meta.json`.
2. Generate the golden from current registry output:

   ```sh
   cd packages/shared
   go test ./transport/normalize/conformance/ -run TestConformanceCorpus -update-golden
   ```

3. Review the generated `expected.json` by hand — the flag records
   *current* behavior, which is not automatically *correct* behavior.
   If the output is wrong, either fix the decoder first or annotate the
   case with `BASELINE-WRONG.md` (below).
4. Run without the flag and confirm the case passes:

   ```sh
   go test -race -count=1 ./transport/normalize/conformance/ -v
   ```

Comparison is canonical: both sides are round-tripped through
`core.NormalizedPayload` and re-marshalled with 2-space indent, so key
order and whitespace in `expected.json` never cause false mismatches —
only real field-level drift does.

## `BASELINE-WRONG:` annotation

Some cases intentionally pin known-bad current behavior so a later fix
shows up as an explicit, reviewed golden change instead of silent drift.
Mark such a case by adding `BASELINE-WRONG.md` next to its `wire` file.
The first line must start with `BASELINE-WRONG:` followed by a one-line
summary of what is wrong; the rest of the file describes the correct
expected behavior and, when known, the responsible code path:

```
BASELINE-WRONG: tool_use input is flattened to a string instead of structured JSON
The anthropic SSE folder concatenates input_json_delta fragments but ...
```

The harness logs this first line next to the case result in `-v` output.
When the decoder is fixed: regenerate the golden with `-update-golden`,
verify the new golden shows the corrected behavior, and delete
`BASELINE-WRONG.md` in the same commit.
