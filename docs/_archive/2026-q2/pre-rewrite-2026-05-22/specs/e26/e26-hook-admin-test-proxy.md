# E26 — Hook Admin Test Proxied Through AI Gateway

**Status:** Draft — 2026-04-22
**Epic:** 26
**Depends on:** Shared hook registry (`packages/shared/policy/hooks/registry.go`), AI Gateway hook registry extension (`packages/ai-gateway/cmd/ai-gateway/main.go`), Control Plane admin hook test endpoint (`packages/control-plane/internal/handler/admin_extras.go`).

## 1. Business Goal

Admin users rely on `POST /api/admin/hooks/{id}/test` to dry-run a hook
configuration against sample input before enabling it on live traffic.
Today, the endpoint works for hook implementations registered in the
shared registry (`pii-detector`, `keyword-filter`, `content-safety`,
`data-residency`, `rate-limiter`, `request-size-validator`,
`ip-access-filter`, `noop`) but fails with
`unknown builtin implementationId "quality-checker"` for any hook whose
factory is registered only on the AI gateway's local registry
(`quality-checker`, plus `webhook-forward` when typed as builtin).

The root cause is that the control-plane instantiates the factory
itself, so it can only see shared factories. The AI gateway has a
superset — the shared clone plus its local extensions. Maintaining two
parallel factory tables invites the exact drift this epic closes.

This epic moves admin hook test execution from the control-plane into
the AI gateway via a new internal endpoint, so the AI gateway's existing
`gwHookRegistry` becomes the single source of truth for which builtin
implementations can run. The control-plane's role reduces to auth,
loading the hook config from the database, and forwarding the call —
the same shape it already uses for provider connectivity tests and
routing simulation.

## 2. Scope

### In scope

- New AI gateway endpoint `POST /internal/hooks-test` that accepts the
  serialized hook config (as stored by the control-plane) plus a raw
  request body, resolves the factory from `gwHookRegistry`, runs
  `Execute` against the built `HookInput`, and returns
  `{output, executionTimeMs, stage}` or an error shape.
- Control-plane `HookTest` handler refactor: builtin branch proxies to
  the new AI gateway endpoint via a `forwardHookTest` helper that
  mirrors the existing `forwardProviderTest`.
- Delete `prepareBuiltinHook`, `runHook`, `buildHookTestInput`, and the
  `hookSetupError` type from the control-plane (moved to AI gateway or
  removed entirely). Per dev-phase policy, the old code path is gone
  the moment the new one ships — no parallel implementation.
- Unit tests: AI gateway gets a new handler test covering happy path
  for `quality-checker` and `content-safety`, unknown implementation,
  invalid config, and timeout; control-plane tests for the deleted
  helpers are removed, and a new test covers the proxy forwarding
  behavior (including the AI-gateway-unreachable path).
- OpenAPI spec for the public `POST /api/admin/hooks/{id}/test`
  endpoint, documenting that builtin execution is delegated to the AI
  gateway.
- Hook architecture doc updated to describe the proxy path.

### Out of scope

- Changes to the `builtinHookImplementations` metadata list — it stays
  in the control-plane because it is consumed by
  `GET /api/admin/hooks/implementations` and is UI-only metadata (no
  factory wiring). It remains the UI's source of truth for the
  `configSchema` rendered by the admin form.
- Webhook-type hook test. That path does not depend on a Go factory —
  it already makes a direct HTTP call to the user-configured endpoint
  — so proxying it through the AI gateway would add a hop without
  value.
- Changes to hook execution semantics or pipeline construction.
  Admin-test execution stays side-effect free: no traffic event, no
  audit row, no MQ emission.

## 3. User Roles & Personas

| Role | Need met by this epic |
|---|---|
| **Platform Admin** | Test any enabled hook, including AI-gateway-local ones (`quality-checker`), against a sample body before letting it see live traffic. |
| **Platform Maintainer** | Stop reconciling two factory tables. One registry in the AI gateway is authoritative. |

## 4. Functional Requirements

### F1 — AI gateway internal endpoint (MUST)

`POST /internal/hooks-test` MUST accept a JSON body containing the full
stored hook config (ID, name, implementationId, stage, priority,
timeoutMs, failBehavior, enabled, applicableIngress, config) plus a
sample input (prompt string and/or messages array). It MUST resolve the
factory through the AI gateway's `gwHookRegistry`, construct a
`shared/hooks.HookConfig`, invoke the factory, and execute the hook
with a context bounded by `timeoutMs`. The response MUST be the same
`{output, executionTimeMs, stage}` (or `{error, executionTimeMs, stage}`)
shape the CP handler returns today so no UI change is required.

### F2 — CP proxy forwarding (MUST)

`POST /api/admin/hooks/{id}/test` in the control-plane MUST fetch the
hook config from the database, marshal it plus the raw request body,
POST to `AI_GATEWAY_URL/internal/hooks-test`, and relay the status
and body back to the admin client verbatim. Timeout for the forwarded
request MUST be at least the hook's configured `timeoutMs` plus a
small operational buffer (default 2 s).

### F3 — Webhook branch unchanged (MUST)

When the stored hook has `type="webhook"` and a non-empty endpoint, the
existing `runWebhookHookTest` local path MUST continue to be used.
Proxying webhook tests adds no value — the hook is already an HTTP call.

### F4 — Factory source of truth (MUST)

After this epic lands, there MUST be no factory lookup in the
control-plane for admin hook test. The only imports from
`packages/shared/policy/hooks` that remain in the control-plane are those
needed for other concerns (if any); none are needed solely by the
test handler.

### F5 — Failure modes (MUST)

- AI gateway unreachable → CP returns HTTP 502 with a clear error body,
  mirroring `forwardProviderTest`.
- Unknown implementationId → AI gateway returns 400 with
  `code: "unknown_implementation"`; CP forwards 400 verbatim.
- Invalid config JSON in the stored row → AI gateway returns 400 with
  `code: "invalid_config"`; CP forwards verbatim.
- Hook `Execute` returns an error or exceeds timeout → AI gateway
  returns 200 with `{error, executionTimeMs, stage}` (matching today's
  contract for runtime-level errors); CP forwards verbatim.

## 5. Non-Functional Requirements

### NF1 — Latency budget

Admin hook test is interactive but not on the hot path. Adding one
HTTP hop between CP and AI gateway MUST NOT increase P50 latency by
more than ~10 ms on a loopback deployment. This is well within the
acceptable budget for an admin action.

### NF2 — Test coverage

- AI gateway: new handler test file covering at minimum
  `quality-checker`, `content-safety`, unknown implementation, invalid
  config, and execute-timeout paths.
- Control-plane: new test asserting the handler forwards to a stub
  AI gateway and relays the response; another asserting a 502 on
  AI-gateway-unreachable.
- Existing control-plane tests for deleted helpers MUST be removed
  (not skipped or commented out) per dev-phase policy.

### NF3 — No regression on shared builtins

All existing shared builtins (`content-safety`, `pii-detector`, etc.)
MUST still execute correctly through the proxy. Post-epic, a manual
simulate of `content-safety` MUST still return the same output shape
as before.

## 6. Constraints & Assumptions

- The AI gateway is running and reachable from the control-plane. This
  is already an assumption for `ProviderTest` and `RoutingSimulate`.
- The stored hook config schema does not change; the wire shape to
  `/internal/hooks-test` is a serialization of existing types plus a
  `rawBody` field.
- Pre-GA policy: no backward compatibility, no parallel implementation,
  no deprecation markers. The old local-factory path is deleted in the
  same change.

## 7. Glossary

- **Builtin hook**: a hook whose behavior is implemented in Go inside
  the data plane; configured via `type=builtin` in the database.
- **Webhook hook**: a hook that delegates to a user-configured HTTP
  endpoint; configured via `type=webhook`.
- **`gwHookRegistry`**: the AI gateway's cloned-and-extended copy of
  the shared hook registry, holding every factory the data plane can
  run.
- **Admin hook test**: the dry-run of a configured hook against
  user-supplied sample input, side-effect free (no traffic event,
  no audit row, no MQ emission).

## 8. Priority

| Req | MoSCoW |
|---|---|
| F1 AI gateway endpoint | Must |
| F2 CP proxy forwarding | Must |
| F3 Webhook branch untouched | Must |
| F4 Single factory source of truth | Must |
| F5 Failure-mode contracts | Must |
| NF1 Latency budget | Should |
| NF2 Test coverage | Must |
| NF3 No regression on shared builtins | Must |
