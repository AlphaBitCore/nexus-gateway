# E40-S4 — MDM Profile + PAC File Template

Status: draft
Epic: E40 — Setup Guide
Story: S4 — MDM Profile and PAC File Template

## User Story

As an IT Admin deploying the Compliance Proxy, I want a downloadable Apple
MDM configuration profile that installs the proxy Sub-CA into macOS System
Keychain automatically, and a downloadable PAC file that routes AI-provider
traffic through my proxy instance, so that I can silently distribute both
artefacts via MDM or policy without requiring users to perform manual TLS
trust steps.

---

## 1. Problem

The existing `nexus-agent.mobileconfig.template` pre-approves System
Extension and Network Extension entitlements for the Nexus Agent, but it
contains no Certificate payload. Administrators who need to distribute the
proxy Sub-CA via MDM must maintain a separate profile, which creates a
synchronisation burden and an error-prone manual step.

There is also no server-side PAC file generator. Operators manually write
PAC files referencing a hardcoded list of AI-provider domains. When the
platform adds or removes a provider, these manually maintained PAC files
diverge silently.

---

## 2. Goal

- Extend the existing mobileconfig template to include a
  `com.apple.security.root` Certificate payload that installs the proxy
  Sub-CA into the macOS System Keychain when the profile is applied.
- Define the Go template struct and rendering logic used by the Control
  Plane `mdm-profile` endpoint (FR-3.2) to produce a complete,
  ready-to-deploy `.mobileconfig` file from a live CA cert.
- Provide a PAC file generator on the Control Plane `pac-file` endpoint
  (FR-3.3) that derives the AI-provider domain list from the
  `interception_domain` table and renders a properly structured PAC
  function.

---

## 3. Non-goals

- Changing the System Extension or Network Extension payloads already
  present in the template.
- MDM push/delivery logistics (Jamf, Intune, Kandji integration).
- Storing CA cert bytes in the PostgreSQL application database (NFR-2).
- PAC file signing or MIME-type negotiation beyond
  `application/x-ns-proxy-autoconfig`.

---

## 4. Design

### 4.1 mobileconfig Template Extension

The template at
`packages/agent/platform/darwin/installer/nexus-agent.mobileconfig.template`
is extended in-place. A third `<dict>` entry is appended inside the existing
`PayloadContent` `<array>` immediately before the closing `</array>` tag:

```xml
<!-- Proxy Sub-CA certificate: installs into System Keychain for TLS trust. -->
<dict>
    <key>PayloadType</key>
    <string>com.apple.security.root</string>
    <key>PayloadIdentifier</key>
    <string>com.nexus.agent.proxy-ca-cert</string>
    <key>PayloadUUID</key>
    <string>{{.ProfileUUID}}-ca</string>
    <key>PayloadVersion</key>
    <integer>1</integer>
    <key>PayloadDisplayName</key>
    <string>{{.Organization}} Nexus Proxy CA</string>
    <key>PayloadDescription</key>
    <string>Installs the Nexus Compliance Proxy Sub-CA into the System Keychain so that TLS-intercepted connections are trusted.</string>
    <key>PayloadContent</key>
    <data>{{.ProxyCACertBase64}}</data>
</dict>
```

Simultaneously, all existing `{{TEAM_ID}}`, `{{ORGANIZATION}}`, and
`{{PROFILE_UUID}}` placeholder strings are converted to Go `text/template`
syntax (`{{.TeamID}}`, `{{.Organization}}`, `{{.ProfileUUID}}`). This means
the same file acts as a Go template for the mdm-profile endpoint and as a
human-annotated template (operators replace `{{.FieldName}}` tokens manually
when using the file statically — the comment block at the top of the file is
updated accordingly).

### 4.2 Go Template Struct

A new file `packages/control-plane/internal/handler/setup_mdm.go` (or
inline in the mdm-profile handler file) defines:

```go
// MDMProfileData holds the variables substituted into
// nexus-agent.mobileconfig.template when rendering an on-demand
// MDM configuration profile for a compliance proxy instance.
type MDMProfileData struct {
    // Organization is displayed in the profile header and CA payload
    // display name. Sourced from the ?organization= query parameter.
    Organization string

    // ProxyCACertBase64 is the DER-encoded Sub-CA certificate, base64-encoded
    // without line wrapping, as required by the com.apple.security.root
    // PayloadContent <data> element.
    ProxyCACertBase64 string

    // ProfileUUID is a freshly generated UUID v4 for this render request.
    // It is used as a suffix base for per-payload UUIDs to ensure uniqueness.
    ProfileUUID string

    // TeamID is the Apple Developer Team ID embedded in the System Extension
    // and Content Filter payloads. Sourced from server config; defaults to
    // the placeholder string when not configured.
    TeamID string
}
```

The template is loaded once at startup (embedded via `//go:embed`) and
executed per request using `text/template.Template.Execute`.

`ProfileUUID` is generated fresh per request via `github.com/google/uuid`
(`uuid.NewString()`), ensuring each downloaded profile has a distinct UUID
and does not conflict with previously installed profiles on managed devices.

### 4.3 PAC File Generator

The `pac-file` handler constructs the PAC JavaScript text at request time.
It does not use a file template; the structure is a Go string built by the
handler.

**Domain source:** The handler queries `interception_domain` WHERE
`enabled = true`. Because the table has no `type` column in the current
schema, the handler filters by `adapter_id` matching a known set of AI
provider adapter identifiers (`openai`, `anthropic`, `gemini`, `deepseek`,
`xai`, `moonshot`, `zhipu`, `minimax`, `copilot`). If the query returns zero
rows (empty or misconfigured DB), the handler falls back to the canonical
hardcoded domain list derived from `docs/operators/ops/pki-and-certs.md`:

```
api.openai.com
api.anthropic.com
generativelanguage.googleapis.com
api.deepseek.com
api.x.ai
api.moonshot.cn
open.bigmodel.cn
api.minimax.chat
copilot-proxy.githubusercontent.com
```

**PAC structure:**

```javascript
function FindProxyForURL(url, host) {
    if (dnsDomainIs(host, "api.openai.com") ||
        dnsDomainIs(host, "api.anthropic.com") ||
        // ... one entry per domain ...
        false) {
        return "PROXY {{proxyHost}}:{{proxyPort}}";
    }
    return "DIRECT";
}
```

The `proxyHost` and `proxyPort` values are sourced from the
`?proxyHost=<host>&proxyPort=<port>` query parameters on the `pac-file`
endpoint. Both parameters are required; the handler returns `400 Bad Request`
if either is absent or if `proxyPort` is not a valid port number (1–65535).

### 4.4 Template Loading (Embedding)

The Control Plane binary embeds the mobileconfig template using the standard
Go embed directive in the handler package:

```go
//go:embed nexus-agent.mobileconfig.template
var mobileconfigTemplateBytes []byte
```

The template is parsed once at `init()` (or lazy-parsed on first request
with a `sync.Once`) and reused for all subsequent renders to avoid repeated
parsing overhead.

---

## 5. Tasks

- T1 — Extend `packages/agent/platform/darwin/installer/nexus-agent.mobileconfig.template`:
  - T1.1 — Convert all `{{TEAM_ID}}`, `{{ORGANIZATION}}`, `{{PROFILE_UUID}}` strings to Go `text/template` syntax: `{{.TeamID}}`, `{{.Organization}}`, `{{.ProfileUUID}}`.
  - T1.2 — Append the `com.apple.security.root` Certificate payload `<dict>` block (see §4.1) inside the existing `PayloadContent` array, before the closing `</array>`.
  - T1.3 — Update the comment header at the top of the template to describe all four substitution variables: `Organization`, `TeamID`, `ProfileUUID`, and `ProxyCACertBase64`.
  - T1.4 — Verify the resulting file is valid XML by running `xmllint --noout` (or equivalent); confirm no pre-existing payload structure is altered.

- T2 — Define `MDMProfileData` struct in `packages/control-plane/internal/handler/setup_mdm.go`:
  - T2.1 — Add the struct with fields `Organization`, `ProxyCACertBase64`, `ProfileUUID`, `TeamID`.
  - T2.2 — Add `//go:embed` directive for the template file (path relative to handler package).
  - T2.3 — Parse the template using `text/template.New("mdm").Parse(string(mobileconfigTemplateBytes))` in a `sync.Once` block; surface parse errors at startup via a `log.Fatal` guard.

- T3 — Implement the `mdm-profile` handler (FR-3.2) in `packages/control-plane/internal/handler/`:
  - T3.1 — Fetch the proxy Thing from Hub by `thingId`; return 404 if not found or not type `compliance-proxy`.
  - T3.2 — Read `managementURL` from the thing's reported shadow state; return 404 if absent.
  - T3.3 — HTTP GET `{managementURL}/management/ca-cert` with a 5 s timeout; return 502 on upstream error; return 503 if upstream returns 503.
  - T3.4 — DER-encode the PEM cert (strip `-----BEGIN/END CERTIFICATE-----` headers, base64-decode body) and re-encode to unpadded base64 without line wraps.
  - T3.5 — Populate `MDMProfileData{Organization: q.Get("organization"), ProxyCACertBase64: ..., ProfileUUID: uuid.NewString(), TeamID: cfg.MDM.TeamID}`.
  - T3.6 — Execute the parsed template into a `bytes.Buffer`; return `application/x-apple-aspen-config` with `Content-Disposition: attachment; filename="nexus-proxy.mobileconfig"`.
  - T3.7 — Emit admin audit log event `setup.mdm_profile_downloaded` with `thing_id`, `organization`, `remote_addr`.

- T4 — Implement the `pac-file` handler (FR-3.3) in `packages/control-plane/internal/handler/`:
  - T4.1 — Validate `proxyHost` and `proxyPort` query parameters; return `400` with JSON error body if absent or invalid.
  - T4.2 — Query `interception_domain` WHERE `enabled = true`; collect `host_pattern` values for known AI adapter IDs (see §4.3).
  - T4.3 — If the query returns zero rows, fall back to the hardcoded canonical domain list (§4.3).
  - T4.4 — Build the PAC JavaScript string: one `dnsDomainIs` clause per domain, formatted as described in §4.3.
  - T4.5 — Return `application/x-ns-proxy-autoconfig` with `Content-Disposition: attachment; filename="nexus-proxy.pac"`.
  - T4.6 — Emit admin audit log event `setup.pac_file_downloaded` with `thing_id`, `proxy_host`, `proxy_port`, `domain_count`, `remote_addr`.

- T5 — Register both new endpoints on the Control Plane admin router:
  - `GET /api/admin/setup/proxy/:thingId/mdm-profile` → mdm-profile handler.
  - `GET /api/admin/setup/proxy/:thingId/pac-file` → pac-file handler.
  - Both require IAM permission `admin:ReadSettings`.

- T6 — Unit tests (`packages/control-plane/internal/handler/setup_mdm_test.go`):
  - T6.1 — Test template rendering: provide a known fake base64 cert and assert the rendered XML contains a `<data>` element with the cert bytes inside the `com.apple.security.root` payload.
  - T6.2 — Test that `ProfileUUID` differs between two successive renders (UUID freshness).
  - T6.3 — Test PAC handler: mock `interception_domain` rows; assert all domains appear in the output as `dnsDomainIs` clauses.
  - T6.4 — Test PAC handler fallback: empty DB result → canonical domain list appears.
  - T6.5 — Test PAC handler validation: missing `proxyHost` returns `400`; `proxyPort=0` returns `400`; `proxyPort=99999` returns `400`.

---

## 6. Acceptance Criteria

- AC1 — The mobileconfig template contains a `PayloadType` of
  `com.apple.security.root` in the `PayloadContent` array.
- AC2 — A rendered mobileconfig (from the `mdm-profile` endpoint) contains
  a non-empty `<data>` element inside the `com.apple.security.root` payload
  whose content equals the base64-encoded DER bytes of the fetched CA cert.
- AC3 — The rendered `PayloadOrganization` key equals the value of the
  `?organization=` query parameter.
- AC4 — Each rendered profile has a unique `ProfileUUID` that differs from
  the UUID in any previously rendered profile.
- AC5 — The PAC file returned by the `pac-file` endpoint contains a
  `dnsDomainIs` check for every domain in the `interception_domain` table
  (adapter-filtered), or for the full canonical fallback list when the table
  is empty.
- AC6 — The PAC file returns `"PROXY {host}:{port}"` for matched domains
  and `"DIRECT"` for all others, where `{host}` and `{port}` match the
  request's `proxyHost` and `proxyPort` parameters.
- AC7 — `GET /api/admin/setup/proxy/{thingId}/pac-file` with missing
  `proxyHost` or `proxyPort` returns HTTP 400 with a JSON error body.
- AC8 — Both endpoints return HTTP 404 when the requested `thingId` does
  not exist or is not of type `compliance-proxy`.
- AC9 — Both endpoints are absent from unauthenticated requests (401) and
  from requests whose token lacks `admin:ReadSettings` (403).
- AC10 — Admin audit log contains one event per download call with the
  documented fields.
- AC11 — `go test -race -count=1 ./packages/control-plane/internal/handler/...`
  passes with the new tests included.

---

## 7. Risks

- **R1** — Template syntax mismatch: Go `text/template` and the hand-edited
  placeholders previously used `{{UPPER_CASE}}` as documentation; converting
  to `{{.CamelCase}}` breaks any operator who was literally copying the
  template from the file. Mitigation: update the file header comment with the
  new field names and examples; the template is also served via the endpoint
  so manual use cases shift to using the UI download.
- **R2** — `interception_domain` rows may use GLOB patterns (e.g.
  `*.openai.com`) that are not valid as `dnsDomainIs` first arguments. The
  PAC builder must strip leading `*.` or skip wildcard patterns with a log
  warning. Mitigation: the fallback list is always fully qualified.
- **R3** — XML entities in `Organization` (e.g. `&`, `<`) could break plist
  XML structure. Mitigation: use `html.EscapeString` on the `Organization`
  value before template execution, or use `text/template`'s built-in HTML
  escaping by switching the `<data>` key to a template action guarded by
  `html/template` — simpler: validate that `Organization` contains only
  printable non-XML-special characters and return 400 otherwise.
