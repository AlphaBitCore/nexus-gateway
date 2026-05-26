---
doc: agent-browser-opener-architecture
area: service
service: agent
tier: 1
updated: 2026-05-21
---

# Agent Browser Opener Architecture

> **Tier 3 architecture doc.** Read when touching `packages/agent/internal/host/openbrowser/` or the user-side OAuth callback. Sister doc: `agent-sso-enrollment-architecture.md` (parent flow).

The agent opens the user's default browser to start OAuth+PKCE flows (user SSO, optional re-authentication). The browser-opener is platform-specific glue around the OAuth flow.

---

## 1. Per-platform launchers

| Platform | Launch command |
|---|---|
| macOS | `open <url>` (via `os/exec`) |
| Windows | `rundll32 url.dll,FileProtocolHandler <url>` |
| Linux | `xdg-open <url>` |

The launcher is a thin shell-out; the daemon doesn't bundle a browser. The child process is launched under a 5s `context.WithTimeout` so a stuck launcher cannot block the IPC handler forever. Source: `packages/agent/internal/host/openbrowser/openbrowser.go` (`dispatch`).

### Safety checks `Opener.Open` enforces

Every shell-out goes through `Opener.Open` (never directly from the WebView); the function rejects the request before invoking the platform launcher when **any** of:

1. The URL fails to parse.
2. The scheme is anything other than `https` (`http://` and custom schemes are rejected).
3. The hostname is empty.
4. The hostname (lowercased, port stripped) is not present in the operator-configured allowlist set via `Opener.SetAllowedHosts(...)`. The allowlist is normally just the Control Plane base URL the daemon learns from Hub bootstrap; the Dashboard uses it for "Manage in admin console" links.

These checks defend against a compromised renderer invoking arbitrary URLs / file:// targets / phishing redirects.

## 2. Why use the system browser

- Respects the user's default browser preference.
- Gets the user's existing cookies for the IdP (single sign-on actually works).
- Uses the user's existing TLS trust store (legitimate certs work without bundle tweaks).
- No browser dependency in the agent binary (smaller install).

## 3. The localhost callback

The daemon binds an ephemeral OS-assigned port on `127.0.0.1:0` (no configured range — `net.Listen("tcp", "127.0.0.1:0")` per `packages/agent/internal/identity/enrollment/sso_server.go`). The chosen port is reported by `callbackServer.Port()` and substituted into the OAuth `redirect_uri` for that flow.

```
http://127.0.0.1:<ephemeral>/callback?code=...&state=...
```

The callback server is a single-use HTTP server with a `/callback` handler that buffers the first `(code, state, error)` tuple, displays a small "you may close this window" HTML page, and tears itself down with a 500 ms graceful shutdown deadline after the first hit (or when `Wait(ctx)` is cancelled by the caller).

## 3a. IPC entry point

The IPC channel into `Opener.Open` is the daemon's statusapi `OPEN_BROWSER?url=...` command — wired by `statusServer.SetOpenBrowserFn(browserOpener.Open)` in `packages/agent/cmd/agent/cmd_run.go`. Any caller that wants to trigger a browser-open (the Wails Dashboard, the menu-bar app) sends that single-line text command over the IPC socket; the statusapi handler invokes `Opener.Open` with the supplied URL, which runs the §1 safety checks before dispatching.

## 4. Headless / restricted-environment fallback

If browser launch fails (corporate lockdown, terminal-only env, broken xdg-open):

1. Daemon detects launch failure (exit code non-zero OR no callback within the SSO flow's outer timeout).
2. Daemon notifies UI via IPC.
3. UI displays the URL with a copy-to-clipboard button + "Open this URL in any browser to sign in" guidance.

The user can then paste the URL on another device (e.g., phone) that has a browser. The OAuth flow still completes via the localhost callback only if the browser navigation lands on the same host where the daemon is listening — the redirect URI is `http://127.0.0.1:<ephemeral>/callback`. If the OAuth user is on a different device, the redirect fails and they must complete the flow on the agent host.

## 5. Security considerations

- Launching the system browser is "anyone with shell-exec privilege can do it" — no escalation.
- The localhost callback is bound to `127.0.0.1` — not reachable from other machines.
- The OAuth `state` parameter prevents CSRF.
- The PKCE `code_challenge` prevents code-interception attacks.
- `Opener.Open`'s hostname allowlist + https-only checks bound the set of URLs the daemon will ever launch — a compromised renderer cannot redirect users to arbitrary phishing pages.
- The `dispatch` function refuses to spawn a real browser when running under `go test` (`testing.Testing()`); tests must stub a seam rather than call `Open` directly.

## 6. Sources

- `packages/agent/internal/host/openbrowser/openbrowser.go` — `Opener.New`, `SetAllowedHosts`, `Open`, `dispatch`.
- `packages/agent/internal/identity/enrollment/sso_server.go` — `callbackServer` (`127.0.0.1:0`, `/callback` handler).
- `packages/agent/internal/identity/enrollment/sso_flow.go` + `sso_pkce.go` — parent SSO flow.
- `packages/agent/cmd/agent/cmd_run.go` — `statusServer.SetOpenBrowserFn(browserOpener.Open)` wiring.
- `packages/agent/internal/host/trayipc/` — IPC for UI ↔ daemon.

## 7. Cross-references

- `agent-sso-enrollment-architecture.md` — full flow.
- `oauth-pkce-admin-auth-architecture.md` — server-side AS.
- `agent-tray-ipc-architecture.md` — IPC the UI uses to trigger.
