---
doc: shared-utility-subpackages-architecture
area: cross-cutting
service: shared
tier: 1
updated: 2026-05-20
---

# `packages/shared/` — Small Utility Subpackages

> **Tier 3 architecture doc.** Reference card for the small subpackages in `packages/shared/` that don't warrant their own dedicated architecture doc. Each is a single-paragraph description plus consumer list.

All paths below use the **8-bucket layout** (see `shared-package-architecture.md` §2): `audit/, core/, identity/, policy/, schemas/, storage/, traffic/, transport/`. Subpackages live under one of those buckets — there are no top-level subpackages outside the 8.

This doc does **not** cover the larger Tier-1 / Tier-2 subpackages (`transport/thingclient`, `policy/hooks`, `traffic/`, `transport/mq`, `audit/`, `identity/iam`, `storage/spillstore`, `transport/wirerewrite`, etc.) — those have their own arch docs reachable from `architecture-doc-triggers.md`.

## `transport/bufconn/`

In-memory `net.Conn` implementation. A `bufconn.Listener` accepts connections backed by an in-memory pipe, used to drive proxy / bridge code in tests and in the agent's intercept-to-relay handoff without binding a real socket. Single Go file. No external dependencies.

**Consumers:** `packages/agent/internal/network/bridge/`, `packages/agent/internal/network/proxy/`, `packages/compliance-proxy/internal/proxy/`.

## `policy/payloadcapture/`

Body-capture primitives used during traffic interception. Wraps a request/response body so the full bytes can be peeled off for audit storage or spillstore upload, while still streaming the upstream/downstream copy. Config-driven size caps; respects the `agent_settings.trafficUploadLevel` enum.

**Consumers:** `packages/ai-gateway/internal/ingress/proxy/`, `packages/ai-gateway/internal/platform/audit/`, `packages/agent/internal/network/intercept/`, `packages/agent/internal/network/proxy/`, `packages/agent/cmd/agent/main.go` + `wire_bridge_{darwin,other}.go`.

## `transport/streaming/`

SSE parsing, incremental JSON streaming, ring-buffer + back-pressure helpers (subdirs `extract/`, `policy/`). Used in two places: (a) the AI Gateway's SSE response pipeline; (b) the agent's intercept-to-upstream forwarder's chunked-body handler.

**Consumers:** `packages/agent/internal/network/intercept/`, `packages/agent/internal/network/proxy/`, `packages/agent/internal/platform/`, `packages/ai-gateway/...`, `packages/shared/traffic/adapter.go`, `packages/shared/traffic/adapters/web/`.

## `storage/cacheconfig/`

Go types for the E38 prompt-cache **config blob** that lives as a Cat B shadow key. Provider tier configs, rule overrides, dry-run flags. No business logic — pure type definitions consumed by the AI Gateway's `internal/cache/` and by admin endpoints.

**Consumers:** `packages/ai-gateway/internal/cache/`, `packages/ai-gateway/cmd/ai-gateway/wiring/` (boot + reload), `packages/control-plane/internal/ai/cache/handler/cache_preview.go`.

## `storage/configstore/`

DB-row → config-object loaders **interface** used by per-service `configloader` packages. The package is intentionally narrow: types and small adapter functions, no concrete pgx code. Each service's `internal/config/` consumes the interface and provides its own concrete loader.

**Consumers:** `packages/ai-gateway/internal/config/`, `packages/compliance-proxy/internal/config/`. The shared-level `transport/configloader/` provides the loader plumbing both services build on.

## `core/metrics/`

Per-service Prometheus metric registration helpers — counter/histogram bucket conventions, namespace string handling, the `WithLabel`/`WithBuckets` builders. Subdirs: `instruments/`, `platform/`, `registry/`.

**Consumers:** Every server service that calls `promauto.New*`. (Note: there is no shared `opsmetrics/` — per-Thing operational-state rollup lives service-locally under each service's `internal/observability/opsmetrics/`.)

## `storage/spillupload/`

Agent-side spill uploader. Consumes Hub-issued presigned S3 URLs (the agent never gets raw AWS credentials), retries on transient failures, integrates with `backpressure/` to ring-buffer when the network is down.

**Consumers:** `packages/agent/internal/observability/spilluploader/` (the agent-side glue that drives it).

## `schemas/thingtype/`

A small constants-only package holding the canonical `ThingType` enum values: `agent`, `ai-gateway`, `compliance-proxy`, `control-plane`, `nexus-hub`. Imported wherever code matches on Thing type without depending on the larger Thing model machinery in `transport/thingclient/`.

**Consumers:** Every service that branches on Thing type.

## `policy/decision/` + `policy/domain/`

Per-domain policy / decision primitives. `policy/domain/` is the matcher (glob / regex over a DomainTrie); `policy/decision/` carries the allow / inspect / passthrough decision types and ordering helpers consumed by the agent and compliance proxy. Distinct from the hook pipeline in `policy/hooks/`, which applies to traffic that has already been admitted.

**Consumers:** `packages/agent/internal/compliance/`, `packages/agent/internal/network/proxy/`, `packages/agent/internal/network/tls/`, `packages/compliance-proxy/cmd/`, `packages/compliance-proxy/internal/proxy/`, `packages/shared/transport/tlsbump/`.

## `policy/rulepack/`

Server-side rule-pack types: keyword / PII rule bundles distributed via Hub shadow to AI Gateway. The Hub-stored rule packs are evaluated by the AI Gateway hook pipeline. **Note:** the agent does not currently consume rule packs (it executes hooks locally without rule-pack input — see memory `project_agent_compliance_audit_2026_05_14`). This package's server-side use is real and stable; the agent-side wiring is a known gap.

**Consumers:** `packages/nexus-hub/internal/...`, `packages/ai-gateway/internal/ingress/proxy/`, `packages/ai-gateway/internal/platform/audit/`, `packages/control-plane/internal/governance/rulepacks/handler/`.

## Sources

- `packages/shared/<subpackage>/` for each.
- `docs/developers/architecture/cross-cutting/shared/shared-package-architecture.md` §2 catalogue — the umbrella catalogue this card supplements.

## When you change one of these

- If a subpackage grows beyond ~5 .go files and gains its own architecture surface, promote it: add a row to `architecture-doc-triggers.md` pointing at a new `docs/developers/architecture/shared/shared-<name>-architecture.md` Tier-2 or Tier-3 doc, and remove the row from this card.
- If a subpackage is shipped to the Agent binary, §5 of `shared-package-architecture.md` (additive-only API stability) applies.
