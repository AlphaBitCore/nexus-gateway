# Coverage Baseline — All Modules Combined

**Date:** 2026-05-19
**Branch:** feature/unit-test (nexus-gateway-unit-test repo)
**Command:** `go test -race -count=1 -cover ./...` per module from each module root
**Modules measured:** agent, ai-gateway, compliance-proxy, control-plane, nexus-hub, shared

---

## Overall Stats

| Metric | Count |
|---|---|
| Total packages (all modules) | 394 |
| Packages ≥ 95% (passing) | 209 |
| Packages < 95% non-allowlisted (actionable gap) | 50 |
| Packages with no test files (0% or [no test files]) | 89 |
| Packages with FAIL | 3 |
| Packages allowlisted | 52 |

> Notes:
> - "No test files" includes packages reported with `[no test files]`, packages with `ok` at 0.0% due to wiring/types-only, and OS-excluded packages.
> - "Allowlisted" count is derived from `.coverage-allowlist` entries matching observed packages (some glob entries cover multiple packages).
> - Some allowlisted packages also have 0% coverage — counted under allowlisted only.

---

## Module: packages/agent (48 packages)

| Package | Coverage % | Status | Allowlisted? | Structural Category |
|---|---|---|---|---|
| `cmd/agent` | 2.0% | ok | yes (A — cmd entry point) | A |
| `cmd/agent/platformshim` | 0.0% | no tests | no | A |
| `cmd/agent/wiring` | 0.0% | no tests | no | A |
| `internal/compliance` | 96.0% | ok | no | A |
| `internal/host/openbrowser` | 0.0% | no tests | yes (D — OS shell-out) | A |
| `internal/host/trayipc` | 0.0% | no tests | yes (D — OS IPC) | A |
| `internal/host/updater` | 98.3% | ok | no | A |
| `internal/identity/auth` | 96.6% | ok | no | C |
| `internal/identity/clockoffset` | 89.5% | ok | yes (backlog) | C |
| `internal/identity/enrollment` | 96.1% | ok | no | C |
| `internal/identity/keystore` | 0.0% | no tests | yes (D — OS keychain) | C |
| `internal/identity/secretstore` | 98.4% | ok | no | C |
| `internal/lifecycle/bootstrap` | 97.1% | ok | no | C |
| `internal/lifecycle/killswitch` | 100.0% | ok | no | C |
| `internal/lifecycle/protectionpause` | 100.0% | ok | no | C |
| `internal/lifecycle/state` | 97.6% | ok | no | C |
| `internal/network/bridge` | 96.8% | ok | no | A |
| `internal/network/intercept` | 95.2% | ok | no | A |
| `internal/network/proxy` | 95.0% | ok | no | A |
| `internal/network/relay` | 95.9% | ok | no | A |
| `internal/network/tls` | 95.6% | ok | no | A |
| `internal/observability/audit/backfill` | 91.7% | ok | yes (backlog) | A |
| `internal/observability/audit/classify` | 100.0% | ok | no | A |
| `internal/observability/audit/event` | 100.0% | ok | no | A |
| `internal/observability/audit/hub` | 95.2% | ok | no | A |
| `internal/observability/audit/queue` | 95.6% | ok | no | A |
| `internal/observability/backpressure` | 97.5% | ok | no | A |
| `internal/observability/diag` | 97.8% | ok | no | A |
| `internal/observability/diagnostics` | 96.3% | ok | no | A |
| `internal/observability/localrollup` | 95.1% | ok | no | A |
| `internal/observability/spilluploader` | 98.0% | ok | no | A |
| `internal/observability/telemetry` | 100.0% | ok | no | A |
| `internal/platform` | 0.0% | no tests | yes (D — OS factory) | A |
| `internal/platform/api` | — | no test files | yes (D — OS-bound) | A |
| `internal/platform/catrust` | 0.0% | no tests | no | D (OS-bound) |
| `internal/platform/darwin` | 0.0% | no tests | no | D (OS-bound) |
| `internal/platform/darwin/bundles` | 0.0% | no tests | no | D (OS-bound) |
| `internal/platform/darwin/flow` | — | no test files | yes (D) | D (OS-bound) |
| `internal/platform/darwin/ne` | 0.0% | no tests | no | D (OS-bound) |
| `internal/platform/darwin/proc` | 0.0% | no tests | no | D (OS-bound) |
| `internal/platform/linux` | N/A | OS-excluded | no | D (OS-bound) |
| `internal/platform/paths` | 0.0% | no tests | no | D (OS-bound) |
| `internal/platform/windows` | N/A | OS-excluded | no | D (OS-bound) |
| `internal/policy/core` | 96.2% | ok | no | C |
| `internal/policy/exemption` | 99.1% | ok | no | C |
| `internal/policy/policies` | 100.0% | ok | no | C |
| `internal/sync/hub` | 95.5% | ok | no | A |
| `internal/sync/schema` | 97.8% | ok | no | A |
| `internal/sync/shadow` | 97.3% | ok | no | A |
| `internal/sync/status` | 99.1% | ok | no | A |

---

## Module: packages/ai-gateway (75 packages)

| Package | Coverage % | Status | Allowlisted? | Structural Category |
|---|---|---|---|---|
| `cmd/ai-gateway` | 0.0% | ok | yes (A — cmd entry point) | A |
| `cmd/ai-gateway/configdispatch` | 22.7% | ok | no | A |
| `cmd/ai-gateway/wiring` | 0.0% | no test files | no | A |
| `internal/auth/vkauth` | 100.0% | ok | no | C |
| `internal/cache/core` | 95.6% | ok | no | A |
| `internal/cache/gemini` | 95.3% | ok | no | A |
| `internal/cache/layer` | 100.0% | ok | no | A |
| `internal/cache/stream` | 99.5% | ok | no | A |
| `internal/config` | 100.0% | ok | no | A |
| `internal/credentials/decrypt` | 96.4% | ok | no | A |
| `internal/credentials/manager` | 97.1% | ok | no | A |
| `internal/credentials/pool` | 97.5% | ok | no | A |
| `internal/credentials/stats` | 97.0% | ok | no | A |
| `internal/execution/canonicalbridge` | 99.0% | ok | no | A |
| `internal/execution/estimator` | 0.0% | no test files | no | A |
| `internal/execution/executor` | 98.1% | ok | no | A |
| `internal/execution/forwardheader` | 98.8% | ok | no | A |
| `internal/execution/passthrough` | 96.8% | ok | no | A |
| `internal/execution/wireformat` | 100.0% | ok | no | A |
| `internal/ingress/debug` | 74.1% | ok | no | E |
| `internal/ingress/envelope` | 60.2% | ok | no | E |
| `internal/ingress/models` | 22.6% | ok | no | E |
| `internal/ingress/proxy` | 94.3% | ok | no | E |
| `internal/ingress/proxy/classify` | 85.2% | ok | no | E |
| `internal/platform/audit` | 98.4% | ok | no | C |
| `internal/platform/metrics` | 100.0% | ok | no | C |
| `internal/platform/middleware` | 100.0% | ok | no | C |
| `internal/platform/store` | 97.1% | ok | no | C |
| `internal/platform/streaming` | 98.3% | ok | no | C |
| `internal/policy/aiguard` | 97.7% | ok | no | C |
| `internal/policy/hooks` | [no statements] | ok | no | C |
| `internal/policy/quota` | 99.7% | FAIL | no | C |
| `internal/policy/ratelimit` | 97.6% | ok | no | C |
| `internal/policy/requestcontext` | 100.0% | ok | no | C |
| `internal/providers/builtins` | 100.0% | ok | no | A |
| `internal/providers/canonicalext` | 95.8% | ok | no | A |
| `internal/providers/core` | 80.6% | ok | no | A |
| `internal/providers/dispatch` | 98.4% | ok | no | A |
| `internal/providers/specs/anthropic` | 98.0% | ok | no | A |
| `internal/providers/specs/anthropic/codec` | 78.4% | ok | no | A |
| `internal/providers/specs/anthropic/errors` | 0.0% | no test files | no | A |
| `internal/providers/specs/anthropic/ingress` | 0.0% | no test files | no | A |
| `internal/providers/specs/anthropic/stream` | 0.0% | no test files | no | A |
| `internal/providers/specs/azure` | 100.0% | ok | no | A |
| `internal/providers/specs/bedrock` | 96.2% | ok | no | A |
| `internal/providers/specs/cohere` | 98.1% | ok | no | A |
| `internal/providers/specs/compat/deepseek` | 100.0% | ok | no | A |
| `internal/providers/specs/compat/fireworks` | 100.0% | ok | no | A |
| `internal/providers/specs/compat/groq` | 100.0% | ok | no | A |
| `internal/providers/specs/compat/huggingface` | 100.0% | ok | no | A |
| `internal/providers/specs/compat/mistral` | 100.0% | ok | no | A |
| `internal/providers/specs/compat/moonshot` | 100.0% | ok | no | A |
| `internal/providers/specs/compat/perplexity` | 100.0% | ok | no | A |
| `internal/providers/specs/compat/together` | 100.0% | ok | no | A |
| `internal/providers/specs/compat/xai` | 100.0% | ok | no | A |
| `internal/providers/specs/gemini` | 98.1% | ok | no | A |
| `internal/providers/specs/gemini/codec` | 0.0% | no test files | no | A |
| `internal/providers/specs/gemini/errors` | 0.0% | no test files | no | A |
| `internal/providers/specs/gemini/ingress` | 0.0% | no test files | no | A |
| `internal/providers/specs/gemini/stream` | 0.0% | no test files | no | A |
| `internal/providers/specs/glm` | 100.0% | ok | no | A |
| `internal/providers/specs/minimax` | 100.0% | ok | no | A |
| `internal/providers/specs/openai` | 98.1% | ok | no | A |
| `internal/providers/specs/openai/codec` | 0.0% | no test files | no | A |
| `internal/providers/specs/openai/errors` | 0.0% | no test files | no | A |
| `internal/providers/specs/openai/responses` | 0.0% | no test files | no | A |
| `internal/providers/specs/openai/rewrites` | 0.0% | no test files | no | A |
| `internal/providers/specs/openai/stream` | 0.0% | no test files | no | A |
| `internal/providers/specs/replicate` | 98.8% | ok | no | A |
| `internal/providers/specs/vertex` | 99.0% | ok | no | A |
| `internal/providers/specutil` | 99.0% | ok | no | A |
| `internal/providers/target` | 100.0% | ok | no | A |
| `internal/routing` | 98.4% | ok | no | E |
| `internal/routing/core` | 29.6% | ok | no | E |
| `internal/routing/llm` | 100.0% | ok | no | E |
| `internal/routing/matcher` | 93.1% | ok | no | E |
| `internal/routing/strategies` | 94.7% | ok | no | E |
| `internal/runtimeapi` | 100.0% | ok | no | A |

---

## Module: packages/compliance-proxy (31 packages)

| Package | Coverage % | Status | Allowlisted? | Structural Category |
|---|---|---|---|---|
| `cmd/compliance-proxy` | 1.7% | ok | yes (A — cmd entry point) | A |
| `cmd/compliance-proxy/breakglass` | 0.0% | ok | no | A |
| `cmd/compliance-proxy/config` | 95.5% | ok | no | A |
| `cmd/compliance-proxy/configdispatch` | 20.4% | ok | no | A |
| `cmd/compliance-proxy/replay` | 16.0% | ok | no | A |
| `cmd/compliance-proxy/wiring` | 0.0% | ok | no | A |
| `internal/access` | 99.3% | ok | no | A |
| `internal/audit` | 95.2% | ok | no | A |
| `internal/compliance` | [no statements] | ok | no | A |
| `internal/config/cache` | 97.8% | ok | no | A |
| `internal/config/loaders` | 100.0% | ok | no | A |
| `internal/config/shadow` | — | no test files | no | A |
| `internal/exemption` | 99.1% | ok | no | A |
| `internal/health` | 97.5% | ok | no | A |
| `internal/metrics` | 100.0% | ok | no | A |
| `internal/proxy/conn` | 100.0% | ok | no | A |
| `internal/proxy/connect` | 0.0% | ok | no | A |
| `internal/proxy/forward` | 0.0% | ok | no | A |
| `internal/proxy/server` | 94.5% | FAIL | no | A |
| `internal/runtime/auth` | 100.0% | ok | no | C |
| `internal/runtime/breakglass` | 95.0% | ok | no | C |
| `internal/runtime/config` | 100.0% | ok | no | C |
| `internal/runtime/handler` | 38.2% | ok | no | C |
| `internal/runtime/killswitch` | 100.0% | ok | no | C |
| `internal/runtime/server` | 94.4% | ok | no | C |
| `internal/siem` | 97.0% | ok | no | A |
| `internal/testutil` | 0.0% | ok | yes (B — test helper) | A |
| `internal/tls/cache` | 95.9% | ok | no | C |
| `internal/tls/issuer` | 92.2% | ok | no | C |
| `internal/tls/kms` | 91.2% | ok | no | C |
| `internal/tls/pinning` | — | no test files | no | C |

---

## Module: packages/control-plane (81 packages)

| Package | Coverage % | Status | Allowlisted? | Structural Category |
|---|---|---|---|---|
| `cmd/control-plane` | 1.6% | ok | yes (A — cmd entry point) | A |
| `cmd/control-plane/config` | 100.0% | ok | no | A |
| `cmd/control-plane/configdispatch` | 25.0% | ok | no | A |
| `cmd/control-plane/wiring` | 0.0% | no test files | no | A |
| `internal/ai/cache/cachestore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/ai/cache/handler` | 65.9% | ok | no | C |
| `internal/ai/providers/credstore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/ai/providers/handler` | 87.2% | ok | no | C |
| `internal/ai/providers/modelstore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/ai/providers/providerstore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/ai/quota/handler` | 0.0% | no test files | no | C |
| `internal/ai/quota/quotastore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/ai/routing/handler` | 99.1% | ok | no | C |
| `internal/ai/routing/routingstore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/ai/simulator/handler` | 0.0% | no test files | no | C |
| `internal/ai/virtualkeys/handler` | 97.8% | ok | no | C |
| `internal/ai/virtualkeys/vkstore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/fleet/handler/agent` | 92.1% | ok | no | C |
| `internal/fleet/store/agentauditstore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/fleet/store/agentstore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/fleet/store/fleetstore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/governance/aiguard/handler` | 98.1% | ok | no | C |
| `internal/governance/dsar/dsarstore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/governance/dsar/handler` | 0.0% | no test files | no | C |
| `internal/governance/exemptions/handler` | 98.8% | ok | no | C |
| `internal/governance/hooks/handler` | 72.8% | ok | no | C |
| `internal/governance/hooks/hookstore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/governance/interception/handler` | 0.0% | no test files | no | C |
| `internal/governance/interception/interceptionstore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/governance/killswitch/handler` | 97.5% | ok | no | C |
| `internal/governance/passthrough/handler` | 99.6% | ok | no | C |
| `internal/governance/rulepacks/handler` | 0.0% | no test files | no | C |
| `internal/handler` | 25.9% | ok | yes (allowlist-backlog) | E |
| `internal/identity/authn` | 100.0% | ok | no | E |
| `internal/identity/authserver` | 100.0% | ok | no | E |
| `internal/identity/authserver/idp` | 100.0% | ok | no | E |
| `internal/identity/authserver/login` | 99.5% | ok | no | E |
| `internal/identity/authserver/oauth` | 100.0% | ok | no | E |
| `internal/identity/authserver/revocation` | 98.6% | ok | no | E |
| `internal/identity/authserver/store` | 98.4% | ok | no | E |
| `internal/identity/authserver/store/storetest` | 0.0% | no test files | yes (B — test helper) | E |
| `internal/identity/authserver/token` | 97.8% | ok | no | E |
| `internal/identity/iam` | 97.3% | ok | no | E |
| `internal/identity/idptest` | 0.0% | no test files | yes (B — test helper) | E |
| `internal/identity/jwt` | 98.5% | ok | no | E |
| `internal/identity/scim/handler` | 0.0% | no test files | no | E |
| `internal/identity/scim/scimstore` | 0.0% | no test files | no | E (DB-bound) |
| `internal/identity/sessions/handler` | 0.0% | no test files | no | E |
| `internal/identity/sso/handler` | 0.0% | no test files | no | E |
| `internal/identity/users/apikeystore` | 0.0% | no test files | no | E (DB-bound) |
| `internal/identity/users/governancestore` | 0.0% | no test files | no | E (DB-bound) |
| `internal/identity/users/handler` | 0.0% | no test files | no | E |
| `internal/identity/users/iamstore` | 0.0% | no test files | no | E (DB-bound) |
| `internal/identity/users/orgstore` | 0.0% | no test files | no | E (DB-bound) |
| `internal/identity/users/userstore` | 0.0% | no test files | no | E (DB-bound) |
| `internal/infrastructure/infra` | 90.6% | ok | no | C |
| `internal/infrastructure/store/federatedstore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/observability/alerts/handler` | 95.5% | ok | no | C |
| `internal/observability/diag/diagstore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/observability/opsmetrics/handler` | 0.0% | no test files | no | C |
| `internal/observability/opsmetrics/opsstore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/observability/retention/handler` | 0.0% | no test files | no | C |
| `internal/observability/siem/handler` | 0.0% | no test files | no | C |
| `internal/observability/thingstats/handler` | 0.0% | no test files | no | C |
| `internal/observability/thingstats/thingstore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/platform/audit` | 100.0% | ok | no | C |
| `internal/platform/configreconcile` | 96.2% | ok | no | C |
| `internal/platform/crypto` | 97.1% | ok | no | C |
| `internal/platform/hub` | 96.9% | ok | no | C |
| `internal/platform/metrics` | 100.0% | ok | no | C |
| `internal/platform/middleware` | 100.0% | ok | no | C |
| `internal/platform/pgx` | 0.0% | no test files | no | C (DB-bound) |
| `internal/settings/handler/settings` | 60.6% | ok | no | C |
| `internal/settings/store/metricsstore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/store` | 0.0% | ok (tests skip) | no | C (DB-bound) |
| `internal/store/systemmetastore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/traffic/analytics/analyticsstore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/traffic/analytics/handler` | 94.7% | FAIL | no | C |
| `internal/traffic/handler/traffic` | 6.8% | ok | no | C |
| `internal/traffic/store/compliancestore` | 0.0% | no test files | no | C (DB-bound) |
| `internal/traffic/store/trafficstore` | 0.0% | no test files | no | C (DB-bound) |

---

## Module: packages/nexus-hub (52 packages)

| Package | Coverage % | Status | Allowlisted? | Structural Category |
|---|---|---|---|---|
| `cmd/nexus-hub` | 0.0% | no test | yes (A — cmd entry point) | A |
| `cmd/nexus-hub/wiring` | 0.0% | no test | no | A |
| `internal/alerts/client` | 100.0% | ok | no | A |
| `internal/alerts/client/spool` | 100.0% | ok | no | A |
| `internal/alerts/engine` | 99.0% | ok | no | A |
| `internal/alerts/engine/rules` | 100.0% | ok | no | A |
| `internal/alerts/engine/senders` | 100.0% | ok | no | A |
| `internal/alerts/eval` | 98.7% | ok | no | A |
| `internal/alerts/eval/aggregators` | 99.8% | ok | no | A |
| `internal/compliance/catbagent` | 88.4% | ok | yes (backlog) | C |
| `internal/config` | 100.0% | ok | no | A |
| `internal/fleet/handler/hubapi` | 0.0% | no test | no | C |
| `internal/fleet/manager` | 95.7% | ok | no | C |
| `internal/fleet/overrides` | 34.3% | ok | no | C |
| `internal/fleet/shadow` | 16.6% | ok | no | C |
| `internal/fleet/smartgroup` | 0.0% | no test | no | C |
| `internal/fleet/store` | 97.0% | ok | no | C |
| `internal/handler` | 0.0% | no test | no | E |
| `internal/identity/agentca` | 95.2% | ok | no | A |
| `internal/identity/enrollment` | 100.0% | ok | no | A |
| `internal/identity/handler/bootstrap` | 0.0% | no test | no | A |
| `internal/identity/handler/enroll` | 0.0% | no test | no | A |
| `internal/identity/store/authstore` | 31.8% | ok | no | A |
| `internal/identity/store/enrollstore` | 0.0% | no test | no | A (DB-bound) |
| `internal/identity/store/userstore` | 0.0% | no test | no | A (DB-bound) |
| `internal/jobs/consumer` | 99.6% | ok | no | A |
| `internal/jobs/defs` | — | no test files | yes (B — types-only) | A |
| `internal/jobs/defs/audit` | 91.7% | ok | yes (backlog) | A |
| `internal/jobs/defs/drift` | 39.3% | ok | yes (backlog) | A |
| `internal/jobs/defs/expiry` | 94.6% | ok | yes (backlog) | A |
| `internal/jobs/defs/health` | 79.9% | ok | yes (backlog) | A |
| `internal/jobs/defs/metrics` | 43.1% | ok | yes (backlog) | A |
| `internal/jobs/defs/quota` | 57.2% | ok | yes (backlog) | A |
| `internal/jobs/defs/retention` | 74.9% | ok | yes (backlog) | A |
| `internal/jobs/defs/rollup` | 45.2% | ok | yes (backlog) | A |
| `internal/jobs/scheduler` | 95.9% | ok | no | A |
| `internal/jobs/store` | 98.9% | ok | no | A |
| `internal/jwks` | 98.6% | ok | no | A |
| `internal/observability/handler/diag` | 0.0% | no test | no | A |
| `internal/observability/opsmetrics` | 98.3% | ok | no | A |
| `internal/quota/rollup` | 100.0% | ok | no | C |
| `internal/quota/store` | 100.0% | ok | no | C |
| `internal/self/reg` | 93.0% | ok | yes (backlog) | A |
| `internal/self/shadow` | 96.4% | ok | no | A |
| `internal/storage/hubstore` | — | no test files | yes (B — interface-only) | A |
| `internal/storage/store` | 51.9% | ok | yes (backlog) | A |
| `internal/traffic/chain` | 95.7% | ok | no | C |
| `internal/traffic/ingest/audit` | 0.0% | no test | no | C |
| `internal/traffic/ingest/spill` | 0.0% | no test | no | C |
| `internal/traffic/siem` | 99.4% | ok | no | C |
| `internal/traffic/store` | 0.0% | no test | no | C (DB-bound) |
| `internal/ws` | 97.4% | ok | no | A |

---

## Module: packages/shared (107 packages)

| Package | Coverage % | Status | Allowlisted? | Structural Category |
|---|---|---|---|---|
| `audit` | 96.5% | ok | no | A |
| `core/bootenv` | 96.8% | ok | no | A (was runtime/bootenv) |
| `core/diag` | 95.2% | ok | no | A (was runtime/diag) |
| `core/diag/runtimeintrospect` | 97.8% | ok | no | A |
| `core/logging` | 98.7% | ok | no | A (was runtime/logging) |
| `core/metrics/instruments` | 97.9% | ok | no | A |
| `core/metrics/platform` | 94.4% | ok | yes (backlog) | A |
| `core/metrics/registry` | 99.3% | ok | no | A |
| `core/telemetry` | 95.2% | ok | no | A |
| `identity/iam` | 100.0% | ok | no | E (was security/iam) |
| `identity/pkce` | 100.0% | ok | no | A (was security/pkce) |
| `identity/rstokenauth` | 100.0% | ok | no | A (was security/rstokenauth) |
| `policy/decision` | — | no test files | yes (C — new) | C |
| `policy/device` | 100.0% | ok | no | B (was devicepredicate) |
| `policy/domain` | 95.7% | ok | no | A |
| `policy/hooks/access` | 100.0% | ok | no | A |
| `policy/hooks/builtins` | 100.0% | ok | no | A |
| `policy/hooks/contract` | 95.2% | ok | no | A |
| `policy/hooks/core` | 95.5% | ok | no | A |
| `policy/hooks/ratelimit` | 100.0% | ok | no | A |
| `policy/hooks/validators` | 99.0% | ok | no | A |
| `policy/hooks/webhook` | 98.4% | ok | no | A |
| `policy/payloadcapture` | 97.6% | ok | no | A |
| `policy/pipeline` | 99.2% | ok | no | C |
| `policy/rulepack` | 95.3% | ok | no | A |
| `schemas/configkey` | 100.0% | ok | no | C |
| `schemas/configtypes` | — | no test files | yes (A) | A |
| `schemas/configtypes/enums` | — | no test files | yes (A) | A |
| `schemas/configtypes/identity` | [no statements] | ok | no | A |
| `schemas/configtypes/interception` | [no statements] | ok | no | A |
| `schemas/configtypes/observability` | — | no test files | yes (A) | A |
| `schemas/configtypes/policy` | 100.0% | ok | no | A |
| `schemas/credstate` | 100.0% | ok | no | A |
| `schemas/domain` | 100.0% | ok | no | A |
| `schemas/thingtype` | 100.0% | ok | no | A |
| `storage/cacheconfig` | 100.0% | ok | no | A |
| `storage/configcache` | 97.3% | ok | no | A |
| `storage/configstore` | 100.0% | ok | no | A |
| `storage/redisfactory` | 99.3% | ok | no | C |
| `storage/spillstore` | 100.0% | ok | no | A |
| `storage/spillstore/localfs` | 95.9% | ok | no | A |
| `storage/spillstore/s3` | 1.4% | ok | yes (E — real AWS S3) | A |
| `storage/spillstore/spillfactory` | 96.0% | ok | no | A |
| `storage/spillupload` | 97.0% | ok | no | A |
| `traffic` | 99.2% | ok | no | A |
| `traffic/adapters` | 98.5% | ok | no | A |
| `traffic/adapters/api/anthropic` | 97.0% | ok | no | A |
| `traffic/adapters/api/azure` | 100.0% | ok | no | A |
| `traffic/adapters/api/bedrock` | 100.0% | ok | no | A |
| `traffic/adapters/api/cohere` | 100.0% | ok | no | A |
| `traffic/adapters/api/deepseek` | 100.0% | ok | no | A |
| `traffic/adapters/api/fireworks` | 100.0% | ok | no | A |
| `traffic/adapters/api/gemini` | 97.9% | ok | no | A |
| `traffic/adapters/api/glm` | 100.0% | ok | no | A |
| `traffic/adapters/api/groq` | 100.0% | ok | no | A |
| `traffic/adapters/api/huggingface` | 100.0% | ok | no | A |
| `traffic/adapters/api/minimax` | 97.5% | ok | no | A |
| `traffic/adapters/api/mistral` | 100.0% | ok | no | A |
| `traffic/adapters/api/moonshot` | 100.0% | ok | no | A |
| `traffic/adapters/api/openai` | 97.5% | ok | no | A |
| `traffic/adapters/api/perplexity` | 100.0% | ok | no | A |
| `traffic/adapters/api/replicate` | 100.0% | ok | no | A |
| `traffic/adapters/api/together` | 100.0% | ok | no | A |
| `traffic/adapters/api/vertex` | 100.0% | ok | no | A |
| `traffic/adapters/api/xai` | 100.0% | ok | no | A |
| `traffic/adapters/generic/generic` | 100.0% | ok | no | A |
| `traffic/adapters/ide/codeium` | 100.0% | ok | no | A |
| `traffic/adapters/ide/continuedev` | 100.0% | ok | no | A |
| `traffic/adapters/ide/cursor` | 98.6% | ok | no | A |
| `traffic/adapters/ide/githubcopilot` | 100.0% | ok | no | A |
| `traffic/adapters/ide/replitai` | 100.0% | ok | no | A |
| `traffic/adapters/ide/tabnine` | 100.0% | ok | no | A |
| `traffic/adapters/web/anthropicconsoleweb` | 100.0% | ok | no | A |
| `traffic/adapters/web/boltweb` | 100.0% | ok | no | A |
| `traffic/adapters/web/characterweb` | 100.0% | ok | no | A |
| `traffic/adapters/web/chatglmweb` | 100.0% | ok | no | A |
| `traffic/adapters/web/chatgptweb` | 97.9% | ok | no | A |
| `traffic/adapters/web/claudeweb` | 96.9% | ok | no | A |
| `traffic/adapters/web/copilotmsweb` | 100.0% | ok | no | A |
| `traffic/adapters/web/deepseekweb` | 100.0% | ok | no | A |
| `traffic/adapters/web/devinweb` | 100.0% | ok | no | A |
| `traffic/adapters/web/geminiweb` | 100.0% | ok | no | A |
| `traffic/adapters/web/githubcopilotweb` | 100.0% | ok | no | A |
| `traffic/adapters/web/googleaistudioweb` | 100.0% | ok | no | A |
| `traffic/adapters/web/grokweb` | 100.0% | ok | no | A |
| `traffic/adapters/web/huggingchatweb` | 100.0% | ok | no | A |
| `traffic/adapters/web/kimiweb` | 100.0% | ok | no | A |
| `traffic/adapters/web/m365copilotweb` | 100.0% | ok | no | A |
| `traffic/adapters/web/mistralweb` | 100.0% | ok | no | A |
| `traffic/adapters/web/openaiplatformweb` | 100.0% | ok | no | A |
| `traffic/adapters/web/perplexityweb` | 100.0% | ok | no | A |
| `traffic/adapters/web/poeweb` | 100.0% | ok | no | A |
| `traffic/adapters/web/v0web` | 100.0% | ok | no | A |
| `traffic/adapters/web/youweb` | 100.0% | ok | no | A |
| `transport/bufconn` | 0.0% | ok | yes (B — test helper / infra) | A |
| `transport/configloader` | 100.0% | ok | no | A |
| `transport/http` | 97.1% | ok | no | B (was httpclient) |
| `transport/mq` | 14.7% | ok | yes (E — real NATS) | A |
| `transport/normalize` | [no statements] | ok | no | A |
| `transport/normalize/codecs` | 94.6% | ok | no | A |
| `transport/normalize/core` | 95.9% | ok | no | A |
| `transport/normalize/extract` | 97.1% | ok | no | A |
| `transport/responseio` | 100.0% | ok | no | A |
| `transport/streaming` | 98.2% | ok | no | A |
| `transport/streaming/extract` | 100.0% | ok | no | A |
| `transport/streaming/policy` | 97.4% | ok | no | A |
| `transport/thingclient` | 96.5% | ok | no | A |
| `transport/tlsbump` | 15.4% | ok | yes (D/E — OS+network) | A |
| `transport/wirerewrite` | 96.8% | ok | no | A |

---

## FAIL Packages

| Package | Module | Coverage at FAIL | First-Line Error |
|---|---|---|---|
| `internal/proxy/server` | compliance-proxy | 94.5% | `listener_coverage_test.go:964: ServeHTTP did not return` (15s timeout; 7 of 8 failing tests are timeout/deadlock in ServeHTTP; 1 is status=403 want 407) |
| `internal/traffic/analytics/handler` | control-plane | 94.7% | `cache_roi_test.go:342: status = 500, want 200; body={"error":{"code":"","message":"query failed","type":"server_error"}}` (preceded by `WARN analytics: dimension label lookup failed err="conn lost"`) |
| `internal/policy/quota` | ai-gateway | 99.7% | `quota_db_test.go:794: weekly TTL too large: 306h19m36.630168s` (time-sensitive TTL boundary bug — test-only, not a production issue) |
