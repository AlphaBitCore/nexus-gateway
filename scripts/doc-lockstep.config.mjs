// scripts/doc-lockstep.config.mjs
//
// Maps code globs → required docs. When a PR touches any code matched by an
// entry's `code` globs, the PR diff must ALSO include at least one of the
// entry's `docs` files (a doc update). Enforced by scripts/check-doc-lockstep.mjs.
//
// Patterns are minimatch-style. Use `**` for recursive.
//
// Each entry can declare multiple `docs` — the checker accepts the diff as
// long as AT LEAST ONE of them is touched. This matches the reality that a
// single code change frequently has one canonical doc but may also need
// runbook / feature-doc / OpenAPI updates depending on the nature of the
// change. List all of them and let the engineer pick which apply.
//
// Add new entries here when you ship a new architecture / feature doc.

/** @type {Array<{ name: string, code: string[], docs: string[], waiverHint?: string }>} */
export default [
    {
        name: 'resource-catalog-engine',
        code: [
            'packages/nexus-agent-core/capabilities/resource/**',
            'packages/nexus-agent-core/capabilities/runtime/tools_resource.go',
        ],
        docs: [
            'docs/developers/architecture/nexus-operator-toolkit-architecture.md',
        ],
        waiverHint: 'The resource engine (catalog/search/distill/cards) and the resource_* agent tools are documented in nexus-operator-toolkit-architecture.md — update its operation-model / tools sections in the same PR.',
    },
    {
        name: 'web-assistant',
        code: [
            'packages/control-plane/internal/assistant/**',
        ],
        docs: [
            'docs/developers/architecture/nexus-operator-toolkit-architecture.md',
            'docs/users/features/cp-ui/web-assistant.md',
            'docs/operators/ops/runbooks/web-assistant.md',
            'docs/users/api/openapi/control-plane/assistant.yaml',
        ],
        waiverHint: 'Changes under internal/assistant/** ("Chat with Nexus") must update the toolkit architecture doc (web-face section), the web-assistant feature doc / runbook, and/or the assistant OpenAPI spec.',
    },
    {
        name: 'operator-toolkit',
        code: [
            'packages/nexus-cli/internal/cli/**',
            'packages/nexus-cli/internal/tui/**',
            'packages/nexus-agent-core/agent/**',
        ],
        docs: [
            'docs/developers/architecture/nexus-operator-toolkit-architecture.md',
            'docs/users/features/operator-toolkit.md',
        ],
        waiverHint: 'The nexus CLI/TUI surfaces and the agent kernel are documented in nexus-operator-toolkit-architecture.md + the operator-toolkit feature doc — update them in the same PR.',
    },
    {
        name: 'agent-linux-platform',
        code: [
            'packages/agent/internal/platform/linux/**',
            'packages/agent/internal/sync/status/status_health.go',
            'packages/shared/transport/tlsbump/egress_proxy.go',
        ],
        docs: [
            'docs/developers/architecture/services/agent/agent-linux-platform-architecture.md',
        ],
        waiverHint: 'The Linux agent platform doc owns the NEXUS_AGENT iptables chain, the reconciler + interception-health verdict, /proc PID attribution, SO_MARK loop avoidance, and the egress-proxy (upstreamProxy) upstream-forwarding path — update it in the same PR when changing any of these.',
    },
    {
        name: 'cost-estimation',
        code: [
            'packages/ai-gateway/internal/ingress/proxy/proxy.go',
            'packages/ai-gateway/internal/ingress/proxy/proxy_cache.go',
            'packages/ai-gateway/internal/ingress/proxy/proxy_responses.go',
            'packages/ai-gateway/internal/ingress/proxy/stage_accounting.go',
            'packages/ai-gateway/internal/ingress/proxy/stream_accounting.go',
            'packages/ai-gateway/internal/cache/layer/pricing.go',
            'packages/ai-gateway/internal/execution/estimator/**',
            'packages/shared/transport/normalize/codecs/anthropic_messages.go',
            'packages/shared/transport/normalize/codecs/openai_chat.go',
            'packages/shared/transport/normalize/codecs/openai_responses.go',
            'packages/shared/transport/normalize/codecs/gemini_generate.go',
        ],
        docs: [
            'docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md',
            'docs/operators/ops/runbooks/prod-deploy-data-changes.md', // if historical recompute is part of the PR
        ],
        waiverHint: 'Cost stamp / pricing path changes require cost-estimation-architecture.md update; if historical data is recomputed in the same PR, also touch the prod-deploy runbook.',
    },
    {
        name: 'provider-adapter',
        code: [
            'packages/ai-gateway/internal/providers/specs/**',
        ],
        docs: [
            'docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md',
        ],
        waiverHint: '§3a Rules 1-8 govern every adapter codec. Update the doc when canonical↔wire translation changes.',
    },
    {
        name: 'normalize-codecs',
        code: [
            'packages/shared/transport/normalize/codecs/**',
            'packages/shared/transport/normalize/extract/**',
            'packages/shared/transport/normalize/core/**',
        ],
        docs: [
            'docs/developers/architecture/services/ai-gateway/normalization-architecture.md',
        ],
    },
    {
        name: 'thing-config-sync',
        code: [
            'packages/nexus-hub/internal/fleet/**',
            'packages/shared/transport/thingclient/**',
            'packages/ai-gateway/cmd/ai-gateway/configdispatch/**',
            'packages/compliance-proxy/cmd/compliance-proxy/configdispatch/**',
            'packages/agent/cmd/agent/configdispatch.go',
        ],
        docs: [
            'docs/developers/architecture/cross-cutting/foundation/thing-config-sync-architecture.md',
            'docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md',
        ],
    },
    {
        name: 'iam-identity',
        code: [
            'packages/control-plane/internal/identity/iam/**',
            'packages/shared/identity/iam/**',
        ],
        docs: [
            'docs/developers/architecture/services/control-plane/iam-identity-architecture.md',
        ],
    },
    {
        name: 'macos-ne-fail-open',
        code: [
            'packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/**',
        ],
        docs: [
            'docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md',
        ],
        waiverHint: 'NE proxy is in the host packet path — any change must explain fail-open invariants in the doc.',
    },
    {
        name: 'jobs-rollup',
        // The rollup + merge tiers live together under defs/rollup/ (rollup_5m.go,
        // rollup_merge.go, thing_rollup_merge.go, rollup_correction.go, …); there
        // is no separate defs/merge/ directory.
        code: [
            'packages/nexus-hub/internal/jobs/defs/rollup/**',
        ],
        docs: [
            'docs/developers/architecture/cross-cutting/observability/metrics-rollup-architecture.md',
            'docs/developers/architecture/cross-cutting/foundation/jobs-architecture.md',
        ],
    },
    {
        // Every job definition (audit, drift, expiry, health, metrics, quota,
        // retention, rollup, semanticcacheflush) is catalogued in jobs-architecture.md;
        // editing any job's logic must keep that catalogue current. defs/rollup/**
        // additionally trips the jobs-rollup entry above for the rollup doc.
        name: 'jobs-defs-catalogue',
        code: [
            'packages/nexus-hub/internal/jobs/defs/**',
        ],
        docs: [
            'docs/developers/architecture/cross-cutting/foundation/jobs-architecture.md',
        ],
    },
    {
        // Hub + compliance-proxy flat `{"error":"…"}` envelope emitters. The
        // error-taxonomy doc §9 catalogs every live error shape; editing one of
        // these emitters (e.g. changing the field set) must keep §9 accurate
        // (F-0321). Scoped to the specific emitter files, not the whole handler
        // trees, to avoid false lockstep failures on unrelated handler edits.
        name: 'error-envelope-service',
        code: [
            'packages/shared/transport/httperr/**',
            'packages/nexus-hub/internal/alerts/engine/handlers_admin.go',
            'packages/nexus-hub/internal/alerts/engine/handlers_internal.go',
            'packages/nexus-hub/internal/identity/handler/bootstrap/agent_bootstrap.go',
            'packages/nexus-hub/internal/observability/handler/diag/runtime_bridge.go',
            'packages/compliance-proxy/internal/runtime/auth/auth.go',
            'packages/compliance-proxy/internal/runtime/breakglass/break_glass.go',
            'packages/compliance-proxy/internal/runtime/config/runtime_config.go',
            'packages/compliance-proxy/internal/runtime/handler/handler.go',
            'packages/compliance-proxy/internal/runtime/server/server.go',
        ],
        docs: [
            'docs/developers/architecture/cross-cutting/safety/error-taxonomy-architecture.md',
        ],
        waiverHint: 'Only needed when the error envelope SHAPE changes (fields added/removed). A no-op behavioural edit to these files can waive with NEXUS_DOC_LOCKSTEP_WAIVE=1.',
    },
    {
        name: 'audit-traffic-event',
        code: [
            'packages/ai-gateway/internal/platform/audit/**',
            'packages/nexus-hub/internal/observability/consumer/**',
            'packages/shared/audit/ndjson/**',
        ],
        docs: [
            'docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md',
            'docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md',
            'docs/developers/architecture/cross-cutting/observability/observability-architecture.md',
            'docs/developers/architecture/cross-cutting/observability/admin-audit-log-coverage.md',
        ],
    },
    {
        name: 'cache-multi-tier',
        code: [
            'packages/ai-gateway/internal/cache/core/**',
            'packages/ai-gateway/internal/cache/semantic/**',
            'packages/ai-gateway/internal/cache/freshness/**',
            'packages/ai-gateway/internal/cache/stream/**',
        ],
        docs: [
            'docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md',
            'docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md',
        ],
    },
    {
        name: 'admin-api-openapi',
        code: [
            'packages/control-plane/internal/ai/**/handler/**',
            'packages/control-plane/internal/handler/**',
        ],
        docs: [
            // any matching openapi yaml under docs/users/api/openapi/
            'docs/users/api/openapi/**',
        ],
        waiverHint: 'Admin endpoint changes require matching OpenAPI 3.1 spec update + IAM impact review (separate rule).',
    },
    {
        name: 'cp-ui-feature',
        code: [
            'packages/control-plane-ui/src/pages/**',
        ],
        docs: [
            // any matching feature doc under docs/users/features/cp-ui/
            'docs/users/features/cp-ui/**',
        ],
        waiverHint: 'User-visible UI changes require the matching feature doc in docs/users/features/cp-ui/.',
    },
    {
        name: 'agent-ui-feature',
        code: [
            'packages/agent/ui/frontend/src/**',
        ],
        docs: [
            'docs/users/features/agent-ui/**',
        ],
    },
    {
        name: 'hook-pipeline',
        code: [
            'packages/shared/policy/pipeline/policy.go',
            'packages/shared/policy/pipeline/pipeline.go',
        ],
        docs: [
            'docs/developers/architecture/services/ai-gateway/hook-architecture.md',
        ],
        waiverHint: 'PolicyResolver / BuildPipeline / pipeline execution semantics (resolve filters, build-time strictFailClosed fail-closed enforcement, per-hook failBehavior on Execute) are documented in hook-architecture.md §4-§5 — update it when the resolve/build/execute contract changes.',
    },
    {
        name: 'sse-streaming-compliance',
        code: [
            // Shared streaming pipeline + policy (#115 unification)
            'packages/shared/transport/normalize/responseprehook/**',
            'packages/shared/transport/streaming/buffer.go',
            'packages/shared/transport/streaming/live.go',
            'packages/shared/transport/streaming/passthrough.go',
            'packages/shared/transport/streaming/locked_buffer.go',
            'packages/shared/transport/streaming/metrics.go',
            'packages/shared/transport/streaming/policy/**',
            'packages/shared/transport/tlsbump/sse.go',
            'packages/shared/transport/tlsbump/bump.go',
            // ai-gateway streaming format + ingress dispatch (R1/R3 fixes)
            'packages/ai-gateway/internal/platform/streaming/live.go',
            'packages/ai-gateway/internal/platform/streaming/format/**',
            'packages/ai-gateway/internal/ingress/proxy/sse_prehook.go',
            'packages/ai-gateway/internal/ingress/proxy/proxy_cache_buffer.go',
            'packages/ai-gateway/internal/ingress/proxy/proxy_cache_live.go',
            'packages/ai-gateway/internal/ingress/proxy/proxy_cache_passthrough.go',
            'packages/ai-gateway/internal/ingress/proxy/proxy_cache_dispatch.go',
            // Hub shadow → data plane streaming policy plumbing
            'packages/ai-gateway/cmd/ai-gateway/configdispatch/configdispatch.go',
            'packages/ai-gateway/cmd/ai-gateway/wiring/hooks.go',
            // CP admin surface + UI (warnings + tooltip — admin-visible contract)
            'packages/control-plane/internal/settings/handler/settings/streaming_compliance.go',
            'packages/control-plane-ui/src/pages/compliance/streaming-compliance/SettingsStreamingComplianceTab.tsx',
            'packages/shared/policy/hooks/core/types.go',
        ],
        docs: [
            'docs/developers/architecture/cross-cutting/safety/sse-streaming-compliance-architecture.md',
        ],
        waiverHint: 'The SSE streaming compliance pipeline runs in 3 services (agent / compliance-proxy via tlsbump, ai-gateway via its own streaming pkg) and they MUST stay in lockstep — see the doc for the PreHookCallback contract + asymmetry table + Modify-on-buffer degradation signal (#115/R3). configdispatch + wiring/hooks.go govern how the Hub-pushed streaming_compliance.config shadow becomes the data-plane streampolicy.Store snapshot; CP settings + UI own the admin-visible warning surface.',
    },
    {
        name: 'e2e-coverage-matrix',
        code: [
            // New user-facing API: every OpenAPI yaml under any service tree.
            'docs/users/api/openapi/**',
            // New / changed user-facing capability docs.
            'docs/users/features/**',
        ],
        docs: [
            'docs/developers/specs/e2e-coverage-matrix.md',
        ],
        waiverHint: 'New / changed user-facing capability must update the E2E coverage matrix in the same PR (capability ↔ test arm map). Endpoint-level scenario coverage lives in tests/scenarios/00-catalog.md; this matrix sits above it at the user-perspective layer.',
    },
];
