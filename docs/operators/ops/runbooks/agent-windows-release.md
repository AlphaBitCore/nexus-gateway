# Agent Windows Release Runbook

**Audience:** Release engineer.
**Scope:** Producing a signed, attestation-validated Windows agent MSI
that installs cleanly on stock Windows 11 24H2 (amd64 + arm64) with
no `bcdedit /set testsigning on`.
**Authority:** This runbook + the E59-S5 SDD bind the release flow.

---

## 1. Pre-flight

Verify on the build workstation before tagging:

| Check | Command | Expected |
|---|---|---|
| FIPS-140-2 HSM connected | `signtool sign /sha1 $env:WINDOWS_CERT_THUMB /f /v /pa <a-test-file>` | Prompts for HSM PIN + touch |
| EV cert thumbprint matches the published key | `certutil -store -user My` then compare against the SHA-1 fingerprint in §5 below | Match |
| Hardware Dev Center credentials valid | `pwsh scripts/submit-driver.ps1 -WhatIf` (lists products on the account) | Returns the Product list without 401 |
| WDK installed (both arches) | `& 'C:\Program Files (x86)\Windows Kits\10\bin\10.0.26100.0\x64\inf2cat.exe' /?` | Help banner |
| Visual Studio 2022 Build Tools | `MSBuild /version` | 17.x |
| Go toolchain | `go version` | 1.26.x |
| WiX 5 + .NET 8 | `wix --version`, `dotnet --version` | 5.x, 8.x |

If any check fails, **stop**. Do not tag.

---

## 2. Tag

```powershell
git checkout main
git pull --ff-only
git tag -s v<VERSION>-windows -m "Windows agent release v<VERSION>"
git push origin v<VERSION>-windows
```

Tag format: `v<MAJOR>.<MINOR>.<PATCH>-windows`. The `-windows` suffix
keeps Windows tags from confusing the unified release tracker that
also tags macOS / Linux builds.

---

## 3. Build the driver

```powershell
cd packages\agent\platform\windows\nexus-wfp-driver
.\build.bat
```

Expect:

```
bin\x64\Release\nexus-wfp.sys      ~80 KB
bin\ARM64\Release\nexus-wfp.sys    ~80 KB
```

Any compiler warning at /W4 fails the build (vcxproj sets
TreatWarningsAsErrors). Fix and re-tag if so — do not bypass.

---

## 4. EV-sign + attestation submit

```powershell
$env:WINDOWS_CERT_THUMB = '<EV cert SHA-1 thumbprint>'
$env:HWDEVCENTER_TENANT_ID     = '<azure tenant guid>'
$env:HWDEVCENTER_CLIENT_ID     = '<azure app reg client id>'
$env:HWDEVCENTER_CLIENT_SECRET = '<azure app reg secret>'

cd packages\agent\platform\windows\scripts
.\sign-driver.ps1                  # interactive PIN + touch on HSM
.\submit-driver.ps1                # 1-3 hours wall-clock Microsoft turnaround
```

`submit-driver.ps1` polls every 60 seconds and times out after 24
hours per epic §6 C-2 binding. On success, `nexus-wfp.cat` in the
driver directory is replaced with the Microsoft-signed CAT. Verify:

```powershell
signtool verify /pa /v ..\nexus-wfp-driver\nexus-wfp.cat
```

Expect: chain ends at Microsoft.

---

## 5. Build the MSI

```powershell
cd packages\agent\platform\windows\scripts
$env:VERSION = '<VERSION>'
.\build.ps1                        # produces dist/windows/staging/*
.\package.ps1                      # produces dist/windows/NexusAgent-<VERSION>.msi
.\sign.ps1                         # EV-signs the MSI itself
```

EV cert SHA-1 thumbprint (verify match against your local cert): _to
be filled in by the release engineer once the production cert is
provisioned. Format: 40 hex chars, uppercase, no separators._

Verify the MSI:

```powershell
signtool verify /pa /v dist\windows\NexusAgent-<VERSION>.msi
Get-AuthenticodeSignature dist\windows\NexusAgent-<VERSION>.msi
```

Expect: `Status : Valid`, `SignerCertificate.Subject` matches our
company DN.

---

## 6. Smoke-install on both arches

| Env | Action | Expected |
|---|---|---|
| amd64 Win 11 24H2 VM (stock, no testsigning) | `msiexec /i NexusAgent-<v>.msi /qb /l*v install.log` | exit 0; `sc query NexusWFP` → RUNNING; `sc query NexusAgent` → RUNNING |
| arm64 Surface Pro 11 (stock, no testsigning) | same | same |

On either env, `nexus-agent install-wfp-check` should exit 0 silently.

Then `msiexec /x ... /qb` and verify no orphan files / services per
E59-S6 TP5.

---

## 7. Publish

Upload the signed MSI to the release artefact server. URL template:
`https://releases.nexus-gateway.com/agent/windows/v<VERSION>/NexusAgent-<VERSION>.msi`.

Retain the previous-release MSI for **90 days** at the URL pattern
`/agent/windows/previous/NexusAgent-<PREV>.msi` so rollback is a
simple URL change.

---

## 8. Failure recovery

| Failure | Action |
|---|---|
| Microsoft attestation rejection | Stop. Run Driver Verifier locally (E59-S6 TP2). Fix the cited rule, re-tag, restart from §3. Do not retry submission with the same artefacts. |
| HSM PIN locked | Use the spare HSM (kept in the secure cabinet — see asset register). If that's also locked, escalate to security ops; do not proceed without a valid cert. |
| DigiCert timestamp outage | sign-driver.ps1 falls back to `http://timestamp.sectigo.com` (configured in env). If both are down, pause the release; do not ship without a timestamp. |
| Submission stalled > 24 h | Inspect at https://partner.microsoft.com/dashboard/hardware/. Microsoft support contact link in the Hardware portal sidebar; quote the submission id. |

---

## 9. Rollback

1. Revert the release URL to the previous MSI.
2. If customers have already installed the bad release:
   - Push a hotfix MSI bump (`<VERSION>-fix1`) with `MajorUpgrade
     DowngradeErrorMessage` removed if necessary.
   - Or instruct customers to `msiexec /x` the bad version + install
     the previous.

Never push `<VERSION>` again after a known-bad signed release —
Microsoft attestation submission for the same version + same
ProductId will be rejected.

---

## 10. Related documents

- `docs/developers/architecture/agent-windows-wfp-driver.md` (§9 signing)
- `docs/developers/specs/e59-s5-signing-pipeline.md` (SDD)
- `packages/agent/platform/windows/scripts/sign-driver.ps1`
- `packages/agent/platform/windows/scripts/submit-driver.ps1`
- `packages/agent/platform/windows/scripts/build.ps1`
- `packages/agent/platform/windows/scripts/package.ps1`
- `packages/agent/platform/windows/scripts/sign.ps1` (MSI-side signing)
