---
doc: agent-sso-enrollment-architecture
area: service
service: agent
tier: 1
updated: 2026-05-20
---

# Agent SSO Enrollment Architecture

> **Tier 2 architecture doc.** Read when touching the agent's SSO-driven enrollment flow under `packages/agent/internal/identity/enrollment/`. This is **one-shot enrollment**, not a long-lived user session: the user's OAuth code is exchanged once for an enrollment JWT, which is exchanged once with Hub for the device mTLS cert. After that, the agent talks to Hub via device cert only — there is no per-user bearer to refresh and no agent-side OAuth revoke flow.

The agent has one runtime identity: the **device** (mTLS cert + Hub-registered Thing). SSO is the *mechanism* used to mint that cert for the right org / user at enrollment time. The signed-in email is persisted (via `Manager.PersistSSOEmail`) for menu-bar display, but it is not a session token — it carries no API authority.

---

## 1. Why SSO is used at enrollment

The device cert needs an org-affiliated identity to be signed against. Rather than ask IT to mint long-lived enrollment tokens by hand, the agent uses the user's existing SSO credentials (via the standard CP OAuth + PKCE flow) to prove org membership exactly once. Hub validates the resulting enrollment JWT and signs the CSR. After that, the user can quit the UI or sign out of CP entirely — the device cert keeps working until rotated.

Traffic events attribute to the device's owner-of-record (set at enrollment), not to a live session.

## 2. The flow

Source: `packages/agent/internal/identity/enrollment/sso_flow.go`.

1. User opens the agent UI window.
2. UI shows "Sign in" button.
3. User clicks → UI calls IPC `OpenBrowser(url=<cpURL>/oauth/authorize?...)` on the daemon.
4. The daemon generates a PKCE verifier/challenge pair and a `state` nonce, then starts an ephemeral HTTP callback server on `127.0.0.1:0` (kernel-assigned port; see §3). The redirect URI is `http://127.0.0.1:<port>/callback`.
5. Daemon opens the OS default browser via platform-specific shell-out (`open` / `rundll32 url.dll,FileProtocolHandler` / `xdg-open`).
6. The OAuth + PKCE flow (cross-ref `oauth-pkce-admin-auth-architecture.md` §2) runs in the user's browser against the CP. The CP redirects to the agent's ephemeral callback with `?code=...&state=...`.
7. Daemon validates `state` (CSRF guard), then POSTs `<cpURL>/api/agent/sso-enroll` with `{code, code_verifier, redirect_uri}` to exchange the auth code for an **enrollment JWT** + user email.
8. Daemon generates a local mTLS keypair + CSR (and an Ed25519 attestation CSR for E60).
9. Daemon calls `HubEnroller.EnrollWithJWT(ctx, enrollmentJWT, HubEnrollRequest{...})`. Hub validates the JWT, signs the CSR, returns the device cert + thing_id.
10. Daemon persists the cert + private key via `Manager.PersistEnrollment` (atomic write); best-effort persists the user email via `Manager.PersistSSOEmail` so the menu bar can show it across restarts.
11. Daemon notifies UI: enrollment complete; UI updates to "Signed in as <email>".

From step 9 onward, the user's OAuth tokens are no longer in play. The agent uses the device cert (mTLS) for every subsequent Hub call.

## 3. Localhost callback port

The agent uses an **OS-assigned ephemeral port** — `net.Listen("tcp", "127.0.0.1:0")` in `newCallbackServer` (`sso_flow.go:173`). There is no fixed `17000-17999` range and no pre-registered set of redirect URIs.

The host is literal `127.0.0.1` (not `localhost`) so the OAuth redirect-URI exact-match check on the CP side is not affected by IPv4-vs-IPv6 resolution. The path is `/callback` (not `/agent-callback`). The full URI is `http://127.0.0.1:<ephemeral-port>/callback`; the CP's OAuth client config must allow the `127.0.0.1` host with any port (standard pattern for native-app PKCE flows per RFC 8252 §7.3).

## 4. Browser launcher behaviour

The agent does NOT bundle its own browser. Launching the system browser:

- Respects the user's default browser preference.
- Gets the user's existing cookies for the IdP (single sign-on actually works).
- Uses the user's existing TLS trust store (legitimate certs work without bundle tweaks).

If browser launch fails (rare; corp lockdown environments), the daemon falls back to displaying the URL in the UI window with "Open this in a browser to sign in" + copy button.

## 5. No token refresh

There is **no agent-side OAuth refresh flow**. The user-bearer (auth code) is consumed exactly once at step 7 above to produce an enrollment JWT, which is consumed exactly once at step 9 to mint the device cert. After that the agent talks to Hub via mTLS only — there is no bearer to refresh.

Device cert rotation is handled separately (cross-ref `agent-enrollment-architecture.md` — distinct flow).

## 6. No agent-side sign-out

There is **no agent-side OAuth revoke flow**. The agent has no live OAuth session to terminate — there are no bearer or refresh tokens stored on the agent (the keystore holds the device cert + private key, not OAuth tokens). To "deauthorize" a device, an admin revokes the device's mTLS cert in CP; the agent's next Hub call fails closed and the device must re-enroll.

What the UI sometimes calls "Sign out" is really "delete the persisted SSO email so the menu bar stops showing it" — purely cosmetic and local.

## 7. Multi-user (rare)

Because enrollment is one-shot and the device cert is shared across OS users on the host, "switching users" is really "re-enroll". An admin who wants to attribute the host to a different SSO identity revokes the existing device cert in CP and triggers a fresh enrollment from the new user's UI session.

This is not a heavily-supported scenario; most enterprise installs are one device → one owner-of-record.

## 8. The "headless" mode

A device whose UI was never opened still:

- Intercepts traffic via OS-level rules (NE on macOS, iptables on Linux, WinDivert on Windows).
- Runs the daemon pipeline.
- Emits traffic events to Hub.

Traffic events are attributed to the device's owner-of-record (set at enrollment). IT-driven mass enrollments often use a single seed user once per device; users on the host never open the UI.

## 9. JIT user provisioning (CP side)

If the user federates through an IdP (cross-ref `idp-sso-architecture.md` §6) and is JIT-provisioned, the daemon doesn't care — at step 7 it just POSTs the auth code and receives an enrollment JWT for a valid user. JIT happens entirely server-side.

A user with insufficient permissions (e.g., not member of any project) gets a 4xx from `/api/agent/sso-enroll` and the agent reports the enrollment failure in the UI. Admin grants access via IAM and the user retries the enrollment flow.

## 10. Sources

- `packages/agent/internal/identity/enrollment/` — flow implementation (sso_flow.go, sso_pkce.go, sso_server.go).
- `packages/agent/internal/host/openbrowser/` — platform-specific browser launcher.
- `packages/agent/internal/host/trayipc/` — IPC for UI ↔ daemon.

## 11. Cross-references

- `oauth-pkce-admin-auth-architecture.md` — OAuth+PKCE server side.
- `agent-keystore-architecture.md` §1 — what (and is not) stored on the agent (no OAuth bearer/refresh persisted).
- `agent-tray-ipc-architecture.md` — the IPC channel used.
- `idp-sso-architecture.md` — external IdP federation.
- `agent-enrollment-architecture.md` — distinct device-cert flow.
