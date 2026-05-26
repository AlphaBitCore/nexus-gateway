# D3b: Device Auth Modes (Enterprise Login) Design

**Date:** 2026-04-13
**Status:** Approved
**Scope:** Agent enterprise login via OIDC, control plane auth endpoints, admin config UI
**Parent:** `docs/superpowers/specs/2026-04-13-d3-handoff.md`
**Dependencies:** D2 (OIDC/SSO) complete, D3a/c/d complete

---

## Overview

Adds optional user-level authentication for agents beyond device mTLS. When enterprise-login mode is enabled, the agent GUI can trigger an OAuth login flow that opens the user's browser, authenticates via the organization's IdP, and links the NexusUser identity to the device.

**Key design decision:** Control plane exchanges the auth code with the IdP (Option A). The agent only captures the code via a local loopback callback server and forwards it. This keeps the client secret on the server side and reuses existing D2 OIDC exchange logic.

---

## Authentication Flow

1. Admin enables enterprise-login mode via `PUT /api/admin/settings/device-auth`
2. GUI sends `AUTHENTICATE` command to agent via status API (Unix socket/named pipe)
3. Agent calls `GET /api/agent/auth-config` to fetch OIDC parameters (clientId, authorizeUrl, scopes)
4. Agent generates random `state` parameter for CSRF protection
5. Agent starts local HTTP server on `127.0.0.1:0` (OS-assigned random port)
6. Agent opens browser to IdP authorize URL: `{authorizeUrl}?client_id={clientId}&redirect_uri=http://127.0.0.1:{port}/callback&response_type=code&scope={scopes}&state={state}`
7. User authenticates in browser
8. IdP redirects to `http://127.0.0.1:{port}/callback?code={code}&state={state}`
9. Agent validates state, captures code, shuts down local server
10. Agent sends `POST /api/agent/authenticate` with `{code, redirectUri}` (via mTLS)
11. Control plane exchanges code with IdP using agent OIDC config (client secret stays server-side)
12. Control plane validates JWT, extracts sub/email/name
13. Control plane calls `FindOrCreateSSOUser` with `canAccessControlPlane=false`
14. Control plane creates DeviceAssignment (source: "enterprise-login")
15. Control plane returns `{userId, displayName, email}` to agent
16. Agent stores auth state locally, reports success to GUI
17. Local callback server serves "Login successful, you can close this tab" HTML page

---

## Control Plane Endpoints

### Agent API (mTLS-protected)

**`GET /api/agent/auth-config`**
- Returns OIDC parameters needed by agent to initiate OAuth flow
- Response: `{enabled: bool, authorizeUrl: string, clientId: string, scopes: string}`
- Reads from `system_metadata` key `"oidc.agent"`
- Does NOT expose client secret
- If mode is "mtls-only" or no OIDC configured: `{enabled: false}`

**`POST /api/agent/authenticate`**
- Request: `{code: string, redirectUri: string}`
- Device identity from mTLS context (`AgentDeviceFromContext`)
- Exchanges code with IdP using full agent OIDC config (including client secret)
- Validates JWT claims (sub, email, name)
- Calls `FindOrCreateSSOUser(ssoSubject, "agent-oidc", email, displayName)` with `canAccessControlPlane=false`
- Releases existing "enterprise-login" DeviceAssignment if any, creates new one
- Audit logs the event (action: "authenticate", entityType: "agentDevice")
- Response: `{userId: string, displayName: string, email: string}`

**`GET /api/agent/auth-status`**
- Returns current enterprise-login auth state for the device
- Looks up active DeviceAssignment with source "enterprise-login" for the device
- If found, joins NexusUser to get display info
- Response: `{authenticated: bool, userId?: string, displayName?: string, email?: string}`

### Admin API (IAM-protected)

**`GET /api/admin/settings/device-auth`**
- Response: `{mode: "mtls-only" | "enterprise-login", oidcConfigured: bool}`
- Reads mode from `system_metadata` key `"device.auth.mode"` (default: "mtls-only")
- Checks if `"oidc.agent"` config exists and has issuer set

**`PUT /api/admin/settings/device-auth`**
- Request: `{mode: "mtls-only" | "enterprise-login", oidc?: {issuer, clientId, clientSecret, authorizeUrl, tokenUrl, scopes}}`
- Stores mode in `system_metadata` key `"device.auth.mode"`
- If oidc provided, stores in `system_metadata` key `"oidc.agent"`
- Audit logs the change
- Response: `{mode, oidcConfigured: bool}`

---

## Agent Components

### New Package: `packages/agent/core/enterpriseauth/`

**`auth.go`** — OAuth flow orchestrator
```go
type LoginResult struct {
    UserID      string `json:"userId"`
    DisplayName string `json:"displayName"`
    Email       string `json:"email"`
}

type AuthConfig struct {
    Enabled      bool   `json:"enabled"`
    AuthorizeURL string `json:"authorizeUrl"`
    ClientID     string `json:"clientId"`
    Scopes       string `json:"scopes"`
}

func StartLogin(ctx context.Context, client GatewayClient) (*LoginResult, error)
```
- Calls `client.GetAuthConfig()` — returns error if not enabled
- Generates random state (crypto/rand, 32 bytes, hex-encoded)
- Starts local callback server
- Opens browser via `exec.Command` (os-specific: `open` on macOS, `xdg-open` on Linux, `rundll32` on Windows)
- Waits for callback with 2-minute timeout
- Validates state matches
- Calls `client.Authenticate(code, redirectUri)`
- Returns result or error

**`server.go`** — Local loopback callback server
```go
type CallbackServer struct {
    Port     int
    codeCh   chan callbackResult
    srv      *http.Server
}

type callbackResult struct {
    Code  string
    State string
    Err   error
}

func NewCallbackServer() (*CallbackServer, error)
func (s *CallbackServer) WaitForCallback(ctx context.Context) (code, state string, err error)
func (s *CallbackServer) Close()
```
- Listens on `127.0.0.1:0`
- Single route: `GET /callback`
- Extracts `code` and `state` query params
- Serves HTML response: "Login successful, you can close this tab"
- Sends result on channel, shuts down after one callback
- Context cancellation triggers timeout error

### Gateway Client Additions

In `packages/agent/core/gateway/client.go`, add:

```go
type AuthConfig struct {
    Enabled      bool   `json:"enabled"`
    AuthorizeURL string `json:"authorizeUrl"`
    ClientID     string `json:"clientId"`
    Scopes       string `json:"scopes"`
}

type AuthResult struct {
    UserID      string `json:"userId"`
    DisplayName string `json:"displayName"`
    Email       string `json:"email"`
}

type AuthStatus struct {
    Authenticated bool   `json:"authenticated"`
    UserID        string `json:"userId,omitempty"`
    DisplayName   string `json:"displayName,omitempty"`
    Email         string `json:"email,omitempty"`
}

func (c *Client) GetAuthConfig(ctx context.Context) (AuthConfig, error)
func (c *Client) Authenticate(ctx context.Context, code, redirectUri string) (AuthResult, error)
func (c *Client) GetAuthStatus(ctx context.Context) (AuthStatus, error)
```

### Status API Addition

In `packages/agent/core/sync/statusapi/server.go`, add:
- `AUTHENTICATE` command in dispatch switch
- New callback: `authenticateFn func() (*enterpriseauth.LoginResult, error)`
- Command triggers `StartLogin`, returns JSON result to GUI
- If already authenticated or mode is mtls-only, returns appropriate error

---

## UI Updates

Update `packages/control-plane-ui/src/pages/settings/DeviceAuthSettingsPage.tsx`:
- Fetch current mode via `GET /api/admin/settings/device-auth`
- Radio/toggle for mode selection (mtls-only / enterprise-login)
- When enterprise-login selected, show OIDC config form fields: issuer, clientId, clientSecret, authorizeUrl, tokenUrl, scopes
- Save via `PUT /api/admin/settings/device-auth`
- Uses existing patterns: useApi, useMutation, Card, FormField, Input, Button

Add to `packages/control-plane-ui/src/api/services/fleet.ts`:
```typescript
getDeviceAuthSettings: () => api.get('/api/admin/settings/device-auth'),
updateDeviceAuthSettings: (data) => api.put('/api/admin/settings/device-auth', data),
```

---

## Storage

No new database tables. All config stored in existing `system_metadata` key-value table:

| Key | Value | Purpose |
|-----|-------|---------|
| `device.auth.mode` | `"mtls-only"` or `"enterprise-login"` | Auth mode toggle |
| `oidc.agent` | JSON OidcConfig (issuer, clientId, clientSecret, authorizeUrl, tokenUrl, scopes) | Agent OIDC provider config |

User identity stored in existing tables:
- `NexusUser` via `FindOrCreateSSOUser` (ssoProvider: "agent-oidc", canAccessControlPlane: false)
- `DeviceAssignment` with source: "enterprise-login"

---

## Security Considerations

- Client secret never leaves the control plane
- Local callback server binds to `127.0.0.1` only (not `0.0.0.0`)
- State parameter prevents CSRF on the callback
- 2-minute timeout prevents abandoned auth flows from leaking server resources
- Auth code is single-use (enforced by IdP)
- mTLS still required for the authenticate endpoint — device must be enrolled first

---

## Testing

- **Agent enterpriseauth**: unit test callback server (start, receive code, timeout)
- **Agent gateway client**: test new method signatures compile
- **Control plane handlers**: test authenticate endpoint logic (mock IdP exchange)
- **Control plane settings**: test get/put device-auth settings
- **UI**: build verification

---

## File Touch List

### New Files
- `packages/agent/core/enterpriseauth/auth.go`
- `packages/agent/core/enterpriseauth/server.go`
- `packages/agent/core/enterpriseauth/server_test.go`

### Modified Files
- `packages/agent/core/gateway/client.go` — add auth types + 3 methods
- `packages/agent/core/sync/statusapi/server.go` — add AUTHENTICATE command
- `packages/control-plane/internal/handler/agent_api.go` — add Authenticate, AuthConfig, AuthStatus handlers
- `packages/control-plane/internal/handler/admin_settings.go` — add device-auth settings endpoints
- `packages/control-plane/internal/handler/routes.go` or agent route registration — register new routes
- `packages/control-plane-ui/src/api/services/fleet.ts` — add settings endpoints
- `packages/control-plane-ui/src/pages/settings/DeviceAuthSettingsPage.tsx` — replace placeholder with real form

---

## Out of Scope

- Re-authentication intervals / token refresh
- Multiple IdP support (single agent OIDC config for now)
- Agent-side session persistence across restarts (can be added later)
- Device compliance checks at authentication time
