# Agent Transport Rewrite — Design Specification

**Date:** 2026-04-26
**Status:** Approved (rewritten 2026-04-26 after Trinity topology clarification)
**Pairs with:** `docs/superpowers/specs/2026-04-26-dataplane-perf-redesign.md` §4.6
**Pairs with:** `docs/superpowers/decision-logs/2026-04-26-dataplane-perf-redesign.md` §5

> **Revision note.** The first draft of this spec assumed the agent
> forwards intercepted flows to an upstream gateway (compliance-proxy or
> ai-gateway), and built a two-pool model around that. That assumption
> was wrong. Under the Trinity data-plane model the three
> services are **parallel**, not chained: the agent terminates at the
> provider origin directly, runs hooks locally, and is unaware of what
> sits between its TLS socket and the provider. Network-level
> redirection (DNS rewrite, PAC, transparent proxy) — when configured by
> ops — sends agent bytes to compliance-proxy or elsewhere transparently;
> the agent does not negotiate that. This rewrite drops the two-pool /
> whitelist / `relaybackend` / `relaytls` machinery and collapses to a
> single shared `*http.Client` with per-host HTTP/2 pooling.

## 1. Overview

The desktop agent (`packages/agent`) intercepts client TLS traffic on
the local box via platform-specific mechanisms (Network Extension on
macOS, WFP on Windows, iptables on Linux), performs a TLS MITM, runs
the local compliance hook pipeline (request + response, with SSE / JSON
buffering), and forwards the bytes to the **original destination
(SNI/Host)**. The relay is implemented in
`packages/agent/core/network/proxy/proxy.go MITMRelay` and today calls
`tls.DialWithDialer(...)` **once per intercepted flow** — there is no
connection reuse. Six other call sites in `packages/agent/`
(auth/bootstrap, enrollment, audit/hub_client, hubhttp, configsync,
event uploaders) construct bare `&http.Client{...}` literals.

For an agent that may see thousands of short flows per minute (e.g. a
chatty IDE plugin firing many requests at `api.openai.com`) the per-flow
TLS handshake is wasted work, and the fragmented client construction
makes pool tuning impossible. This spec replaces both layers with a
single process-lifetime `*http.Client` (per surface) backed by
`packages/shared/transport/httpclient`, while keeping the existing MITM trust
chain on the client-facing side untouched.

## 2. Trinity Topology Premise

This spec assumes — and depends on — the canonical Trinity topology
documented in `docs/dev/architecture.md`:

- The agent **connects to Hub only** (WebSocket + HTTP). Outbound
  traffic flows toward the original SNI/Host the client requested. The
  agent does not know whether the IP the kernel resolves that hostname
  to belongs to the real provider, the customer's compliance-proxy, or
  any other middlebox. **Topology is a network-layer concern, not an
  agent-layer concern.**
- Customers can buy any combination of {agent, compliance-proxy,
  ai-gateway}. When all three are deployed and DNS is configured to
  send agent flows through compliance-proxy, the agent's transport
  behavior must remain identical to the standalone-agent case. The
  rewrite preserves this property.
- The agent's **local** hook pipeline runs regardless of what is
  downstream. There is no "skip hooks because the gateway will run
  them" path. This was an explicit Trinity design choice (each data
  plane is self-sufficient and survives the others being absent).

Anything in this spec that would couple the agent to a specific
downstream identity (gateway URL, mTLS to gateway, host-specific
routing decisions baked into agent config) is **out of scope** and
explicitly forbidden.

## 3. Key Decisions

| # | Decision | Choice | Rationale |
|---|----------|--------|-----------|
| 1 | Hot-path transport | One process-lifetime `*http.Client` constructed via `httpclient.New(httpclient.Config{ForceHTTP2: On(), H2ReadIdleTimeout: 30*time.Second, ...})` for the MITM relay outbound side | Per-host HTTP/2 multiplex absorbs concurrency to a single provider host; the small per-host idle pool absorbs serial bursts. PING-frame health (30s) heals dead connections without manual probing. |
| 2 | Two-pool / whitelist / `relaybackend` / `relaytls` packages from the prior draft | **Removed** | The premise was a "gateway hot path + origin escape" split. With Trinity-parallel topology there is no gateway path from the agent. One pool, no router. |
| 3 | MITM trust chain (client-facing) | Unchanged | The agent's locally-generated CA continues to mint mimic leaf certs for the client-facing TLS handshake (`packages/agent/core/cert/`, `agent/internal/proxy/proxy.go:307–322`). The change is exclusively on the outbound side of the relay. |
| 4 | Streaming (SSE, NDJSON) over HTTP/2 | Native | HTTP/2 carries streaming bodies as one logical stream over a multiplexed connection. The existing `streaming.Accumulator` consumes `io.Reader` so no API change. The 10 MB body cap and SSE accumulator behaviour stay identical. |
| 5 | Multiple destination hosts (different providers) | Single `*http.Client` with one transport; per-host pool sized by `MaxIdleConnsPerHost = 8`, `MaxConnsPerHost = 0` (uncapped) | When the agent talks to many providers the same transport keeps an independent H2 connection per host. Cross-host fairness is unaffected. |
| 6 | Existing `MITMRelay` outer signature | Preserved (callers unaffected) | `MITMRelay` keeps its public signature; only its outbound side swaps from `tls.Dial` + manual byte relay to `http.Client.Do` + body copy. The interception shims (`agent/internal/intercept/`) do not change. |
| 7 | Client-side connection from interceptor → MITMRelay | Unchanged | Local TCP loopback; per-flow accept is fine. |
| 8 | Non-HTTP / parse-failure fallback | A **single, un-pooled** `tls.DialWithDialer` per flow on the fallback path only (today's behaviour) — preserved as a guarded fallback | `MITMRelay` already falls back to byte-level `Relay(clientTLS, serverTLS)` at `proxy.go:337` when the first request cannot be parsed (HTTP/0.9, raw TCP-over-TLS, malformed clients). With the rewrite, this branch retains its own one-shot raw-TLS dial; it is rare, never pooled, and never affects the hot path. |
| 9 | Five Hub-bound bare-client call sites | Replace each with `httpclient.New(...)`; pool sizes vary by call frequency | These already hold long-lived connections to Hub. Switching to the shared factory gives consistent timeouts, H2 negotiation, and lint compliance for free. |
| 10 | Hub enrollment (mutual-TLS bootstrap) | Use `httpclient.New` plus a small helper that mutates the resulting transport's `TLSClientConfig.Certificates` once at startup | The bootstrap path needs a client cert today. Encapsulating the cast in `internal/relay/withclientcert.go` keeps direct-Transport access localized. |
| 11 | Per-host metrics | Add `nexus_agent_relay_dial_total{host, mode="reused\|new"}` counter. Stream-count metric (`nexus_agent_relay_streams_total{host}`) shape — counter vs gauge — is left to plan/implementation; see §9. | Proves the rewrite landed: a healthy steady-state shows `mode="reused" >> mode="new"`. `mode="reused"` increments when a request multiplexes onto an existing connection (H2) or pulls from the idle pool (H1); `mode="new"` increments on a fresh TLS handshake. |
| 12 | New agent config / shadow keys | **None** | The rewrite is fully internal. No new fields in agent yaml, no new shadow categories, no new SQLite columns. Tunables are code constants in `internal/relay/`. |
| 13 | Backwards compatibility | None | Pre-GA; the rewrite is the only design. The old per-flow `tls.Dial` is removed in the same PR, except for the fallback path documented in Decision 8. |

## 4. Scope

### In Scope

- New small package `packages/agent/core/network/relay`:
  - `RelayClient` — singleton wrapping the `*http.Client` constructed via `httpclient.New`.
  - `WithClientCert(c *http.Client, cert tls.Certificate) error` helper for the mTLS bootstrap path (Decision 10).
- Rewrite `packages/agent/core/network/proxy/proxy.go MITMRelay` outbound side to consume `RelayClient` instead of `tls.DialWithDialer`. Public signature preserved.
- Preserve and isolate the byte-level `Relay()` fallback path with its own one-shot `tls.Dial` (Decision 8).
- Replace bare `&http.Client{...}` and field-style equivalents in:
  - `packages/agent/core/security/auth/bootstrap.go:80`
  - `packages/agent/core/security/enrollment/hub_enroll.go:85`
  - `packages/agent/core/observability/audit/hub_client.go:27`
  - `packages/agent/core/sync/hubhttp/client.go:179`
  - `packages/agent/core/sync/configsync/manager.go:82`
- New Prometheus metrics:
  - `nexus_agent_relay_dial_total{host, mode}` (counter)
  - `nexus_agent_relay_streams_total{host}` (counter or gauge — pinned in plan; see §9)
- Enable `forbidigo` lint in `packages/agent/.golangci.yml` and remove the existing TODO note that referenced the prior draft of this spec.
- Update relay unit tests for the H2 fast path and the byte-level fallback path.

### Out of Scope

- **Client-side MITM TLS trust chain.** The agent CA generation and mimic leaf issuance (`agent/internal/cert/`) is untouched; tests for it run unchanged.
- **Compliance-proxy's transparent TLS proxy / CONNECT path.** Separate service, separate spec if needed.
- **Hub-side connection-handling changes.** Hub already serves both H1 and H2; no changes needed.
- **Switching the agent's local proxy listener from raw TCP+SNI peek to a full HTTP listener.** The existing approach intercepts before TLS is established and is necessary to make the local cert authority work; it stays.
- **Any agent → ai-gateway forwarding feature.** Per Trinity topology, customers needing centralized routing point their SDKs at ai-gateway directly, or ops uses DNS / network redirection. Adding agent-driven forwarding would be a separate product feature with its own spec; this spec explicitly does not enable it.
- **Whitelist / per-host routing config.** Removed (Decision 2).

## 5. Architecture

### 5.1 Single-pool model

```
┌──────────────┐              ┌──────────────────────────────┐
│  Local app   │              │   Agent (MITM relay)          │
│ (browser/SDK)│ TCP+TLS      │                                │
│   on host    │─────────────▶│   ┌────────────────────────┐  │
└──────────────┘              │   │ packages/agent/core│  │
                              │   │ /relay.RelayClient     │  │
                              │   │  (httpclient.New)      │  │
                              │   │  per-host H2 pool      │  │
                              │   └────────────┬───────────┘  │
                              └────────────────┼──────────────┘
                                               │  TLS to original SNI/Host
                                               ▼
                          (whatever the kernel's DNS resolution + any
                           network-layer redirection sends it to —
                           real provider, compliance-proxy, etc.)
```

The agent itself does not branch on destination. There is one client,
one transport, one pool. Fan-out happens implicitly inside
`http.Transport` per host.

### 5.2 `MITMRelay` outbound rewrite

**Today** (proxy.go:284–325, abbreviated):

```go
func MITMRelay(...) (...) {
    serverTLS, err := tls.DialWithDialer(
        &net.Dialer{Timeout: 10*time.Second},
        "tcp", net.JoinHostPort(dstHost, strconv.Itoa(dstPort)),
        &tls.Config{ServerName: dstHost},
    )
    // ... write request, copy response back ...
}
```

**After** (sketch):

```go
// internal/relay package owns the singleton client and metric labels.
type RelayClient struct {
    client *http.Client
    dials  *prometheus.CounterVec   // {host, mode}
}

func (rc *RelayClient) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
    // Stamp X-Nexus-Trace-ID (existing behaviour).
    // After the call returns, inspect the underlying connection state via
    // an http.RoundTripper wrapper to label "reused" vs "new" on the
    // dials counter. (See §5.3 for the wrapper.)
    return rc.client.Do(req.WithContext(ctx))
}

// MITMRelay path (simplified):
//   1. Read peeked client TLS hello, perform client-side handshake with
//      mimic cert (unchanged).
//   2. Parse first HTTP request from the decrypted stream (existing
//      inspectFirstRequest).
//   3. Convert it into an outbound *http.Request whose URL = "https://"
//      + dstHost + ":" + strconv.Itoa(dstPort) + path. Copy headers and
//      body.
//   4. RelayClient.Do(ctx, req).
//   5. Pipe response back through inspectFirstResponse + the existing
//      streaming.Accumulator path. No body-size or SSE behaviour changes.
//   6. On parse failure at step 2, fall back to the existing
//      Relay(clientTLS, serverTLS) byte-level loop. That fallback dials
//      its own one-shot raw TLS connection (Decision 8), unpooled.
```

The public signature of `MITMRelay` (the function in `proxy.go:284`) is
preserved. Callers in `agent/internal/intercept/` are unaffected.

### 5.3 Reused-vs-new dial accounting

`http.Transport` does not directly expose "did this Do() reuse a
connection" outside of `httptrace.ClientTrace`. The implementation
attaches a `httptrace.ClientTrace` to each outbound request inside
`RelayClient.Do`:

```go
trace := &httptrace.ClientTrace{
    GotConn: func(info httptrace.GotConnInfo) {
        mode := "reused"
        if !info.Reused {
            mode = "new"
        }
        rc.dials.WithLabelValues(host, mode).Inc()
    },
}
ctx = httptrace.WithClientTrace(ctx, trace)
```

This is a stdlib mechanism, no extra dependency, and works equally for
H1 and H2.

### 5.4 Bare-client cleanups

For each of the five agent sub-packages with a bare client today, the
replacement is:

```go
client := httpclient.New(httpclient.Config{
    Timeout:             /* per-callsite, see below */,
    MaxIdleConnsPerHost: 5,            // Hub-bound; small enough
    IdleConnTimeout:     90 * time.Second,
    ForceHTTP2:          httpclient.On(),
    H2ReadIdleTimeout:   30 * time.Second,
})
```

Per-callsite `Timeout` values follow the existing literals (e.g. 30s
for audit, 10s for enrollment); they are not changed. Pool sizes are
small because Hub is the only host these clients touch.

### 5.5 mTLS bootstrap helper

The Hub-enrollment client needs a client certificate. The existing
inline `&tls.Config{Certificates: ...}` is replaced with:

```go
// internal/relay/withclientcert.go
func WithClientCert(c *http.Client, cert tls.Certificate) error {
    tr, ok := c.Transport.(*http.Transport)
    if !ok {
        return errors.New("relay: client transport is not *http.Transport")
    }
    if tr.TLSClientConfig == nil {
        tr.TLSClientConfig = &tls.Config{}
    }
    tr.TLSClientConfig.Certificates = []tls.Certificate{cert}
    // The transport must be reset for the new TLSClientConfig to take
    // effect on the next dial.
    tr.CloseIdleConnections()
    return nil
}
```

Called once at enrollment startup, **before** the first outbound
request on the client. `httpclient.New` invokes
`http2.ConfigureTransports(tr)` during construction, which means the
H2 transport already references `tr.TLSClientConfig` — mutating its
`Certificates` slice and then calling `tr.CloseIdleConnections()`
forces the next dial (H1 or H2) to read the updated config. The
helper is unsafe to call after concurrent dials are in flight; the
enrollment bootstrap path is single-threaded at startup so this
restriction is naturally satisfied. This is the **only** place in
agent code allowed to type-assert on `*http.Transport`; lint
exemptions on this file are documented in `.golangci.yml` with a
one-line comment referencing this spec section.

### 5.6 Lint enablement

`packages/agent/.golangci.yml` flips on `forbidigo` with the same rule
set as the other agent-adjacent modules:

```yaml
linters:
  enable:
    - forbidigo
linters-settings:
  forbidigo:
    forbid:
      - p: '^http\.DefaultClient$'
        msg: "use packages/shared/transport/httpclient.New(...) instead"
      - p: 'http\.Client\{'
        msg: "use packages/shared/transport/httpclient.New(...) instead"
```

The pre-existing TODO note in `.golangci.yml` referencing the prior
draft of this spec is removed in the same commit that lands the
rewrite.

## 6. Implementation Sequence

1. `packages/agent/core/network/relay` package: `RelayClient` + `WithClientCert` + unit tests.
2. Rewrite `MITMRelay` outbound to use `RelayClient`; preserve byte-level `Relay()` fallback with its own one-shot dial.
3. Replace bare clients in the five Hub-bound sub-packages.
4. Wire `WithClientCert` into the enrollment bootstrap call site.
5. Enable `forbidigo` in agent's `.golangci.yml`; delete the prior TODO.
6. Add the new Prometheus counter and gauge; register them on the agent's existing metrics registry.
7. Integration smoke test: run agent against a local echo upstream with TLS; verify `nexus_agent_relay_dial_total{mode="reused"}` increments on subsequent flows after the first.

## 7. Test Plan

### 7.1 Unit tests

- `RelayClient.Do` happy path: `httptest.NewTLSServer` → assert response body forwarded byte-identical, headers preserved, `X-Nexus-Trace-ID` stamped.
- `RelayClient.Do` reuse accounting: drive 10 sequential requests at the same host; assert `dials{mode="new"} == 1` and `dials{mode="reused"} == 9`.
- `RelayClient.Do` separate-host accounting: drive 5 requests each at host A and host B; assert one new connection per host.
- `WithClientCert`: returns error when transport is not `*http.Transport`; on success, the next dial presents the cert (asserted via a custom server `VerifyPeerCertificate`).
- `MITMRelay` byte-level fallback: client opens TLS and writes a non-HTTP byte sequence; assert `Relay()` is reached, fallback dial is performed, no `RelayClient.Do` call is made (verified via mock counter).
- `MITMRelay` H2 path: client makes a normal HTTPS GET; assert `RelayClient.Do` was called once and response was relayed.
- `inspectFirstResponse` SSE accumulator: behaviour unchanged from today; regression-test the existing accumulator over the new outbound path.

### 7.2 Integration tests (`packages/agent/core/network/proxy/integration_test.go`)

- Spin up an in-process HTTPS echo server. Drive 100 sequential agent flows. Assert handshake count on the server ≈ 1 (H2 multiplex), not 100.
- Repeat with 50 concurrent flows. Assert handshake count stays small (1–2 depending on H2 max concurrent streams, stdlib default 100).
- Drive a flow against a non-HTTPS server (plain TCP + TLS but no HTTP layer). Assert the byte-level fallback runs and bytes are echoed.

### 7.3 Manual verification (executed before reporting done)

- Start agent locally, point a curl-via-agent flow at `https://api.openai.com/v1/models` (or any reachable HTTPS endpoint) 50 times.
- Curl the agent metrics endpoint; assert `nexus_agent_relay_dial_total{mode="reused"} >= 49`.
- Restart only the upstream simulator; the agent's connection drops; the next request must reconnect cleanly. The PING idle-timeout typically does not fire in this test (the server hangs up on shutdown); the test verifies the request-error → reconnect path works.
- Confirm the `streams` metric (shape pinned in plan per §9) shows behaviour consistent with H2 stream activity (monotonic-non-decreasing if counter; non-monotonic if gauge).

## 8. Risks & Mitigations

| Risk | Mitigation |
|---|---|
| H2 connection pinned to one TLS connection caps server-side fairness when the agent handles bursty large uploads to one provider | `MaxConnsPerHost = 0` (uncapped) lets the transport open additional connections under stream-concurrency pressure. H2 stream-concurrency cap defaults to 100 streams/connection in `golang.org/x/net/http2`. |
| Long-idle H2 connections silently die behind aggressive corporate firewalls | `H2ReadIdleTimeout: 30s` triggers a PING; failure-detection is sub-second; the transport reconnects transparently on the next request. |
| Customer DNS sends agent traffic to compliance-proxy, and compliance-proxy presents a mimic cert that the agent's stdlib TLS layer rejects | The agent already ships with the compliance-proxy CA in its trust store under the all-three-bought deployment (existing operational requirement; not new). The transport rewrite preserves the existing `tls.Config` semantics on the outbound dial — no change in trust behaviour. |
| Client-side MITM cert chain churn during the rewrite | The MITM cert generation lives in a separate path (`agent/internal/cert/`) untouched by this spec; tests for it run unchanged. |
| `WithClientCert` introduces a transport cast that breaks if `httpclient.New` later returns a different transport type | Encapsulated in one file; if the helper breaks at compile time after a future `httpclient` refactor, the failure is localized and obvious. The cast is documented and lint-exempted at that one site. |
| Byte-level fallback path is rarely exercised in CI and could regress silently | Add a dedicated unit test that drives a non-HTTP byte sequence through `MITMRelay`, asserting the fallback dial happens. Run it on every push (covered in §7.1). |

## 9. Open Items (deferred to plan/implementation)

- Whether `nexus_agent_relay_streams_total` should be a counter (cumulative) or a gauge (currently active). Counter is simpler; gauge is more diagnostic. Default to counter unless the implementation surfaces a clean way to track active streams without `httptrace` polling.
- Should the relay client carry an explicit `User-Agent: nexus-agent/<version>` header so downstream logs (provider or compliance-proxy) can see the agent version? Recommend yes; trivial to add. Defer to plan.
- Per-host pool tuning. Defaults (`MaxIdleConnsPerHost = 8`) are a guess; load-test data will refine. No config knob exposed initially.
