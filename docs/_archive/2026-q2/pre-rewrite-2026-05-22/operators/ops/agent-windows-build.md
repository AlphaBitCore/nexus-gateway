# Agent — Windows MSI Build

Operator's reference for building, signing, and distributing the
Windows Nexus Agent installer (`NexusAgent-<VERSION>.msi`).

This file mirrors the macOS `build-agent` skill for shape: same
sections, same fail-recovery guidance, scoped to the Windows
toolchain.

---

## What the MSI ships

A single signed-or-unsigned `NexusAgent-<VERSION>.msi` that on
install (per-machine, perMachine MSI scope):

| Artifact | Path | Owner |
|---|---|---|
| `nexus-agent.exe` | `C:\Program Files\Nexus Agent\` | Windows Service `NexusAgent` (LocalSystem, auto-start) |
| `nexus-agent-tray.exe` | `C:\Program Files\Nexus Agent\` | HKLM Run key (per-user auto-start at login) |
| `nexus-dashboard.exe` | `C:\Program Files\Nexus Agent\` | Start-menu shortcut + tray "Open Dashboard" |
| `WinDivert64.sys` | `%SystemRoot%\System32\drivers\` | Kernel SCM service `WinDivert` (auto-start, ServiceDependency of `NexusAgent`) |
| `WinDivert.dll` | `C:\Program Files\Nexus Agent\` | userspace DLL (search-path resolution) |
| `%ProgramData%\NexusAgent\` | empty dir | LocalSystem rw + Users r — daemon writes `device-ca.pem`, `agent.yaml`, `Logs\` here on first boot |
| `NEXUS_DEVICE_CA_PEM` | HKLM env | `[CommonAppDataFolder]NexusAgent\device-ca.pem` (always set — our own var) |
| `NODE_EXTRA_CA_CERTS` | HKLM env | same — Action="create" (skipped if user already set it) |
| `REQUESTS_CA_BUNDLE` | HKLM env | same — Action="create" |
| `SSL_CERT_FILE` | HKLM env | same — Action="create" |

The standard MSI wizard runs: Welcome → License → Install Location →
Confirm → Install (progress bar) → Finish. Same multi-step
experience as macOS `.pkg` — no PowerShell prompts, no command line.

---

## ⚠ Build path decision table

| Goal | How to build | Where to run | Output state |
|---|---|---|---|
| Test installer locally on a Windows VM | `build.ps1` + `package.ps1` | Windows host (10/11/Server) | Unsigned MSI — SmartScreen warning on first install, kernel driver still loads |
| Ship to internal pilot users | Manual build on a Windows host (no `agent-release.yml` workflow yet — see "CI" section below); long-term plan is a `windows-2022` runner job triggered by `v*` / `agent-v*` / `prod-*` tags | Manual host or planned `windows-2022` runner | Signed MSI when `WINDOWS_CERT_PATH` env is set during the manual run |
| Ship to end users / customers | Same as above + manual scp of MSI to nginx `/downloads/` | windows-2022 runner | Signed + counter-signed kernel driver, GPO-deployable |

**You cannot cross-build from Mac/Linux.** Wails dashboard requires
Windows-only WebView2 host headers + CGO; WiX v4 currently lacks a
Linux MSI codepath that emits a valid Windows Installer database.

---

## Prerequisites (Windows build host)

```powershell
# 1. Go 1.25+
choco install golang
go version    # expect 1.25.x

# 2. Wails CLI (matches go.mod toolchain)
go install github.com/wailsapp/wails/v2/cmd/wails@v2.10.1

# 3. rsrc (embeds tray manifest + icon as .syso)
go install github.com/akavel/rsrc@latest

# 4. WiX v4 CLI (.NET tool)
dotnet tool install --global wix
wix --version    # expect 4.x

# 5. Node.js 20+
choco install nodejs

# 6. Repo + workspace deps
git clone https://github.com/ai-nexus-platform/nexus-gateway.git
cd nexus-gateway
npm ci
```

Optional for signing:
```powershell
# WINDOWS_CERT_PATH — full path to .pfx
# WINDOWS_CERT_PASSWORD — pfx password
# TIMESTAMP_URL — defaults to http://timestamp.digicert.com
```

---

## Build command

```powershell
$env:VERSION = '1.0.0'

# Stage binaries (~3-4 min: Go cross-build + Wails + rsrc + WinDivert fetch)
pwsh -NoProfile -File packages/agent/platform/windows/scripts/build.ps1

# Sign individual exes + WinDivert64.sys (only if WINDOWS_CERT_PATH set)
pwsh -NoProfile -File packages/agent/platform/windows/scripts/sign.ps1

# Wrap into MSI + sign MSI itself
pwsh -NoProfile -File packages/agent/platform/windows/scripts/package.ps1

# Output
Get-Item dist/windows/NexusAgent-1.0.0.msi
```

---

## CI (GitHub Actions)

There is **no** dedicated `agent-release.yml` workflow yet. `.github/workflows/` carries only `ci.yml` + `go-ci.yml`, neither of which builds the signed MSI. Today the Windows MSI is produced manually on a Windows host by running the per-step build commands in the section above (or via the `/build-agent` skill when invoked with `--platform windows`). A dedicated CI workflow that signs + attaches MSIs to a GitHub Release is planned but not implemented.

When an automated workflow lands, it should:
- Trigger on push of tag `v*` / `agent-v*` / `prod-*` (release-shape) and on workflow_dispatch.
- Use `windows-2022` runners with `WINDOWS_CERT_PATH` / `WINDOWS_CERT_PASSWORD` secrets for signing.
- Upload `dist/windows/*.msi` as an artifact and attach to the GitHub Release for tag pushes.

Until that ships, ship a release manually:
1. Build + sign + wrap on a Windows host using the commands above.
2. Upload the `.msi` to the GitHub Release of the corresponding tag via `gh release upload <tag> dist/windows/*.msi`.

---

## Install / verify on a target Windows machine

```powershell
# 1. Install
Start-Process msiexec.exe -Wait `
    -ArgumentList '/i', 'NexusAgent-1.0.0.msi', '/qb' `
    -Verb RunAs

# 2. Verify service + driver
sc.exe query NexusAgent           # STATE: 4 RUNNING
sc.exe query WinDivert            # STATE: 4 RUNNING

# 3. Verify env vars (in a FRESH PowerShell)
Get-Item Env:NEXUS_DEVICE_CA_PEM  # → C:\ProgramData\NexusAgent\device-ca.pem
Get-Item Env:NODE_EXTRA_CA_CERTS  # same value

# 4. Verify CA imported into Root store
certutil -store Root "nexus-agent-device-ca"

# 5. Tail daemon log
Get-Content 'C:\ProgramData\NexusAgent\Logs\agent.log' -Tail 40 -Wait
```

Expected log lines on a clean install:
```
device CA minted + persisted   cert_path=C:\ProgramData\NexusAgent\device-ca.pem
device CA installed into OS trust store
WinDivert capture started  proxy_port=19080
```

---

## Uninstall

```powershell
# Via MSI uninstaller (recommended)
Start-Process msiexec.exe -Wait `
    -ArgumentList '/x', 'NexusAgent-1.0.0.msi', '/qb' `
    -Verb RunAs

# Or via Apps & Features GUI: Settings → Apps → Installed apps → Nexus Agent → Uninstall
```

MSI uninstall:
1. Stops `NexusAgent` service, then `WinDivert` service
2. Removes both from SCM
3. Deletes program files + WinDivert64.sys
4. Removes the four env vars (HKLM Environment)
5. Removes the Start-menu shortcut + HKLM Run key for tray
6. Leaves `%ProgramData%\NexusAgent\` (intentional — device CA reuse
   on reinstall; certutil idempotent on duplicate)

To wipe the leftover ProgramData (and force CA regeneration on next
install):
```powershell
Remove-Item -Recurse -Force 'C:\ProgramData\NexusAgent'
certutil -delstore Root "nexus-agent-device-ca"
```

---

## Fail-recovery

### Service is stuck STOP_PENDING after a crashed uninstall

```powershell
sc.exe queryex NexusAgent
# If PID = 0 and STATE = STOP_PENDING:
sc.exe delete NexusAgent
# Reboot, then re-run the MSI install.
```

### WinDivert driver wedged (BSOD-on-uninstall risk)

If `sc.exe delete WinDivert` returns `failure (1051: dependent
services still running)` or hangs, **do not force-kill** —
that's how previous teams have triggered DRIVER_POWER_STATE_FAILURE
BSODs at next boot. Instead:

1. Reboot (drops the kernel driver cleanly)
2. After reboot, `sc.exe stop WinDivert ; sc.exe delete WinDivert`
3. Re-run the MSI uninstall

### MSI install hangs at "Starting services"

Usually a sign that WinDivert load was blocked by an EDR (CrowdStrike
/ SentinelOne / Defender for Endpoint) but the EDR returned "blocked"
asynchronously. Check Event Viewer → Applications and Services Logs
→ Microsoft → Windows → CodeIntegrity. If the EDR logged the block:
work with corporate IT to add a per-vendor allowlist for WinDivert
v2.2.2 published by basil00 (`A6:62:F3:6C:...` cross-cert chain).

### Env vars not picked up by an existing terminal

MSI sets HKLM\System\...\Environment and broadcasts WM_SETTINGCHANGE
via the standard Windows Installer environment manager. New cmd /
PowerShell sessions launched after install see them; sessions that
were already open don't. Two recovery options:
- Close + reopen the terminal
- In an existing pwsh: `refreshenv` (chocolatey-provided) or
  `[System.Environment]::GetEnvironmentVariable('NEXUS_DEVICE_CA_PEM', 'Machine')`

---

## Reference

- WiX source: `packages/agent/platform/windows/installer/NexusAgent.wxs`
- WiX variables: `Variables.wxi` (Version, StagingDir, UpgradeCode)
- WinDivert fragment: `windivert.wxi`
- Service metadata: `service-config.xml` (human-readable, kept in
  sync with the WiX `<ServiceInstall>` block by hand)
- Build / sign / package scripts: `packages/agent/platform/windows/scripts/`
- CI workflow: **planned** — see "CI" section above; today the Windows MSI is produced manually on a Windows host.
- Daemon CA persistence: `packages/agent/internal/identity/secretstore/windows.go`
  (Windows Credential Manager backed store; CA material persisted under
  `%ProgramData%\NexusAgent\`)
- Uninstaller PS1 (legacy ops aid): `packages/agent/platform/windows/scripts/uninstall.ps1`
