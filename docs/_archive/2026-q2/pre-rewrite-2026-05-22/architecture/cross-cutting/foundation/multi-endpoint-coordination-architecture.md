---
doc: multi-endpoint-coordination-architecture
area: cross-cutting
service: foundation
tier: 1
updated: 2026-05-20
---

# Multi-Endpoint Coordination Architecture

> **Tier 1 architecture doc.** Read this when reasoning about a flow that spans more than one service: admin actions that propagate through Hub to data-plane Things; traffic events that flow back through MQ to UI; kill switches that fan out and reconcile. Individual sub-mechanisms are documented in their own Tier-1 docs; this doc is the **connective tissue**, the "golden flows" companion.

`service-call-framework.md` covers the CP ↔ Hub HTTP API contract specifically. This doc covers the **end-to-end seams** across all five Things.

---

## 1. Flow 1 — Admin creates a Virtual Key

The Control Plane owns the VK lifecycle. The Hub is **not** in the create/update path — its only role is to fan the resulting config change out to the AI Gateway via the shadow / change-signal pipeline.

```
Admin UI ── POST /api/admin/virtual-keys ──► CP (vk.go:72 CreateVirtualKey)
                                              ├── IAM check (iamMW, "admin:virtual-key.create")
                                              ├── Validate payload
                                              ├── Persist VirtualKey row in Postgres
                                              │     (vkstore.CreateVirtualKey)
                                              └── CP ── HTTP InvalidateConfig ──► Hub
                                                                                   └── WS change-signal to AI Gateway
                                                                                        └── AI GW.OnConfigChanged pulls virtual_keys from CP (Cat B)
                                                                                              └── atomic.Pointer swap → in-process cache
Application ── /v1/chat/completions Authorization: Bearer vk-... ─► AI GW
                                                                    └── vkauth resolves vk → traffic_event with org/project attribution
                                                                                                  │
                                                                                                  ▼
                                                                                            MQ ── nexus.event.ai-traffic ── Hub traffic writer ── Postgres
                                                                                                                                                      │
                                                                                                                                                      ▼
                                                                                                                                                Cost analytics UI
```

Key invariants:
- The vk is **persisted by CP in Postgres before the change-signal fires**; the AI Gateway never pulls an unknown vk reference.
- `virtual_keys` is a **Category B** config key (`packages/shared/schemas/configkey/configkey.go:134`) — the Hub broadcasts a change-signal only; the AI Gateway pulls the fresh list from CP, not from Hub.
- The traffic_event row carries `request_id` + `trace_id` from the AI GW edge; the analytics UI joins on `request_id` for drill-down.

## 2. Flow 2 — Admin creates a Routing Rule

Same pattern as Flow 1: CP-owned CRUD, Hub-coordinated signal only.

```
Admin UI ── POST /api/admin/routing-rules ──► CP (routing/handler/routing.go:239 CreateRoutingRule)
                                                ├── IAM check (iamMW, "admin:routing-rule.create")
                                                ├── Validate payload
                                                ├── Persist RoutingRule row in Postgres
                                                │     (routingstore.Store.CreateRoutingRule at routing_rule.go:142)
                                                └── CP ── HTTP InvalidateConfig ──► Hub
                                                                                     └── WS change-signal to AI Gateway
                                                                                          └── AI GW.OnConfigChanged pulls routing_rules from CP (Cat B)
                                                                                                └── atomic.Pointer swap → routing engine
                                                                                                      └── Next request evaluates new rule
                                                                                                            └── ResolvedRequest carries canonical payload
                                                                                                                  └── traffic_event records routing trace
```

The **canonical payload** (E47-S2) is what makes smart-routing match operate on a stable shape regardless of the upstream provider format — critical for cross-provider routing rules.

Atomic-pointer swap (rather than mutex-guarded mutation) keeps in-flight requests unblocked; new requests pick up the new rules on next dispatch.

Cross-refs: `routing-architecture.md`, `thing-config-sync-architecture.md`.

## 3. Flow 3 — Admin enables a Hook

Same pattern: CP persists the HookConfig row; Hub fans out the change-signal to whichever Things the hook's `applicableIngress` selects.

```
Admin UI ── PUT /api/admin/hooks/:id ──► CP (governance/hooks/handler/handler.go:75 UpdateHookConfig)
                                              ├── IAM check (iamMW, "admin:hook.update")
                                              ├── Validate payload (canonical onMatch schema)
                                              ├── Persist HookConfig row in Postgres
                                              └── CP ── HTTP InvalidateConfig("hooks") ──► Hub
                                                                                            └── WS change-signal to Things matching `applicableIngress`
                                                                                                ├──► AI GW.OnConfigChanged pulls → atomic.Pointer swap
                                                                                                ├──► Compliance Proxy pulls → atomic.Pointer swap
                                                                                                └──► Agent pulls → atomic.Pointer swap
                                                                                                                    ▼
                                                                                                            Next request: hook runs, traffic_event records hook outcome
                                                                                                                    └── audit-pipeline records hook decisions per stage
                                                                                                                            └── alert evaluator may fire on threshold breach
```

The `applicableIngress` filter determines which Things receive the signal. A hook with `applicableIngress: ['aiGateway']` does not signal the compliance proxy or agents.

Hook decisions are recorded per stage (`request_hook_decision`, `response_hook_decision`) on the traffic_event row. Investigators can see exactly which hook fired in which stage and which rule blocked.

Cross-refs: `hook-architecture.md`, `compliance-pipeline-architecture.md` + `agent-forwarder-architecture.md`.

## 4. Flow 4 — Kill Switch + Emergency Passthrough (E48)

Two independent Cat-A surfaces, governed by separate admin endpoints, share the same fail-open + auto-revert posture.

```
A. Kill switch (compliance-proxy + agent — fleet TLS-bumping off)

Admin UI ── POST /api/admin/compliance/killswitch ──► CP (governance/killswitch/handler/handler.go:97 Post)
                                              ├── Persist + version-bump
                                              └── Hub.NotifyConfigChange("compliance-proxy"|"agent", "killswitch",
                                                       interception.Killswitch{Enabled: <bool>})
                                                        └── Hub shadow Cat A inline; signal Things
                                                              ├──► Compliance Proxy: forwards in tunnel mode (no MITM)
                                                              └──► Agent: forwards without hook pipeline

B. Gateway passthrough (ai-gateway — hooks/cache/normalize bypass)

Admin UI ── PUT /api/admin/passthrough/{global,adapter/:adapter_type,provider/:provider_id} ──► CP (governance/passthrough/handler/handler.go:91-100)
                                               ├── Persist 3-tier blob (global / adapters / providers) into
                                               │   gateway_passthrough_config_global / _adapter / _provider tables
                                               ├── Assemble pushable blob + version-bump
                                               └── Hub.NotifyConfigChange("ai-gateway", "gateway_passthrough", <blob>)
                                                        └── ai-gateway: ResolvedRequest carries bypassHooks=true
                                                                                                bypassCache=true
                                                                                                bypassNormalize=true

   Hub passthrough.expiry job (cron, ~60s default):
      └── if global.expiresAt < now: revert blob to safe defaults,
             push via Hub.NotifyConfigChange, emit system:passthrough_expiry_revert audit.
```

Critical invariants:

- **Cat A inline** — both states ride the shadow itself (no separate pull). This is the fast path because kill switch must propagate in seconds.
- **Distinct shadow keys** — `killswitch` (`interception.Killswitch{Enabled bool}`) and `gateway_passthrough` (3-tier blob with optional `global.expiresAt`) are separate `configKey` values per `packages/shared/schemas/configkey/configkey.go`. There is no global "killswitch.scope" / "passthrough.expiresAt" tri-field state on the shadow; that shape is an older draft.
- **Fail-closed cold-start** — a Thing that boots and finds no shadow does NOT default to passthrough. It defaults to **enforced**, waits for shadow, then applies. (Prevents a Hub-down boot from silently disabling enforcement.)
- **Mandatory expiry on gateway_passthrough** — `global.expiresAt` is the auto-revert trigger; persistent passthrough would defeat compliance.
- **Audit trail non-optional** — every bypassed request still emits a traffic_event with `passthrough=true` and the appropriate bypass flags. Activation itself emits an admin-audit event.
- **Auto-revert** — Hub `passthrough.expiry` job (`packages/nexus-hub/internal/jobs/defs/expiry/passthrough_expiry.go`) runs on a cron schedule (deployment-tunable from `cmd/nexus-hub/main.go`; cluster default ~60s).

Verified end-to-end 2026-05-13 (prod-20260513-e48). Cross-ref `error-taxonomy-architecture.md` for what the gateway returns to clients during passthrough.

## 5. Flow 5 — Agent Enrollment + SSO Login

```
Operator (in CP UI) ── Issue Enrollment Token ──► Hub
                                                   └── Single-use token persisted with expiry

User on workstation ── Install Agent.pkg ──► Agent boots
                                              ├── Receives enrollment token (out of band)
                                              ├── Generates ECDSA keypair
                                              ├── CSR + token ──► Hub
                                              │                    ├── Validate token (single-use, expiry)
                                              │                    ├── Sign CSR with Hub CA
                                              │                    └── Return device cert + chain
                                              ├── Stores device cert in platform keystore
                                              ├── Establishes mTLS WS to Hub
                                              └── Begins pulling shadow

User ── Sign in to Nexus desktop UI via SSO ──► Browser opens
                                                  └── OAuth+PKCE through IdP federation
                                                          └── JIT user provision if first-time
                                                                  └── CP issues bearer to agent UI
Agent UI ── shows device row in CP Devices page ─►
```

Cross-refs: `agent-enrollment-architecture.md`, `idp-sso-architecture.md`.

## 6. Flow 6 — Provider Credential Rotation

```
Admin UI ── PUT /api/admin/credentials/:id ──► CP (providers/handler/credentials.go:148 UpdateCredential)
                                                 ├── IAM check (admin:credential.update)
                                                 ├── Validate payload
                                                 ├── Encrypt new key with CREDENTIAL_ENCRYPTION_KEY (CP-side `crypto.EncryptResult`)
                                                 ├── Persist updated credential row (vault, IV, tag, key-id) in Postgres
                                                 └── h.hub.InvalidateConfig(ctx, "ai-gateway", configkey.Credentials)
                                                         └── Hub bumps `credentials` shadow version; WS change-signal to AI Gateway
                                                                 └── AI GW.OnConfigChanged refreshes credstate (Cat B)
                                                                         └── Next request through this credential:
                                                                                 ├── credstate.Acquire reads new ciphertext
                                                                                 ├── Decrypts in memory only
                                                                                 └── Forwards to provider with new key
                                                                                         └── Old credential never reused
```

CP owns the credential CRUD path; Hub is signal-only. No service restart needed. The previous credential value is overwritten in DB (no credential history); rollback requires another `PUT`.

Cross-ref: `credentials-architecture.md`.

## 7. Flow 7 — Traffic Event Lifecycle

```
[origin] ─► [process pipeline] ─► [emit] ─► MQ ──► [consume] ──► Postgres ──► [query]
                                                                       ▲
                                                                       └─ also: spillstore for body overflow
```

Concrete versions for each origin:

- **AI GW request** — pipeline = hook → route → upstream → resp_hook; emit at completion (success or failure).
- **Compliance Proxy request** — pipeline = access control → TLS bump → hook → upstream → resp_hook; emit at completion.
- **Agent intercepted request** — pipeline = OS intercept → hook → upstream → resp_hook; emit locally to SQLCipher; drain to Hub on connectivity.

The MQ subjects are `nexus.event.ai-traffic` / `nexus.event.compliance` / `nexus.event.agent` (all on the `NEXUS_EVENTS` stream — see `mq-architecture.md` §2-3); the Hub consumer is the traffic writer in `packages/nexus-hub/internal/jobs/consumer/`. Bodies above 256 KiB overflow to spillstore (S3 in prod; memory `project_prod_s3_spillstore_done`).

Cross-refs: `audit-pipeline-architecture.md`, `mq-architecture.md`.

## 8. Flow 8 — Alert Evaluation

```
nexus.event.{ai-traffic,compliance,agent,admin-audit} ──► alerteval aggregators (in Hub)
                                              ├── threshold check per rule
                                              ├── dedup (silence window)
                                              └── raise alert row + publish on `nexus.event.alert`
                                                                       └── alert-dispatcher
                                                                                ├──► webhook
                                                                                ├──► SIEM
                                                                                └──► email
                                                              └── insert into alert_inbox (CP UI surface)
```

Rules come from two sources: Go `BuiltinRules` (compile-time) and DB `AlertRule` rows. Drift between them is a known concern (memory `project_alerting_builtin_drift_2026_05_15`).

Cross-ref: `alerting-architecture.md`.

## 9. Fail-open boundaries

What stays up when something is down:

| Failure | Consequence |
|---|---|
| Hub down | Traffic continues (Things keep their last applied config). Admin UI degrades (no config changes possible). Audit upload retries via MQ + HTTP fallback. |
| CP down | Admin UI unreachable. Traffic continues. Hub continues. Alerts still emitted. |
| MQ down | Traffic continues. Hub buffers locally; agents buffer in SQLCipher. Audit fan-out lags but recovers on MQ return. |
| Postgres down | Traffic continues (in-memory snapshots). Admin reads degrade. Audit writes lag. |
| Redis down | Cache miss → fall through to source of truth. Performance degrades but functional. |

The data plane is designed so a control-plane outage degrades visibility, not enforcement. The macOS NE provider has a stricter fail-open posture for its own reasons; cross-ref `agent-ne-fail-open-architecture.md`.

## 10. The `trace_id` + `request_id` constant

Every golden flow above carries both ids end-to-end. They are the **operational substrate** that lets the UI and the runbooks reconstruct what happened. Whenever you add a new flow, the **first question** is: where is `request_id` generated, and how does it reach every downstream record? If the answer is unclear, the flow is under-instrumented. Cross-ref `trace-id-propagation-architecture.md`.

## 11. Sources

Spans the codebase. Anchor on:
- `packages/control-plane/internal/governance/{killswitch,passthrough}/handler/handler.go` — Flow 4 admin entry points (separate killswitch + gateway_passthrough surfaces).
- `packages/control-plane/internal/handler/` — Flow 1 / 2 / 3 / 5 / 6 entry points.
- `packages/nexus-hub/internal/fleet/handler/hubapi/` — Hub HTTP API (shadow CRUD, override push).
- `packages/nexus-hub/internal/jobs/defs/expiry/passthrough_expiry.go` — Flow 4 auto-revert job.
- `packages/ai-gateway/internal/routing/` — Flow 2 dispatch (capability + matcher + llm).
- `packages/shared/schemas/credstate/` — Flow 6 dirty-set (cross-service constants; consumed by AI Gateway, CP, and Hub).
- `packages/nexus-hub/internal/alerts/eval/` — Flow 8.

## 12. Cross-references

- `service-call-framework.md` — CP ↔ Hub HTTP contracts (one slice of these flows).
- All other Tier-1 docs — each flow above is a coordinated path through specific docs; the cross-refs in each subsection point to the canonical reference.
