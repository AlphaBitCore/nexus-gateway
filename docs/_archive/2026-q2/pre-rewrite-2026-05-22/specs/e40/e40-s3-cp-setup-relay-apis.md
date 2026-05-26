# E40-S3 — Control Plane Setup Relay APIs

> Story: e40-s3
> Epic: 40 (Setup Guide)
> Status: Approved

## User Story

As an IT Admin using the CP-UI Setup Guide, I want the Control Plane to relay
the compliance proxy's CA certificate, generate a macOS MDM profile and PAC
file, and toggle onboarding mode for a specific proxy node — all through
authenticated admin API endpoints — so that the Setup Guide page can present
one-click download and configuration actions without requiring direct network
access from the user's browser to the internal management port.

---

## Background

The compliance proxy management endpoint (`/management/ca-cert`) is only
reachable on the internal network from the Control Plane host. The CP-UI runs
in the user's browser and cannot call it directly. The Control Plane acts as a
relay: it looks up the proxy's `managementURL` from Hub's Thing shadow, fetches
the certificate on behalf of the caller, and either returns it as-is (PEM) or
renders it into derived artifacts (MDM profile, PAC file). The `PATCH
onboarding` endpoint pushes a Shadow desired-state update through the existing
Hub config-change path.

No CA certificate or private key is stored in PostgreSQL at any point.

---

## Scope

### In

- New handler file `packages/control-plane/internal/handler/setup.go`.
- Four new Echo routes registered under
  `GET/PATCH /api/admin/setup/proxy/:thingId/...`.
- New `RegisterSetupRoutes` function on `AdminHandler` wired into
  `admin_routes.go`.
- Embedded mobileconfig template via `//go:embed`.
- `HubNotifier` interface gains `GetThingReported(ctx, thingID) (map[string]json.RawMessage, error)`.
- PAC file domain list read from `store.DB.ListEnabledInterceptionDomains`
  filtered to `type = 'ai_provider'` (using the existing store query or a new
  narrow one).
- All four endpoints emit admin audit log events via `h.Audit.Write`.
- IAM middleware applied per-endpoint: `admin:ReadSettings` for GETs,
  `admin:WriteSettings` for PATCH.

### Out

- No database schema changes.
- No MDM enrollment or SCEP integration — the mobileconfig is a manual-install
  profile only.
- No Agent-side changes.
- No CP-UI implementation (that is S6/FR-6.3).

---

## Tasks

### T1. `HubNotifier` interface — add `GetThingReported`

File: `packages/control-plane/internal/handler/helpers.go`

Extend the `HubNotifier` interface with one new method:

```go
// GetThingReported returns the reported shadow state for a single Thing.
// Returns nil map + no error when the thing exists but has no reported state.
// Returns an error wrapping hubclient.ErrNotConfigured when Hub is unavailable.
GetThingReported(ctx context.Context, thingID string) (map[string]json.RawMessage, error)
```

### T2. `hubclient.Client` — implement `GetThingReported`

File: `packages/control-plane/internal/hubclient/client.go`

Add a method that reads `thing.reported` from Hub's Thing Registry HTTP API:

```go
// GetThingReported calls GET /api/hub/things/:id and extracts the
// reported JSONB shadow state. Returns an empty map (not an error) when
// the thing has no reported state yet.
func (c *Client) GetThingReported(ctx context.Context, thingID string) (map[string]json.RawMessage, error) {
    if c.baseURL == "" {
        return nil, ErrNotConfigured
    }
    url := fmt.Sprintf("%s/api/hub/things/%s", c.baseURL, thingID)
    r, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
    if err != nil {
        return nil, err
    }
    r.Header.Set("Authorization", "Bearer "+c.token)
    r.Header.Set("Accept", "application/json")

    resp, err := c.httpClient.Do(r)
    if err != nil {
        return nil, fmt.Errorf("hubclient: get thing %s: %w", thingID, err)
    }
    defer resp.Body.Close() //nolint:errcheck
    body, _ := io.ReadAll(resp.Body)

    if resp.StatusCode == http.StatusNotFound {
        return nil, nil // thing does not exist
    }
    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("hubclient: get thing %s: status %d: %s", thingID, resp.StatusCode, strings.TrimSpace(string(body)))
    }

    var envelope struct {
        Reported map[string]json.RawMessage `json:"reported"`
    }
    if err := json.Unmarshal(body, &envelope); err != nil {
        return nil, fmt.Errorf("hubclient: decode thing reported: %w", err)
    }
    if envelope.Reported == nil {
        envelope.Reported = map[string]json.RawMessage{}
    }
    return envelope.Reported, nil
}
```

If Hub's `/api/hub/things/:id` endpoint returns the Thing in a nested
`data` or `thing` envelope (inspect the actual Hub response shape and adjust
the struct accordingly).

### T3. Store query — `ListAIProviderInterceptionDomains`

File: `packages/control-plane/internal/store/interception_domain.go` (or
extend existing interception_domain store file)

Add:

```go
// ListAIProviderInterceptionDomains returns all enabled interception_domain
// rows where type = 'ai_provider'. Used by the PAC file generator to
// enumerate the set of AI-provider hostnames to route through the proxy.
func (db *DB) ListAIProviderInterceptionDomains(ctx context.Context) ([]InterceptionDomain, error) {
    rows, err := db.Pool.Query(ctx, `
        SELECT id, host_pattern, type, enabled, created_at
        FROM interception_domain
        WHERE type = 'ai_provider' AND enabled = true
        ORDER BY host_pattern ASC
    `)
    if err != nil {
        return nil, fmt.Errorf("list ai_provider domains: %w", err)
    }
    defer rows.Close()
    var result []InterceptionDomain
    for rows.Next() {
        var d InterceptionDomain
        if err := rows.Scan(&d.ID, &d.HostPattern, &d.Type, &d.Enabled, &d.CreatedAt); err != nil {
            return nil, fmt.Errorf("scan interception domain: %w", err)
        }
        result = append(result, d)
    }
    return result, rows.Err()
}
```

Adjust field names and the `InterceptionDomain` struct to match the existing
codegen output in `packages/shared/schemas/configtypes/interception_domain.go`.

### T4. mobileconfig template

File: `packages/control-plane/internal/handler/templates/nexus-proxy-ca.mobileconfig.tmpl`
(new directory + file, embedded via `//go:embed`)

Create a minimal Apple mobileconfig template with a Certificate payload:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>PayloadContent</key>
  <array>
    <dict>
      <key>PayloadCertificateFileName</key>
      <string>nexus-proxy-ca.crt</string>
      <key>PayloadContent</key>
      <data>{{.CACertBase64}}</data>
      <key>PayloadDescription</key>
      <string>Nexus Gateway Proxy CA Certificate</string>
      <key>PayloadDisplayName</key>
      <string>Nexus Gateway Proxy CA</string>
      <key>PayloadIdentifier</key>
      <string>com.nexus-gateway.proxy-ca.{{.ThingID}}</string>
      <key>PayloadType</key>
      <string>com.apple.security.root</string>
      <key>PayloadUUID</key>
      <string>{{.UUID}}</string>
      <key>PayloadVersion</key>
      <integer>1</integer>
    </dict>
  </array>
  <key>PayloadDescription</key>
  <string>Installs the Nexus Gateway Proxy CA certificate as a trusted root.</string>
  <key>PayloadDisplayName</key>
  <string>Nexus Gateway Proxy CA — {{.Organization}}</string>
  <key>PayloadIdentifier</key>
  <string>com.nexus-gateway.proxy-ca-profile.{{.ThingID}}</string>
  <key>PayloadOrganization</key>
  <string>{{.Organization}}</string>
  <key>PayloadType</key>
  <string>Configuration</string>
  <key>PayloadUUID</key>
  <string>{{.ProfileUUID}}</string>
  <key>PayloadVersion</key>
  <integer>1</integer>
</dict>
</plist>
```

Template variables (populated by the handler):
- `{{.CACertBase64}}` — base64-encoded DER bytes of the CA cert (standard
  encoding, no line wrapping; Apple's XML parser handles arbitrary-length
  `<data>` values).
- `{{.Organization}}` — caller-supplied organization name (URL query param
  `organization`; defaults to `"Nexus Gateway"` when absent).
- `{{.ThingID}}` — the proxy thing ID (used to make payload identifiers
  unique per proxy instance).
- `{{.UUID}}` and `{{.ProfileUUID}}` — freshly generated UUIDs (use
  `github.com/google/uuid` or the stdlib equivalent) so each download
  produces a distinct profile that MDM treats as a new install.

Embed the template with:

```go
//go:embed templates/nexus-proxy-ca.mobileconfig.tmpl
var mobileconfigTmpl string
```

Parse once at package init (or lazily on first request) with
`template.Must(template.New("mobileconfig").Parse(mobileconfigTmpl))`.

### T5. Handler file — `setup.go`

File: `packages/control-plane/internal/handler/setup.go`

#### T5a. `RegisterSetupRoutes`

```go
func (h *AdminHandler) RegisterSetupRoutes(g *echo.Group, iamMW func(string) echo.MiddlewareFunc) {
    p := g.Group("/setup/proxy/:thingId")
    p.GET("/ca-cert",     h.GetProxyCACert,     iamMW("admin:ReadSettings"))
    p.GET("/mdm-profile", h.GetProxyMDMProfile, iamMW("admin:ReadSettings"))
    p.GET("/pac-file",    h.GetProxyPACFile,    iamMW("admin:ReadSettings"))
    p.PATCH("/onboarding", h.PatchProxyOnboarding, iamMW("admin:WriteSettings"))
}
```

Wire `h.RegisterSetupRoutes(g, iamMW)` into
`packages/control-plane/internal/handler/admin_routes.go`
`RegisterAdminRoutes` alongside the other resource groups.

#### T5b. `resolveManagementURL` — shared helper

```go
// resolveManagementURL retrieves the managementURL from the proxy thing's
// reported shadow state. Returns ("", nil) when the thing exists but has
// no managementURL. Returns ("", non-nil-err) on Hub error.
// Returns ("", ErrThingNotFound) when the thing ID is unknown.
func (h *AdminHandler) resolveManagementURL(ctx context.Context, thingID string) (string, error) {
    reported, err := h.Hub.GetThingReported(ctx, thingID)
    if err != nil {
        return "", fmt.Errorf("hub get thing reported: %w", err)
    }
    if reported == nil {
        return "", ErrThingNotFound
    }
    raw, ok := reported["managementURL"]
    if !ok || string(raw) == "null" || string(raw) == `""` {
        return "", nil // thing exists but has no managementURL yet
    }
    var u string
    if err := json.Unmarshal(raw, &u); err != nil {
        return "", fmt.Errorf("decode managementURL: %w", err)
    }
    return u, nil
}
```

`ErrThingNotFound` is a package-level sentinel:
`var ErrThingNotFound = errors.New("setup: thing not found")`.

#### T5c. `fetchCACertPEM` — shared helper

```go
// fetchCACertPEM calls {managementURL}/management/ca-cert with a 5-second
// timeout and returns the PEM bytes.
func (h *AdminHandler) fetchCACertPEM(ctx context.Context, managementURL string) ([]byte, error) {
    reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
    defer cancel()
    url := strings.TrimRight(managementURL, "/") + "/management/ca-cert"
    req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
    if err != nil {
        return nil, fmt.Errorf("build ca-cert request: %w", err)
    }
    client := h.ComplianceProxyClient
    if client == nil {
        client = &http.Client{Timeout: 5 * time.Second}
    }
    resp, err := client.Do(req)
    if err != nil {
        return nil, fmt.Errorf("ca-cert fetch: %w", err)
    }
    defer resp.Body.Close() //nolint:errcheck
    if resp.StatusCode != http.StatusOK {
        b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
        return nil, fmt.Errorf("ca-cert endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
    }
    pem, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
    if err != nil {
        return nil, fmt.Errorf("read ca-cert body: %w", err)
    }
    return pem, nil
}
```

#### T5d. `GetProxyCACert`

```go
// GetProxyCACert relays the proxy Sub-CA PEM certificate to the caller.
//
//   GET /api/admin/setup/proxy/:thingId/ca-cert
//   → 200 application/x-pem-file
//   → 404 thing not found or no managementURL
//   → 502 proxy management endpoint unreachable
func (h *AdminHandler) GetProxyCACert(c echo.Context) error {
    ctx := c.Request().Context()
    thingID := c.Param("thingId")
    actor := actorFromContext(c)

    mgmtURL, err := h.resolveManagementURL(ctx, thingID)
    if err != nil {
        if errors.Is(err, ErrThingNotFound) {
            return c.JSON(http.StatusNotFound, errJSON("Proxy not found", "not_found", "THING_NOT_FOUND"))
        }
        h.Logger.Error("resolve managementURL", "thingId", thingID, "error", err)
        return c.JSON(http.StatusInternalServerError, errJSON("Failed to resolve proxy", "server_error", "INTERNAL_ERROR"))
    }
    if mgmtURL == "" {
        return c.JSON(http.StatusNotFound, errJSON("Proxy has no management URL", "not_found", "NO_MANAGEMENT_URL"))
    }

    pemBytes, err := h.fetchCACertPEM(ctx, mgmtURL)
    if err != nil {
        h.Logger.Warn("ca-cert fetch failed", "thingId", thingID, "mgmtURL", mgmtURL, "error", err)
        return c.JSON(http.StatusBadGateway, errJSON("Proxy management endpoint unreachable", "bad_gateway", "MANAGEMENT_UNREACHABLE"))
    }

    h.Audit.Write(ctx, audit.Event{
        Action:   "setup.proxy.ca_cert.download",
        ActorID:  actor.ID,
        ActorName: actor.Name,
        ResourceType: "compliance-proxy",
        ResourceID: thingID,
    })

    c.Response().Header().Set("Content-Type", "application/x-pem-file")
    c.Response().Header().Set("Content-Disposition", `attachment; filename="nexus-proxy-ca.crt"`)
    c.Response().Header().Set("Cache-Control", "no-store")
    return c.String(http.StatusOK, string(pemBytes))
}
```

#### T5e. `GetProxyMDMProfile`

```go
// GetProxyMDMProfile renders a macOS MDM configuration profile embedding
// the proxy CA cert. The profile installs the cert as a trusted root.
//
//   GET /api/admin/setup/proxy/:thingId/mdm-profile?organization=<org>
//   → 200 application/x-apple-aspen-config
//   → 404 / 502 (same conditions as GetProxyCACert)
func (h *AdminHandler) GetProxyMDMProfile(c echo.Context) error {
    ctx := c.Request().Context()
    thingID := c.Param("thingId")
    org := c.QueryParam("organization")
    if org == "" {
        org = "Nexus Gateway"
    }
    actor := actorFromContext(c)

    mgmtURL, err := h.resolveManagementURL(ctx, thingID)
    if err != nil {
        if errors.Is(err, ErrThingNotFound) {
            return c.JSON(http.StatusNotFound, errJSON("Proxy not found", "not_found", "THING_NOT_FOUND"))
        }
        return c.JSON(http.StatusInternalServerError, errJSON("Failed to resolve proxy", "server_error", "INTERNAL_ERROR"))
    }
    if mgmtURL == "" {
        return c.JSON(http.StatusNotFound, errJSON("Proxy has no management URL", "not_found", "NO_MANAGEMENT_URL"))
    }

    pemBytes, err := h.fetchCACertPEM(ctx, mgmtURL)
    if err != nil {
        return c.JSON(http.StatusBadGateway, errJSON("Proxy management endpoint unreachable", "bad_gateway", "MANAGEMENT_UNREACHABLE"))
    }

    // Decode PEM → DER → base64 for the mobileconfig <data> payload.
    block, _ := pem.Decode(pemBytes)
    if block == nil {
        h.Logger.Error("ca-cert PEM decode failed", "thingId", thingID)
        return c.JSON(http.StatusBadGateway, errJSON("Proxy returned invalid PEM", "bad_gateway", "INVALID_PEM"))
    }
    certBase64 := base64.StdEncoding.EncodeToString(block.Bytes)

    var buf bytes.Buffer
    if err := mobileconfigTemplate.Execute(&buf, map[string]string{
        "CACertBase64": certBase64,
        "Organization": org,
        "ThingID":      thingID,
        "UUID":         newUUID(),
        "ProfileUUID":  newUUID(),
    }); err != nil {
        return c.JSON(http.StatusInternalServerError, errJSON("Failed to render profile", "server_error", "TEMPLATE_ERROR"))
    }

    h.Audit.Write(ctx, audit.Event{
        Action:       "setup.proxy.mdm_profile.download",
        ActorID:      actor.ID,
        ActorName:    actor.Name,
        ResourceType: "compliance-proxy",
        ResourceID:   thingID,
    })

    c.Response().Header().Set("Content-Type", "application/x-apple-aspen-config")
    c.Response().Header().Set("Content-Disposition", `attachment; filename="nexus-proxy-ca.mobileconfig"`)
    c.Response().Header().Set("Cache-Control", "no-store")
    return c.String(http.StatusOK, buf.String())
}
```

`newUUID()` returns a new random UUID string (use `crypto/rand` + hex
encoding or import `github.com/google/uuid` if already in the module's
`go.mod`).

#### T5f. `GetProxyPACFile`

```go
// GetProxyPACFile generates a PAC file routing all ai_provider domains
// through the specified proxy host and port.
//
//   GET /api/admin/setup/proxy/:thingId/pac-file?proxyHost=<host>&proxyPort=<port>
//   → 200 application/x-ns-proxy-autoconfig
//   → 400 missing proxyHost or proxyPort
func (h *AdminHandler) GetProxyPACFile(c echo.Context) error {
    ctx := c.Request().Context()
    thingID := c.Param("thingId")
    proxyHost := c.QueryParam("proxyHost")
    proxyPort := c.QueryParam("proxyPort")
    actor := actorFromContext(c)

    if proxyHost == "" || proxyPort == "" {
        return c.JSON(http.StatusBadRequest, errJSON("proxyHost and proxyPort are required", "bad_request", "MISSING_PARAMS"))
    }
    if _, err := strconv.Atoi(proxyPort); err != nil {
        return c.JSON(http.StatusBadRequest, errJSON("proxyPort must be a number", "bad_request", "INVALID_PORT"))
    }

    domains, err := h.DB.ListAIProviderInterceptionDomains(ctx)
    if err != nil {
        h.Logger.Error("list ai_provider domains", "error", err)
        return c.JSON(http.StatusInternalServerError, errJSON("Failed to load domain list", "server_error", "DB_ERROR"))
    }

    pac := buildPACFile(proxyHost, proxyPort, domains)

    h.Audit.Write(ctx, audit.Event{
        Action:       "setup.proxy.pac_file.download",
        ActorID:      actor.ID,
        ActorName:    actor.Name,
        ResourceType: "compliance-proxy",
        ResourceID:   thingID,
    })

    c.Response().Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
    c.Response().Header().Set("Content-Disposition", `attachment; filename="nexus-proxy.pac"`)
    c.Response().Header().Set("Cache-Control", "no-store")
    return c.String(http.StatusOK, pac)
}
```

`buildPACFile` is a pure function that renders the PAC JavaScript:

```go
func buildPACFile(proxyHost, proxyPort string, domains []InterceptionDomain) string {
    var sb strings.Builder
    sb.WriteString("function FindProxyForURL(url, host) {\n")
    for _, d := range domains {
        // host_pattern may be a wildcard like "*.openai.com" or an exact
        // hostname. Render a shExpMatch for wildcard patterns and a plain
        // equality check for exact ones.
        if strings.HasPrefix(d.HostPattern, "*.") {
            sb.WriteString(fmt.Sprintf("  if (shExpMatch(host, %q)) return %q;\n",
                d.HostPattern, "PROXY "+proxyHost+":"+proxyPort))
        } else {
            sb.WriteString(fmt.Sprintf("  if (host === %q) return %q;\n",
                d.HostPattern, "PROXY "+proxyHost+":"+proxyPort))
        }
    }
    sb.WriteString("  return \"DIRECT\";\n}\n")
    return sb.String()
}
```

#### T5g. `PatchProxyOnboarding`

```go
// PatchProxyOnboarding enables or disables onboarding mode for a specific
// compliance proxy instance by pushing the desired state to Hub shadow.
//
//   PATCH /api/admin/setup/proxy/:thingId/onboarding
//   Body: {"enabled": true|false}
//   → 200 {"ok": true}
//   → 400 malformed body
//   → 404 thing not found
func (h *AdminHandler) PatchProxyOnboarding(c echo.Context) error {
    ctx := c.Request().Context()
    thingID := c.Param("thingId")
    actor := actorFromContext(c)

    var body struct {
        Enabled *bool `json:"enabled"`
    }
    if err := c.Bind(&body); err != nil || body.Enabled == nil {
        return c.JSON(http.StatusBadRequest, errJSON("Body must include {\"enabled\": true|false}", "bad_request", "MISSING_ENABLED"))
    }

    // Verify the thing exists before pushing to Hub shadow.
    thing, err := h.DB.GetThing(ctx, thingID)
    if err != nil {
        return c.JSON(http.StatusInternalServerError, errJSON("Failed to look up proxy", "server_error", "DB_ERROR"))
    }
    if thing == nil {
        return c.JSON(http.StatusNotFound, errJSON("Proxy not found", "not_found", "THING_NOT_FOUND"))
    }

    state := map[string]any{"enabled": *body.Enabled}
    _, err = h.Hub.NotifyConfigChange(ctx, hubclient.ConfigChangeRequest{
        ThingType: "compliance-proxy",
        ConfigKey: "onboarding",
        State:     state,
        Action:    "update",
        ActorID:   actor.ID,
        ActorName: actor.Name,
        SourceIP:  c.RealIP(),
    })
    if err != nil {
        h.Logger.Error("patch onboarding notify hub", "thingId", thingID, "error", err)
        return c.JSON(http.StatusInternalServerError, errJSON("Failed to update onboarding state", "server_error", "HUB_ERROR"))
    }

    h.Audit.Write(ctx, audit.Event{
        Action:       "setup.proxy.onboarding.patch",
        ActorID:      actor.ID,
        ActorName:    actor.Name,
        ResourceType: "compliance-proxy",
        ResourceID:   thingID,
        Detail:       fmt.Sprintf("enabled=%v", *body.Enabled),
    })

    return c.JSON(http.StatusOK, map[string]any{"ok": true, "enabled": *body.Enabled})
}
```

Note: `NotifyConfigChange` pushes the `onboarding` key to the Shadow desired
state for all compliance-proxy Things of this type. If per-instance targeting
is needed (i.e., only this `thingID`), use `store.DB.UpdateThingShadowDesired`
directly and then call `Hub.InvalidateConfig("compliance-proxy", "onboarding")`
to trigger a WebSocket push. Per the requirements the PATCH uses
`thingId` from the path, implying per-instance control; use
`UpdateThingShadowDesired` for the targeted approach and notify Hub with the
specific thing ID.

### T6. Unit tests

File: `packages/control-plane/internal/handler/setup_test.go`

Use a mock `HubNotifier` (implementing the updated `HubNotifier` interface),
a mock management HTTP server (`httptest.NewServer`), and the existing
`store.DB` test helpers or an in-memory double.

**T6a — `GetProxyCACert` success:**
- Mock Hub returns `{"managementURL": "http://127.0.0.1:<port>"}`.
- Mock management server at that port serves a valid PEM on `/management/ca-cert`.
- Assert response 200, `Content-Type: application/x-pem-file`, body equals
  the PEM from the mock server.

**T6b — `GetProxyCACert` 404 — thing not found:**
- Mock Hub `GetThingReported` returns `(nil, nil)`.
- Assert response 404.

**T6c — `GetProxyCACert` 404 — no managementURL:**
- Mock Hub returns `{}` (empty reported map, no `managementURL` key).
- Assert response 404.

**T6d — `GetProxyCACert` 502 — proxy unreachable:**
- Mock Hub returns a valid `managementURL` pointing to a port that is not
  listening.
- Assert response 502.

**T6e — `GetProxyMDMProfile` success:**
- Same mock setup as T6a.
- Assert response 200, `Content-Type: application/x-apple-aspen-config`.
- Parse the mobileconfig XML; assert the `<data>` element inside the
  Certificate payload is non-empty base64 that decodes to the mock CA DER.

**T6f — `GetProxyPACFile` success:**
- Seed two `interception_domain` rows with `type = 'ai_provider'` and
  `enabled = true` into the test DB (or stub `ListAIProviderInterceptionDomains`).
- Call `GET /pac-file?proxyHost=10.0.0.1&proxyPort=3128`.
- Assert response 200, `Content-Type: application/x-ns-proxy-autoconfig`.
- Assert the PAC JavaScript contains `PROXY 10.0.0.1:3128` for each domain
  and ends with `return "DIRECT"`.

**T6g — `GetProxyPACFile` 400 — missing params:**
- Call without `proxyHost` param; assert 400.
- Call with `proxyPort` = `"notaport"`; assert 400.

**T6h — `PatchProxyOnboarding` success:**
- Mock `Hub.NotifyConfigChange` records the call.
- Mock `DB.GetThing` returns a valid `ThingRegistry`.
- Call `PATCH /onboarding` with `{"enabled": true}`.
- Assert 200 `{"ok": true, "enabled": true}`.
- Assert `NotifyConfigChange` was called with `ConfigKey = "onboarding"` and
  `State = {"enabled": true}`.

**T6i — `PatchProxyOnboarding` 400 — missing body:**
- Call with empty body; assert 400.
- Call with `{"foo": 1}` (no `enabled`); assert 400.

**T6j — Auth enforcement (all four endpoints):**
- Table-driven: unauthenticated request (no IAM middleware applied in the
  test Echo context) to each of the four routes returns 401 or is rejected
  by the middleware. This verifies route registration wires IAM correctly.

---

## API Summary

| Method | Path | IAM | Description |
|--------|------|-----|-------------|
| GET | `/api/admin/setup/proxy/:thingId/ca-cert` | `admin:ReadSettings` | Relay proxy Sub-CA PEM |
| GET | `/api/admin/setup/proxy/:thingId/mdm-profile` | `admin:ReadSettings` | Render macOS mobileconfig |
| GET | `/api/admin/setup/proxy/:thingId/pac-file` | `admin:ReadSettings` | Generate PAC file |
| PATCH | `/api/admin/setup/proxy/:thingId/onboarding` | `admin:WriteSettings` | Toggle onboarding mode |

---

## Acceptance Criteria

1. **AC1 — `GET ca-cert` success:** Returns 200 with `Content-Type:
   application/x-pem-file` and the PEM body when the thing exists and its
   management endpoint is reachable. Body is parseable as a valid X.509
   certificate in PEM format.

2. **AC2 — `GET ca-cert` 404:** Returns 404 when the `:thingId` does not
   exist in Hub's Thing Registry, or when the thing exists but has no
   `managementURL` in its reported shadow state.

3. **AC3 — `GET ca-cert` 502:** Returns 502 when the management endpoint
   is unreachable (connection refused, timeout, or non-200 status from the
   proxy).

4. **AC4 — `GET mdm-profile` valid XML:** Returns 200 with `Content-Type:
   application/x-apple-aspen-config` and a well-formed mobileconfig XML
   document containing a `com.apple.security.root` Certificate payload
   whose `<data>` value is valid base64 encoding the CA cert DER bytes.

5. **AC5 — `GET pac-file` valid PAC:** Returns 200 with `Content-Type:
   application/x-ns-proxy-autoconfig` and a PAC file that routes all
   `ai_provider`-type interception domains to `proxyHost:proxyPort` and
   returns `"DIRECT"` for all other hosts. Returns 400 when `proxyHost` or
   `proxyPort` is absent.

6. **AC6 — `PATCH onboarding` updates shadow:** Returns 200 `{"ok":true}`
   and calls `Hub.NotifyConfigChange` with `ThingType = "compliance-proxy"`,
   `ConfigKey = "onboarding"`, and the correct `{"enabled": <value>}` state.
   Returns 400 when the request body is absent or lacks the `enabled` field.

7. **AC7 — Auth enforcement:** All four endpoints return 401 for
   unauthenticated requests. GET endpoints allow `admin:ReadSettings`;
   PATCH requires `admin:WriteSettings`.

8. **AC8 — Audit events:** Every successful call (200) emits an audit log
   event with the appropriate action name, actor identity, and resource ID.

9. **AC9 — Unit tests pass:** `go test -race -count=1
   ./packages/control-plane/internal/handler/...` is green with all ten
   test cases in `setup_test.go`.

---

## Risks

- **Hub `/api/hub/things/:id` response envelope:** If Hub returns the Thing
  object in a wrapper (e.g. `{"thing": {...}}`) rather than at the root, the
  `GetThingReported` implementation must unwrap accordingly. Verify against
  the live Hub API or Hub handler source before finalizing T2.
- **Per-instance vs. fleet-wide onboarding push:** `NotifyConfigChange`
  currently targets all Things of a given type. The onboarding endpoint
  accepts a `:thingId` implying per-instance targeting. The implementation
  must use `UpdateThingShadowDesired(thingID, ...)` + Hub invalidate, not
  `NotifyConfigChange` with `ThingType`, to avoid enabling onboarding mode
  on every compliance-proxy instance when the admin only intended to target
  one. This is a design constraint confirmed in the requirements.
- **PAC wildcard matching:** Browser PAC engines use `shExpMatch`, which
  handles `*.openai.com` style wildcards. FQDN patterns without wildcards
  (e.g. `api.openai.com`) must use a strict equality check. The
  `buildPACFile` helper must correctly distinguish the two cases based on
  whether the pattern starts with `*.`.
