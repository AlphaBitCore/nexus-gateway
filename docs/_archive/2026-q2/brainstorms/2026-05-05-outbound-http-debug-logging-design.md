# Outbound HTTP Debug Logging — Design

**Date:** 2026-05-05
**Status:** Draft (awaiting user spec review)
**Owner:** Platform / shared
**Scope:** `shared/httpclient`, plus call-site labelling across `ai-gateway`, `compliance-proxy`, `agent`, `nexus-hub`, `control-plane`.

## 1. Problem

When something goes wrong with an outbound HTTP call — an LLM provider returns 401, the agent fails to reach the hub, the compliance proxy stalls on a CONNECT — there is currently **no uniform way to find that single request in the logs**. Each call site logs (or doesn't log) on its own; the only consistent breadcrumb is the Prometheus `UpstreamRequestMs` histogram, which tells you "something was slow" but not "which request, with what nexus-request-id, against what URL, returning what status".

We want every outbound HTTP call from every Go service to emit one structured debug-level log line with enough fields that a developer can:

1. grep a single `nexus_request_id` and follow the chain across services
2. see request size / response size / duration / HTTP status / error string per call
3. identify which logical caller made the call (`provider-upstream`, `agent-hub`, `cp-upstream`, …) without reverse-engineering stack traces

## 2. Non-goals

- Replacing or extending the `traffic_event` audit row (that is a business audit record, not a transport log).
- Inbound request logging (already handled by per-service middleware).
- WebSocket logging (`thingclient` primary path).
- OTel tracing / span export. Future work; orthogonal.
- Body-content logging. Only byte counts. Sensitive payloads stay out of logs.
- Test code paths. The lint rule that forbids bare `http.Client{}` in production already excludes tests; this spec keeps the same boundary.

## 3. Architectural decision (Q1)

**Wire the logging RoundTripper inside `shared/httpclient.New` and `httpclient.NewProbe`.** Default-on. Additionally export `httpclient.WrapTransport(base, opts) http.RoundTripper` for the one production call site (`compliance-proxy/internal/proxy.UpstreamTransport`) that builds its own `*http.Transport` and calls `transport.RoundTrip` directly.

Rejected alternatives:
- **Per-call-site `NewLoggingTransport(base, …)` only**: forces every existing call site to remember to wrap; new call sites silently lose logging.
- **OTel `otelhttp.NewTransport`**: doesn't deliver the "grep a log file" UX, requires collector wiring, adds dependency burden on the `shared` core tier.

## 4. Header propagation policy (Q2)

`x-nexus-request-id` is added to outbound headers **only when the constructing call site opts in** via `Config.PropagateReqID = true`.

- Calls to **external** providers (OpenAI, Anthropic, Bedrock, Vertex, Gemini, Cohere, Replicate, MiniMax, GLM, Azure OpenAI, agent → external upstream relay): `PropagateReqID = false`. Preserves the existing `provider-adapter` strip-header invariant (verified by `spec_adapter_test.go`).
- Calls to **our own** services (agent → hub, agent → control-plane, control-plane → hub, hub alerting webhooks the customer points at internal endpoints, …): `PropagateReqID = true`. The receiving service's inbound middleware reads the header and puts it back on its own `ctx`, so a single `nexus_request_id` survives the whole chain.

The wrapper **never overwrites** an `x-nexus-request-id` header the caller already set explicitly on the request.

The log line emits `nexus_request_id` regardless of `PropagateReqID` — propagation only controls the outbound header, not the local log line.

## 5. compliance-proxy MITM coverage (Q3)

`compliance-proxy/internal/proxy.UpstreamTransport` is migrated to call `httpclient.WrapTransport` on its `*http.Transport`. Every MITM-intercepted user request gets the same log schema as everything else, with `caller="cp-upstream"` and `PropagateReqID=false`. This is the highest-volume HTTP call path in the platform; it is acceptable at debug level because:

- production default log level is `info`, so this path is silent in production
- when an operator does flip a service to `debug`, they're explicitly opting into the volume during incident triage
- the wrapper short-circuits to the bare base transport when the logger is below `debug` level, so production overhead is one extra interface dispatch + the existing httptrace cost only when debug is on

No sampling. Sampling defeats the diagnostic purpose ("the one request I need is the one that didn't get sampled").

## 6. Log line schema (Q4)

One slog record per HTTP call.

- Level: `slog.LevelDebug` on success (any HTTP response received, including 4xx/5xx).
- Level: `slog.LevelWarn` when the transport itself failed (no response — dial / TLS / context-cancel / read-header timeout).
- Message: `"outbound http"` (constant — easy grep, easy filter).

Fields:

| key                | type   | always present? | source |
|--------------------|--------|-----------------|--------|
| `caller`           | string | yes             | `Config.Caller` (or `WrapOpts.Caller`); `"unknown"` if empty |
| `method`           | string | yes             | `req.Method` |
| `url`              | string | yes             | `req.URL.String()` (full, including query) |
| `host`             | string | yes             | `req.URL.Host` |
| `status`           | int    | yes             | `resp.StatusCode`; `0` on transport error |
| `req_bytes`        | int64  | yes             | see §7 |
| `resp_bytes`       | int64  | yes             | counted via response-body wrapper; finalised on `Close` |
| `duration_ms`      | int64  | yes             | `RoundTrip` start → response body `Close` (or to error point) |
| `nexus_request_id` | string | yes             | `httpclient.RequestIDFromContext(req.Context())`; `""` if absent |
| `attempt`          | int    | yes             | from a context value; defaults to `1` if unset (router retries can populate later) |
| `proto`            | string | success only    | `resp.Proto` (`"HTTP/2.0"` etc.) |
| `reused`           | bool   | success only    | `httptrace.GotConnInfo.Reused` |
| `err`              | string | error only      | `err.Error()` |

Headers are **never** logged. Bodies are **never** logged (only byte counts). URL query is preserved EXCEPT for values of known-sensitive parameter names (api_key, apikey, key, access_token, token, auth, authorization, signature, password, secret, …) which are replaced with `***`. The parameter NAMES are kept for diagnostic value. Spec amended 2026-05-06 after code review surfaced Gemini's `?key=...` API-key pattern — the original "internal calls only" rationale was too narrow once the wrapper is platform-wide. Allowlist lives at `packages/shared/transport/httpclient/logging.go:sensitiveQueryParams`.

## 7. Request body byte counting

Priority order, evaluated per request:

1. If `req.ContentLength > 0`, use it directly. (No allocation, no wrapping.)
2. Otherwise, if `req.Body != nil`, wrap it in a counting `io.ReadCloser` so byte count is exact regardless of chunked encoding.
3. Otherwise (GET / DELETE / no body), `req_bytes = 0`.

The wrapper **never** buffers the body in memory — it counts on the fly. Streaming uploads stay streaming.

## 8. Emit timing

- **Success path**: response body is wrapped in a counting `io.ReadCloser`. The slog record is emitted on `Close()`, when `resp_bytes` and `duration_ms` are final. Single line per request.
- **Transport-error path**: emit immediately at the `RoundTrip` return point with `status=0`, `resp_bytes=0`, the `err` string, and `duration_ms` measured from `RoundTrip` entry to error.
- **Caller forgets `Body.Close()`**: no log line. This is a Go correctness bug already (leaks the connection); transport-layer logging should not paper over it. Static analysis (existing `bodyclose` linter where present) is the right place to catch it.
- **Streaming / SSE**: handled by the success path. The line emits when the stream closes; `resp_bytes` is the total streamed. If "stream stalled" diagnostics become a real need, add an explicit `"outbound http stream started"` line later (deferred YAGNI; not in this spec).

### debug-disabled fast path

Before wrapping anything, the RoundTripper checks `logger.Enabled(req.Context(), slog.LevelDebug)`:

- If false: skip body counting, skip `httptrace`, skip wrapping `resp.Body`. Just propagate the request-id header (if `PropagateReqID`) and call the base transport. The transport-error branch still runs (errors must always log).
- If true: full instrumentation.

This keeps the production cost of "logging is off" down to one boolean check + an interface dispatch.

## 9. `shared/httpclient` API additions

```go
package httpclient

type Config struct {
    // …existing fields unchanged…

    // Logger receives the per-request debug record. nil → slog.Default().
    Logger *slog.Logger

    // Caller is the static identifier written to every log line as caller=…
    // Empty string → "unknown".
    Caller string

    // PropagateReqID, when true, causes the RoundTripper to copy
    // RequestIDFromContext(req.Context()) into the x-nexus-request-id
    // outbound header (only if the request does not already have one).
    PropagateReqID bool
}

// WrapTransport wraps base with the same logging RoundTripper that
// New uses internally. Production code that constructs its own
// *http.Transport (currently only compliance-proxy/internal/proxy)
// uses this entry point.
func WrapTransport(base http.RoundTripper, opts WrapOpts) http.RoundTripper

type WrapOpts struct {
    Logger         *slog.Logger
    Caller         string
    PropagateReqID bool
}

// WithRequestID returns a context carrying id. The companion
// RequestIDFromContext lookup is what the wrapper uses to populate
// the nexus_request_id log field and, when PropagateReqID is true,
// the x-nexus-request-id outbound header.
func WithRequestID(ctx context.Context, id string) context.Context
func RequestIDFromContext(ctx context.Context) string
```

`New` change: after constructing the base `*http.Transport`, wrap it with `WrapTransport(tr, WrapOpts{Logger: cfg.Logger, Caller: cfg.Caller, PropagateReqID: cfg.PropagateReqID})` before assigning to the returned `*http.Client`.

The context key is a private type inside `httpclient` — callers must use the helpers, not raw `context.WithValue`.

## 10. Per-service injection of `nexus_request_id` into context

Each service is responsible for putting the request ID on `ctx` at its inbound edge so downstream `httpclient` calls can read it:

| Service | Edge point | Action |
|---|---|---|
| ai-gateway | `internal/middleware.RequestID` | After setting the request header, also `r = r.WithContext(httpclient.WithRequestID(r.Context(), id))` |
| compliance-proxy | `internal/proxy` MITM dispatch (after CONNECT, before forwarding) | Generate-or-read request id, put on ctx |
| agent | `internal/proxy` (and any other inbound entry that issues outbound calls) | Same |
| nexus-hub | echo middleware | Same — adopt or extend existing request-id middleware |
| control-plane | echo middleware | Same |

Inbound HTTP middleware on the receiving side reads the `x-nexus-request-id` header (if present) and seeds `ctx` from it; otherwise it generates a fresh id. This part is already in place for ai-gateway; the others get a small middleware addition.

## 11. Caller / propagation table for every existing `httpclient.New` site

Applied during implementation; this table is the source of truth.

| Call site | Caller | PropagateReqID |
|---|---|---|
| `ai-gateway/internal/providers/specutil.NewHTTPClient` | `provider-upstream` | false |
| `ai-gateway/internal/providers/specutil.NewProbeClient` | `provider-probe` | false |
| `ai-gateway/internal/handler.NewUpstreamClient` | `ai-gateway-upstream` | false |
| `ai-gateway/internal/hooks/webhook` | `webhook-hook` | true |
| `ai-gateway/cmd/ai-gateway/main.go` aiguard ext client | `aiguard-external` | false |
| `ai-gateway/cmd/ai-gateway/main.go` webhookClient | `webhook-shared` | true |
| `compliance-proxy/internal/proxy.UpstreamTransport` (via `WrapTransport`) | `cp-upstream` | false |
| `compliance-proxy/internal/siem/sinks` | `cp-siem` | true |
| `agent/internal/hubhttp` | `agent-hub` | true |
| `agent/internal/audit/hub_client` | `agent-audit` | true |
| `agent/internal/configsync` | `agent-configsync` | true |
| `agent/internal/enrollment/hub_enroll` | `agent-enroll` | true |
| `agent/internal/auth/bootstrap` | `agent-auth-bootstrap` | true |
| `agent/internal/relay.Client` | `agent-relay` | false |
| `agent/internal/updater` | (reuses `agent-hub` client via `client.HTTPClient()` — no separate `httpclient.New`) | n/a |
| `nexus-hub/internal/alerting/senders/webhook` | `hub-alert-webhook` | true |
| `nexus-hub/internal/alerting/senders/pagerduty` | `hub-alert-pagerduty` | true |
| `nexus-hub/internal/alerting/senders/slack` | `hub-alert-slack` | true |
| `nexus-hub/internal/alertclient` | `hub-alertclient` | true |
| `nexus-hub/internal/siem/sink` | `hub-siem` | true |
| `control-plane/cmd/control-plane/main.go` hubHTTPC | `cp-hub-main` | true |
| `control-plane/cmd/control-plane/main.go` HubProxyClient | `cp-hub-proxy` | true |
| `control-plane/cmd/control-plane/main.go` ComplianceProxyClient | `cp-compliance-proxy-admin` | true |
| `control-plane/cmd/control-plane/main.go` dispatch HTTPClient | `cp-dispatch` | true |
| `control-plane/internal/handler/admin_settings` | `cp-admin-settings` | true |
| `control-plane/internal/handler/admin_routing` | `cp-admin-routing` | true |
| `control-plane/internal/handler/admin_proxy` | `cp-admin-proxy` | true |
| `control-plane/internal/handler/admin_siem` | `cp-admin-siem` | true |
| `control-plane/internal/handler/admin_aiguard` | `cp-admin-aiguard` | true |
| `control-plane/internal/handler/admin_hub_proxy` | `cp-admin-hub-proxy` | true |
| `control-plane/internal/handler/admin_alerts` | `cp-admin-alerts` | true |
| `control-plane/internal/handler/admin_extras` line 252 (provider register-test) | `cp-admin-extras-provider-test` | true |
| `control-plane/internal/handler/admin_extras` line 613 (hook test forward) | `cp-admin-extras-hook-test` | true |
| `control-plane/internal/handler/admin_extras` line 638 (`runWebhookHookTest`) | `cp-admin-extras-webhook-test` | true |
| `control-plane/internal/handler/admin_extras` line 1381 (hub healthz check) | `cp-admin-extras-hub-healthz` | true |
| `control-plane/internal/middleware/jwt` | `cp-jwt-middleware` | true |
| `control-plane/internal/hubclient` | `cp-hubclient` | true |
| `control-plane/internal/jwtverifier/jwks` | `cp-jwt-jwks` | true |
| `control-plane/internal/jwtverifier/mqrevocation` | `cp-jwt-mqrevocation` | true |
| `shared/thingclient/http` | `thingclient-http` | true |

Any call site not in the table that is added later defaults to `caller="unknown"`, `PropagateReqID=false`. CI lint check: a `golangci-lint` `forbidigo` rule rejects `httpclient.New(httpclient.Config{` literal that omits `Caller:`.

## 12. Testing

`shared/httpclient/`:

- Table-driven `TestRoundTripper`:
  - 200, 4xx, 5xx → debug-level record with correct fields and no `err`
  - dial error / TLS error / context cancel / response-header timeout → warn-level record with `status=0` and an `err` string
  - body counting:
    - request with `Content-Length` set → uses Content-Length
    - request with chunked body → counted via reader wrap
    - GET no body → `req_bytes=0`
    - response body counted across multiple `Read` calls; `Close` after last `Read` finalises count
    - `Close` called twice → does not double-count
    - response body never read, only closed → `resp_bytes=0`
- `TestPropagateReqID`:
  - `PropagateReqID=true`, ctx has id → outbound `x-nexus-request-id` set
  - `PropagateReqID=true`, request already has the header → not overwritten
  - `PropagateReqID=false` → header never added
  - `PropagateReqID=true`, no id in ctx → header never added (don't propagate empty)
- `TestDebugDisabled`:
  - logger configured to info-only → response body NOT wrapped (use a marker `io.Reader` to assert it sees the raw `Read` call)
  - logger configured to info-only → transport error STILL logs at warn
- `TestWrapTransport_StandalonePath`:
  - drives `WrapTransport(base, opts)` directly without going through `New`; confirms identical behaviour

Per-caller smoke tests: existing call-site unit tests must continue to pass with no behaviour change other than the new log lines appearing on debug.

## 13. Rollout

Per the project's pre-GA / no-backcompat policy:

- No feature flag, no env toggle. The success-path level is fixed at `slog.LevelDebug` and the error-path level is fixed at `slog.LevelWarn`. The operational on/off switch is the slog Handler's level (already configurable per service via `*.dev.yaml` `log` section). There is intentionally **no** `Config.LogLevel` field — per-client level overrides would fragment the operator's mental model.
- `compliance-proxy/internal/proxy.NewUpstreamTransport` is **rewritten** to use `httpclient.WrapTransport`. The previous "bare `*http.Transport` + direct `transport.RoundTrip`" code is deleted in the same change. No wrapper, no shim, no parallel path.
- All `httpclient.New(...)` call sites in §11 are updated in the same change. No incremental "Phase 1 = shared package, Phase 2 = call sites" — that would leave half the platform with `caller="unknown"`.
- Documentation: a short note in `docs/dev/conventions.md` (or wherever current outbound HTTP guidance lives) pointing at the new fields and the lint rule.

## 14. Implementation order

This is the implementation sequencing for the writing-plans step — **not** a phased rollout in the compatibility sense.

1. `shared/httpclient`: add `Config.Logger/Caller/PropagateReqID`, add `WrapTransport`/`WrapOpts`, add `WithRequestID`/`RequestIDFromContext`, add the RoundTripper itself. Tests green.
2. `shared/httpclient`: add a small custom analyzer (or, if cheaper, a regex-based pre-commit grep) that rejects `httpclient.New(httpclient.Config{` literals omitting `Caller:`. `forbidigo` cannot match field presence, so the analyzer is the path; if its cost outweighs its value during implementation, the fallback decision is "PR review catches it" and the analyzer is dropped. Document the decision in the PR.
3. Per-service edge middleware to seed `httpclient.WithRequestID(ctx, id)`.
4. Update all call sites in §11 to populate `Caller` and `PropagateReqID`. (Bulk mechanical edit.)
5. Rewrite `compliance-proxy/internal/proxy.NewUpstreamTransport` to use `httpclient.WrapTransport`; delete the now-redundant transport construction code.
6. `go test -race -count=1 ./...` per affected module; manual smoke: tail one service's log at debug level, hit the relevant endpoint, confirm one log line per outbound call with correct `nexus_request_id`.

## 15. Open questions

None. Q1–Q4 are locked.
