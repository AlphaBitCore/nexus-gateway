# E40-S2 — Proxy CA Management Endpoint + Shadow `managementURL`

> Story: e40-s2
> Epic: 40 (Setup Guide)
> Status: Approved

## User Story

As an IT Admin or Control Plane service, I want to download the proxy Sub-CA
certificate in PEM format from a dedicated management endpoint on the compliance
proxy, so that the Control Plane can relay it to the CP-UI Setup Guide page
without ever storing the certificate in the application database or exposing
the CA private key.

---

## Background

The compliance proxy Sub-CA certificate is the trust anchor that client devices
must install before TLS interception succeeds. Today there is no programmatic
way to retrieve the public certificate; IT Admins must copy it from the proxy
filesystem manually. S2 exposes a single `GET /management/ca-cert` endpoint on
the proxy's existing health/metrics HTTP server and reports the base URL of
that server (`managementURL`) into the Hub Thing shadow reported state so the
Control Plane can discover it without static configuration.

The CA private key never leaves the proxy process. Only the public PEM is served.

---

## Scope

### In

- New route `GET /management/ca-cert` registered on `healthHandler` (the
  existing `*health.Handler` mux in `cmd/compliance-proxy/main.go`).
- Handler reads the PEM from `cert.Issuer.CACertPEM()` (a new method to add)
  and writes it as `Content-Type: application/x-pem-file`.
- If the `*cert.Issuer` is nil at request time (startup race or KMS error),
  handler returns 503.
- `managementURL` is derived from the existing `MetricsConfig` fields
  (`advertiseHost` + `Address` port) and reported via the thingclient shadow
  reported state once after startup.
- `thingclient.Config` gains no new fields; the existing mechanism for
  registering extra reported metadata (used for `metricsUrl`) is reused or
  extended to carry `managementURL`.

### Out

- No new authentication on `GET /management/ca-cert` beyond network-level
  access (the management port is internal and should not be publicly exposed).
  The same posture applies to `/metrics` and `/health` today.
- No CA private key endpoint under any path.
- No database schema changes.

---

## Tasks

### T1. Add `CACertPEM()` to `cert.Issuer`

File: `packages/compliance-proxy/internal/cert/issuer.go`

Add a method that returns the Sub-CA certificate in PEM format:

```go
// CACertPEM returns the CA certificate in PEM-encoded format.
// The returned slice is safe to write directly to an HTTP response body.
func (i *Issuer) CACertPEM() []byte {
    return pem.EncodeToMemory(&pem.Block{
        Type:  "CERTIFICATE",
        Bytes: i.caCert.Raw,
    })
}
```

The `i.caCert` field is already populated by `NewIssuer` and
`NewIssuerWithRemoteSigner` before either constructor returns, so there is no
race on this field after construction.

### T2. Register `GET /management/ca-cert` on `healthHandler`

File: `packages/compliance-proxy/cmd/compliance-proxy/main.go`

After the `issuer` is constructed (line ~107, `certResult, err :=
initCertIssuer(...)`) and before the health server is started (~line 630),
register the new handler on `healthHandler`:

```go
// CA certificate endpoint: serves the proxy Sub-CA PEM for the CP-UI
// Setup Guide relay (E40). The private key is never exposed.
// issuerForHandler is captured by closure; nil if cert init failed.
issuerForHandler := certResult.Issuer // *cert.Issuer, may be nil on KMS error
healthHandler.HandleFunc("/management/ca-cert", func(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet {
        http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
        return
    }
    if issuerForHandler == nil {
        http.Error(w, "CA not loaded", http.StatusServiceUnavailable)
        return
    }
    pemBytes := issuerForHandler.CACertPEM()
    w.Header().Set("Content-Type", "application/x-pem-file")
    w.Header().Set("Content-Disposition", `attachment; filename="nexus-proxy-ca.crt"`)
    w.Header().Set("Cache-Control", "no-store")
    w.WriteHeader(http.StatusOK)
    _, _ = w.Write(pemBytes)
})
```

Note: `healthHandler` is a `*health.Handler` which embeds or wraps
`http.ServeMux`. Use the existing `healthHandler.Handle` or
`healthHandler.HandleFunc` pattern already used for `/debug/runtime` and
similar mounts in `main.go`.

### T3. Derive and report `managementURL` in Hub shadow

File: `packages/compliance-proxy/cmd/compliance-proxy/main.go`

**Derive the URL.** The management server binds on `cfg.Metrics.Address`
(e.g. `:9090` or `0.0.0.0:9090`). The advertise host comes from
`cfg.Metrics.AdvertiseHost` (or defaults to `127.0.0.1` when empty), exactly
as the existing `composeMetricsURL` helper does. Add a parallel
`composeManagementURL` helper (or extend `composeMetricsURL` to accept a port
override) to build:

```
http://{advertiseHost}:{managementPort}
```

where `managementPort` is the port from `cfg.Metrics.Address`.

The management server shares the same bind address as the metrics server in
the current design (both on `healthHandler`). The `managementURL` is therefore
the same base as `metricsUrl` — e.g. `http://10.0.1.5:9090`.

**Report into shadow.** After `thingClient` is constructed and before
`tc.Start()` is called, push the derived URL as a one-time reported-state
entry. Use the existing thingclient `Config.ExtraReported` mechanism if
present, or call `tc.SetReportedField("managementURL", managementURL)` (add
this method to `thingclient.Client` if it does not exist yet). The reported
state is pushed on the initial connection handshake so Hub stores it in
`thing.reported`.

Implementation note: check whether the thingclient already carries a map of
static reported fields (similar to how `metricsUrl` is sent as a query
parameter on registration). If the field can be sent as part of the
registration query (`?managementURL=...`) rather than a separate report call,
use that approach for consistency. In either case the field must appear in
`thing.reported` (JSON column in Hub's `thing` table) within 5 seconds of
startup.

### T4. Add `managementHost` config field

File: `packages/compliance-proxy/internal/config/config.go`

Add `ManagementHost string `yaml:"managementHost"`` to `MetricsConfig`:

```go
// MetricsConfig defines the address for the health/metrics HTTP server
// and the host Hub uses to reach it.
type MetricsConfig struct {
    Address        string `yaml:"address"`
    AdvertiseHost  string `yaml:"advertiseHost"`
    // ManagementHost, when set, overrides AdvertiseHost for the managementURL
    // reported into the Hub shadow. Useful when the management port is behind
    // a different load balancer or hostname than the metrics port.
    // Defaults to AdvertiseHost when empty.
    ManagementHost string `yaml:"managementHost"`
}
```

`composeManagementURL` prefers `ManagementHost` over `AdvertiseHost` so
operators can override the management URL independently of the metrics URL.

### T5. Unit tests

File: `packages/compliance-proxy/internal/cert/issuer_test.go` (extend
existing) and `packages/compliance-proxy/cmd/compliance-proxy/main_test.go`
(or a new `management_endpoint_test.go` under the `cmd` package).

**T5a — `CACertPEM()` round-trip:**

Extend the existing `TestNewIssuer` test to call `issuer.CACertPEM()`,
decode the returned PEM block, and assert:
- `block.Type == "CERTIFICATE"`
- `block.Bytes` equals `issuer.caCert.Raw`

**T5b — Handler returns correct PEM:**

Construct an `httptest.Server` serving the `GET /management/ca-cert` handler
with a real `*cert.Issuer` (use the test CA from `issuer_test.go`). Issue a
`GET /management/ca-cert` request and assert:
- Status 200
- `Content-Type: application/x-pem-file`
- Body is valid PEM that decodes to the same certificate as the issuer's
  `caCert`.

**T5c — Handler returns 503 when Issuer is nil:**

Register the handler with a `nil` issuer pointer. Issue `GET /management/ca-cert`
and assert status 503.

---

## Acceptance Criteria

1. **AC1 — PEM response:** `GET /management/ca-cert` on a running compliance
   proxy returns HTTP 200 with `Content-Type: application/x-pem-file` and a
   body that is a valid PEM-encoded X.509 certificate matching the proxy's
   Sub-CA.

2. **AC2 — 503 when not loaded:** `GET /management/ca-cert` returns HTTP 503
   when the `*cert.Issuer` is nil (CA not yet initialised or KMS error on
   startup).

3. **AC3 — Key never exposed:** No HTTP endpoint on any port of the compliance
   proxy returns PEM data for the private key. The endpoint serves only the
   public certificate bytes.

4. **AC4 — `managementURL` in shadow:** Within 5 seconds of startup, the Hub
   thing record for the compliance proxy (`thing.reported`) contains a
   `managementURL` field with the value
   `http://{advertiseHost}:{managementPort}` (or the `managementHost`
   override). Verified by querying `SELECT reported FROM thing WHERE id = $1`
   in the dev DB.

5. **AC5 — Unit tests pass:** `go test -race -count=1
   ./packages/compliance-proxy/...` is green with the new test cases in
   T5a–T5c.

---

## Data Flow

```
CP-UI Setup Guide Page
        │ user clicks "Download CA Certificate"
        ▼
Control Plane  GET /api/admin/setup/proxy/{thingId}/ca-cert   (S3)
        │ reads thing.reported.managementURL from Hub
        │ calls GET {managementURL}/management/ca-cert  (5s timeout)
        ▼
Compliance Proxy  /management/ca-cert
        │ calls issuer.CACertPEM()
        ▼
PEM bytes (public cert only, never the private key)
```

---

## Risks

- **Management port accessibility from Control Plane:** The compliance proxy
  management server binds on the metrics port (default `:9090`). In
  Kubernetes this must be exposed via a ClusterIP service. In EC2 the VPC
  security group must allow TCP from the Control Plane host. If the port is
  unreachable, the Control Plane relay (S3) returns 502, which is the correct
  signal for the operator to fix networking.
- **`managementURL` staleness:** The reported state is written once at startup.
  If the proxy restarts with a different advertise host, Hub will receive the
  updated URL on reconnect. The Control Plane relay always reads the live
  `thing.reported` from Hub (not a local cache), so a stale URL between
  restart and reconnect is a brief window — acceptable.
