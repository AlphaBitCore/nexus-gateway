# Compliance Proxy runtime API architecture

The Compliance Proxy exposes an operator-facing runtime HTTP API — separate from
the Control Plane admin API — plus the shadow-driven local state behind it: the
kill switch, temporary exemptions, break-glass overrides, and config loading.
This doc covers that control surface.

## What this doc covers (and what it does not)

The request data path (CONNECT, access, forward gate) is in
[compliance-proxy-connect-forward-architecture.md](compliance-proxy-connect-forward-architecture.md);
certificate handling in
[compliance-proxy-tls-cert-architecture.md](compliance-proxy-tls-cert-architecture.md).
The kill-switch propagation model across services is owned by
[kill-switch-architecture.md](../../cross-cutting/safety/kill-switch-architecture.md);
the Hub config-sync model by
[thing-config-sync-architecture.md](../../cross-cutting/foundation/thing-config-sync-architecture.md).
This doc describes the proxy-local implementation.

## 1. Runtime HTTP API

`runtime.Server` (`packages/compliance-proxy/internal/runtime/server`) registers
the operator surfaces:

- `GET /healthz` — liveness/readiness, **no auth** (for probes).
- `GET /metrics` — Prometheus scrape.
- `GET /connections` — active-connection snapshot.
- `GET /runtime/config`, `GET /runtime/config/{key}`, `GET /runtime/sync-status`,
  `GET /runtime/health` — shadow-aligned read surfaces.
- `PUT /runtime/config/{key}` — break-glass write.

Every surface except `/healthz` is wrapped by `tokenAuth.Require`.
`runtime.auth.TokenAuth` loads `COMPLIANCE_PROXY_API_TOKEN` and compares with
`subtle.ConstantTimeCompare`; when the variable is unset, auth is disabled and a
warning is logged (dev mode).

## 2. Kill switch (local state)

`runtime.killswitch.KillSwitch` (`packages/compliance-proxy/internal/runtime/killswitch`)
is the local state behind the forward-path `IsEngaged` check, which is a
lock-free `atomic.Bool` read on the hot path. `Toggle` sets the state; `ApplyBreakGlass` applies
a break-glass payload. There is no `ForceClose` and no in-memory history: the
method, its registered callback and the ring were removed, none of them having
a caller, a route or a UI affordance — and the callback shut the whole proxy
down rather than closing bumped connections. Durable history lives on the Hub,
which is what the console reads. State is driven by the Hub shadow — there is no local
persistence and no cross-instance publisher.

## 3. Break-glass overrides

`runtime.breakglass` (`packages/compliance-proxy/internal/runtime/breakglass`) is
the local override path when the Hub is unreachable. `PUT /runtime/config/{key}`
decodes the raw payload and dispatches to a per-key `ApplyBreakGlass` branch — a
malformed payload fails with `400` before anything is applied. The reported
version is bumped to `max(desired, reported) + 1`, the change is recorded in an
event log, and if Hub delivery fails the mutation is spooled to disk and
reconciled on reconnect. Break-glass is scoped to the keys that support it
(`killswitch`, `exemptions`).

Delivery to Hub uses a **dedicated break-glass wire** (the report carries the
per-key version map plus `reason` / `sourceIp` / `actorTokenId`): the WebSocket
client sends a `shadow_report_break_glass` frame, and the HTTP fallback (plus the
deferred `ReplayPending` retry) POSTs to
`/api/internal/things/shadow/break-glass`. Hub mirrors the client's
`{killswitch, exemptions}` allowlist server-side and validates each state against
its canonical configtypes schema before adoption — a non-allowlisted key or a
malformed shape is rejected, never silently adopted. `killswitch` is adopted into
the fleet-wide template; `exemptions` is adopted as a per-Thing desired override
on the reporting node only. See the kill-switch architecture doc §6 for the
Hub-side authority gate and scope rules.

## 4. Temporary exemptions

`exemption.Store` (`packages/compliance-proxy/internal/exemption`) holds
operator-granted temporary exemptions that let traffic skip the compliance hooks
while still being TLS-bumped — the escape hatch for false-positive blocks. Each
entry matches a source IP (exact or CIDR) and a target host (exact or
`*.example.com` wildcard) and carries an expiry. The store is driven by the Hub
shadow — `Rebuild` atomically replaces the set from the `activeExemptions`
snapshot — with no local persistence; a background `StartCleanup` loop purges
expired entries. The forward gate consults `IsExempt` per flow.

## 5. Config loading and hot-reload

`config/loaders` reads enable-gated rows from PostgreSQL into typed structures —
domain allowlist, hook config, active exemptions, observability config,
payload-capture config. `config/cache` wraps each loader in a generic `Cache[T]`
guarded by a `sync.RWMutex` (fast read-locked hits, write-locked reload on miss
or invalidation) with TTL and metrics. When the Hub pushes a config change, the
command's config dispatcher
(`packages/compliance-proxy/cmd/compliance-proxy/configdispatch`) invalidates the
affected cache and re-applies derived state — for example
`AccessChecker.SwapDomainAllowlist` and `ExemptionStore.Rebuild`.

## References

- `packages/compliance-proxy/internal/runtime/server/` — runtime HTTP routes
- `packages/compliance-proxy/internal/runtime/auth/` — bearer `COMPLIANCE_PROXY_API_TOKEN`, constant-time compare
- `packages/compliance-proxy/internal/runtime/killswitch/` — local kill-switch state
- `packages/compliance-proxy/internal/runtime/breakglass/` — break-glass PUT, version bump, event log, spool
- `packages/compliance-proxy/internal/exemption/` — shadow-driven temporary hook exemptions
- `packages/compliance-proxy/internal/config/` — PostgreSQL loaders + typed config cache
- `packages/compliance-proxy/cmd/compliance-proxy/configdispatch/` — Hub config-change dispatcher
