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
            // The engine, not the specs it embeds. Everything under
            // capabilities/resource/openapi/ is a mirror of
            // docs/users/api/openapi/control-plane/, kept in lockstep by
            // catalog_test.go — a value added to one of those contracts is a
            // change to the CONTRACT, already carried by its own entry, and
            // says nothing about the catalog/search/distill/cards engine this
            // doc describes. Triggering on it asked for an update to a doc
            // with nothing to update, which is how a lockstep gate teaches
            // people to waive it.
            'packages/nexus-agent-core/capabilities/resource/*.go',
            'packages/nexus-agent-core/capabilities/runtime/tools_resource.go',
        ],
        docs: [
            'docs/developers/architecture/nexus-operator-toolkit-architecture.md',
        ],
        waiverHint: 'The resource engine (catalog/search/distill/cards) and the resource_* agent tools are documented in nexus-operator-toolkit-architecture.md — update its operation-model / tools sections in the same PR.',
    },
    {
        // Routing had NO entry, which is how a branch that changed the strategy
        // set, the matching semantics, the plan's shape and the refusal
        // behaviour reached review with four architecture docs describing the
        // system it replaced. CI could not see any of it.
        //
        name: 'ai-gateway-routing',
        // Directory anchors rather than file-name patterns: the checker requires
        // a glob's pre-wildcard prefix to exist, which is what stops a glob from
        // going stale when a file is renamed — and these files have been split
        // three times for the size ratchet already. The three routing
        // directories named here hold nothing BUT routing behaviour; the
        // executor's two files are listed literally because the rest of that
        // package is dispatch mechanics the routing docs do not describe.
        code: [
            'packages/ai-gateway/internal/routing/*.go',
            'packages/ai-gateway/internal/routing/core/*.go',
            'packages/ai-gateway/internal/routing/matcher/*.go',
            'packages/ai-gateway/internal/routing/strategies/*.go',
            'packages/ai-gateway/internal/execution/executor/classify.go',
            'packages/ai-gateway/internal/execution/executor/select_next.go',
            // The recovery engine's knobs. Their EFFECT is stated in
            // smart-routing-architecture.md (the call budget bounds what one
            // auto-routed request may spend) and in the routing-rules OpenAPI.
            'packages/shared/schemas/configtypes/policy/retry_policy.go',
        ],
        docs: [
            'docs/developers/architecture/services/ai-gateway/routing-architecture.md',
            'docs/developers/architecture/services/ai-gateway/smart-routing-architecture.md',
            'docs/developers/architecture/services/ai-gateway/recovery-engine-architecture.md',
        ],
        waiverHint: 'Routing behaviour is stated in routing-architecture.md (rule selection, the plan, the passthrough precondition, the trace) and smart-routing-architecture.md (the decision pipeline, the re-selection pool, context-upgrade arming). A change to which rule wins, what the plan holds, when a request is refused, or what the trace records must update the matching doc in the same PR.',
    },
    {
        // The configuration KEYS themselves, whose 4-layer model and R1-R5
        // invariants configuration-architecture.md states. Adding, renaming or
        // removing one without touching the per-key catalogue there leaves the
        // key undocumented at exactly the moment an operator needs to find it.
        name: 'config-key-schemas',
        // configkey is the registry itself; interception holds the killswitch
        // shape the §7 table names. configtypes/policy is deliberately NOT here
        // — retry and quota policies ride inside other payloads rather than
        // being config keys, and the routing entry above carries the retry one.
        code: [
            'packages/shared/schemas/configkey/**',
            'packages/shared/schemas/configtypes/interception/**',
        ],
        docs: [
            'docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md',
        ],
        waiverHint: 'A new / renamed / removed config key must update the §7 per-key catalogue in configuration-architecture.md in the same PR, alongside the constants and ValidByThingType registration.',
    },
    {
        name: 'vendor-bill-reconciliation',
        code: [
            'packages/nexus-hub/internal/vendorbill/**',
            'packages/nexus-hub/internal/jobs/defs/vendorbill/**',
            'packages/control-plane/internal/traffic/analytics/handler/vendor_bill_reconciliation.go',
        ],
        docs: [
            'docs/operators/ops/runbooks/vendor-bill-reconciliation.md',
            'docs/operators/ops/runbooks/alerts.md',
            'docs/users/features/cp-ui/overview.md',
        ],
        waiverHint: 'Vendor-bill reconciliation is operator-facing setup (admin key TYPE per vendor, scope pinning, coverage semantics). Changes to the sources, the job, or the report endpoint must update the vendor-bill-reconciliation runbook, and the alerts runbook if drift-alert behaviour changes.',
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
            // The two files under internal/local the docs actually describe —
            // and only those. The doc names local.Config, Config.Resolve and
            // Config.Save by behaviour, and cites internal/local/secretstore.go
            // for the keychain seam; the glob stopped at cli/ and tui/, so a
            // rewrite of Save's write strategy left the doc describing an
            // O_TRUNC the code no longer performs.
            //
            // Deliberately NOT internal/local/** — that also binds h2health,
            // httplog, logging, retry, validate and paths, which neither doc
            // mentions. A PR touching only retry.go would have to edit an
            // architecture doc that says nothing about retries, or take a
            // waiver, and a gate that asks for impossible edits is a gate
            // people learn to waive.
            'packages/nexus-cli/internal/local/config.go',
            'packages/nexus-cli/internal/local/secretstore.go',
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
        // The client-facing API surface. This doc is what an integrator reads,
        // and it is the only place the ingress dialects are described together
        // with runnable requests — a route added or removed without it leaves
        // integrators reading a surface that no longer matches.
        name: 'gateway-client-api',
        code: [
            'packages/ai-gateway/cmd/ai-gateway/wiring/routes.go',
        ],
        docs: [
            'docs/users/api/gateway-api.md',
        ],
        waiverHint: 'Only needed when a CLIENT-facing route is added, removed or renamed. Internal /internal/* and ops routes can waive with NEXUS_DOC_LOCKSTEP_WAIVE=1.',
    },
    {
        name: 'provider-adapter',
        code: [
            'packages/ai-gateway/internal/providers/specs/**',
            // §3a Rule 7's machine checks — the doc describes their behaviour,
            // so changing the lints must revisit the doc. The registry
            // (quirk-coverage.config.mjs) is data and deliberately excluded.
            'scripts/check-quirk-coverage.mjs',
            'scripts/check-quirk-evidence.mjs',
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
            'packages/shared/transport/normalize/locator/**',
        ],
        docs: [
            'docs/developers/architecture/services/ai-gateway/normalization-architecture.md',
        ],
    },
    {
        name: 'peer-url-resolution',
        code: [
            'packages/shared/transport/peerurl/**',
            'packages/shared/core/metrics/platform/staticinfo.go',
        ],
        docs: [
            'docs/developers/architecture/cross-cutting/foundation/service-call-framework.md',
            'docs/developers/architecture/cross-cutting/foundation/thing-model.md',
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
            // The AI Gateway's own error writers. §4 catalogs each one with the
            // audit code it stamps, and that table is the only place the
            // error_code vocabulary is written down — a writer added or renamed
            // without it leaves an error class nothing can be grouped by.
            'packages/ai-gateway/internal/ingress/proxy/proxy_errors.go',
            'packages/ai-gateway/internal/ingress/proxy/cross_format.go',
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
        // Registered after a spillstore change (the failed-spill inline fallback
        // being bounded) shipped with its architecture doc still describing the
        // old, unbounded behaviour. The store had no lockstep entry, so nothing
        // caught it. The Control Plane read path is included because it owns the
        // integrity gate and the read-failure diagnosis the doc documents.
        name: 'spillstore',
        code: [
            'packages/shared/storage/spillstore/**',
            'packages/control-plane/internal/traffic/handler/traffic/traffic_spill.go',
            'packages/control-plane/internal/traffic/handler/traffic/spill_diag.go',
        ],
        docs: [
            'docs/developers/architecture/cross-cutting/storage/spillstore-architecture.md',
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
            // Substrate-agnostic Model A engine + the per-host wire redactor, the
            // scope-routing predicate, and both substrate adapters (canonical + wire)
            // that drive the engine — three-end hooks/compliance parity.
            'packages/shared/transport/streaming/modela/**',
            'packages/shared/transport/streaming/frame_redactor.go',
            'packages/shared/transport/streaming/frame_redactor_splice.go',
            'packages/shared/transport/tlsbump/sse_modela.go',
            'packages/shared/transport/tlsbump/sse_frame_redactor.go',
            'packages/shared/policy/pipeline/enforcement.go',
            'packages/ai-gateway/internal/ingress/proxy/proxy_cache_modela.go',
            'packages/ai-gateway/internal/ingress/proxy/proxy_cache_modela_substrate.go',
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
    {
        name: 'nexus-headers',
        code: [
            // Both direction registries + the chain/CORS helpers.
            'packages/shared/traffic/markers.go',
            // Marker injection shared by CP + Agent.
            'packages/shared/transport/tlsbump/markerhook.go',
            'packages/shared/transport/tlsbump/markercontext.go',
            // Request-header read sites the §8 roster documents.
            'packages/ai-gateway/internal/auth/vkauth/**',
            'packages/ai-gateway/internal/platform/middleware/middleware.go',
        ],
        docs: [
            'docs/developers/architecture/cross-cutting/foundation/nexus-headers.md',
        ],
        waiverHint: 'nexus-headers.md is the registry of every Nexus-owned HTTP header, both directions. Adding/renaming/retiring a marker or request header, changing a VK carrier, or changing the CORS composition must update its catalogue (§2 response / §8 request) in the same PR.',
    },
    {
        name: 'container-images-and-release',
        code: [
            'docker/**',
            'deploy/**',
            'scripts/release/**',
            '.github/workflows/release.yml',
            '.github/workflows/buildbase.yml',
        ],
        docs: [
            'docs/developers/architecture/cross-cutting/deployment/container-image-architecture.md',
            'docs/operators/ops/container-deployment.md',
        ],
        waiverHint: 'Image layout, the Vectorscan baseline, the tag contract, and the quickstart compose are documented in container-image-architecture.md (design) and container-deployment.md (operations). Changing the build, the compose topology, or the release pipeline must update at least one of them in the same PR.',
    },
];
