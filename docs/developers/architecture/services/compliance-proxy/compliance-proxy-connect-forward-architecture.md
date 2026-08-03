# Compliance Proxy connect & forward architecture

The Compliance Proxy is an explicit forward proxy: clients send HTTP `CONNECT`,
the proxy gates the connection, establishes the tunnel, and either bumps TLS to
inspect the flow or passes it through. This doc covers the request entry path —
CONNECT handling, access control, connection lifecycle, and the forward gate
sequence that decides bump-vs-passthrough.

## What this doc covers (and what it does not)

The normalize stage and adapter catalog are in
[compliance-pipeline-architecture.md](compliance-pipeline-architecture.md);
certificate issuance and caching in
[compliance-proxy-tls-cert-architecture.md](compliance-proxy-tls-cert-architecture.md);
the runtime admin API, temporary exemptions, kill-switch state, and config
loading in
[compliance-proxy-runtime-api-architecture.md](compliance-proxy-runtime-api-architecture.md).
The shared TLS bump itself (`packages/shared/transport/tlsbump`, driven by both
the proxy and the Agent) is described from the Agent side in
[agent-forwarder-architecture.md](../agent/agent-forwarder-architecture.md).
Domain-allowlist matching detail is in
[domain-device-predicate-architecture.md](domain-device-predicate-architecture.md).

## 1. CONNECT entry and access control

`ProxyServer.ServeHTTP` (`packages/compliance-proxy/internal/proxy/server`) is
the entry point for every inbound `CONNECT`. Before any tunnel is established it
runs the pre-tunnel gates:

- **Onboarding intercept.** For monitored domains the proxy can answer with a
  `407` to drive client onboarding before the tunnel opens.
- **Access control.** `access.Checker.CheckConnect`
  (`packages/compliance-proxy/internal/access`) applies three checks in order
  and returns the first failure: a source-IP allowlist (`ErrIPDenied`), a
  destination domain allowlist (`ErrDomainDenied`), and a resolved
  private/reserved-IP check (`ErrPrivateIP`), then returns nil. The IP allowlist
  is yaml-fixed at boot; the domain allowlist merges static yaml with dynamic DB
  entries and is hot-swapped atomically via `SwapDomainAllowlist` when the Hub
  pushes a change.
- **Unlisted-passthrough downgrade (`allowUnlistedPassthrough`, default off).**
  When enabled, a `CONNECT` rejected *only* for `ErrDomainDenied` (target not in
  the domain allowlist) is downgraded to a transparent raw-TCP relay
  (`serveUnlistedPassthrough`) instead of a `403` — no MITM, no audit. It is a
  monitor-a-subset-only dev posture; other rejections (`ErrIPDenied`,
  `ErrPrivateIP`) still refuse. Two guards keep this from becoming an **open
  proxy** (the failure mode behind the 2026-07-09 incident, where the flag was on
  with an all-permitting source allowlist and `:3128` reachable from the internet,
  and external parties relayed SMTP scans through the appliance):
  - **Boot fail-closed guard** (`config.validate`): the process refuses to start
    when `allowUnlistedPassthrough=true` *and* `sourceIpAllowlist` contains a `/0`
    (all-address) range — that combination is an open relay by definition. An
    empty `sourceIpAllowlist` denies all (safe); only an explicit `/0` trips it.
  - **SSRF guard on the downgrade** (`Checker.CheckPassthroughTarget`): because
    `CheckConnect` returns `ErrDomainDenied` *before* its private-IP check, the
    passthrough branch runs that check explicitly before relaying, so an unlisted
    `CONNECT` can never be tunneled to a private/reserved target (cloud metadata,
    RFC1918, loopback) — a `403 rejected_private_ip`.

  **Production posture:** `allowUnlistedPassthrough=false`, `sourceIpAllowlist`
  restricted to the internal/managed-device network, and `:3128` **never** exposed
  to `0.0.0.0/0` at the security-group / firewall layer (the appliance is reached
  by enrolled devices over internal paths, not the public internet).
- **Connection-stage hooks.** With only the target host known (pre-tunnel),
  connection-stage compliance hooks run and can block before the tunnel opens.

## 2. Connection lifecycle

`conn.Manager` (`packages/compliance-proxy/internal/proxy/conn`) reserves a slot
per accepted connection (`AcquireWithInfo` returns a connection ID), enforces a
maximum-concurrency limit, and tracks active connections for the runtime
`/connections` surface. Idle connections are wrapped with an idle timeout, and a
shutdown coordinator drains in-flight connections on graceful shutdown.

## 3. Tunnel establishment

`connect.EstablishTunnel` (`packages/compliance-proxy/internal/proxy/connect`)
hijacks the HTTP connection, writes `HTTP/1.1 200 Connection Established`, and
returns the raw `net.Conn` — preserving any TLS `ClientHello` bytes the HTTP
server already buffered so the handshake is not truncated.

## 4. The forward gate sequence

`forward.Run` (`packages/compliance-proxy/internal/proxy/forward`) decides what
to do with the established tunnel, in a fixed order:

```
forward.Run(conn)
  ├─ 1. kill switch engaged?      → tlsbump.PassThrough + audit (no inspection)
  ├─ 2. host pinning-exempt?      → tlsbump.PassThrough (no audit)
  ├─ 3. source+host hook-exempt?  → bump, but skip compliance hooks + audit "exempted"
  ├─ 4. build BumpOptions (identity, attestation verifier, compliance pipeline,
  │      source info, reject config, payload capture, domain engine,
  │      adapter registry, normalize registry, WithStrictFailClosed)
  ├─ 5. tlsbump.BumpConnection(...) → TLS interception + compliance pipeline
  └─    pinning error during bump? → record failure + tlsbump.PassThrough (fallback)
```

The proxy passes `tlsbump.WithStrictFailClosed()` in step 4 (SEC-W3-01). Because
the Compliance Proxy is a **dedicated forward-proxy appliance** that already
returns `403` for disallowed `CONNECT`s — i.e. it can safely refuse — an
admin-configured `failBehavior="fail-closed"` compliance hook that is *unbuildable*
(its `implementationId` is not registered in the running binary on a partial
deploy, or a malformed rule was pushed) makes `BuildPipeline` **error**, and the
request is **refused** rather than forwarded uninspected. This honours the admin's
"this scanner is mandatory" intent at *build* time, not just on runtime hook
errors. The agent NE proxy shares the same `tlsbump` code but does **not** set this
option: it is in the host outbound packet path where refusing would brick the
host's DNS/DHCP/networking, so it stays fail-open by design (M8 invariant). The
build-time gate itself lives in `shared/policy/pipeline` (`strictFailClosed`).

The kill-switch check is a lock-free `atomic.Bool` read on the hot path (the
proxy-side kill-switch state machine is in the runtime doc; the propagation
model is in
[kill-switch-architecture.md](../../cross-cutting/safety/kill-switch-architecture.md)).
Pinning exemptions and the pinning-failure fallback let traffic the proxy cannot
bump (certificate-pinned clients) pass through rather than break; the tracker is
the shared `tlsbump.PinningTracker`. Temporary hook exemptions (still bumped, but
skipping the compliance hooks) come from the exemption store described in the
runtime doc. The bump itself — TLS handshake, request/response hook execution,
streaming modes — is the shared `tlsbump` package.

## References

- `packages/compliance-proxy/internal/proxy/server/` — CONNECT entry, onboarding intercept, connection-stage hooks
- `packages/compliance-proxy/internal/access/` — IP / domain / private-IP access checks
- `packages/compliance-proxy/internal/proxy/conn/` — connection lifecycle, concurrency limit, shutdown drain
- `packages/compliance-proxy/internal/proxy/connect/` — tunnel hijack + `200 Connection Established`
- `packages/compliance-proxy/internal/proxy/forward/` — forward gate sequence, bump options
- `packages/shared/transport/tlsbump/` — shared MITM bump + pinning tracker driven by the forward path
