# Install and Enrol — Agent UI feature doc

> Audience: end users and IT administrators installing the Nexus Agent on macOS (primary), Linux, or Windows, and enrolling a device into a Nexus Gateway organisation.

---

## macOS install flow

### Step 1 — Download the installer

Download the `.pkg` installer from your organisation's distribution point (e.g. `https://<your-nexus-host>/downloads/NexusAgent-latest.pkg`). Two build flavours are available:

| Flavour | Filename pattern | Notes |
|---|---|---|
| **pf-only** (default as of E74) | `NexusAgent-pf-<version>.pkg` | No system extension. No Privacy & Security approval required. |
| **NE legacy** | `NexusAgent-ne-<version>.pkg` | Includes the `NETransparentProxyProvider` system extension. |

If your IT team has not specified a flavour, use the pf-only build.

### Step 2 — Run the installer

Double-click the downloaded `.pkg` file and follow the standard macOS Installer prompts:

1. Introduction — click Continue.
2. License — read and click Agree.
3. Installation type — click Install.
4. Authenticate — enter your macOS administrator password.
5. Summary — click Close.

### System extension prompt (NE legacy builds only)

**This step applies to `NexusAgent-ne-<version>.pkg` installs only.**

After the installer completes, macOS will display a notification that a system software component has been blocked. To activate the Network Extension:

1. Open **System Settings → Privacy & Security**.
2. Scroll to the **Security** section and find the "System software from Nexus…" entry.
3. Click **Allow**.
4. Authenticate with Touch ID or your administrator password.
5. Click **Restart** if prompted (required on some macOS versions).

The agent UI will nudge you to complete this step if the extension is not yet active.

> **pf-only builds do not install a system extension and do not require the Privacy & Security approval step.** The install completes after the standard `.pkg` installer without any additional user action. The pf-only `.pkg` (built via `build-agent --target=pf-only`) compiles with `interceptMode="pf"` as the default and ships no NetworkExtension entitlement.

### Step 3 — Enrol the device

On first launch, the Nexus Agent will open a browser window or display an enrolment prompt:

1. Enter your organisation's **Hub URL** (e.g. `https://<your-nexus-host>`).
2. Sign in with your identity provider credentials (SSO / Nexus Local account as configured by your admin).
3. The agent fetches an enrolment token, generates a device keypair, and submits a Certificate Signing Request (CSR) to the Nexus Hub.
4. Upon approval, the agent receives a signed device certificate and begins normal operation.

### Step 4 — Verify the agent is running

Open the Nexus Agent tray icon. A green status indicator confirms:

- The agent daemon is running.
- The Hub connection is established.
- Interception is active (pf anchor loaded or NE extension active).

---

## Linux install flow

Download the `.deb` / `.rpm` package or the binary tarball from your organisation's distribution point. The installer creates a systemd unit (`nexus-agent.service`). After install, run `nexus-agent enroll --hub-url <url> --token <token>` (or `nexus-agent enroll-sso --hub-url <url>` for interactive SSO) to enrol the device — flag set matches `packages/agent/cmd/agent/cmd_enroll.go:89`.

No system extension or kernel-module approval step is required on Linux.

---

## Windows install flow

Download and run the `.msi` installer. The installer creates a Windows Service registered with SCM as `NexusAgent` (WiX internal id `NexusAgentSvc`). After install, open the Nexus Agent tray icon and follow the enrolment flow.

No driver signing approval step is required in standard builds.

---

## Post-install checklist

- [ ] Agent tray icon shows green status.
- [ ] Dashboard page shows device name and enrolled organisation.
- [ ] At least one traffic event appears in the Activity page after browsing a monitored site.
- [ ] (NE legacy builds only) System Settings → Privacy & Security shows the extension as "Allowed".

---

## Uninstall

### macOS (either build)

Run the canonical uninstall script that ships with the agent:

```bash
sudo bash /Library/Application\ Support/com.nexus-gateway.agent/Scripts/uninstall.sh
```

The script unloads the LaunchDaemon (`com.nexus-gateway.agent.plist`), removes the system extension when present, flushes the pf anchor (`pfctl -a nexus-agent/transparent -F all`) when the pf path was used, and cleans `/Library/Application Support/com.nexus-gateway.agent/` and `/Library/Logs/com.nexus-gateway.agent/`.

### Order matters for NE-legacy builds

The system extension must be deactivated before the daemon plist is removed — skipping that order can leave the extension registered with a missing daemon. The canonical uninstall script above sequences this correctly; avoid manual `launchctl unload` + `rm` recipes.

---

## Architecture references

- `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md` — intercept paths (NE and pf), recovery procedures.
- `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md` — fail-open invariants for both paths.
- `docs/developers/architecture/services/agent/agent-enrollment-architecture.md` — CSR + device cert lifecycle.
- `docs/operators/ops/runbooks/agent-recovery.md` — operator recovery runbook.
