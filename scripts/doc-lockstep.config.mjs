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
        name: 'web-assistant',
        code: [
            'packages/control-plane/internal/assistant/**',
        ],
        docs: [
            'docs/developers/specs/e90-s4-navigation.md',
            'docs/developers/specs/e90-s5-write-confirm.md',
            'docs/developers/specs/e90-s6-persistence.md',
            'docs/developers/specs/e90-s7-builtin-skills.md',
            'docs/developers/specs/e90-s8-hardening.md',
            'docs/operators/ops/runbooks/e90-web-assistant.md',
        ],
        waiverHint: 'Changes under internal/assistant/** ("Chat with Nexus") must update the matching e90 story doc (s4 navigation / s5 write+confirm / s6 persistence / s7 skills+files / s8 hardening) and/or the web-assistant runbook.',
    },
    {
        name: 'cost-estimation',
        code: [
            'packages/ai-gateway/internal/ingress/proxy/proxy.go',
            'packages/ai-gateway/internal/ingress/proxy/proxy_cache.go',
            'packages/ai-gateway/internal/observability/metrics/cost.go',
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
            'packages/nexus-hub/internal/things/**',
            'packages/shared/transport/thingclient/**',
            'packages/ai-gateway/cmd/ai-gateway/configdispatch/**',
            'packages/compliance-proxy/cmd/compliance-proxy/configdispatch/**',
            'packages/agent/cmd/agent/configdispatch/**',
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
        code: [
            'packages/nexus-hub/internal/jobs/defs/rollup/**',
            'packages/nexus-hub/internal/jobs/defs/merge/**',
        ],
        docs: [
            'docs/developers/architecture/cross-cutting/observability/metrics-rollup-architecture.md',
            'docs/developers/architecture/cross-cutting/foundation/jobs-architecture.md',
        ],
    },
    {
        name: 'audit-traffic-event',
        code: [
            'packages/ai-gateway/internal/platform/audit/**',
            'packages/nexus-hub/internal/jobs/consumer/**',
        ],
        docs: [
            'docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md',
            'docs/developers/architecture/cross-cutting/observability/observability-architecture.md',
        ],
    },
    {
        name: 'cache-multi-tier',
        code: [
            'packages/ai-gateway/internal/cache/core/**',
            'packages/ai-gateway/internal/cache/semantic/**',
            'packages/ai-gateway/internal/cache/freshness/**',
            'packages/ai-gateway/internal/cache/budget/**',
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
            // Roadmap edits that flip an epic to shipped or retire one.
            'docs/developers/roadmap.md',
        ],
        docs: [
            'docs/developers/specs/e2e-coverage-matrix.md',
        ],
        waiverHint: 'New / changed user-facing capability must update the E2E coverage matrix in the same PR (capability ↔ test arm map). Endpoint-level scenario coverage lives in tests/scenarios/00-catalog.md; this matrix sits above it at the user-perspective layer.',
    },
];
