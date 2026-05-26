# E40-S1 — Compliance Proxy Onboarding Intercept

> Story: e40-s1
> Epic: 40 (Setup Guide)
> Status: Approved

## User Story

As an IT Admin deploying the compliance proxy for the first time, I want the
proxy to intercept browser CONNECT requests to monitored AI-provider domains
and return a human-readable 407 page linking to the CP-UI Setup Guide, so that
end users immediately understand they need to install the CA certificate rather
than seeing an opaque TLS error.

---

## Background

When a client has the proxy configured but has not yet trusted the proxy Sub-CA,
every CONNECT to a monitored domain fails with a TLS certificate verification
error — the browser shows a cryptic security warning with no actionable
guidance. Onboarding mode short-circuits the CONNECT before TLS interception
begins and returns a 407 with an HTML body that points the user to the correct
setup page. The proxy then returns to normal MITM behavior once onboarding mode
is disabled via the Shadow desired state.

---

## Scope

### In

- New `OnboardingConfig` struct and `Onboarding OnboardingConfig` field on
  `packages/compliance-proxy/internal/config/config.go` `Config`.
- `ProxyConfig` and `ProxyServer` gain `onboardingEnabled atomic.Bool`,
  `onboardingCPUIBaseURL string`, and a `SetOnboardingEnabled(bool)` method.
- `ProxyServer.ServeHTTP` checks the onboarding gate immediately after the
  HTTP/2 CONNECT rejection, before the access-control check.
- Gate logic: if `onboardingEnabled` is `true` AND the target hostname is in
  the monitored domain allowlist, respond 407 with HTML body and return.
- The HTML page is a small static string constant (not a file): valid HTML5
  with `<title>`, `<h1>`, a one-sentence explanation, and an `<a>` linking to
  `{cpUIBaseURL}/setup/proxy`.
- Shadow reducer in `cmd/compliance-proxy/main.go` handles the `onboarding`
  config key (Category A inline) and calls `proxyServer.SetOnboardingEnabled`.

### Out

- No change to the forward handler, SSE pipeline, audit path, or compliance
  pipeline — only the CONNECT entry point (ServeHTTP) is affected.
- No database schema changes.
- No CP-UI changes (that is S3/FR-6).

---

## Tasks

### T1. Config struct — add `OnboardingConfig`

File: `packages/compliance-proxy/internal/config/config.go`

- Add the following types:

  ```go
  // OnboardingConfig controls the proxy onboarding intercept mode.
  // When Enabled is true, CONNECT requests targeting monitored AI-provider
  // domains return 407 + HTML setup instructions instead of proceeding to TLS
  // interception. This guides users to install the CA cert before first use.
  type OnboardingConfig struct {
      // Enabled activates the onboarding intercept. Default false.
      Enabled bool `yaml:"enabled"`
      // CPUIBaseURL is the base URL of the Control Plane UI, used to construct
      // the link in the 407 HTML body (e.g. "https://cp.nexus.example.com").
      CPUIBaseURL string `yaml:"cpUIBaseURL"`
  }
  ```

- Add `Onboarding OnboardingConfig `yaml:"onboarding"`` to `Config`.
- No validation rule needed (both fields optional; empty CPUIBaseURL
  means onboarding mode is effectively misconfigured but not a startup
  error — the handler will render a link with an empty base URL).

### T2. `ProxyConfig` and `ProxyServer` — onboarding fields

File: `packages/compliance-proxy/internal/proxy/listener.go`

- Add to `ProxyConfig`:

  ```go
  // OnboardingCPUIBaseURL is the base URL of the Control Plane UI.
  // Used to render the link in the 407 onboarding HTML body.
  OnboardingCPUIBaseURL string
  ```

  (`onboardingEnabled` starts as false; the atomic is initialised from
  `ProxyConfig` in `NewProxyServer`.)

- Add to `ProxyServer`:

  ```go
  onboardingEnabled    atomic.Bool
  onboardingCPUIBaseURL string
  ```

- In `NewProxyServer`, wire both fields from `ProxyConfig`:
  - `s.onboardingEnabled.Store(cfg.OnboardingEnabled)` — requires adding
    `OnboardingEnabled bool` to `ProxyConfig` as well, seeded from YAML at
    construction time.
  - `s.onboardingCPUIBaseURL = cfg.OnboardingCPUIBaseURL`

- Add the public setter:

  ```go
  // SetOnboardingEnabled hot-swaps the onboarding intercept mode without
  // restarting the server. Called by the Shadow reducer in main.go.
  func (p *ProxyServer) SetOnboardingEnabled(enabled bool) {
      p.onboardingEnabled.Store(enabled)
  }
  ```

### T3. `ServeHTTP` — onboarding gate

File: `packages/compliance-proxy/internal/proxy/listener.go`

Insert the following block immediately after the HTTP/2 CONNECT rejection
(after the `r.ProtoMajor >= 2` check, before the `r.RemoteAddr` / access
control section):

```go
// Onboarding intercept: redirect monitored-domain CONNECTs to the
// setup guide before any TLS bump or access-control logic runs.
if p.onboardingEnabled.Load() && p.isMonitoredDomain(host) {
    p.serveOnboarding(w)
    return
}
```

Where `host` is parsed from `r.Host` (existing parsing logic below can be
reused; extract host parsing into a helper before the gate, or duplicate the
net.SplitHostPort call in the gate).

Add two private helpers:

```go
// isMonitoredDomain returns true if the given hostname (without port) is
// present in the access checker's domain allowlist. When p.checker is nil
// (test mode), returns false so the gate never fires.
func (p *ProxyServer) isMonitoredDomain(host string) bool { ... }

// serveOnboarding writes the 407 onboarding HTML response.
func (p *ProxyServer) serveOnboarding(w http.ResponseWriter) {
    const htmlTemplate = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="UTF-8"><title>Nexus Gateway Setup Required</title></head>
<body>
<h1>Nexus Gateway Setup Required</h1>
<p>Your browser is configured to route AI traffic through the Nexus compliance proxy,
but the proxy CA certificate has not yet been trusted on this device.
Please visit the <a href="%s/setup/proxy">Nexus Gateway Setup Guide</a> to install
the CA certificate and complete proxy configuration.</p>
</body>
</html>`
    body := fmt.Sprintf(htmlTemplate, p.onboardingCPUIBaseURL)
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    w.Header().Set("Proxy-Authenticate", `Basic realm="Nexus Gateway — CA certificate required"`)
    w.WriteHeader(http.StatusProxyAuthRequired) // 407
    _, _ = w.Write([]byte(body))
}
```

`isMonitoredDomain` delegates to `p.checker`: call the existing
`CheckConnect` domain-allowlist path or expose a narrower
`DomainAllowed(host string) bool` method on `*access.Checker` (preferred —
avoids constructing a dummy IP). If `p.checker == nil`, return `false`.

### T4. Shadow reducer — `onboarding` config key

File: `packages/compliance-proxy/cmd/compliance-proxy/main.go`

Add a `case "onboarding":` branch to the existing `OnConfigChanged` switch
(the same switch that handles `killswitch`, `payload_capture`, etc.):

```go
case "onboarding":
    var ob struct {
        Enabled bool `json:"enabled"`
    }
    if err := json.Unmarshal(cs.State, &ob); err != nil {
        logger.Warn("onboarding shadow: decode failed", "error", err)
        reported[key] = thingclient.ConfigState{State: cs.State, Version: cs.Version}
        continue
    }
    proxyServer.SetOnboardingEnabled(ob.Enabled)
    logger.Info("onboarding mode updated", "enabled", ob.Enabled)
    reported[key] = thingclient.ConfigState{State: cs.State, Version: cs.Version}
```

This is Category A (inline state in shadow desired); `needsPull: false`.
Hub sends the full `{"enabled": true}` payload; the reducer decodes and
applies it atomically.

### T5. Seed YAML defaults

File: `packages/compliance-proxy/compliance-proxy.dev.yaml` (and any other
environment YAML files present)

Add the onboarding section with `enabled: false`:

```yaml
onboarding:
  enabled: false
  cpUIBaseURL: "http://localhost:3000"
```

This makes the feature discoverable and ensures CI builds with the YAML pass
validation without activating onboarding mode.

### T6. Unit tests

File: `packages/compliance-proxy/internal/proxy/listener_onboarding_test.go`
(new file; table-driven)

Cover three cases:

| Case | onboarding.enabled | target host | expected outcome |
|------|-------------------|-------------|-----------------|
| A | true | `api.openai.com` (monitored) | 407, body contains `/setup/proxy` link |
| B | true | `example.com` (unlisted) | passes through (no 407; request handled normally by existing logic) |
| C | false | `api.openai.com` (monitored) | no 407; proceeds to normal CONNECT handling |

Test approach:
- Construct a `ProxyServer` with a stub `access.Checker` whose allowlist
  includes `api.openai.com`.
- Use `httptest.ResponseRecorder` and craft a minimal `http.Request` with
  `Method = "CONNECT"` and `Host = "api.openai.com:443"`.
- For case B, the request proceeds past the gate; stub the downstream to
  avoid a real dial. Verify the response is not 407.
- For case C, set `onboardingEnabled = false` via `SetOnboardingEnabled(false)`
  before serving; verify response is not 407.
- A fourth sub-test: call `SetOnboardingEnabled(true)` after construction and
  verify the gate activates without restart (hot-swap coverage).

---

## Acceptance Criteria

1. **AC1 — Monitored domain, onboarding on:** A CONNECT request to a domain in
   the proxy allowlist (e.g. `api.openai.com:443`) when `onboarding.enabled =
   true` returns HTTP 407 with `Content-Type: text/html; charset=utf-8` and a
   response body containing the string `/setup/proxy`.

2. **AC2 — Unlisted domain, onboarding on:** A CONNECT to a domain not in the
   allowlist when `onboarding.enabled = true` is not intercepted by the
   onboarding gate (response is not 407 from the onboarding handler; normal
   access-control logic runs).

3. **AC3 — Monitored domain, onboarding off:** A CONNECT to a monitored domain
   when `onboarding.enabled = false` (default) receives normal MITM handling —
   no 407 is returned by the onboarding gate.

4. **AC4 — Hot-swap via Shadow:** A Hub shadow push of
   `{"onboarding": {"enabled": true}}` causes the next CONNECT to a monitored
   domain to return 407 without restarting the compliance-proxy process. A
   subsequent push of `{"onboarding": {"enabled": false}}` restores normal
   behavior.

5. **AC5 — No blast radius:** Non-CONNECT methods, the health/metrics server,
   the break-glass endpoint, and all other proxy behavior are entirely
   unaffected by the `onboarding.enabled` flag.

6. **AC6 — Unit tests pass:** `go test -race -count=1
   ./packages/compliance-proxy/internal/proxy/...` is green with the new
   `listener_onboarding_test.go` covering AC1–AC3 and the hot-swap path.

---

## Risks

- **`isMonitoredDomain` delegation path:** If the access checker's allowlist is
  loaded asynchronously at startup and the first few CONNECTs arrive before it
  is populated, `isMonitoredDomain` might return `false` for a monitored domain,
  letting the CONNECT through instead of returning 407. This is benign
  (onboarding mode is an opt-in helper, not a security gate), but the
  implementation should be noted in code comments.
- **`Proxy-Authenticate` header and browser behavior:** RFC 7235 requires a
  `Proxy-Authenticate` challenge on 407. Some HTTP clients may attempt to
  satisfy the challenge; since we return a `Basic realm` header without a
  real authentication server behind it, the second attempt will also return 407.
  This is the intended behavior — the HTML body is the actual communication
  channel.
