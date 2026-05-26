# Fix — PII Detector Config Schema Alignment + Hook Test Endpoint Real Execution

## Context

Two related defects in the Go compliance hook subsystem:

1. **Config schema mismatch (silent production failure).** Seed data in `tools/db-migrate/seed/seed-hook-configs.ts:48-80` persists the `pii-scanner` / `pii-outbound-scanner` hooks with:

   ```json
   {
     "patternDefinitions": [
       { "id": "email", "regex": "\\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\\.[A-Za-z]{2,}\\b", "flags": "g" },
       { "id": "phone",  "regex": "...", "flags": "g" },
       { "id": "ssn",    "regex": "...", "flags": "g" },
       { "id": "credit_card", "regex": "...", "flags": "g" }
     ],
     "action": "block"
   }
   ```

   The Go implementation at `packages/shared/policy/hooks/pii_detector.go:39-117` expects a different shape — `{ types: [...], customPatterns: [...], action: "reject_hard" | "reject_soft" | "redact" }` — inherited from the archived Node gateway. When the pipeline calls `factory(cfg)` (via `packages/shared/compliance/policy.go:161`), `NewPiiDetector` returns `error: pii-detector: unknown action "block"`, the factory fails, and the hook is never resolved into the pipeline. PII scanning has therefore never actually protected production traffic since the Go cutover.

2. **Hook test endpoint stub.** `packages/control-plane/internal/handler/admin_extras.go:519-524` — `HookTest` returns hardcoded `{"decision":"approve","reason":"test mode"}` for every builtin hook type, masking bug (1) and preventing operators from verifying hook behavior in the UI's Test tab. This violates the project rule "Real implementation only (production code)" in CLAUDE.md.

The UI side (`packages/control-plane-ui/src/pages/config/hooks/HookDetail.tsx:283-311`) renders the hook config as read-only JSON, so the seed file is the de facto source-of-truth for the config schema. This story aligns the Go implementation with the seed's `patternDefinitions` shape and replaces the stub with real execution, eliminating both defects in the same change.

## User Story

**As a** compliance operator using the Control Plane UI,
**I want** the PII scanner hook to actually detect PII in requests, and the Test tab to actually run the hook against my input,
**so that** I can trust that PII rules work in production and verify them through the UI without reading logs from the AI Gateway.

## Tasks

### T1 — New `pii-detector` config schema (canonical)

Redefine the canonical `pii-detector` config shape in `packages/shared/policy/hooks/pii_detector.go` documentation and code:

```jsonc
{
  "patternDefinitions": [
    {
      "id": "email",              // required — short name, used in reason code and default replacement
      "regex": "\\b[...]\\b",     // required — Go-regexp2-compatible pattern (RE2 syntax, no lookarounds)
      "flags": "g",               // optional — subset of JS flags: "g" (no-op), "i", "m", "s"
      "luhn": false,              // optional — when true, matches are additionally validated with Luhn (credit-card use case)
      "replacement": "[REDACT]"   // optional — used only when action = "redact"; defaults to "[REDACTED_<ID_UPPER>]"
    }
  ],
  "action": "block"               // required — one of "block" | "warn" | "redact"
}
```

Action enum mapping:

| config action | Decision        | Semantics                                         |
|---------------|-----------------|---------------------------------------------------|
| `block`       | `REJECT_HARD`   | Hard block — short-circuit on first match         |
| `warn`        | `REJECT_SOFT`   | Soft block — short-circuit on first match         |
| `redact`      | `MODIFY`        | Replace all matches across all content blocks     |

Flag translation (JS → Go inline flag prefix):

| flag | Go equivalent   | Notes                                                          |
|------|-----------------|----------------------------------------------------------------|
| `g`  | (no-op)         | Go's `FindAllString` is globally-scoped by default             |
| `i`  | `(?i)` prefix   | Case-insensitive                                               |
| `m`  | `(?m)` prefix   | Multi-line — `^` / `$` match line boundaries                   |
| `s`  | `(?s)` prefix   | Dot-all — `.` matches newlines                                 |

Any other flag character (including `u`, `y`, `d`) returns `fmt.Errorf("pii-detector: patternDefinitions[%d] unsupported flag %q", i, flag)`. Flags may be empty; duplicate flags are reduced to a single inline prefix.

### T2 — Rewrite `NewPiiDetector`

`packages/shared/policy/hooks/pii_detector.go`:

- Delete the `builtinPII` map, the `types` branch, the `customPatterns` branch, and the `reject_hard` / `reject_soft` string handling. Per CLAUDE.md ("no backwards compatibility in development phase") there is no dual-schema reading.
- Rename `piiPattern.name` → `piiPattern.id` for consistency with the new schema.
- New parsing order:
  1. Require `config.patternDefinitions` as `[]any`. Each entry must be `map[string]any` with string `id` (non-empty), string `regex` (non-empty). `flags` / `luhn` / `replacement` are optional.
  2. Translate `flags` → inline prefix (`translatePiiFlags(flags string) (string, error)`).
  3. Compile `<prefix><regex>` with `regexp.Compile`. On error, return `fmt.Errorf("pii-detector: patternDefinitions[%d] invalid regex: %w", i, err)`.
  4. Resolve `replacement` with fallback `fmt.Sprintf("[REDACTED_%s]", strings.ToUpper(id))`.
  5. Build `piiPattern{id, re, luhn, replacement}`; append.
- Require `config.action` as a non-empty string. Accept only `"block"` / `"warn"` / `"redact"`. Map to `RejectHard` / `RejectSoft` / `Modify`. Unknown → error.
- `Execute` / `executeReject` / `executeRedact` logic is unchanged aside from referencing `p.id` instead of `p.name` in `result.Reason` / log lines.
- `luhnValid` helper unchanged.

### T3 — Add `pii_detector_test.go`

New file `packages/shared/policy/hooks/pii_detector_test.go` — this package has zero tests today. Use table-driven style consistent with the project.

Cases:

1. **Factory accepts seed-shaped config** — the exact `patternDefinitions` + `action: "block"` from `SEED_DEFAULT_PII_PATTERN_DEFINITIONS`; assert no error and `len(patterns) == 4`.
2. **Detect path — each built-in id** — for each of `email` / `phone` / `ssn` / `credit_card`, construct input with a known match; assert `Decision == REJECT_HARD`, `Reason` starts with `"PII detected:"`, `ReasonCode == "PII_DETECTED"`, `DataClassification == ClassConfidential`.
3. **No match — approve** — input `"hello world"` with all four patterns → `Decision == APPROVE`.
4. **Action — warn** → `REJECT_SOFT`.
5. **Action — redact** — email pattern + input `"email: user@example.com"` → `Decision == MODIFY`, `ModifiedContent[0].Text == "email: [REDACTED_EMAIL]"`.
6. **Redact — custom replacement** — `replacement: "***"` overrides the default.
7. **Luhn — true rejects invalid** — `luhn: true` on `credit_card` pattern; input `"4111 1111 1111 1112"` (invalid Luhn) → `APPROVE`; input `"4532 0151 1283 0366"` (valid Luhn) → `REJECT_HARD`.
8. **Luhn — false on credit-card pattern** (default) matches any 16-digit sequence without validation.
9. **Flags — `i` translated** — pattern `"SECRET"` with `flags: "i"` matches `"secret"`.
10. **Flags — `g` is no-op** — single occurrence of `email` pattern in redact mode still replaces the single match.
11. **Flags — unknown** — `flags: "u"` → factory error containing `unsupported flag`.
12. **Unknown action** — `action: "purge"` → factory error containing `unknown action`.
13. **Invalid regex** — `regex: "["` → factory error containing `invalid regex`.
14. **Missing `patternDefinitions`** → factory error.
15. **Missing `action`** → factory error.
16. **Empty `patternDefinitions`** → factory succeeds (pipeline is allowed to disable a rule by clearing patterns); Execute always returns APPROVE.

### T4 — Replace `HookTest` builtin stub

`packages/control-plane/internal/handler/admin_extras.go:487-525`. Restructure:

1. Existing: look up `hc := h.DB.GetHookConfig(...)`, 404 on miss.
2. Existing: `hc.Type == "webhook"` branch — keep unchanged.
3. **New builtin branch** (replaces lines 519-524):

   ```go
   // Builtin: resolve factory, construct HookConfig from stored JSONB, run hook.
   factory, ok := hooks.Registry.Get(hc.ImplementationID)
   if !ok {
       return c.JSON(http.StatusBadRequest, errJSON(
           fmt.Sprintf("unknown builtin implementationId %q", hc.ImplementationID),
           "unknown_implementation", ""))
   }
   var cfgMap map[string]any
   if len(hc.Config) > 0 {
       if err := json.Unmarshal(hc.Config, &cfgMap); err != nil {
           return c.JSON(http.StatusBadRequest, errJSON(
               "hook config JSON invalid: "+err.Error(), "invalid_config", ""))
       }
   }
   runtimeCfg := &hooks.HookConfig{
       ID:                hc.ID,
       ImplementationID:  hc.ImplementationID,
       Name:              hc.Name,
       Priority:          hc.Priority,
       Enabled:           hc.Enabled,
       Stage:             hc.Stage,
       FailBehavior:      hc.FailBehavior,
       TimeoutMs:         hc.TimeoutMs,
       ApplicableIngress: hc.ApplicableIngress,
       Config:            cfgMap,
   }
   hook, err := factory(runtimeCfg)
   if err != nil {
       return c.JSON(http.StatusInternalServerError, errJSON(
           "hook factory failed: "+err.Error(), "factory_error", ""))
   }

   // Parse test body: { input?: {prompt?:string, messages?:[...], ...}, sampleBody?: ..., statusCode?: int }
   var body struct {
       Input *struct {
           Prompt   string           `json:"prompt"`
           Messages []map[string]any `json:"messages"`
       } `json:"input"`
   }
   _ = json.NewDecoder(c.Request().Body).Decode(&body) // body is optional

   input := &hooks.HookInput{
       RequestID:   uuid.NewString(),
       Stage:       hc.Stage,
       IngressType: "AI_GATEWAY", // test harness default
       Method:      http.MethodPost,
       Path:        "/v1/chat/completions",
   }
   switch {
   case body.Input != nil && len(body.Input.Messages) > 0:
       for _, m := range body.Input.Messages {
           role, _ := m["role"].(string)
           content, _ := m["content"].(string)
           input.Content = append(input.Content, hooks.ContentBlock{
               Role: role, Type: "text", Text: content,
           })
       }
   case body.Input != nil && body.Input.Prompt != "":
       input.Content = []hooks.ContentBlock{{Role: "user", Type: "text", Text: body.Input.Prompt}}
   }

   timeout := time.Duration(hc.TimeoutMs) * time.Millisecond
   if timeout <= 0 {
       timeout = 3 * time.Second
   }
   ctx, cancel := context.WithTimeout(c.Request().Context(), timeout)
   defer cancel()

   start := time.Now()
   result, err := hook.Execute(ctx, input)
   elapsed := time.Since(start).Milliseconds()
   if err != nil {
       return c.JSON(http.StatusOK, map[string]any{
           "error":           err.Error(),
           "executionTimeMs": elapsed,
           "stage":           hc.Stage,
       })
   }
   return c.JSON(http.StatusOK, map[string]any{
       "output":          result,
       "executionTimeMs": elapsed,
       "stage":           hc.Stage,
   })
   ```

   Imports added: `context`, `encoding/json`, `fmt`, `time`, `github.com/google/uuid`, `github.com/ai-nexus-platform/nexus-gateway/packages/shared/policy/hooks`. Verify `uuid` is already a dependency (it is — used elsewhere in control-plane).

4. No use of `shared/store.BuildHookConfig`: that helper lives in the shared/store layer and is tied to a DB row projection not exactly matching `admin.HookConfig`. Inline construction is ~20 lines and avoids a cross-package coupling change. The helper can be consolidated in a later audit.

### T5 — Handler test

Extend (or create) `packages/control-plane/internal/handler/admin_extras_test.go`:

1. **builtin pii-detector — PII in input** — seed-shaped config, input `{"prompt":"hello user@example.com"}` → HTTP 200, `output.decision == "REJECT_HARD"`, `output.reasonCode == "PII_DETECTED"`, `output.hookName == "pii-scanner"`.
2. **builtin pii-detector — clean input** → HTTP 200, `output.decision == "APPROVE"`.
3. **not found** — random UUID → 404 + `error.code == "not_found"`.
4. **invalid implementationId** — factory miss → 400 + `error.code == "unknown_implementation"`.
5. **factory-level config error** — bad regex in `patternDefinitions` → 500 + `error.code == "factory_error"`.
6. **webhook path** — if a test exists already, keep green; if not, skip (out of scope for this story).

Use `httptest` + the existing test harness in the handler package (check neighbors like `admin_compliance_alert_channels_test.go` for the DB stub pattern).

### T6 — OpenAPI spec

New file `docs/users/api/openapi/admin/admin-hooks-test.yaml`. See Task #2 in the todo list — details intentionally separated for clarity. At minimum it must describe:

- `POST /api/admin/hooks/{id}/test`
- Request body: `{ input?: { prompt?: string, messages?: Message[] }, sampleBody?: object, statusCode?: integer }`
- 200 response for builtin: `{ output: HookResult, executionTimeMs: integer, stage: string }` where `HookResult` matches `packages/shared/policy/hooks/types.go:120-133`.
- 200 response for webhook: `{ output: any, executionTimeMs: integer, stage: string }` or `{ error: string, executionTimeMs: integer, stage: string }`.
- 400 `unknown_implementation` / `invalid_config`, 404 `not_found`, 500 `factory_error`.

### T7 — Update dev docs

`docs/developers/architecture/services/ai-gateway/hook-architecture.md` and `docs/developers/architecture/services/compliance-proxy/compliance-pipeline-architecture.md`:

- Replace any paragraph describing `types: [email, phone, ...]` / `customPatterns: [...]` with the `patternDefinitions` schema from T1.
- Update action list from `reject_hard | reject_soft | redact` to `block | warn | redact`.
- Keep the Luhn description — now an opt-in per-pattern flag rather than a hardcoded `credit_card` behaviour.
- English only. Keep whatever document structure exists; do not reformat surrounding sections.

### T8 — Verification

- `go test -race -count=1 ./packages/shared/policy/hooks/... ./packages/control-plane/...` passes (new tests included).
- `go build ./packages/shared/... ./packages/control-plane/... ./packages/ai-gateway/... ./packages/compliance-proxy/...` succeeds.
- Reproduce original curl: `curl -b /tmp/nexus_cookie -X POST http://localhost:3001/api/admin/hooks/1823a476-4796-46d7-b6fa-ec365aa181bf/test -H 'Content-Type: application/json' -d '{"input":{"prompt":"Hello world email: user@example.com"}}'` → returns `output.decision == "REJECT_HARD"` (not `"test mode"`).
- Start `ai-gateway`; confirm logs contain no `factory failed` / `unknown action` error for `pii-detector`.

## Acceptance Criteria

1. `POST /api/admin/hooks/{id}/test` against the seeded `pii-scanner` with input `{"prompt":"user@example.com"}` returns HTTP 200 with body where `output.decision == "REJECT_HARD"` and `output.reasonCode == "PII_DETECTED"`.
2. `POST /api/admin/hooks/{id}/test` against the seeded `pii-scanner` with input `{"prompt":"hello world"}` returns `output.decision == "APPROVE"`.
3. At AI Gateway boot, the compliance policy resolver produces a non-nil hook instance for the seeded `pii-scanner` — verifiable by sending a chat completion containing an email and observing a 400/403 block response (or the equivalent for `REJECT_HARD` in the gateway's error mapping) and a `traffic_event` row with a PII decision.
4. `NewPiiDetector` no longer accepts `types` / `customPatterns` / `reject_hard` / `reject_soft` — a unit test asserts all four shapes return an error.
5. `go test -race -count=1 ./packages/shared/policy/hooks/... ./packages/control-plane/...` is green.
6. `docs/developers/architecture/services/ai-gateway/hook-architecture.md` and `docs/developers/architecture/services/compliance-proxy/compliance-pipeline-architecture.md` describe the new `patternDefinitions` schema and the `block` / `warn` / `redact` action enum. No stale `types: [...]` mention remains in `docs/developers/architecture/**`.
7. `docs/users/api/openapi/admin/admin-hooks-test.yaml` exists and documents the real response shape (including the `output: HookResult` contract for builtin hooks).
