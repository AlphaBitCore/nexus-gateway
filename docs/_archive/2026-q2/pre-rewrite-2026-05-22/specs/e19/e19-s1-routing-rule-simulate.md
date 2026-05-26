# E19 — Story 1: Routing Rule Simulate Endpoint

## Context

The Control Plane UI at `/config/routing/:id` renders a "Routing preview (simulate)" card that POSTs to `POST /api/admin/routing-rules/simulate`, but no handler is registered on either the Control Plane or the AI Gateway — the call returns HTTP 404. The IAM action `admin:SimulateRoutingRule` is already defined in `packages/control-plane/internal/iam/managed.go` and the IAM Simulator exposes it as a testable action.

The AI Gateway's routing engine (`packages/ai-gateway/internal/router`) already produces a `RoutingPlan` containing per-stage pipeline trace, per-strategy trace, winning rule identity, primary targets, recovery targets, and optional narrowing summary — exactly the information the simulate feature needs. This story exposes that plan via a new internal endpoint and a new Control Plane forwarder.

## User Story

**As a** platform admin or provider-ops engineer,
**I want** to POST a hypothetical `{modelId, endpointType, messages?}` to the Control Plane admin API and receive the full routing decision trace,
**so that** I can validate a new routing rule's behavior — or reproduce a production routing incident — without sending real traffic.

## Tasks

### T1 — Define response DTO in AG handler package

Create `packages/ai-gateway/internal/handler/routing_simulate_endpoint.go` with:

```go
type simulateRequest struct {
    ModelID      string           `json:"modelId"`
    EndpointType string           `json:"endpointType"`
    Messages     []map[string]any `json:"messages,omitempty"`
}

type simulateResponse struct {
    Request          simulateRequestEcho     `json:"request"`
    OriginalModelID  string                  `json:"originalModelId"`
    Substituted      bool                    `json:"substituted"`
    RuleID           string                  `json:"ruleId,omitempty"`
    RuleName         string                  `json:"ruleName,omitempty"`
    Stages           []stageEntry            `json:"stages"`
    Trace            []traceEntry            `json:"trace"`
    Targets          []targetEntry           `json:"targets"`
    RecoveryTargets  []targetEntry           `json:"recoveryTargets"`
    NarrowingSummary *router.NarrowingSummary `json:"narrowingSummary,omitempty"`
    Warnings         []string                `json:"warnings,omitempty"`
}
```

`stageEntry`, `traceEntry`, `targetEntry` are local DTOs that project `router.PipelineTraceEntry`, `router.TraceEntry`, and `router.RoutingTarget` with explicit JSON tags (the engine's `RoutingTarget` struct has no JSON tags today). Keeping the DTOs local to the handler package keeps engine types untouched.

### T2 — Implement AG handler

Add to `packages/ai-gateway/cmd/ai-gateway/main.go` right next to `POST /internal/provider-test`:

```go
mux.HandleFunc("POST /internal/routing-simulate", handler.RoutingSimulateHandler(routerResolver, logger))
```

Handler behavior:

1. Decode body; return 400 on malformed JSON.
2. Require `modelId` non-empty; return 400 `{"error":"modelId is required"}` otherwise.
3. Default `endpointType` to `"chat/completions"` when empty; normalize `"chat"` → `"chat/completions"` to match production.
4. Build `RoutingContext`:
   - `RequestedModel = {ID: modelId}` (Type/ProviderID/ProviderModelID left blank; engine fills from catalog during evaluation).
   - `EndpointType = normalized`.
   - `VirtualKey = nil` (MVP).
   - `Headers`: if `messages` provided, stringify via `json.Marshal` and set `headers["x-smart-messages"] = string(encoded)` to feed smart strategies — mirrors `Resolver.ResolveTargets` at `resolver.go:200-206`.
5. Call `resolver.Resolve(ctx, rctx)`. On engine error, return 500 with the error message; do not return 404.
6. Project the returned `RoutingPlan` into `simulateResponse`:
   - `Stages` ← `plan.PipelineTrace`.
   - `Trace` ← `plan.Trace`.
   - `Targets` ← `plan.Targets` (mapped to `targetEntry`).
   - `RecoveryTargets` ← `plan.RecoveryTargets` (mapped).
   - `NarrowingSummary` ← `plan.NarrowingSummary`.
   - `OriginalModelID` ← `plan.OriginalModelID`.
   - `Substituted` ← `plan.Substituted`.
   - `RuleID` / `RuleName` ← `plan.RuleID` / `plan.RuleName`.
7. Append warnings:
   - `"no stage-1 rule matched — request would be rejected by router"` when `plan.RuleID == ""` and `len(plan.Targets) == 0`.
   - `"smart routing requested but messages is empty"` when `modelId == "auto"` and `len(Messages) == 0`.
8. Emit a structured log line `logger.Info("routing simulate", "modelId", ..., "endpointType", ..., "ruleId", ..., "targets", len(targets))`.

Return JSON with `Content-Type: application/json`, HTTP 200.

### T3 — Implement CP forwarder

Add to `packages/control-plane/internal/handler/admin_routing.go`:

```go
g.POST("/routing-rules/simulate", h.RoutingSimulate, iamMW("admin:SimulateRoutingRule"))
```

And implement `func (h *AdminHandler) RoutingSimulate(c echo.Context) error` mirroring the `forwardProviderTest` pattern at `admin_extras.go:291`:

1. Bind the body as `map[string]any` (pass-through shape).
2. POST to `strings.TrimRight(h.Proxy.AIGatewayURL, "/") + "/internal/routing-simulate"` with `Content-Type: application/json`, 15s timeout.
3. Stream the upstream response body back verbatim with the upstream status code.
4. On transport error: return 502 `{"error":"AI Gateway unreachable: <detail>"}` (not 404 — 404 is reserved for the endpoint itself being absent).

### T4 — Frontend type + rendering

Modify `packages/control-plane-ui/src/api/services/routing.ts`:

```ts
export interface RoutingSimulateRequest {
  modelId: string;
  endpointType: string;
  messages?: Array<{ role: string; content: string }>;
}

export interface RoutingSimulateResponse {
  request: { modelId: string; endpointType: string };
  originalModelId: string;
  substituted: boolean;
  ruleId?: string;
  ruleName?: string;
  stages: Array<{ stage: number; decision: string; durationMs: number }>;
  trace: Array<{ ruleId?: string; ruleName?: string; strategyType: string; decision: string; durationMs: number }>;
  targets: Array<{ providerId: string; providerName: string; modelId: string; modelName: string; providerModelId: string; source: string }>;
  recoveryTargets: Array<{ providerId: string; providerName: string; modelId: string; modelName: string; providerModelId: string; source: string }>;
  narrowingSummary?: { allowModelIds: string[]; denyModelIds: string[]; allowProviderIds: string[]; denyProviderIds: string[] };
  warnings?: string[];
}
```

Update `routingApi.simulate` return type to `Promise<RoutingSimulateResponse>` and `runSimulation` in `useRoutingRuleDetail.ts` to store typed data. The existing `<pre>{JSON.stringify(simData, null, 2)}</pre>` render is kept.

### T5 — Frontend button alignment fix

Rewrite `RoutingRuleDetailPage.tsx:62-72`. The current `<Stack direction="horizontal" align="end">` aligns the button to the bottom of the entire `FormField` (label + input + helpText). Replace with an input-row-level layout so the button sits at the same vertical center as the Input box regardless of label/helpText height.

Chosen approach: place the `<Button>` inside the `<FormField>` on the same flex row as `<Input>` by wrapping them in a small horizontal flex container. Remove `align="end"` from the outer Stack. Add a dedicated CSS class `.simInputGroup` in `RoutingRuleDetail.module.css` with `display:flex; gap:var(--g-space-2); align-items:center;` and `flex:1` on the Input wrapper.

### T6 — Unit tests (Go)

Add `packages/ai-gateway/internal/handler/routing_simulate_endpoint_test.go` with table-driven cases:

1. **modelId empty** → 400.
2. **no rules in DB** → 200, `targets: []`, `warnings` contains "no stage-1 rule matched".
3. **single-strategy rule matches** → 200, `targets[0].providerId` matches the rule, `ruleId`/`ruleName` populated.
4. **auto modelId with smart rule + non-empty messages** → 200, `substituted: true`.
5. **auto modelId with no messages** → 200, `warnings` contains "smart routing requested but messages is empty".
6. **policy (stage-0) rule applies narrowing** → 200, `narrowingSummary` populated.
7. **endpointType normalization** → `"chat"` input returns same logical route as `"chat/completions"` input.

Use an in-memory stub `StrategyRegistry` where necessary (existing test helpers in `internal/router/*_test.go` provide patterns); reuse `packages/ai-gateway/internal/store` test harness for the DB layer if one exists, otherwise use `httptest.Server` at the handler level with a hand-built `*Resolver` using a stubbed `db` fake.

Minimal additional test on CP side: `admin_routing_test.go` or extend existing if present — smoke test that the route is registered and forwards body to the upstream URL (use `httptest.NewServer` as AG stand-in).

## Acceptance Criteria

1. `curl -X POST -b /tmp/nexus_cookie http://localhost:3001/api/admin/routing-rules/simulate -H 'Content-Type: application/json' -d '{"modelId":"auto","endpointType":"chat","messages":[{"role":"user","content":"Hello"}]}'` returns HTTP 200 with a JSON body conforming to `RoutingSimulateResponse`.
2. The same request authenticated as a user **without** `admin:SimulateRoutingRule` returns HTTP 403.
3. On the Routing Rule Detail page, clicking "Run simulation" renders a JSON response. The button is vertically aligned with the Input box, not the help text.
4. `go test -race -count=1 ./packages/ai-gateway/internal/handler/... ./packages/control-plane/internal/handler/...` passes.
5. `tsc --noEmit` in `packages/control-plane-ui` passes.
6. `go build ./packages/ai-gateway/... ./packages/control-plane/...` succeeds.
7. The simulate call does not produce a `traffic_event` row and does not emit MQ traffic events (verify by inspecting `SELECT count(*) FROM traffic_event WHERE ...` before/after).
