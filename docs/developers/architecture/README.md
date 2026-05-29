# Architecture doc triggers

> **Binding for all contributors and agents.** Before editing code in any of the areas below, you MUST first read the listed architecture doc(s). This rule is one of three required reads (see CLAUDE.md "Pre-edit reading"): **architecture doc(s)** (this file) + **feature doc(s)** for any user-visible surface (in `docs/users/features/`) + **`docs/developers/workflow/conventions.md`** for code style. Together they keep architecture, user surface, and code style from drifting apart. Skipping any of the three requires **explicit user approval** in chat.

## Why this file exists separately

CLAUDE.md is the project's binding rules charter. The trigger table is a **living catalog** that grows row by row as module docs land. Keeping the table in CLAUDE.md would inflate that charter past usefulness; keeping it here lets the table grow freely and live next to the docs it indexes.

The CLAUDE.md "Pre-edit reading" section points at this file. The binding force is the same.

## How to use the table

1. Find the row whose "Editing area / file glob" matches what you are about to touch.
2. Open the listed doc(s) **before** writing code.
3. If your edit creates a new subsystem that has no row here yet, **adding the row is part of the PR** — same PR as the new architecture doc.

CI lockstep enforcement runs via `npm run check:arch-doc-triggers`: every `docs/developers/architecture/**/*-architecture.md` file (plus the two functional-name docs `cross-cutting/foundation/thing-model.md` and `cross-cutting/observability/admin-audit-log-coverage.md`) must have a row here, and every architecture doc referenced from a row must exist on disk.

Doc tree: the global `docs/developers/architecture/overview.md` is the system front door; each service then has a `<service>-architecture.md` overview that indexes its per-concern docs; cross-cutting concerns live under `cross-cutting/`.

If you are about to edit code in an area that is genuinely **not** covered by any row, that itself is a signal — either the architecture is undocumented (raise it with the user), or this is a new subsystem that needs its own doc + row.

## System

| Editing area / file glob | Read FIRST |
|---|---|
| Top-level system, service boundaries, data flow, deployment topology | `docs/developers/architecture/overview.md` |
| End-to-end flows across services (admin → CP → Hub → Thing → effect → audit) | `docs/developers/architecture/cross-cutting/foundation/service-call-framework.md` + `docs/developers/architecture/cross-cutting/foundation/multi-endpoint-coordination-architecture.md` |
| Adding/changing an admin API endpoint, sidebar nav, or route path | CLAUDE.md "API / menu / route changes require IAM impact review" rule + `docs/developers/architecture/services/control-plane/iam-identity-architecture.md` |
| Adding/changing a Prisma migration (any `tools/db-migrate/migrations/**`) | CLAUDE.md "Migration timestamp prefix must be unique" rule + `docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md` |

## Operator Toolkit (`packages/nexus-cli/`)

| Editing area / file glob | Read FIRST |
|---|---|
| The `nexus` TUI / CLI / MCP binary as a whole; `packages/nexus-cli/internal/{core,cli,tui,mcp}/**` — auth, profiles, keychain, typed client, command tree, Bubble Tea console, MCP server | `docs/developers/architecture/nexus-operator-toolkit-architecture.md` |

## AI Gateway (`packages/ai-gateway/`)

| Editing area / file glob | Read FIRST |
|---|---|
| AI Gateway as a whole; `packages/ai-gateway/internal/{ingress,platform,auth,execution}/**` request-entry + runtime internals without their own doc | `docs/developers/architecture/services/ai-gateway/ai-gateway-architecture.md` (service overview + index) |
| Caller-facing ingress API — endpoints, virtual-key auth, cross-format translation, response headers, error envelopes (`packages/ai-gateway/internal/auth/vkauth/**`, `packages/ai-gateway/internal/ingress/envelope/**`, route registration) | `docs/developers/architecture/services/ai-gateway/ingress-api.md` |
| `packages/ai-gateway/internal/routing/**`, routing-rule handlers, canonical payload, `ResolvedRequest`, model catalog, fallback chain | `docs/developers/architecture/services/ai-gateway/routing-architecture.md` |
| Smart routing (LLM-dispatch routing strategy) | `docs/developers/architecture/services/ai-gateway/smart-routing-architecture.md` |
| `packages/ai-gateway/internal/providers/**` (specs, core, dispatch), `packages/shared/traffic/adapters/**`, format translators, streaming, token-field stamping | `docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md` + `docs/developers/architecture/services/ai-gateway/provider-coverage.md` |
| `packages/shared/transport/normalize/**`, `packages/ai-gateway/internal/execution/canonicalbridge/**`, codec decode/parse, `NormalizedPayload` shape | `docs/developers/architecture/services/ai-gateway/normalization-architecture.md` |
| `packages/ai-gateway/internal/policy/hooks/**`, `packages/shared/policy/hooks/**`, HookConfig, `onMatch` logic | `docs/developers/architecture/services/ai-gateway/hook-architecture.md` |
| Prompt-cache code, Gemini cached content, multi-tier prompt cache config | `docs/developers/architecture/services/ai-gateway/prompt-cache-architecture.md` |
| Response cache config + service integration (`packages/ai-gateway/internal/cache/**`, `extract_cache_config` / `semantic_cache_config` fleet config, cache shadow-key dispatch); tier mechanism lives in `cache-multi-tier-architecture.md` | `docs/developers/architecture/services/ai-gateway/response-cache-architecture.md` |
| `packages/ai-gateway/internal/execution/estimator/**`, `metrics.CalculateCost`, price-per-million fields, cost stamping, cache savings UI | `docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md` |
| `packages/ai-gateway/internal/policy/aiguard/**` — judge-model classification pipeline | `docs/developers/architecture/services/ai-gateway/aiguard-architecture.md` |
| Forward-header allowlist (`packages/ai-gateway/internal/execution/forwardheader/**`) | `docs/developers/architecture/services/ai-gateway/forward-header-allowlist-architecture.md` |

## Compliance Proxy (`packages/compliance-proxy/`)

| Editing area / file glob | Read FIRST |
|---|---|
| Compliance Proxy as a whole; audit emission wiring; leftover internals without their own doc | `docs/developers/architecture/services/compliance-proxy/compliance-proxy-architecture.md` (service overview + index) |
| Normalize stage + adding a Tier-1 traffic adapter / Tier-2 detector (`packages/shared/traffic/adapters/{api,web,ide}/`, `packages/shared/transport/normalize/extract/detector.go`) | `docs/developers/architecture/services/compliance-proxy/compliance-pipeline-architecture.md` |
| `packages/compliance-proxy/internal/proxy/{server,connect,forward,conn}/**`, `packages/compliance-proxy/internal/access/**`, CONNECT handling, access control, forward gate sequence | `docs/developers/architecture/services/compliance-proxy/compliance-proxy-connect-forward-architecture.md` |
| `packages/compliance-proxy/internal/tls/{issuer,cache,kms}/**` — leaf cert issuance, cert cache, KMS unwrap (pinning fallback uses the shared `tlsbump.PinningTracker`) | `docs/developers/architecture/services/compliance-proxy/compliance-proxy-tls-cert-architecture.md` |
| `packages/compliance-proxy/internal/runtime/**`, `packages/compliance-proxy/internal/exemption/**`, `packages/compliance-proxy/internal/config/**` — runtime admin API, token auth, break-glass, local kill-switch state, exemptions, config load | `docs/developers/architecture/services/compliance-proxy/compliance-proxy-runtime-api-architecture.md` |
| `packages/shared/policy/domain/**`, `packages/shared/policy/device/**` matchers — domain matching + device predicate | `docs/developers/architecture/services/compliance-proxy/domain-device-predicate-architecture.md` |

## Control Plane (`packages/control-plane/`)

| Editing area / file glob | Read FIRST |
|---|---|
| Control Plane as a whole; `packages/control-plane/internal/{platform,fleet,traffic,infrastructure,settings,observability}/**`, admin HTTP stack, pgx, Hub client, config reconciliation, admin-audit writer | `docs/developers/architecture/services/control-plane/control-plane-architecture.md` (service overview + index) |
| `packages/control-plane/internal/ai/**` — provider/model/credential CRUD, virtual-key approval workflow, routing-rule / quota / prompt-cache admin | `docs/developers/architecture/services/control-plane/cp-ai-providers-virtualkeys-architecture.md` |
| `packages/control-plane/internal/governance/**` — kill-switch toggle, hook config, interception policy, AI-Guard snapshots, DSAR, exemptions, passthrough admin | `docs/developers/architecture/services/control-plane/cp-governance-compliance-admin-architecture.md` |
| `packages/control-plane/internal/identity/iam/**`, `packages/shared/identity/iam/**`, `iamMW(...)`, `allowedActions`, NRN building | `docs/developers/architecture/services/control-plane/iam-identity-architecture.md` |
| `packages/control-plane/internal/identity/{authserver/login,scim,sso,idptest}/**`, `identity/users/handler/identity_provider.go`, IdP + federated stores (`authserver/store/{idp_store,idp_oidc_config,idp_saml_config,saml_request_store,federated_store}.go`) — external IdP config + probe, SSO login (local / OIDC / SAML start + return legs), SCIM provisioning, agent SSO enroll, JIT user provisioning | `docs/developers/architecture/services/control-plane/idp-sso-architecture.md` |
| `packages/control-plane/internal/identity/jwt/**`, JWKS polling, admin access-token verification + revocation | `docs/developers/architecture/services/control-plane/jwt-verifier-architecture.md` |
| `packages/control-plane/internal/identity/authserver/**`, OAuth+PKCE flow, bearer issuance, refresh rotation | `docs/developers/architecture/services/control-plane/oauth-pkce-admin-auth-architecture.md` |
| Organisation + project hierarchy, parent/ancestor path, policy inheritance (single-tenant; orgs are intra-deployment scope, not tenant isolation) | `docs/developers/architecture/services/control-plane/organization-hierarchy-architecture.md` |
| `packages/ai-gateway/internal/platform/store/**` (vkSelectSQL), `packages/control-plane/internal/ai/virtualkeys/vkstore/**`, `traffic_event.org_id`/`org_name` population | `docs/developers/architecture/services/control-plane/vk-org-resolution.md` |

## Nexus Hub (`packages/nexus-hub/`)

| Editing area / file glob | Read FIRST |
|---|---|
| Nexus Hub as a whole; `packages/nexus-hub/internal/{fleet,traffic,storage,ws,self,handler,observability,compliance}/**` — Thing registry, route registration, self-registration, agent config aggregation | `docs/developers/architecture/services/hub/nexus-hub-architecture.md` (service overview + index) |
| `packages/nexus-hub/internal/identity/**` (agentca, enrollment, jwks) — agent enrollment authority: token lifecycle, device CA, enrollment-JWT validation, bootstrap | `docs/developers/architecture/services/hub/nexus-hub-enrollment-architecture.md` |

## Agent (`packages/agent/`)

| Editing area / file glob | Read FIRST |
|---|---|
| Agent as a whole; `packages/agent/internal/{lifecycle,sync}/**` daemon wiring without their own doc | `docs/developers/architecture/services/agent/agent-architecture.md` (service overview + index) |
| `packages/agent/internal/network/**` forwarder paths (intercept → req_hooks → upstream_ttfb → upstream_total → resp_hooks), HTTP/2 + QUIC handling | `docs/developers/architecture/services/agent/agent-forwarder-architecture.md` |
| `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/**`, `NETransparentProxyProvider`, `handleNewFlow`, fail-open paths | CLAUDE.md "macOS NE proxy must fail-open" rule + `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md` |
| `packages/agent/internal/platform/darwin/**`, `packages/agent/platform/darwin/**` — macOS platform: system-extension ↔ daemon IPC, NE network interception, interception-mode selection | `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md` |
| `packages/agent/internal/platform/windows/**`, `packages/agent/platform/windows/**` — WFP driver network interception, Service Control Manager, named-pipe IPC | `docs/developers/architecture/services/agent/agent-windows-platform-architecture.md` |
| `packages/agent/internal/platform/linux/**`, `packages/agent/platform/linux/**` — iptables REDIRECT + `SO_ORIGINAL_DST` network interception, systemd integration | `docs/developers/architecture/services/agent/agent-linux-platform-architecture.md` |
| macOS agent build / sign / notarize / package (`packages/agent/platform/darwin/`, `dist/macos/`) | `docs/developers/architecture/services/agent/macos-build-signing-architecture.md` + `.claude/skills/build-agent/` (binding) |
| `packages/agent/internal/platform/paths/**`, any new filesystem path key, hardcoded path candidates | `docs/developers/architecture/services/agent/agent-paths-abstraction-architecture.md` |
| `packages/agent/internal/identity/keystore/**`, `packages/agent/internal/identity/secretstore/**`, SQLCipher DB-key handling | `docs/developers/architecture/services/agent/agent-keystore-architecture.md` |
| `packages/agent/internal/identity/{enrollment,attestation,auth}/**`, `packages/agent/internal/host/openbrowser/**` — device enrollment, attestation header, SSO/PKCE, browser callback | `docs/developers/architecture/services/agent/agent-identity-enrollment-architecture.md` |
| `packages/agent/internal/policy/{core,policies,exemption}/**`, `packages/agent/internal/lifecycle/protectionpause/**` — policy engine, host exemptions, protection pause | `docs/developers/architecture/services/agent/agent-policy-eval-architecture.md` |
| `packages/agent/internal/observability/{audit,telemetry,backpressure}/**` — audit-upload queue (SQLCipher), OTel tracing, backpressure rollup | `docs/developers/architecture/services/agent/agent-observability-architecture.md` |
| `packages/agent/internal/host/updater/**`, release manifest polling, Ed25519 signature verification | `docs/developers/architecture/services/agent/agent-autoupdater-architecture.md` |
| `packages/agent/internal/host/trayipc/**`, daemon ↔ tray IPC protocol, socket / named-pipe wiring | `docs/developers/architecture/services/agent/agent-tray-ipc-architecture.md` |

## Cross-cutting — foundation

| Editing area / file glob | Read FIRST |
|---|---|
| Thing model rows, `packages/shared/transport/thingclient/**`, `packages/shared/schemas/configtypes/**`, shadow desired/reported, Cat A/B/C keys, `thing_config_template` | `docs/developers/architecture/cross-cutting/foundation/thing-model.md` + `docs/developers/architecture/cross-cutting/foundation/thing-config-sync-architecture.md` |
| Any change to yaml fields, env variables, `thing_config_template` keys, `system_metadata` keys, the 4-layer model, the override-merge mechanism, or a config rename | `docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md` |
| `packages/<service>/<service>.dev.yaml`, bootstrap-config loaders, env-var override paths | `docs/developers/architecture/cross-cutting/foundation/service-bootstrap-config-architecture.md` |
| Adding a new AI endpoint (embeddings, image-gen, TTS/STT, batch), extending `SchemaCodec`, the `Model` capability matrix, endpoint typology constants (`WireShape*`, `Endpoint*`) | `docs/developers/architecture/cross-cutting/foundation/endpoint-typology-architecture.md` |
| Multi-endpoint coordination, per-endpoint-kind routing fan-out | `docs/developers/architecture/cross-cutting/foundation/multi-endpoint-coordination-architecture.md` |
| `packages/shared/transport/mq/**`, NATS JetStream subjects / streams / consumers, MQ-vs-HTTP/WS decision | `docs/developers/architecture/cross-cutting/foundation/mq-architecture.md` |
| `packages/nexus-hub/internal/jobs/**`, cron jobs, retention purge, drift check | `docs/developers/architecture/cross-cutting/foundation/jobs-architecture.md` |
| Response metadata envelope (`nexus.*` markers: version, cost, model, cache status, routing) | `docs/developers/architecture/cross-cutting/foundation/nexus-response-markers.md` |

## Cross-cutting — observability

| Editing area / file glob | Read FIRST |
|---|---|
| Choosing among the observability surfaces (Audit / Diag / Metrics / Traces / SIEM), cross-surface `trace_id` correlation | `docs/developers/architecture/cross-cutting/observability/observability-architecture.md` (umbrella) |
| Audit event schema, `packages/shared/audit/**`, `packages/nexus-hub/internal/traffic/chain/**`, MQ audit sink, body storage with spillstore | `docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md` |
| Admin mutation audit — which handlers emit which action / entityType | `docs/developers/architecture/cross-cutting/observability/admin-audit-log-coverage.md` |
| Built-in Go alert rules, `AlertRule` rows, alerteval pipeline, channel fan-out | `docs/developers/architecture/cross-cutting/observability/alerting-architecture.md` |
| `packages/nexus-hub/internal/observability/siem/**`, `packages/control-plane/internal/observability/siem/**`, SIEM bridge poll/checkpoint, sink + wire formats, `siem.config` admin surface | `docs/developers/architecture/cross-cutting/observability/siem-bridge-architecture.md` |
| `packages/control-plane/internal/observability/opsmetrics/**`, `packages/nexus-hub/internal/observability/opsmetrics/**`, per-Thing stats rollup, quota rollup | `docs/developers/architecture/cross-cutting/observability/metrics-rollup-architecture.md` |
| Adding / renaming a Prometheus metric (any `*.go` calling `promauto.New*`) | `docs/developers/architecture/cross-cutting/observability/prometheus-naming-architecture.md` |
| `packages/shared/core/telemetry/**`, OTel setup, `traceparent` / `trace_id` propagation, span attribute conventions | `docs/developers/architecture/cross-cutting/observability/otel-tracing-architecture.md` |
| `packages/shared/core/diag/**`, `packages/agent/internal/observability/diagnostics/**`, diag-mode shadow keys, silence rules | `docs/developers/architecture/cross-cutting/observability/diag-event-triage-architecture.md` |
| `/debug/runtime` snapshot, `runtimeintrospect` sources, Hub introspection bridge, `/runtime/*` read API, snapshot redaction | `docs/developers/architecture/cross-cutting/observability/runtime-introspection-architecture.md` |
| `tests/lib/`, `tests/integration-go/`, new smoke / protocol / AI-judge test, new test skill | `docs/developers/architecture/cross-cutting/observability/test-harness-architecture.md` |

## Cross-cutting — safety

| Editing area / file glob | Read FIRST |
|---|---|
| Virtual keys, credentials, encryption, credential pool, provider/model health rollup | `docs/developers/architecture/cross-cutting/safety/credentials-architecture.md` |
| `packages/ai-gateway/internal/policy/quota/**`, `packages/nexus-hub/internal/quota/**`, sliding window, org/provider/model scoping, burst, overage | `docs/developers/architecture/cross-cutting/safety/quota-architecture.md` |
| `ProviderError`, `ErrorClass`, `executor.classify`, 429 response shape, retry / circuit breaker | `docs/developers/architecture/cross-cutting/safety/error-taxonomy-architecture.md` |
| Kill-switch admin handler, `kill_switch_activation` table, shadow propagation, activation UI | `docs/developers/architecture/cross-cutting/safety/kill-switch-architecture.md` |
| Emergency-passthrough code (`ResolvedRequest.BypassHooks`, kill-switch shadow read), the `passthrough.expiry` Hub job | `docs/developers/architecture/cross-cutting/safety/emergency-passthrough-architecture.md` |
| PII detection / redaction (`packages/shared/policy/hooks/validators/pii_detector.go`, `packages/shared/policy/pipeline/` redaction stage, `packages/shared/policy/hooks/webhook/webhook.go`) | `docs/developers/architecture/cross-cutting/safety/pii-redaction-policy-architecture.md` |
| SSE streaming compliance contract across agent / compliance-proxy / ai-gateway (the three streaming modes, `PreHookCallback`) | `docs/developers/architecture/cross-cutting/safety/sse-streaming-compliance-architecture.md` |

## Cross-cutting — shared

| Editing area / file glob | Read FIRST |
|---|---|
| Any `packages/shared/**` subpackage (the 8 buckets: audit/core/identity/policy/schemas/storage/traffic/transport), dep-tier decisions, new shared dependency, small utility libs | `docs/developers/architecture/cross-cutting/shared/shared-packages-architecture.md` |
| `packages/shared/transport/wirerewrite/**` — byte-level upstream wire rewriter, distinct from `normalize/` | `docs/developers/architecture/cross-cutting/shared/shared-wirerewrite-architecture.md` |

## Cross-cutting — storage

| Editing area / file glob | Read FIRST |
|---|---|
| Any cache code (`packages/shared/storage/cacheconfig/**`, in-process LRU, Redis cache, IAM cache, cert cache, desired-state cache, response cache, quota counters) | `docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md` |
| `packages/shared/storage/spillstore/**`, S3 driver, presigned URL handlers, body overflow paths | `docs/developers/architecture/cross-cutting/storage/spillstore-architecture.md` |
| `tools/db-migrate/schema.prisma`, manual scripts under `tools/db-migrate/manual-scripts/`, hand-maintained Go mirrors under `packages/shared/schemas/configtypes/**` | `docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md` |
| Retention config, per-table purge jobs, `dsar_request` rows, right-to-erasure flow | `docs/developers/architecture/cross-cutting/storage/data-retention-purge-architecture.md` |

## Cross-cutting — UI (Control Plane UI + Agent Dashboard)

| Editing area / file glob | Read FIRST |
|---|---|
| Design tokens / theme packs / Recharts palette (`*.module.css`, `packages/ui-shared/src/styles/**`, `public/themes/*.json`, `chartColors.ts`) | `docs/developers/architecture/cross-cutting/ui/ui-theming-architecture.md` |
| i18n keys (`t('namespace:section.key')`), locale files (`packages/*/src/i18n/locales/**`), `packages/ui-shared/src/i18n/**` | `docs/developers/architecture/cross-cutting/ui/ui-i18n-architecture.md` |
| `useApi` / `useApiMutation` hooks + queryKey shape, `shellRouteConfig.tsx` / `Sidebar.tsx` IA, `packages/ui-shared/**` cross-bundle components | `docs/developers/architecture/cross-cutting/ui/ui-shell-architecture.md` |

## Adding a new arch doc

When you ship a new `docs/developers/architecture/**/*-architecture.md`:

1. Append a row above with `*(planned)*` while drafting.
2. Remove the `*(planned)*` marker on merge.
3. The same PR must update the row to point at the real doc path.
4. No `docs/developers/architecture/**/*-architecture.md` should ship without a row in this table.

The two functional-name docs that are not `*-architecture.md` — `cross-cutting/foundation/thing-model.md` and `cross-cutting/observability/admin-audit-log-coverage.md` — are also required to appear in the table; the lockstep check treats them the same as suffixed docs.

## References

- `scripts/check-arch-doc-triggers.mjs` — lockstep check that enforces this table against the architecture doc tree
- `docs/developers/architecture/` — the architecture doc tree this table indexes
- `docs/README.md` — full documentation inventory
