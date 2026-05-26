# Fail Open Posture

*Audience: security reviewers evaluating the system's safety model, and contributors making changes to the hook pipeline, macOS Network Extension, or emergency passthrough.*

Nexus Gateway keeps AI traffic flowing even when enforcement infrastructure is unavailable. The guiding principle: a compliance control-plane outage must not become a productivity outage. This posture is expressed at three layers — the data-plane hook pipeline (automatic fail-open on hook errors), emergency passthrough (explicit, audited, time-limited bypass), and the kill switch (Hub shadow Cat A, instant propagation). The macOS Network Extension applies a substantially stricter variant of this rule because it occupies the host's outbound packet path, where a misbehaving NE provider kills the entire Mac's network — not just AI traffic.

This page states each mechanism plainly. For implementation depth, follow the canonical doc links at the bottom.

---

## Data-plane hook pipeline fail-open

All three data-plane services (AI Gateway, Compliance Proxy, Desktop Agent) run the shared hook pipeline from `packages/shared/policy/hooks`. If a hook evaluation fails — timeout, process crash, schema validation error, dependency unavailability — the request proceeds as if the hook returned `Approve`. The traffic event records the hook failure reason. An alert fires.

This is the correct behavior for a service that sits in-line with AI traffic. Dropping traffic on hook failure would mean a single misconfigured PII scanner or a flapping webhook integration could silently stop all AI requests for affected users.

**Streaming compliance modes** give operators graduated control over the fail-open surface for streaming responses:

| Mode | Hook timing | Client impact on hard reject |
|---|---|---|
| `passthrough` | No hook inspection | None (traffic always flows) |
| `buffer_full_block` | Hook runs at stream end, before any byte is forwarded | HTTP 451; client sees nothing before reject |
| `chunked_async` | Hook runs per chunk and at stream end | Reject recorded; already-sent bytes cannot be retracted |

**Boot behavior differs.** Data-plane services that boot without being able to pull their shadow from Hub block on startup — they do not serve traffic with an empty config. Silently starting with no hooks and no routing rules would disable enforcement without any signal to operators. The fail-closed cold-start is intentional.

When Hub becomes unreachable after a data-plane service has loaded its config, the service continues from its in-memory config snapshot. Enforcement continues with the last-known config. See [Control Plane Vs Data Plane](Control-Plane-Vs-Data-Plane) for the full resilience behavior.

---

## macOS Network Extension — five fail-open invariants (safety-critical)

The macOS `NETransparentProxyProvider` in `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/TransparentProxyProvider.swift` is **safety-critical**. It is in every outbound packet path on the host. A hang, panic, or silent claim-without-relay does not slow AI traffic — it takes down DNS, DHCP, mDNS, NTP, Apple Push, and VPNs. The entire Mac loses network connectivity. Recovery requires:

```bash
sudo launchctl unload /Library/LaunchDaemons/com.nexus-gateway.agent.plist
sudo rm /Library/LaunchDaemons/com.nexus-gateway.agent.plist
sudo reboot
```

The 2026-05-15 incident validated that this recovery procedure is necessary. Every code change to `TransparentProxyProvider.swift` or its dependents must satisfy all five invariants below before merging.

### Rule 1 — `handleNewFlow` decides synchronously

`handleNewFlow` is the OS callback that claims or releases a network flow. It MUST:
- Decide **synchronously** — no blocking on async I/O or daemon calls inside the callback itself.
- Return `true` only for flows the provider can **fully relay**.
- For UDP flows without a relay implementation: return `false` for unknown bundles.

A flow claimed with `return true` that is not subsequently relayed stalls forever. The OS does not recover it. Returning `false` routes the flow through the default path — the user loses AI traffic visibility but keeps their network.

### Rule 2 — Every async daemon callback has a fail-open timeout

Some flows require a decision from the Agent daemon (e.g., "is this domain in scope?"). The daemon may be slow or absent. Every async callback into the daemon MUST have a fail-open timeout:

- `requestDecision` defaults to passthrough after **2 seconds** — verified at `IPCProtocol.swift:328-351` (`queue.asyncAfter(deadline: .now() + 2.0)` fires a synthetic passthrough if the daemon does not respond).
- `peekSNIThenRelay` falls through to plain relay after **500 milliseconds** — verified at `TransparentProxyProvider.swift:309-317, 631-639`.

Never let a flow hang waiting on an absent daemon. Degraded visibility (the daemon is gone, the NE provider cannot make a policy decision) is acceptable. Broken networking is not.

### Rule 3 — No hardcoded enforcement lists in NE Swift code

Bundle-ID allowlists, domain allowlists, and any admin-controlled enforcement set MUST NOT be hardcoded in Swift. The required pattern:

1. Hub pushes the list in the `agent_settings` shadow blob (Cat B key, sub-second propagation).
2. The Agent daemon writes the list to a file (`/var/run/nexus-agent/quic-bundles.json` or equivalent).
3. The NE provider reads the file **file-only**, with **empty-as-fail-safe**: an empty file means enforcement is off, not enforcement everywhere.

A hardcoded fallback list would silently override admin policy. If an admin disables a bundle via the CP UI, the file becomes empty, and the NE provider stops enforcing — correct behavior. A hardcoded entry would continue enforcing it regardless of the admin's change, breaking the admin's mental model.

### Rule 4 — `isLikelyXyz = true` patterns are banned

A flag named `isLikelyXyz` set to `true` "for now" with a comment to fix it later is a time bomb. Either write the real condition:

```swift
let isLikelyQUIC = (proto == .udp && (dst == 443 || dst == 80) && payload.matches(.quicInitial))
```

…or return `false` explicitly:

```swift
let isLikelyQUIC = false
// QUIC detection not yet implemented; default to false (fail-open for QUIC flows).
```

A `= true` flag looks intentional in code review but breaks the moment the NE provider encounters a flow that matches the unmarked-true case. The 2026-05-15 incident had a flavour of this pattern.

### Rule 5 — System DNS / DHCP / Push services MUST NOT have their UDP closed

The following process names MUST always be relayed under any circumstance, even if they appear in a deny list or bundle kill-list:

- `mdnsresponder`
- `configd`
- `dhcpcd`
- `apsd`
- `nsurlsessiond`
- `kdc`
- `ntpd`

When adding any process to a kill-list or deny-egress list, validate the addition against this list. These processes are how macOS stays connected — DNS queries (`mdnsresponder`), network configuration (`configd`), DHCP (`dhcpcd`), Apple Push Notification Service (`apsd`), NTP (`ntpd`). Closing their UDP connections breaks the operating system's network stack for all applications, not just AI tools.

---

## Emergency passthrough

Emergency passthrough is the controlled, audited, time-limited bypass for situations where the compliance pipeline itself cannot run: a hook service outage, mass misconfiguration, or dependency failure. It is not a silent fallback — it requires an explicit admin action, has a mandatory expiry, and maintains a complete audit trail.

### Three tiers

| Tier | Scope | Example |
|---|---|---|
| Global | All `/v1/*` traffic | Total hook-pipeline outage |
| Adapter | One adapter family (e.g., `anthropic`) | Anthropic-specific hook rule misfiring |
| Provider | One specific `Provider` row | Single-provider credential / rule issue |

Tiers compose: if Global is active, Adapter and Provider tiers are moot.

### Three bypass flags (orthogonal)

Each tier carries three independent bypass flags:

| Flag | What it skips |
|---|---|
| `bypassHooks` | Request + response hook stages (and quota gates when hooks are bypassed) |
| `bypassCache` | Response cache reads + writes; every request goes upstream |
| `bypassNormalize` | Wire-format normalization layer; raw provider bytes forwarded |

Admins can disable hooks while keeping cache and normalization intact, or disable the cache while keeping hooks running.

### Mandatory expiry and auto-revert

`expiresAt` is bounded: maximum **8 hours** from now, default **1 hour**. The admin UI rejects manual input exceeding 8 hours; the server validates server-side as well. A persistent passthrough would defeat compliance; the expiry is non-negotiable.

The `kill_switch.reconcile` Hub job runs every **60 seconds**. For every passthrough row where `enabled=true` and `expiresAt < now`, Hub:
1. Sets `enabled=false`.
2. Emits `system:passthrough.auto_reverted` to the admin audit log.
3. Signals affected Things — all Things re-pull their shadow and resume enforcement.

Admin forgetting to revert is safe — Hub reverts automatically.

### Fail-closed cold-start

A data-plane service that boots and finds no shadow defaults to **enforced** mode. It waits for the shadow to load before serving traffic. A Hub-down boot cannot silently disable enforcement.

### Audit trail

Every bypassed request emits a `traffic_event` with `passthrough=true` and `bypass_reason`. The L4 view (provider, model, credential, routing context) remains accurate — only the enforcement stages are skipped. Activation itself emits `admin:kill_switch.activated`; auto-revert emits `system:kill_switch.auto_reverted`. Both are non-optional for compliance review of passthrough windows.

---

## Kill switch (Category A shadow)

The kill switch shares the same three-tier, three-flag DB tables (`GatewayPassthroughConfigGlobal/Adapter/Provider`) and bypass semantics as emergency passthrough. The distinction is administrative framing: emergency passthrough is used for compliance-pipeline failures; the kill switch is used for immediate operational interventions (provider outage, runaway cost, security incident).

Both are propagated as Cat A shadow keys — inline values that ride the change-signal directly — providing millisecond propagation to all Things without a separate pull round-trip. The Cat A classification is why the kill switch achieves instant-fleet-wide effect: the change-signal carries the bypass blob, and every Thing applies it in memory before serving the next request.

## Failure mode summary

| Scenario | Behavior |
|---|---|
| Hook evaluation timeout | Request proceeds (fail-open); alert fires; traffic event records failure reason. |
| Hook process crash | Same as above. |
| Hub unreachable (post-boot) | Data-plane serves from last-pulled config snapshot; enforcement continues; alerts fire on WS heartbeat loss. |
| Hub unreachable (at boot) | Server-side services block on startup (fail-closed cold-start). |
| Emergency passthrough active | Bypassed layers skip enforcement; audit trail still complete; auto-revert at expiry. |
| macOS NE daemon absent | Async callbacks time out (2s `requestDecision`, 500ms SNI peek); NE provider falls through to passthrough — network stays up. |
| macOS NE daemon returns wrong decision | Flows claim `return true` only for relayable flows; unknown UDP = `return false` — network stays up. |
| macOS NE enforcement file empty | `empty-as-fail-safe` semantics: enforcement off, traffic passes through — network stays up. |
| Compliance Proxy down | Path A (AI Gateway) and Path C (Agent) continue unaffected; Path B traffic fails at the TCP level (proxy unreachable). |
| Desktop Agent restart | In-flight flows complete; new flows re-intercepted after restart; local SQLCipher queue preserved. |

## Security review guidance

Reviewers assessing the fail-open posture should verify:

1. **Hook pipeline fail-open is bounded.** Fail-open only applies to hook *evaluation* errors. The hook pipeline itself does not fail open for *network errors to upstream providers* — those are handled by the error taxonomy / retry / circuit breaker layer.

2. **Emergency passthrough expiry is enforced server-side.** The UI enforces the 8-hour cap, but the Hub's `kill_switch.reconcile` job is the authoritative expiry enforcement. A compromised admin UI that extends the expiry beyond 8 hours would be rejected at Hub.

3. **Cat A shadow blob validates at Hub.** Corrupt passthrough writes are rejected at Hub's shadow CRUD API before they propagate to Things. A corrupted in-flight change-signal triggers a pull error and falls back to the last good snapshot.

4. **NE Rule 3 file is written by the daemon, not the NE provider.** The NE provider only reads the file. The daemon (running as a privileged process) writes it after validating the shadow contents. An attacker who can modify the file can affect enforcement, but cannot gain elevated privileges beyond what the enforcement policy controls.

5. **Audit trail completeness.** Emergency passthrough and kill-switch events always emit to the audit log. A passthrough window without an `admin:kill_switch.activated` audit record would indicate an out-of-band bypass — investigate immediately.

---

## macOS NE test invariants

Before merging any change to `TransparentProxyProvider.swift` or its dependents, the following tests are required:

- Boot the agent on a fresh macOS. Verify Wi-Fi browsing works (DNS resolution, DHCP assignment, basic HTTPS fetch).
- Disable the Hub daemon (simulate the daemon disappearing mid-session). Verify Wi-Fi still works — no flows should hang; all should pass through or fail gracefully.
- Send malformed flows (random UDP packets, unknown TCP ports). Verify the OS does not see these flows stall.
- Verify QUIC traffic (`udp.dst == 443`) either passes through or is captured per the file-only bundle list, never both simultaneously, never stalled.
- Run for 24 hours on a developer machine. No human intervention required; no "did I lose internet?" report on Slack.

These tests are not optional — the 2026-05-15 incident validated that every one of these scenarios is reachable by real-world flows.

## Quota enforcement and fail-open

Quota enforcement (per-VK rate and cost limits) is not part of the hook pipeline. It is a distinct enforcement layer in the AI Gateway that runs before the hook pipeline in the request lifecycle:

1. VK authentication → hydrate `RequestContext`
2. Quota check → decrement Valkey counter; reject with HTTP 429 if exhausted **(fail-closed for quota)**
3. Request-stage hooks → **(fail-open)**
4. Route + dispatch upstream
5. Response-stage hooks → **(fail-open)**
6. Stamp traffic event

Quota enforcement is **fail-closed**: if Valkey is unavailable, quota counters cannot be read or decremented, and the request is rejected with a descriptive error (not allowed to serve without quota data, because silent pass-through would permit unlimited spend). This is the opposite of the hook pipeline's fail-open policy.

The design rationale: quota is a hard contract between the org and the gateway — breaching it has financial consequences. Hook enforcement is best-effort compliance — blocking traffic because a PII scanner is down is worse than logging the detection miss.

## Multi-instance kill-switch fan-out

When a kill-switch activation is applied to a fleet with multiple AI Gateway instances, the propagation sequence matters:

1. Admin activates in CP UI.
2. Control Plane writes to Hub: `POST /api/hub/shadow/global-passthrough`.
3. Hub persists and increments version.
4. Hub fans out a Cat A change-signal to **every** registered `ai-gateway` Thing simultaneously (one WebSocket message per registered Thing).
5. Each AI Gateway instance's `thingclient` receives the signal and applies the bypass blob atomically.

**Ordering guarantee.** Every instance applies the Cat A value from the same version of the change-signal. Because Cat A values ride the signal itself (no separate pull), there is no window where half the fleet is bypassed and half is not — all instances update within milliseconds of each other, bounded by network latency from Hub to each instance.

**What happens to in-flight requests.** Requests that are already past the hook-pipeline stage (already evaluated and dispatched upstream) when the Cat A signal arrives complete with the old enforcement state. Requests that enter the hook stage after the signal has been applied skip enforcement. The `passthrough=true` flag is set on the traffic event for requests served while the passthrough is active.

This is the correct behavior: there is no mechanism to retroactively apply a bypass to an already-evaluated request, and the audit record correctly marks the boundary.

## Passthrough activation: the full admin workflow

Emergency passthrough should be rare. When it is needed, the activation workflow matters — operators need to know exactly what they are enabling, for how long, and that the audit trail is capturing the window.

**Standard activation via CP UI:**

1. Navigate to Infrastructure → Kill Switch.
2. Select the scope: Global, Adapter (select which adapter family), or Provider (select which provider row).
3. Select the bypass flags: `bypassHooks`, `bypassCache`, `bypassNormalize` (each is an independent toggle).
4. Set the expiry time (minimum 5 minutes; maximum 8 hours; default 1 hour pre-filled).
5. Add an activation reason (required field; stored in the `admin_audit_log` row).
6. Confirm. Hub persists the row and emits the Cat A change-signal.

Within 1–2 seconds, all affected Things display the bypass flags on their status. The Kill Switch page updates in real time.

**Manual deactivation.** If the incident resolves before expiry, the admin returns to the Kill Switch page and presses "Deactivate." Hub emits a new change-signal with `enabled: false`. Enforcement resumes within 1–2 seconds.

**Auto-revert.** If the admin does not return, the `kill_switch.reconcile` job reverts at expiry. The `system:kill_switch.auto_reverted` audit log entry records the automatic reversion. This entry is important for compliance review: if an active passthrough window closed via auto-revert rather than manual action, it was forgotten — review whether the reason for activation was resolved.

**API activation.** The same workflow is available via:
```bash
cp_curl POST /admin/emergency-passthrough/global \
  -d '{"bypassHooks":true,"bypassCache":false,"bypassNormalize":false,"expiresInMinutes":60,"reason":"hook service outage incident #123"}'
```

This requires `admin:emergency_passthrough.write` IAM permission.

## Fail-open scope: hook evaluation vs traffic forwarding

A common point of confusion is distinguishing what "fail-open" applies to in the hook pipeline. It applies to hook **evaluation** failures — not to traffic **forwarding** failures.

**Hook evaluation fail-open (in scope):**
- PII scanner process crashes or returns an error.
- Webhook hook times out waiting for the external endpoint's response.
- Hook schema validation fails because the hook config was partially applied.
- Any exception inside the `HookEvaluator.Evaluate` function.

In all of these cases: the hook is skipped; the request proceeds; an alert fires; the traffic event records `hook_decision=hook_error`.

**Traffic forwarding errors (out of scope — handled separately):**
- The upstream AI provider returns HTTP 500 or connection timeout.
- The AI Gateway's outbound connection pool exhausts.
- The provider returns an authentication error (401) — credential pool circuit-breaker trips.

These are handled by the error taxonomy and routing fallback chain. A `fallback-chain` routing strategy will try the next provider on 5xx or 429; a `single` strategy will return the provider's error directly to the client. The hook pipeline fail-open does not apply here.

This distinction matters for security review: "fail-open" refers to enforcement decisions under infrastructure failure, not to traffic forwarding. The gateway does not silently forward traffic when the upstream provider is unavailable.

## Streaming compliance: the three-mode tradeoff table

Streaming compliance modes introduce a fundamental tradeoff between latency, completeness of enforcement, and what happens when the hook rejects mid-stream. Operators choose a mode per hook config; the default is `passthrough` for backward compatibility with existing SDK integrations.

| Property | `passthrough` | `buffer_full_block` | `chunked_async` |
|---|---|---|---|
| First-byte latency | Lowest (no buffering) | Highest (full response buffered before forwarding any byte) | Low (chunks relayed in real time) |
| Hook runs | Never | Once at stream end | Per chunk + at stream end |
| Hard reject prevents client from seeing content | N/A | ✅ (HTTP 451 before first byte) | ❌ (already-sent bytes cannot be retracted) |
| Response in audit | ❌ (nothing captured) | ✅ (full body captured before any bytes forwarded) | Partial (captured up to first reject) |
| Use case | Maximum throughput; audit-only visibility | Highest compliance; latency-tolerant workloads | Real-time UX; partial content rejection acceptable |

When a new hook is enabled for streaming traffic, operators should consider: "Can this workflow tolerate buffering the full response before the user sees it?" If yes, `buffer_full_block` is the safest choice. If the user expects to see the response as it streams, `chunked_async` accepts the tradeoff that already-sent chunks cannot be recalled.

The fail-open behavior for hook evaluation errors applies to all three modes: if the hook crashes or times out during evaluation, the stream continues regardless of mode.

## Hook pipeline fail-open: alert lifecycle

When a hook evaluation fails and the request proceeds (fail-open), the alert pipeline fires to notify operators. The alert lifecycle:

1. The hook evaluator returns an error (timeout, schema invalid, dependency unavailable).
2. The hook pipeline marks the stage result as `hook_error` and records the error reason in the `traffic_event` row.
3. The request proceeds as if the hook returned `Approve`.
4. The traffic event's `hook_decision` field is set to `hook_error` (not `approve`) so compliance analytics can filter for these events.
5. If the alert rule `hook.evaluation_error_rate` is active (enabled by default), an alert fires when the rate of `hook_error` events for a given hook ID exceeds the configured threshold within the rolling window.
6. The alert is visible on the CP UI Alerts page and optionally forwarded to the SIEM bridge.

This means a single flapping webhook integration that fails 10% of evaluations will trigger a yellow alert without stopping traffic. Operators can investigate the hook, fix the issue, and the failure rate drops — no manual reset needed. An alert that fires continuously for > 30 minutes should escalate to incident response.

**Hook error vs hook reject.** These are different: a `hook_error` means the hook could not evaluate (infrastructure failure), and the pipeline fails open. A `hook_reject` means the hook evaluated successfully and decided to block (expected enforcement behavior). The `hook_decision` column distinguishes the two, and they have separate alert rule types.

## macOS NE IPC protocol overview

Rule 2 (async daemon timeouts) requires understanding the IPC protocol between the NE provider (Swift, sandboxed) and the Agent daemon (Go, privileged). The protocol:

- The NE provider sends a `RequestDecision` message to the daemon over a local Unix domain socket.
- The daemon evaluates the flow against the current enforcement policy (from the last-known shadow state).
- The daemon responds with `Allow` or `Block` within 2 seconds (or the NE provider's timeout fires first, returning passthrough).

The 2-second timeout at `IPCProtocol.swift:328-351` is implemented as a `DispatchWorkItem` that fires a synthetic `Allow` response if the daemon's response has not arrived. This is a safety mechanism — not an error path. The NE provider has no way to distinguish "daemon said Allow" from "daemon timeout, defaulting Allow". This is intentional: the fail-open behavior is the same regardless of why the daemon did not respond.

The 500ms timeout on `peekSNIThenRelay` (`TransparentProxyProvider.swift:309-317, 631-639`) covers a shorter operation: extracting the SNI hostname from a TLS ClientHello to enable domain-based routing decisions. If the peek times out, the NE provider relays the flow as-is (no SNI-based filtering). Network stays up; compliance coverage for that flow is lost.

Both timeouts are configurable via the `agent_settings` shadow blob. The defaults (2s and 500ms) were chosen as conservative values that work on slow corporate laptops with high daemon startup latency.

## Compliance review: what each bypass flag actually skips

Each emergency-passthrough bypass flag has a defined scope. Understanding the scope is important for compliance reviewers assessing the risk of a passthrough window:

**`bypassHooks`** — skips the request-stage and response-stage hook pipelines. PII detection, keyword blocking, content classification, outbound webhook notifications, and custom hook logic do not run. Quota gate (if wired to a hook) may also be skipped. The upstream call still happens; traffic still reaches the provider; the response still reaches the client. Billing still accrues (unless `bypassCache` is also set). The traffic event is still emitted with `passthrough=true`.

**`bypassCache`** — skips response cache reads and writes. Every request goes directly to the upstream provider, even if an identical request was served from cache moments ago. This is useful when the cache holds stale or incorrect data that needs to be flushed via fresh upstream calls without waiting for TTL expiry.

**`bypassNormalize`** — skips the wire-format normalization layer. Raw provider bytes are forwarded to the client without canonical extraction. Token counts, cost, and body text in the traffic event may be incomplete or absent. This is useful when the normalizer is misconfigured or crashing, and the immediate priority is restoring traffic flow rather than extracting analytics data.

**What is NOT bypassed regardless of flags:**

- Traffic event emission. Every request emits a `traffic_event` row, even with all three flags set. `passthrough=true` and `bypass_reason` are stamped.
- Routing engine and credential resolution. The request still goes through the routing engine; the credential pool still selects a provider credential.
- Upstream TLS verification. Provider TLS certificates are still verified; the gateway does not disable upstream TLS on passthrough.
- Admin IAM check. Activating passthrough itself requires `admin:emergency_passthrough.write`. The bypass does not grant extra API permissions.

## Desktop Agent fail-open vs macOS NE fail-open

The Desktop Agent has two distinct fail-open policies that apply at different layers:

**Agent hook pipeline fail-open.** If the hook pipeline on the agent fails to return a decision (hook process crash, timeout), the request proceeds with `Approve` semantics — the same policy as the AI Gateway and Compliance Proxy. This applies at the Go service level within the agent daemon.

**macOS NE provider fail-open.** If the NE provider cannot reach the agent daemon for a policy decision, it defaults to passthrough after 2 seconds (Rule 2). This is a different, more urgent fail-open: it prevents the NE extension from stalling the entire host's network because the daemon is slow or absent.

These two policies are independent:

| Scenario | Layer | Behavior |
|---|---|---|
| Hook evaluation timeout in agent daemon | Agent Go service | Request proceeds as Approve; alert fires |
| NE provider cannot reach agent daemon | macOS NE Swift extension | Flow passes through after 2s; NE stays healthy |
| Agent daemon crashes entirely | Both layers | NE provider falls through immediately; agent hooks not consulted |
| Agent daemon restarts | Both layers | NE re-establishes IPC; hooks resume on new flows |

The NE-level fail-open is strictly stronger than the agent hook-level fail-open: if the daemon is gone, both layers simultaneously default to passthrough, but through different mechanisms. This defense in depth ensures that even if one layer's fail-open mechanism has a bug, the other layer provides the network-safety backstop.

## Kill switch vs emergency passthrough: operational distinction

Both mechanisms use the same DB tables and Cat A shadow propagation. The difference is operational framing and intended use:

| Aspect | Kill switch | Emergency passthrough |
|---|---|---|
| Intended for | Immediate operational interventions | Compliance pipeline failures |
| Activation trigger | Provider outage, runaway cost, security incident | Hook service outage, mass misconfiguration |
| Typical duration | Minutes to hours | Minutes to hours |
| Bypass granularity | Global / adapter / provider | Global / adapter / provider |
| Bypass flags | Same three (`bypassHooks`, `bypassCache`, `bypassNormalize`) | Same three |
| Auto-revert | Yes (same 60s reconcile job) | Yes (same 60s reconcile job) |
| Audit event emitted | `admin:kill_switch.activated` | `admin:emergency_passthrough.activated` |

In practice, operators often use "kill switch" colloquially for any passthrough activation. The platform's audit trail uses the precise terminology — compliance reviewers should check for both event types when reconstructing a passthrough window.

## Recovery procedure

If a macOS user reports "Wi-Fi is connected but nothing works" after installing or updating the Nexus Agent:

1. Confirm the NE provider is the culprit: Activity Monitor → check for `NexusAgentExtension` with high CPU or many pending I/O operations.
2. Unload via the canonical uninstall script:
   ```bash
   sudo bash packages/agent/platform/darwin/Scripts/uninstall.sh
   ```
3. If the script is unavailable on the host:
   ```bash
   sudo launchctl unload /Library/LaunchDaemons/com.nexus-gateway.agent.plist
   sudo rm /Library/LaunchDaemons/com.nexus-gateway.agent.plist
   sudo reboot
   ```
4. Verify network is restored.
5. Capture diagnostic bundle (logs, plist, recent config) before remediation.

Engineering's goal is to make this procedure unnecessary. The five fail-open invariants exist precisely to prevent network-kill bugs from reaching users.

## Canonical docs

- [`agent-ne-fail-open-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md) — the five NE invariants with code-line citations, the blast-radius reality, recovery procedure, and test invariants
- [`emergency-passthrough-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/safety/emergency-passthrough-architecture.md) — three-tier schema, bypass flag semantics, mandatory expiry, Hub reconcile loop, fail-closed cold-start, audit trail
- [`thing-config-sync-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/cross-cutting/foundation/thing-config-sync-architecture.md) — Cat A/B/C key classification and change-signal propagation

**Adjacent wiki pages**: [Control Plane Vs Data Plane](Control-Plane-Vs-Data-Plane) · [Architecture Overview](Architecture-Overview) · [Three Traffic Paths](Three-Traffic-Paths) · [Trust Boundaries](Trust-Boundaries) · [Emergency Passthrough](Emergency-Passthrough) · [Agent macOS NE Architecture](Agent-macOS-NE-Architecture)
