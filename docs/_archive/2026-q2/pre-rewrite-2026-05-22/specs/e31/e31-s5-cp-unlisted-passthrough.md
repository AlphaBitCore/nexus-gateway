# E31 S5 — Compliance proxy unlisted-domain passthrough toggle

**Epic:** 31
**Story:** 5
**Status:** Draft — 2026-04-27
**Requirements:** inline (operator convenience; no separate requirements doc)

## User Story

As an operator running the compliance proxy on a developer / lab machine, I want a deploy-time switch that downgrades the proxy from "strict allowlist gate" to "transparent forward proxy for unlisted domains", so a single host can use it as the system-wide HTTPS proxy without triggering `ERR_TUNNEL_CONNECTION_FAILED` on every non-AI domain.

The default stays the production-safe behavior (reject unlisted CONNECTs with 403). The flag is opt-in, lives in YAML (deployment posture, not runtime toggle), and emits a startup WARN so the downgraded mode is impossible to overlook in logs.

## Scope

In:
- New YAML field `accessControl.allowUnlistedPassthrough` (bool, default `false`).
- Listener: when the flag is `true` and `Checker.CheckConnect` returns `ErrDomainDenied`, hijack the connection, send `200 Connection Established`, and bidirectionally relay raw TCP via the existing `PassThrough` helper.
- Metric: pivot the existing `tunnels.total{result}` counter on a new label value `unlisted_passthrough`. No new instrument.
- Startup WARN log when the flag is enabled.
- Other rejection reasons (`ErrIPDenied`, `ErrPrivateIP`, `ErrSNIMismatch`) continue to return 403 unchanged — those are security gates, not allowlist misses.

Out:
- DB / shadow-driven runtime toggle (deferred; promote later if needed).
- Audit-event emission for unlisted passthrough (no decrypted traffic, nothing meaningful to record).
- ConnManager / ShutdownCoordinator tracking for unlisted passthrough connections (acceptable for dev posture; production deployments keep the flag off).

## Tasks

### T1. Config

- Add `AllowUnlistedPassthrough bool` to `AccessControlConfig` in `packages/compliance-proxy/internal/config/config.go`.
- Update `compliance-proxy.dev.yaml` with the new key and a comment referencing this SDD.

### T2. Listener

- Add `AllowUnlistedPassthrough` to `ProxyConfig` and `ProxyServer`.
- In `ServeHTTP`, when `Checker.CheckConnect` returns an error matching `access.ErrDomainDenied` and the flag is set, take the passthrough branch:
  - Increment `tunnels.total{result="unlisted_passthrough"}`.
  - `establishTunnel(w, r)` → wrap with `conn.NewIdleConn` if a positive idle timeout is configured → `PassThrough(ctx, tunnelConn, targetHost)` → close on exit.
  - Log at INFO with target host.
- All other access-control errors keep the existing 403 path.

### T3. Wiring

- `cmd/compliance-proxy/main.go` propagates `cfg.AccessControl.AllowUnlistedPassthrough` into `ProxyConfig`.
- Emit `slog.Warn("⚠️ unlisted-passthrough mode ENABLED — proxy is no longer a strict compliance gate")` once at startup when the flag is true.

### T4. Tests

- Unit tests in `packages/compliance-proxy/internal/proxy/listener_unlisted_passthrough_test.go`:
  - Flag off + unlisted host → 403 (regression).
  - Flag on + unlisted host → tunnel path entered (observable as 500 via the recorder-not-hijacker marker, mirroring existing connection-stage tests), counter incremented.
  - Flag on + IP-denied → 403 (gate not bypassed).
  - Flag on + listed host → standard accepted path (counter `accepted`, no `unlisted_passthrough`).

### T5. Verify

- `go test -race -count=1 ./packages/compliance-proxy/...` passes locally.
- Restart the local proxy. From an upstream client configured to use the proxy, `curl https://www.openai.com` succeeds (TCP relay, no MITM); `curl https://api.openai.com` continues to be intercepted (TLS bumped or kill-switch passthrough per the existing path).

## Acceptance Criteria

1. With `allowUnlistedPassthrough: false` (default), a CONNECT to a domain not in `interception_domain` returns 403 with body `connection denied: rejected_domain` — no behavior change from current production.
2. With `allowUnlistedPassthrough: true`, the same CONNECT returns `200 Connection Established` and the tunnel relays raw TCP to the target. Counter `tunnels.total{result="unlisted_passthrough"}` increments by one per such CONNECT.
3. With the flag on, IP-allowlist violations and private-IP rejections continue to return 403 — the flag does not bypass non-allowlist gates.
4. Startup logs at WARN that the proxy is in unlisted-passthrough mode whenever the flag is on.
5. CONNECTs to listed domains take the existing accepted path; behavior (TLS bump or kill-switch passthrough) is unchanged.
6. New/updated unit tests pass under `go test -race -count=1`.

## Risks

- **Compliance regression if enabled in production.** A misconfigured prod YAML would silently relay non-AI traffic without audit. Mitigations: default `false`, mandatory startup WARN, comment in `compliance-proxy.config.yaml` warning DEV ONLY, mention in any deployment docs that touch this flag.
- **No tunnel accounting for unlisted passthrough.** Active count, max-concurrent gate, and graceful-shutdown coordination are skipped on this branch. Acceptable for dev posture; production keeps the flag off so the gap never materializes.
