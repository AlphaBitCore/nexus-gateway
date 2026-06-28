# Windows kernel-debug VM (NexusWFP-Debug)

Crash-safe Hyper-V VM for developing the E59 **NexusWFP** kernel driver
(`packages/agent/platform/windows/nexus-wfp-driver/`). A kernel-mode driver bug
bugchecks (BSODs) the machine it runs on, so the driver is **never** loaded on
your workstation — it is built on the host and loaded inside this disposable VM,
which can be reverted in ~10 seconds and attached to a kernel debugger.

> Build the driver: see the driver [`README.md`](../../../packages/agent/platform/windows/nexus-wfp-driver/README.md)
> (`build.ps1`). Architecture: [`agent-windows-wfp-driver.md`](../architecture/agent-windows-wfp-driver.md).

## TL;DR — the dev loop (once the VM is provisioned)

Run **elevated on the Hyper-V host**:

```powershell
cd packages\agent\platform\windows\nexus-wfp-driver
pwsh -NoProfile -File build.ps1                 # build nexus-wfp.sys on the host
pwsh -NoProfile -File dev-deploy.ps1            # build + revert + deploy + verify on the VM
pwsh -NoProfile -File dev-deploy.ps1 -SkipBuild # deploy an already-built .sys
```

`dev-deploy.ps1` reverts the VM to the clean checkpoint, copies the `.sys` in
over PowerShell Direct, trusts the test-signing cert, `sc start`s the driver
under a 45-second timeout (a timeout means it bugchecked), and verifies the
`NexusConnectRedirectV4/V6` callouts registered. A bugcheck is recovered by
reverting the checkpoint.

## VM facts

| | |
|---|---|
| Name | `NexusWFP-Debug` (Hyper-V, Generation 2) |
| OS | Windows 11 Pro 25H2 (build 26100) |
| Spec | 4 vCPU, dynamic RAM |
| Guest admin | `dev2` / `abc123` (throwaway local account — override the scripts' `-GuestUser`/`-GuestPass` if yours differ) |
| Control channel | **PowerShell Direct** over VMBus (no guest NIC needed) |
| Kernel debug | KDNET over the Default Switch, UDP port 50000 |
| Clean baseline | checkpoint **`clean-debug-ready`** |

PowerShell Direct credentials do not persist between shells — rebuild each time:

```powershell
$cred = New-Object System.Management.Automation.PSCredential('dev2',
    (ConvertTo-SecureString 'abc123' -AsPlainText -Force))
Invoke-Command -VMName NexusWFP-Debug -Credential $cred -ScriptBlock { ... }
```

## Initial provisioning (one-time)

### The three blockers — solve all three or the driver will not load

1. **Secure Boot OFF.** With the VM powered off:
   ```powershell
   Set-VMFirmware NexusWFP-Debug -EnableSecureBoot Off
   ```
   Required for both test-signing and KDNET. Setting `testsigning on` while
   Secure Boot is ON silently no-ops.

2. **Smart App Control (SAC) OFF — only the guest Windows Security UI can do it.**
   Clean Win11 25H2 ships SAC in *Evaluation* mode running an **enforced** CI
   policy (`VerifiedAndReputableDesktopEvaluation`) that (a) rejects test-signed
   drivers → `sc start` fails with **error 577**, and (b) **strips the
   `testsigning` BCD flag on every reboot**. It cannot be removed by registry
   edit, `citool --remove-policy`, or deleting the `.cip` (all blocked/reverted).
   The only fix: in the guest, **Windows Security → App & browser control →
   Smart App Control settings → Off** (a ~20-second one-way action — fine for a
   throwaway VM). After that, `bcdedit /set {current} testsigning on` finally
   persists across reboot.

3. **KDNET needs DHCP → use the Default Switch, not a static internal switch.**
   KDNET's boot stack gets the target IP via DHCP; a static internal switch has
   no DHCP, so `kd` sits forever at "Waiting to reconnect...". The Default Switch
   host IP (e.g. `172.21.224.1`) provides NAT + DHCP. That subnet can change on
   host reboot — re-run `kdnet` if so.

### KDNET wiring

In the guest (copy `kdnet.exe` + `VerifiedNICList.xml` from
`…\Windows Kits\10\Debuggers\x64\` into `C:\KDNET\`):

```cmd
C:\KDNET\kdnet.exe 172.21.224.1 50000
bcdedit /debug on
```
Reboot. On the host, allow the debugger port:
```powershell
New-NetFirewallRule -DisplayName 'KDNET 50000 UDP In' -Direction Inbound `
    -Protocol UDP -LocalPort 50000 -Action Allow -Profile Any
```
`kdnet.exe` prints (and regenerates each run) the connection key; it is saved at
`D:\vm-iso\NexusWFP-Debug-kdnet.txt`.

### Attaching the kernel debugger

The usable CLI debugger is **`kd.exe` inside the WinDbg MSIX**, not the WDK
stubs (those are 1-byte placeholders):

```
C:\Program Files\WindowsApps\Microsoft.WinDbg_*_x64__8wekyb3d8bbwe\amd64\kd.exe
```
Drive it headlessly (pass commands via a `-cf` script file — inline `-c "..."`
gets mangled by quoting):
```powershell
kd -k net:port=50000,key=<key> -b -cf <cmdscript.txt> -logo <log.txt>
```
Force a break on an idle/booted target:
```powershell
Debug-VM -Name NexusWFP-Debug -InjectNonMaskableInterrupt -Force
```

### The `clean-debug-ready` checkpoint

The fully-provisioned baseline: SAC off, `testsigning` persists, KDNET wired, the
test-signing cert trusted in the guest `Root` + `TrustedPublisher` stores, the
`NexusWFP` kernel service created **demand-start + STOPPED**, and **0 WFP
objects** loaded. It is the 10-second revert target the dev loop reverts to
before every deploy. Refresh it (never with the driver loaded) whenever
provisioning changes:

```powershell
Checkpoint-VM -Name NexusWFP-Debug -SnapshotName clean-debug-ready
```

## How `dev-deploy.ps1` uses all of this

1. (optional) `build.ps1` — build `nexus-wfp.sys` on the host.
2. `Restore-VMCheckpoint -Name clean-debug-ready` — clean kernel baseline.
3. `Start-VM` + wait for PowerShell Direct.
4. `Copy-Item -ToSession` the `.sys` into guest `C:\nexus\`.
5. Push + trust the signer cert (`certutil -addstore -f Root|TrustedPublisher`).
6. Ensure the `NexusWFP` kernel service (`sc create … type= kernel`).
7. `sc start NexusWFP` inside a `Start-Job` with a 45s timeout — returns ⇒ no
   crash; times out ⇒ bugcheck (attach `kd`, `!analyze -v`, revert checkpoint).
8. Verify `Win32_SystemDriver` = Running and `netsh wfp show state` lists
   `NexusConnectRedirectV4/V6`.

## Common failures

| Symptom | Cause | Fix |
|---|---|---|
| `sc start` → **error 577** | SAC still enforcing | Turn SAC off in the guest UI (blocker 2) |
| `testsigning` reverts every reboot | SAC strips the BCD flag | Same — SAC off |
| `kd` stuck "Waiting to reconnect" | static switch, no DHCP | Use the Default Switch (blocker 3); re-run `kdnet` |
| `sc start` → `FWP_E_ALREADY_EXISTS` | prior load left BFE objects | Revert `clean-debug-ready` (the default deploy path does this) |
| PowerShell Direct refused | VM not booted / wrong creds | Wait for boot; check `-GuestUser`/`-GuestPass` |
