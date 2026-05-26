# E40-S6 — CP-UI Setup Section

Status: draft
Epic: E40 — Setup Guide
Story: S6 — Control Plane UI Setup Section

## User Story

As an IT Admin or Developer, I want a dedicated Setup section in the Control
Plane admin UI that walks me through configuring and verifying each Nexus
Gateway deployment mode — AI Gateway, Compliance Proxy, and Agent — so that
I can self-serve the initial deployment without consulting external
documentation or professional services.

---

## 1. Problem

The Control Plane UI currently has no self-service onboarding surface.
Administrators must discover CA certificate download, MDM profile generation,
PAC file configuration, enrollment token creation, and SDK code samples by
consulting separate runbooks. This drives professional services costs, delays
initial deployments, and creates support tickets when configuration steps are
performed out of order.

---

## 2. Goal

Add a "Setup" top-level navigation section to the CP-UI sidebar with three
sub-pages:

- **AI Gateway** — SDK code snippets auto-populated with the selected Virtual
  Key and gateway base URL.
- **Compliance Proxy** — CA certificate download, MDM profile download, PAC
  file generator, onboarding toggle, per-OS trust instructions, and
  verification commands.
- **Agent** — Platform download links, enrollment token generator, and
  per-platform install instructions.

All text is internationalised under the `setup:` namespace. An API service
layer `src/api/setup.ts` provides typed fetch wrappers for the four
E40-specific Control Plane relay endpoints.

---

## 3. Non-goals

- Hosting agent binary artefacts (`.pkg`, `.msi`, `.deb`, `.rpm`) — download
  links point to a configuration constant; actual hosting is out of scope.
- Multi-step wizard flow with progress persistence — the three sub-pages are
  independent; there is no wizard state machine.
- Any changes to the Control Plane backend beyond the relay API endpoints
  defined in FR-3 (covered in S1–S3 SDDs).

---

## 4. Design

### 4.1 Navigation and Routing

A new `'setup'` entry is added to `NavSectionKey` in
`packages/control-plane-ui/src/routes/shellRouteConfig.tsx`. The section
metadata is:

```ts
setup: { titleKey: 'setup', collapsible: true, defaultOpen: false },
```

Three routes are registered in `shellRouteConfig.tsx` (exact paths):

| Path | Lazy component | Section | Label key | Order |
|------|---------------|---------|-----------|-------|
| `setup/ai-gateway` | `LazySetupAIGatewayPage` | `setup` | `aiGateway` | 0 |
| `setup/proxy` | `LazySetupProxyPage` | `setup` | `complianceProxy` | 1 |
| `setup/agent` | `LazySetupAgentPage` | `setup` | `agent` | 2 |

Navigating to `/setup` redirects to `/setup/ai-gateway` (handled by a
`<Navigate replace to="setup/ai-gateway" />` index route).

All three routes are allowed for `['super-admins', 'compliance-admins']`
(consistent with other admin-level sections).

### 4.2 Page File Structure

```
packages/control-plane-ui/src/pages/setup/
    SetupAIGatewayPage.tsx
    SetupProxyPage.tsx
    SetupAgentPage.tsx
```

There is no shared `SetupLayout.tsx` — the Shell already provides the
sidebar + content layout. Each page uses the existing page-level layout
pattern (heading + content area).

### 4.3 AI Gateway Page (`SetupAIGatewayPage.tsx`)

**`VirtualKeyPicker` component** (inline, not shared):

- `useApi` with key `['admin', 'virtual-keys', 'list', 'setup']` to call the
  existing virtual-key list endpoint.
- Renders a `<Select>` dropdown of VK names; stores the selected VK value
  (the raw key string or a reference to the VK ID used to fetch the full
  key) in local `useState`.
- Shows a "Create new VK" link navigating to the VK management page.

**Code snippet panel:**

Three tabs (Python / Node.js / curl), each showing a read-only `<pre>` code
block. Snippets are derived from two values: `NEXUS_GATEWAY_URL` (sourced
from a runtime config constant, e.g. `window.__CONFIG__.gatewayBaseUrl`) and
the selected VK value.

Python snippet:

```python
from openai import OpenAI

client = OpenAI(
    base_url="{{NEXUS_GATEWAY_URL}}/v1",
    api_key="{{SELECTED_VK}}",
)

response = client.chat.completions.create(
    model="anthropic/claude-sonnet-4-5",
    messages=[{"role": "user", "content": "Hello!"}],
)
print(response.choices[0].message.content)
```

Node.js snippet:

```javascript
import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "{{NEXUS_GATEWAY_URL}}/v1",
  apiKey: "{{SELECTED_VK}}",
});

const response = await client.chat.completions.create({
  model: "anthropic/claude-sonnet-4-5",
  messages: [{ role: "user", content: "Hello!" }],
});
console.log(response.choices[0].message.content);
```

curl snippet:

```bash
curl {{NEXUS_GATEWAY_URL}}/v1/chat/completions \
  -H "Authorization: Bearer {{SELECTED_VK}}" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "anthropic/claude-sonnet-4-5",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

Each snippet block has a copy-to-clipboard button (using the browser
`navigator.clipboard.writeText` API, with a fallback for older environments).
When no VK is selected, the placeholders show `<YOUR_API_KEY>`.

### 4.4 Compliance Proxy Page (`SetupProxyPage.tsx`)

**`ProxyThingPicker` component** (inline):

- `useApi` with key `['admin', 'nodes', 'list', 'compliance-proxy']` — calls
  the existing nodes/things list endpoint filtered by `type=compliance-proxy`.
- Renders a `<Select>` dropdown; stores `thingId` in local `useState`.
- Shows a loading skeleton while the list loads.

**CA Certificate section:**

- "Download CA Cert" button: calls `getProxyCACert(thingId)` from
  `src/api/setup.ts`; triggers a browser file download
  (`nexus-proxy-ca.pem`).
- On successful download, computes and displays the SHA-256 fingerprint of
  the PEM bytes client-side using the Web Crypto API
  (`crypto.subtle.digest('SHA-256', certBuffer)`).
- Shows a loading spinner on the button while the download is in progress;
  shows an inline error banner on failure.

**MDM Profile section:**

- Organization name text input (`<input type="text">`).
- "Download MDM Profile" button: calls `downloadMDMProfile(thingId, org)`
  from `src/api/setup.ts`; triggers a browser file download
  (`nexus-proxy.mobileconfig`).
- Button is disabled when no thing is selected.

**PAC File section:**

- Proxy host text input and proxy port number input.
- "Download PAC File" button: calls `downloadPACFile(thingId, host, port)`
  from `src/api/setup.ts`; triggers a browser file download
  (`nexus-proxy.pac`).
- Button is disabled when host or port is empty.

**Per-OS Installation Instructions:**

Tabs with three OS entries: macOS, Windows, Linux.

- macOS tab: two sub-sections. "Manual" shows `sudo security add-trusted-cert
  -d -r trustRoot -k /Library/Keychains/System.keychain nexus-proxy-ca.pem`
  as a copyable code block; "MDM" shows instructions to deploy the downloaded
  `.mobileconfig` via Jamf/Mosyle/Intune.
- Windows tab: `certutil -addstore -f Root nexus-proxy-ca.crt` as a copyable
  code block; GPO note for enterprise distribution.
- Linux tab: Debian/Ubuntu and RHEL/CentOS sub-sections, each with the
  appropriate copy command and update command.

**Onboarding mode toggle:**

- Toggle switch (`<Switch>`): shows current state (Enabled/Disabled) fetched
  from the thing's shadow reported state (`onboarding.enabled` field).
- On toggle: calls `patchOnboardingMode(thingId, enabled)` from
  `src/api/setup.ts`; optimistically updates local state; rolls back on
  API error.
- Status badge (green "Enabled" / grey "Disabled") adjacent to the toggle.
- Explanatory text (i18n key `setup:proxy.onboardingDescription`) describing
  what onboarding mode does.

**Verification section:**

Two copyable code blocks:

```bash
# Verify TLS interception is working
openssl s_client -connect api.openai.com:443 \
  -proxy <proxy-host>:3128 \
  -CAfile nexus-proxy-ca.pem \
  </dev/null 2>&1 | grep "Verify return code"
```

```bash
# Verify with curl
curl -x <proxy-host>:3128 \
  --cacert nexus-proxy-ca.pem \
  https://api.openai.com/v1/models \
  -H "Authorization: Bearer <your-openai-key>"
```

The `<proxy-host>` placeholder is replaced with the `proxyHost` input value
when the user has entered one.

### 4.5 Agent Page (`SetupAgentPage.tsx`)

**Download links section:**

Three platform rows (macOS, Windows, Linux) each with a download link button.
Links are constructed from a compile-time constant
`AGENT_DOWNLOAD_BASE_URL` (e.g. `https://releases.nexus.ai/agent`). Exact
filenames per platform:

- macOS: `nexus-agent-<VERSION>-darwin-arm64.pkg`
- Windows: `nexus-agent-<VERSION>-windows-amd64.msi`
- Linux (deb): `nexus-agent-<VERSION>-linux-amd64.deb`
- Linux (rpm): `nexus-agent-<VERSION>-linux-amd64.rpm`

`<VERSION>` is read from the runtime config constant
`window.__CONFIG__.agentVersion` (falls back to `latest`).

**Enrollment token section:**

- "Generate Token" button: calls `POST /api/admin/enrollment/tokens` (the
  existing enrollment token creation endpoint; not a new endpoint).
- On success: displays the token string with a copy button and an expiry
  timestamp (`Expires: <ISO datetime>`).
- Token is shown only once (matches the existing API behaviour); refreshing
  the page does not re-display it.

**Per-platform install command:**

Tab strip (macOS / Windows / Linux), each showing a copyable install command
with the generated token pre-filled in the `--enrollment-token` flag. If no
token has been generated yet, the placeholder `<ENROLLMENT_TOKEN>` is shown.

Example (macOS):

```bash
sudo installer -pkg nexus-agent-<VERSION>-darwin-arm64.pkg -target /
sudo launchctl start com.nexus.agent
# Then configure the enrollment token:
sudo nexus-agent enroll --token <ENROLLMENT_TOKEN>
```

**Navigation link:**

"View enrolled devices →" link navigating to `/infrastructure/nodes`.

### 4.6 API Service Layer (`src/api/setup.ts`)

```ts
// Fetch the proxy Sub-CA certificate PEM for the given compliance-proxy thing.
// Returns the raw PEM string. Throws on HTTP error.
export async function getProxyCACert(thingId: string): Promise<string>

// Download an MDM configuration profile (.mobileconfig) for the given proxy.
// Triggers a browser file download. Returns void on success; throws on error.
export async function downloadMDMProfile(
  thingId: string,
  organization: string,
): Promise<void>

// Download a PAC file for the given proxy and host/port combination.
// Triggers a browser file download. Returns void on success; throws on error.
export async function downloadPACFile(
  thingId: string,
  proxyHost: string,
  proxyPort: number,
): Promise<void>

// Toggle onboarding mode for the given compliance-proxy thing.
// enabled=true activates the 407-intercept path; false deactivates.
export async function patchOnboardingMode(
  thingId: string,
  enabled: boolean,
): Promise<void>
```

All four functions use the existing `apiClient` (or `client.ts`) from
`src/api/` for authentication headers. File downloads use a `Blob` response
and `URL.createObjectURL` + a temporary `<a>` element with `download`
attribute to trigger the browser download dialog.

### 4.7 i18n Keys

All user-visible strings use `t('setup:<key>')` from `react-i18next`. Keys
are added to `src/i18n/locales/{en,zh,es}/pages.json` under a `setup` top-
level object.

Key structure (English values shown for reference):

```json
{
  "setup": {
    "aiGateway": {
      "title": "AI Gateway Setup",
      "description": "Connect your application to Nexus Gateway using a Virtual Key.",
      "selectVk": "Select Virtual Key",
      "createVk": "Create new Virtual Key",
      "snippetTitle": "Code Snippets",
      "tabs": { "python": "Python", "nodejs": "Node.js", "curl": "curl" },
      "copy": "Copy",
      "copied": "Copied!"
    },
    "proxy": {
      "title": "Compliance Proxy Setup",
      "description": "Configure TLS inspection and distribute the CA certificate to endpoints.",
      "selectProxy": "Select Proxy Instance",
      "caCert": {
        "title": "CA Certificate",
        "download": "Download CA Cert",
        "fingerprint": "SHA-256 Fingerprint"
      },
      "mdmProfile": {
        "title": "MDM Profile",
        "orgLabel": "Organization Name",
        "download": "Download MDM Profile"
      },
      "pacFile": {
        "title": "PAC File",
        "proxyHostLabel": "Proxy Host",
        "proxyPortLabel": "Proxy Port",
        "download": "Download PAC File"
      },
      "installInstructions": {
        "title": "CA Trust Installation",
        "macos": "macOS",
        "windows": "Windows",
        "linux": "Linux",
        "macosManual": "Manual Installation",
        "macosMdm": "MDM Deployment",
        "linuxDebian": "Debian / Ubuntu",
        "linuxRhel": "RHEL / CentOS / Fedora"
      },
      "onboarding": {
        "title": "Onboarding Mode",
        "description": "When enabled, users accessing AI providers through the proxy see a setup guide instead of a TLS error. Disable after CA trust is distributed.",
        "enabled": "Enabled",
        "disabled": "Disabled",
        "toggle": "Toggle onboarding mode"
      },
      "verification": {
        "title": "Verify Setup",
        "opensslCmd": "Verify with openssl",
        "curlCmd": "Verify with curl"
      }
    },
    "agent": {
      "title": "Agent Setup",
      "description": "Install the Nexus Agent on endpoint devices to enable local traffic interception.",
      "downloads": {
        "title": "Download Agent",
        "macos": "macOS (Apple Silicon / Intel)",
        "windows": "Windows (x64)",
        "linuxDeb": "Linux (Debian / Ubuntu)",
        "linuxRpm": "Linux (RHEL / CentOS / Fedora)"
      },
      "enrollmentToken": {
        "title": "Enrollment Token",
        "generate": "Generate Token",
        "expires": "Expires",
        "copy": "Copy Token"
      },
      "installInstructions": {
        "title": "Installation",
        "macos": "macOS",
        "windows": "Windows",
        "linux": "Linux"
      },
      "viewNodes": "View enrolled devices"
    }
  }
}
```

Chinese (`zh`) and Spanish (`es`) translations must provide all the same keys
with appropriate localised values. Technical terms (`VK`, `API`, `MDM`, `PAC`,
`CA`, `mTLS`, `curl`, `openssl`, `PKG`, `MSI`, `DEB`, `RPM`) stay in English
across all locales.

---

## 5. Tasks

- T1 — Update `src/routes/shellRouteConfig.tsx`:
  - T1.1 — Add `'setup'` to the `NavSectionKey` union type.
  - T1.2 — Add `setup: { titleKey: 'setup', collapsible: true, defaultOpen: false }` to the `NAV_SECTION_META` map.
  - T1.3 — Add the lazy import entries for all three Setup pages to the lazy loader file (wherever `LazyDashboardPage` and similar are declared).
  - T1.4 — Add the three route entries + the index-redirect route to `SHELL_ROUTES`.

- T2 — Create `src/pages/setup/SetupAIGatewayPage.tsx`:
  - T2.1 — Implement `VirtualKeyPicker` using `useApi(['admin', 'virtual-keys', 'list', 'setup'])`.
  - T2.2 — Wire selected VK value to the three snippet templates (Python, Node.js, curl).
  - T2.3 — Render three-tab code panel; each tab has a copy button.
  - T2.4 — All labels, headings, and tab names use `t('setup:aiGateway.*')` keys.

- T3 — Create `src/pages/setup/SetupProxyPage.tsx`:
  - T3.1 — Implement `ProxyThingPicker` using `useApi(['admin', 'nodes', 'list', 'compliance-proxy'])` with type filter.
  - T3.2 — Implement CA Cert download: call `getProxyCACert`, compute SHA-256 fingerprint via Web Crypto, trigger download.
  - T3.3 — Implement MDM Profile download: org-name input + call `downloadMDMProfile`.
  - T3.4 — Implement PAC file download: host + port inputs + call `downloadPACFile`.
  - T3.5 — Implement per-OS install instructions tabs (macOS / Windows / Linux) with copyable code blocks.
  - T3.6 — Implement onboarding toggle: fetch current state from thing shadow; call `patchOnboardingMode` on change; show status badge.
  - T3.7 — Implement verification section: two copyable openssl / curl commands with proxy-host substitution.
  - T3.8 — All labels use `t('setup:proxy.*')` keys.

- T4 — Create `src/pages/setup/SetupAgentPage.tsx`:
  - T4.1 — Render platform download buttons using `AGENT_DOWNLOAD_BASE_URL` constant.
  - T4.2 — Implement "Generate Token" button: POST to existing enrollment token API; display token + expiry + copy button.
  - T4.3 — Render per-platform install command tabs with token placeholder or generated token pre-filled.
  - T4.4 — Render "View enrolled devices" link to `/infrastructure/nodes`.
  - T4.5 — All labels use `t('setup:agent.*')` keys.

- T5 — Create `src/api/setup.ts`:
  - T5.1 — Implement `getProxyCACert(thingId)` — `GET /api/admin/setup/proxy/{thingId}/ca-cert` → return response text.
  - T5.2 — Implement `downloadMDMProfile(thingId, organization)` — `GET /api/admin/setup/proxy/{thingId}/mdm-profile?organization=<org>` → Blob download via anchor element.
  - T5.3 — Implement `downloadPACFile(thingId, proxyHost, proxyPort)` — `GET /api/admin/setup/proxy/{thingId}/pac-file?proxyHost=<h>&proxyPort=<p>` → Blob download via anchor element.
  - T5.4 — Implement `patchOnboardingMode(thingId, enabled)` — `PATCH /api/admin/setup/proxy/{thingId}/onboarding` with `{"enabled": true|false}`.
  - T5.5 — All functions throw on non-2xx HTTP responses; error message is extracted from the JSON `error` field.

- T6 — Add i18n keys:
  - T6.1 — Add the complete `setup` key tree to `src/i18n/locales/en/pages.json`.
  - T6.2 — Add the complete `setup` key tree to `src/i18n/locales/zh/pages.json` with Chinese translations.
  - T6.3 — Add the complete `setup` key tree to `src/i18n/locales/es/pages.json` with Spanish translations.
  - T6.4 — Copy all three updated locale files to `public/locales/{en,zh,es}/pages.json`.
  - T6.5 — Verify key counts match across all three files (script or manual diff).

- T7 — Update nav i18n: add `setup` label key to `src/i18n/locales/{en,zh,es}/nav.json` (the nav section title for the sidebar).

- T8 — Vitest unit tests:
  - T8.1 — `SetupAIGatewayPage.test.tsx`: render with mocked VK list API response; assert dropdown shows VK names; assert selecting a VK updates the Python snippet text.
  - T8.2 — `SetupProxyPage.test.tsx`: render with mocked nodes list; assert "Download CA Cert" button calls `getProxyCACert`; assert "Download MDM Profile" button is disabled when no thing is selected.
  - T8.3 — `SetupProxyPage.test.tsx`: onboarding toggle test — mock `patchOnboardingMode`; click toggle; assert API called with `{ enabled: true }`.
  - T8.4 — `SetupAgentPage.test.tsx`: render; assert "Generate Token" button calls the enrollment token API; assert token is displayed after success.

---

## 6. Acceptance Criteria

- AC1 — A "Setup" section appears in the CP-UI sidebar nav; clicking it
  expands to show "AI Gateway", "Compliance Proxy", and "Agent" sub-items.
- AC2 — Navigating to `/setup` redirects to `/setup/ai-gateway` without a
  404.
- AC3 — AI Gateway page: selecting a different VK from the dropdown updates
  all three code snippets (Python, Node.js, curl) in real time, with the new
  VK value substituted for the API key placeholder.
- AC4 — AI Gateway page: each code tab's copy button writes the snippet text
  to the clipboard.
- AC5 — Compliance Proxy page: clicking "Download CA Cert" triggers a
  browser file download named `nexus-proxy-ca.pem`; after download, a
  SHA-256 fingerprint string is displayed below the button.
- AC6 — Compliance Proxy page: onboarding toggle calls
  `PATCH /api/admin/setup/proxy/{thingId}/onboarding` and the status badge
  updates to reflect the new state without a full page reload.
- AC7 — Compliance Proxy page: "Download MDM Profile" button is disabled
  when no proxy thing is selected from the dropdown.
- AC8 — Compliance Proxy page: "Download PAC File" button is disabled when
  either `proxyHost` or `proxyPort` is empty.
- AC9 — Agent page: clicking "Generate Token" shows the enrollment token in
  a code block with a copy button; the displayed expiry timestamp is in the
  future.
- AC10 — Agent page: "View enrolled devices" link navigates to
  `/infrastructure/nodes`.
- AC11 — No JSX in any Setup page component contains a hardcoded English
  string that should be translated; all visible text uses `t('setup:...')`.
- AC12 — Key counts in `en/pages.json`, `zh/pages.json`, and `es/pages.json`
  are equal after the i18n additions (no missing keys in any locale).
- AC13 — `npm run build` (Vite production build) completes without TypeScript
  errors in the new files.
- AC14 — All Vitest tests in T8 pass (`npm test` or `npx vitest run`).

---

## 7. Risks

- **R1** — The existing nodes/things list API may not support filtering by
  `type=compliance-proxy` as a query parameter. Mitigation: confirm the
  existing `GET /api/admin/nodes` (or equivalent) endpoint supports type
  filtering; if not, filter client-side after fetching all nodes (with a
  TODO for server-side filtering). The `useApi` query key must include a
  `'compliance-proxy'` discriminator to avoid cache collisions with other
  node-list callers.
- **R2** — `navigator.clipboard.writeText` is not available in non-secure
  (non-HTTPS) contexts during local development. Mitigation: wrap in a
  try/catch; fall back to a `document.execCommand('copy')` approach via a
  hidden `<textarea>` if the Clipboard API is unavailable.
- **R3** — The Web Crypto `crypto.subtle.digest` API is async; the
  fingerprint must be computed in a `useEffect` after the CA PEM is fetched,
  not synchronously during render. Mitigation: use a local `useState` for the
  fingerprint value, set it in a `useEffect` triggered by the PEM state
  variable.
- **R4** — The enrollment token API response format may change independently
  of E40. Mitigation: use a typed response interface defined in
  `src/api/setup.ts`; any mismatch surfaces as a TypeScript type error at
  compile time.
- **R5** — Locale key-count divergence is easy to introduce when adding new
  keys. Mitigation: add a CI step (or at minimum a pre-commit check) that
  compares key counts across the three locale files. For E40, manual diff
  verification (T6.5) is the minimum gate.
