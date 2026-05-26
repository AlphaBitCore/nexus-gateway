# E59-S5 — Driver Signing Pipeline

**Status:** Design — pending implementation
**Date:** 2026-05-24
**Epic:** [E59](e59-windows-wfp-migration.md)
**Architecture:** [agent-windows-wfp-driver.md](../architecture/agent-windows-wfp-driver.md) §9
**Depends on:** E59-S4 (cross-arch .sys + INF set ready to sign), EV cert procured, Microsoft Hardware Dev Center account registered.

---

## 1. User story

> **As** the release engineer
> **I want** to push a release tag and have the build pipeline
> produce a `nexus-wfp.cat` signed by Microsoft (attestation),
> uploaded to the Hardware Dev Center and round-tripped back into
> the MSI staging dir
> **so that** the resulting MSI installs cleanly on customer
> Windows boxes without `testsigning on` — i.e. on stock,
> Secure-Boot-enabled enterprise machines.

## 2. Goal

Replace the E59-S1–S4 dev workflow (test-signed builds running
under `bcdedit /set testsigning on`) with a release-grade signing
flow:

1. EV Authenticode cert signs the `.cat` and the two `.sys` files.
2. Microsoft Hardware Dev Center attestation flow returns a
   Microsoft-signed CAT.
3. The returned CAT is embedded in the MSI staging dir;
   `package.ps1` finalises the MSI with the signed CAT in place.
4. MSI itself is Authenticode-signed by the same EV cert
   (`sign.ps1` existing flow).

## 3. Tasks

### T1 — EV cert custody (~1 day, off-engineering)

Requirements per epic §11 risk R5 / §4 NFR-6:

- EV cert private key on FIPS-140-2 HSM (YubiKey 5 FIPS or
  equivalent).
- PIN + touch acknowledgement required on every sign.
- Cert installed on a designated build workstation; only release
  engineering accounts have local logon.
- Cert public-key fingerprint committed to
  `docs/operators/ops/runbooks/agent-windows-release.md` so any
  team member can verify the produced MSI is signed by the right
  key.

### T2 — Signing script `sign-driver.ps1` (~1 day)

`packages/agent/platform/windows/scripts/sign-driver.ps1`:

```powershell
# Inputs:
#   -SysFiles (paths to nexus-wfp.sys for each arch)
#   -InfFile  (path to nexus-wfp.inf)
#   -OutCat   (path to write nexus-wfp.cat)
#
# Env:
#   WINDOWS_CERT_PATH      = HSM-backed cert reference (PKCS#11 URI)
#   TIMESTAMP_URL          = default http://timestamp.digicert.com

# 1. Use inf2cat to generate an unsigned CAT covering both .sys
inf2cat /driver:<staging> /os:10_X64,10_ARM64

# 2. signtool sign /sha1 <thumbprint> /fd sha256 /tr <ts> nexus-wfp.cat nexus-wfp.sys (x2)
signtool sign /sha1 $env:WINDOWS_CERT_THUMB /fd sha256 /tr $env:TIMESTAMP_URL `
    nexus-wfp.cat <each sys>

# 3. (Optional embed-only check) signtool verify /pa /v on each
```

The EV-signed CAT is ready for submission to Hardware Dev Center.

### T3 — Hardware Dev Center submission (~0.5 day automation, 1-3 hours wall-clock)

`packages/agent/platform/windows/scripts/submit-driver.ps1`:

- Wraps the [Microsoft Hardware Dev Center API](https://learn.microsoft.com/en-us/windows-hardware/drivers/dashboard/hardware-api-reference).
- Authenticated via Azure AD client credentials (NOT a user
  account — release pipeline runs unattended).
- Creates a new product → new submission → uploads the
  EV-signed `nexus-wfp.cat` bundle.
- Polls submission status; on `signed`, downloads the
  Microsoft-attestation-signed CAT.
- Returns exit 0 only when the signed CAT is back in
  `dist/windows/staging/wfp-driver/nexus-wfp.cat` (overwriting
  the EV-signed one).

### T4 — Pipeline integration (~1 day)

Updated release sequence (replaces the `build.ps1 → package.ps1`
pair from E59-S3):

```
1. build.ps1                       (Go binaries, dashboard, WFP .sys + .inf staged unsigned)
2. sign-driver.ps1                 (inf2cat → CAT → EV-sign CAT + .sys)
3. submit-driver.ps1               (upload, wait, download Microsoft-signed CAT)
4. package.ps1                     (wix build → MSI)
5. sign.ps1 (existing)             (EV-sign the MSI)
```

Step 3 can take 1-3 hours; the pipeline MUST tolerate this without
human intervention. A 24-hour fail-safe timeout (epic §6 C-2 binding)
exists for the unusual case of Patch Tuesday submission backlog.

### T5 — Test-mode bypass for dev (~0.25 day)

`build.ps1` accepts `-SignMode` parameter:

- `attestation` (default for release tags): runs T2+T3.
- `test-sign`: signs with a local test cert under
  `tools/test-cert/` only; produces an unsigned-by-Microsoft CAT
  that only loads under `bcdedit /set testsigning on`.

CI on `main` and `feature/*` branches uses `-SignMode test-sign`.
Release-tag CI uses `-SignMode attestation`.

### T6 — Release runbook (~1 day)

`docs/operators/ops/runbooks/agent-windows-release.md`:

- Pre-flight: HSM connected, EV cert PIN known, Hardware Dev Center
  credentials valid (test via dry-run submission).
- Step-by-step: tag, build, sign, submit, package, verify, ship.
- Failure modes: cert PIN locked, HSM removed mid-sign, Hardware
  Dev Center rejection (driver-quality issues), Authenticode timestamp
  server outage.
- Rollback: previous-release MSI is always retained for 90 days
  on the release artefact server; rollback = serve the previous URL.

## 4. Acceptance criteria

1. `pwsh -File sign-driver.ps1` on a host with HSM connected
   produces an EV-signed `nexus-wfp.cat` and EV-signed `.sys` files.
2. `pwsh -File submit-driver.ps1` round-trips with a Microsoft-
   attestation-signed CAT within 24 hours of invocation.
3. The MSI produced by the full pipeline installs cleanly on a
   stock Windows 11 24H2 amd64 machine (NO `testsigning on`).
4. The MSI installs cleanly on a stock Windows 11 24H2 arm64
   Surface Pro 11 (NO `testsigning on`).
5. `signtool verify /pa /v` on the installed `nexus-wfp.sys` shows
   a Microsoft attestation signature.
6. `Get-AuthenticodeSignature` on the MSI shows the EV cert as
   signer + a valid timestamp.
7. Runbook in T6 is exercised by a release engineer who is not the
   author, dry-run, end-to-end, on a non-release tag.

## 5. Risks

- **R-S5.1:** Microsoft Hardware Dev Center rejection — driver
  quality issues caught only at submission. Mitigation: E59-S6 runs
  Driver Verifier and HLK Filter Driver tests BEFORE we even
  attempt submission. A rejection is a hard fail that blocks the
  release.
- **R-S5.2:** HSM unavailability during release — physical HSM at
  the build workstation breaks or is locked out. Mitigation: a
  spare HSM with the same cert imported (HSM-to-HSM provisioning
  via Microsoft documented procedure) lives in a secure cabinet.
- **R-S5.3:** Timestamp-server outage — DigiCert's timestamp
  service has had hours-long outages historically. Mitigation: a
  secondary timestamp URL (`http://timestamp.sectigo.com`)
  configured as fallback in sign-driver.ps1.

## 6. Out of scope

- Microsoft Hardware Dev Center account *registration* (off-eng,
  done by company ops before this story starts).
- EV cert *procurement* (off-eng).
- Cert revocation / re-issuance procedure (separate operational
  doc, not E59 scope).
- macOS / Linux signing (existing flows, untouched).
