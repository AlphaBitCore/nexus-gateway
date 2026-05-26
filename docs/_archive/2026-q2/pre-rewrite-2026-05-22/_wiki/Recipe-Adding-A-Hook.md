# Recipe Adding A Hook

*Audience: contributors adding a new compliance enforcement rule to the three-path pipeline.*

A hook is a Go implementation of the `Hook` interface that runs inside the AI Gateway, Compliance Proxy, and Desktop Agent to inspect, modify, or block traffic at one of three stages: `request` (before the upstream call), `response` (after the upstream returns), or `connection` (once per TLS connection). The same Go code runs in all three traffic paths, driven by the same `HookConfig` shape from the Hub-pushed shadow. The canonical architecture reference is [`hook-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/hook-architecture.md).

---

## The Hook interface

Every hook implements:

```go
// packages/shared/policy/hooks/core/types.go
type Hook interface {
    Execute(ctx context.Context, input *HookInput) (*HookResult, error)
    SupportsEndpoint(EndpointType) bool
    SupportsModality(Modality) bool
}
```

`HookInput` carries the canonicalized request or response — the `NormalizedPayload` from `shared/transport/normalize` (extracted text, messages, usage metadata, tool calls) plus stage-specific context. Hooks never receive raw provider JSON. `HookResult` carries the verdict (`Approve`, `RejectHard`, `BlockSoft`, `Modify`, `Abstain`) plus per-hook telemetry. Latency is stamped by the runner, not the hook.

---

## Step 1 — Choose the stage

Decide which stage the hook fires at:

- `request` — inspects the outbound prompt before the upstream call; can block, redact, or approve.
- `response` — inspects the upstream response; can block or redact a completed or streamed response.
- `connection` — fires once per TLS connection (receives SNI and cert context, no body); used for network-level policies.

Most content-inspection hooks use `request`. Quality checks and PII scanning of completions use `response`. `connection` is for firewall-style policies.

## Step 2 — Implement the hook package

Create `packages/shared/policy/hooks/<hook_name>/hook.go`:

```go
package myhook

import (
    "context"
    "packages/shared/policy/hooks/core"
    "packages/shared/policy/decision"
)

type Hook struct {
    // hook-specific config snapshot (read from HookConfig.config map)
}

func (h *Hook) Execute(ctx context.Context, input *core.HookInput) (*core.HookResult, error) {
    // Inspect input.Payload (NormalizedPayload — extracted text, messages, tools)
    // Return a verdict.
    return &core.HookResult{Verdict: decision.Approve}, nil
}

func (h *Hook) SupportsEndpoint(t core.EndpointType) bool {
    // Until endpoint-modality awareness ships, return t == core.EndpointTypeChat
    return t == core.EndpointTypeChat
}

func (h *Hook) SupportsModality(m core.Modality) bool {
    return m == core.ModalityText
}
```

The `onMatch` object in `HookConfig.config` drives what the pipeline does on a positive verdict — the hook itself only classifies. Wire `onMatch` reading into your struct's factory function.

## Step 3 — Register in init()

Add an `init()` function that registers the hook with the global registry:

```go
func init() {
    core.Register("<hook-name>", func(config map[string]any) (core.Hook, error) {
        return newFromConfig(config)
    })
}
```

The registry is built at process start. Each service constructs a dispatcher per stage; the dispatcher holds a `HookConfig` snapshot in an `atomic.Pointer` that hot-swaps when the Hub pushes a config change.

## Step 4 — Wire metrics

Add per-hook Prometheus counters inside `Execute`. The expected metrics are:

- `nexus_hook_decisions_total{hook="<name>",verdict="approve|reject_hard|block_soft|modify|abstain"}` — increment per call.
- `nexus_hook_latency_seconds{hook="<name>"}` — observe the execution duration.

The runner already stamps latency on the `HookResult`; re-expose it as a histogram so Prometheus can alert on slow hooks.

## Step 5 — Define HookConfig extension fields

If the hook needs configuration beyond `onMatch` (e.g., a keyword list, a threshold, a webhook URL), document the fields in the hook's package README and handle them in `newFromConfig`. The `HookConfig.config` column is JSONB — any well-typed JSON structure is valid. Validate the schema in `newFromConfig` and return an error for invalid configs; the pipeline surfaces config errors in the Config Sync status.

## Step 6 — Write table-driven unit tests

Cover each verdict path and each `inflightAction × storageAction` combination the hook supports. Minimum test cases:

1. Input that triggers the hook → verify `RejectHard` or the correct verdict.
2. Input that does not trigger → verify `Approve`.
3. `inflightAction=redact` config → verify the `HookResult.Transforms` span is populated.
4. Malformed config → verify `newFromConfig` returns an error.

```bash
go test -race -count=1 ./packages/shared/policy/hooks/<hook_name>/...
```

## Step 7 — Create the HookConfig row via seed or migration

Add a default HookConfig row to `tools/db-migrate/seed/seed.ts` if the hook should ship enabled by default. The canonical `onMatch` shape:

```json
{
  "enabled": false,
  "stage": "request",
  "applicableIngress": ["ALL"],
  "config": {
    "onMatch": {
      "inflightAction": "block-hard",
      "storageAction": "keep"
    }
  }
}
```

Start with `enabled: false`. The staged rollout (§ below) turns it on gradually.

## Step 8 — Staged rollout

A new hook should not ship blocking real traffic on day one. The `hook-rollout.md` flow prescribes:

1. Deploy with `applicableIngress: ["AI_GATEWAY"]` and `inflightAction: "block-soft"` (alert but do not block). Observe false-positive rate on the CP Traffic page.
2. Promote to `block-hard` after validating the match rate.
3. Extend `applicableIngress` to `["AI_GATEWAY", "COMPLIANCE_PROXY"]`, then `["ALL"]`.

The Hub's change-signal fans out only to Things whose path appears in `applicableIngress`, so an AI-Gateway-only hook does not reach the Compliance Proxy or Desktop Agent.

## Step 9 — Smoke test

```bash
# Issue a request that should trip the hook:
curl -H "Authorization: Bearer <VIRTUAL_KEY>" \
     http://localhost:3050/v1/chat/completions \
     -d '{"messages":[{"role":"user","content":"<trigger content>"}],"model":"<model>"}'

# Verify the traffic_event row records the correct verdict:
docker exec postgres psql -U postgres -d nexus_gateway \
  -c "SELECT request_hook_decision, request_hooks_pipeline \
      FROM traffic_event ORDER BY emitted_at DESC LIMIT 1;"
```

Confirm the hook ID appears in `request_hooks_pipeline` with the expected verdict.

---

## What links break if you skip this

- **Skipping metrics wiring**: the hook runs silently; operators cannot see its decision rate, latency, or false-positive ratio in Prometheus, making it impossible to safely promote from `block-soft` to `block-hard`.
- **Skipping the `SupportsEndpoint` / `SupportsModality` declaration**: the hook is invoked on embeddings and future multimodal endpoints where `NormalizedPayload` contains no inspectable text; `Execute` silently returns `Abstain` for every call, wasting CPU on no-op invocations.
- **Skipping staged rollout**: shipping `block-hard` on day one with a broad pattern has caused false-positive production incidents — the PII scanner saga (incorrect `inflightAction`) is the canonical example. Stage the rollout through `applicableIngress` scope and verdict escalation.
- **Skipping the onMatch validation check**: a misconfigured `inflightAction` (e.g., `"redact"` for a hook that doesn't implement `TransformSpans`) silently applies an invalid redaction, producing corrupted payloads.

---

## Canonical docs

- [`hook-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/hook-architecture.md) — Hook interface, onMatch schema, three stages, streaming modes, built-in hooks catalog
- [`hook-rollout.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/flows/hook-rollout.md) — Staged rollout flow: flag → block-soft → block-hard → expand applicableIngress
- [`audit-pipeline-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md) — How hook decisions land in traffic_event rows

**Adjacent wiki pages**: [AI Gateway Hooks](AI-Gateway-Hooks) · [Feature Hooks Framework](Feature-Hooks-Framework) · [Control Plane Audit Log](Control-Plane-Audit-Log) · [Recipe Index](Recipe-Index)
