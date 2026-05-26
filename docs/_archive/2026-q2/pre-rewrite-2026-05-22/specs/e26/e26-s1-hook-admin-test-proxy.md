# E26 Story 1 — Admin hook test proxied through the AI gateway

**Epic:** 26 — Hook Admin Test Proxied Through AI Gateway
**Story:** 1
**Status:** Draft — 2026-04-22
**Requirements:** `docs/developers/specs/e26/e26-hook-admin-test-proxy.md`
**OpenAPI:** `docs/users/api/openapi/admin/e26-s1-hook-test.yaml`

## User Story

> **As a** platform admin,
> **I want** the admin "test hook" action to work for every builtin hook
> the AI gateway can actually run — including AI-gateway-local ones
> like `quality-checker` — without the control-plane needing to know
> which factories are registered where,
> **so that** I can dry-run any enabled hook against sample input before
> trusting it with live traffic, and the factory registry stops drifting
> between the two services.

## Context

- `POST /api/admin/hooks/{id}/test` is registered at
  `packages/control-plane/internal/handler/admin_extras.go:55`.
- The current handler (`HookTest`, admin_extras.go:548-589) instantiates
  builtin factories locally via `prepareBuiltinHook`, which calls
  `hooks.Registry.Get(hc.ImplementationID)` on the **shared** registry.
  AI-gateway-local registrations (see `main.go:178-179`) — `quality-checker`
  and `webhook-forward` — are invisible to that lookup.
- `ProviderTest` (admin_extras.go:144-163) and `RoutingSimulate`
  (admin_routing.go:44-74) already follow the pattern this story
  generalizes: CP handles auth and context, then proxies to an AI
  gateway `/internal/*` endpoint via `h.Proxy.AIGatewayURL`.
- Field shapes to bridge:
  - `store.HookConfig` (control-plane DB row) carries
    `Config json.RawMessage`.
  - `shared/hooks.HookConfig` (what factories accept) carries
    `Config map[string]any`.
  - The conversion lives inside the AI gateway handler; CP sends the
    row verbatim.

## Tasks

### T1. AI gateway — new handler + helpers

Files:

- `packages/ai-gateway/internal/handler/hooks_test_endpoint.go` (new)
- `packages/ai-gateway/cmd/ai-gateway/main.go`

Changes:

1. Introduce a request DTO that matches the JSON shape of
   `store.HookConfig` plus a `rawBody` field. Example:
   ```go
   type hooksTestRequest struct {
       HookConfig storedHookConfig `json:"hookConfig"`
       RawBody    string           `json:"rawBody"`
   }

   type storedHookConfig struct {
       ID                string            `json:"id"`
       Name              string            `json:"name"`
       ImplementationID  string            `json:"implementationId"`
       Stage             string            `json:"stage"`
       Config            json.RawMessage   `json:"config"`
       Priority          int               `json:"priority"`
       TimeoutMs         int               `json:"timeoutMs"`
       FailBehavior     string             `json:"failBehavior"`
       Enabled           bool              `json:"enabled"`
       ApplicableIngress []string          `json:"applicableIngress"`
   }
   ```
   The DTO is intentionally local to the AI gateway handler so the
   AI gateway never imports `packages/control-plane/internal/store`.
2. Add `HooksTestHandler(registry *hooks.HookRegistry, logger *slog.Logger) http.HandlerFunc`
   that:
   - Decodes the request DTO.
   - Unmarshals `Config` into a `map[string]any`.
   - Builds `shared/hooks.HookConfig` from the DTO.
   - Calls `registry.Get(implementationId)`; if nil, returns 400 with
     `{error, code: "unknown_implementation"}`.
   - Invokes the factory; on factory error returns 400 with
     `{error, code: "factory_error"}`.
   - Calls `buildHookTestInput(stage, strings.NewReader(rawBody))`
     (moved from the control-plane) to construct the `HookInput`.
   - Calls `runHook(ctx, hook, input, timeoutMs)` (also moved) to
     execute with a context-bounded timeout.
   - Returns 200 with `{output, executionTimeMs, stage}` on success or
     `{error, executionTimeMs, stage}` when `Execute` itself errored.
3. Move `buildHookTestInput` and `runHook` from
   `packages/control-plane/internal/handler/admin_extras.go` to the
   new AI gateway file. Update the imports accordingly.
4. Register the route in `main.go` immediately after
   `/internal/routing-simulate`:
   ```go
   mux.HandleFunc("POST /internal/hooks-test", handler.HooksTestHandler(gwHookRegistry, logger))
   ```

### T2. AI gateway — tests

Files:

- `packages/ai-gateway/internal/handler/hooks_test_endpoint_test.go` (new)

Changes:

Table-driven coverage for at minimum:

- **Happy path, shared builtin:** `content-safety` in request stage
  with a keyword that matches — expect decision `REJECT_HARD`,
  reasonCode `CONTENT_SAFETY_VIOLATION`.
- **Happy path, AI-gateway-local:** `quality-checker` in response
  stage with a short response body — expect the checker to flag
  the anomaly (mirror an existing `pipeline_test.go` scenario).
- **Unknown implementation:** `implementationId="nope"` → 400 with
  code `unknown_implementation`.
- **Invalid config:** malformed `config` bytes → 400 with code
  `invalid_config`.
- **Factory rejects:** `content-safety` missing `categories` →
  400 with code `factory_error`.
- **Execute timeout:** a stub hook whose `Execute` sleeps longer
  than `timeoutMs` → response `{error: "...", executionTimeMs: ≥ timeoutMs, stage: ...}`
  (not a panic).

Use `httptest.NewRecorder` and construct a small stub
`hooks.HookRegistry` by registering real factories or a test-local
fake for the timeout case.

### T3. Control-plane — proxy refactor and deletions

Files:

- `packages/control-plane/internal/handler/admin_extras.go`

Changes:

1. In `HookTest`, replace the builtin branch with:
   ```go
   return h.forwardHookTest(c, hc)
   ```
   Keep the webhook branch exactly as-is.
2. Add `forwardHookTest(c echo.Context, hc *store.HookConfig) error`
   mirroring `forwardProviderTest`:
   - Reads the raw request body (up to 256 KiB).
   - Marshals `{hookConfig: hc, rawBody: string(body)}`.
   - POSTs to `strings.TrimRight(h.Proxy.AIGatewayURL, "/") + "/internal/hooks-test"`
     with a 15 s client timeout.
   - Relays status code and body bytes verbatim (`io.LimitReader`
     capped at 512 KiB like `forwardProviderTest`).
   - On transport error, returns HTTP 502 with
     `{"error": "AI Gateway unreachable: <detail>"}`.
3. Delete `prepareBuiltinHook`, `hookSetupError`, `runHook`,
   `buildHookTestInput`, and any now-unused imports (notably the
   `packages/shared/policy/hooks` import if it is only used by these
   helpers).

### T4. Control-plane — tests

Files:

- `packages/control-plane/internal/handler/admin_extras_hook_test_test.go`

Changes:

1. Delete every test that exercised the deleted helpers
   (`TestBuildHookTestInput_*`, `TestPrepareBuiltinHook_*`). Do not
   leave skipped or commented-out tests — the helpers are gone.
2. Add `TestHookTest_ProxiesBuiltinToAIGateway` that:
   - Stands up an `httptest.NewServer` whose
     `POST /internal/hooks-test` returns a canned JSON body.
   - Configures the admin handler with that server's URL as
     `Proxy.AIGatewayURL`.
   - Seeds a `builtin` hook config in the DB fixture.
   - Issues `POST /api/admin/hooks/{id}/test`, asserts the response
     body matches the canned payload byte-for-byte.
3. Add `TestHookTest_ReturnsBadGatewayWhenAIGatewayDown` that points
   at an unreachable port and asserts HTTP 502 with the error shape.

### T5. OpenAPI + hook architecture doc

Files:

- `docs/users/api/openapi/admin/e26-s1-hook-test.yaml` (new)
- `docs/developers/architecture/services/ai-gateway/hook-architecture.md`

Changes:

1. Create the OpenAPI spec documenting
   `POST /api/admin/hooks/{id}/test`: request body, response shape
   (both success and runtime-error variants), 400/404/502 responses,
   and a description line explaining that builtin tests are
   delegated to the AI gateway.
2. Update `docs/developers/architecture/services/ai-gateway/hook-architecture.md` §5 to include a new
   subsection "Admin Hook Test (proxy path)" (completed in parallel
   to this SDD).

## Acceptance Criteria

| AC | Description |
|---|---|
| AC1 | `go build ./packages/...` succeeds. |
| AC2 | `go test -race -count=1 ./packages/ai-gateway/... ./packages/control-plane/...` all green, including the new handler tests in T2 and T4. |
| AC3 | `POST /api/admin/hooks/{quality-checker-id}/test` returns a valid `{output, executionTimeMs, stage}` body; previously it returned `{error: "unknown builtin implementationId \"quality-checker\""}`. |
| AC4 | `POST /api/admin/hooks/{content-safety-id}/test` still returns a correct decision (no regression on shared builtins). |
| AC5 | `grep -n "prepareBuiltinHook\|buildHookTestInput\|runHook" packages/control-plane/internal/handler/` returns nothing — the deletions landed. |
| AC6 | The control-plane binary no longer imports `packages/shared/policy/hooks` from `admin_extras.go` (verified via `goimports -l` or manual inspection). |
| AC7 | `docs/users/api/openapi/admin/e26-s1-hook-test.yaml` validates as OpenAPI 3.1 and contains request + response schemas for the public endpoint. |

## Risks

- **AI gateway must be restarted** to pick up the new route. The
  running debug binary on a developer's workstation will 404 until
  it is restarted; the close-out reply explicitly reminds the
  developer.
- **Wire schema drift:** if either side of the proxy ever deviates
  from the shared JSON shape, the test endpoint will silently
  mis-decode fields. Mitigation: the AI gateway DTO JSON tags
  exactly mirror `store.HookConfig` JSON tags, and a test round-trips
  a real serialized `store.HookConfig` through the handler.
- **Scope creep into the UI:** no UI changes should be required —
  the public endpoint shape is unchanged. If a reviewer flags UI
  adjustments, they belong in a follow-up story, not here.

## Out of Scope

- UI changes (`packages/control-plane-ui`). The public endpoint shape
  is unchanged.
- `builtinHookImplementations` list in `admin_extras.go` — stays as
  UI metadata.
- Renaming or reshaping hook configs, adding new hook types, or
  changing execution semantics.

## Amendment (2026-04-24) — Rule pack enrichment on hook test

`POST /internal/hooks-test` (`HooksTestHandler`) now takes an optional
`rulepack.InstallLister`. When the AI gateway runs with Postgres, the
handler calls `rulepack.Enrich` on the supplied hook row **before**
factory construction, identical to the live `HookConfigCache` loader path.
Without this step, admin tests used only the inline `HookConfig.config`
JSON; hooks with bound rule packs incorrectly fell back to legacy
keyword/category paths and could return `Approve` for text that production
would reject.

Wire: `cmd/ai-gateway/main.go` passes the same `rulepack.Store` instance
used for pipeline loads. Regression test:
`TestHooksTestHandler_ContentSafety_RulePackEnrichMatches` in
`hooks_test_endpoint_test.go`.

Control Plane UI: the Hook **Test** tab shows an extra note for
rule-pack-capable implementations clarifying that the gateway resolves
installs from the database for the test.
