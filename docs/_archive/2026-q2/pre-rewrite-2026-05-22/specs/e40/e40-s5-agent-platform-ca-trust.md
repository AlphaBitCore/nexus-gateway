# E40-S5 — Agent Platform CA Auto-Trust

Status: draft
Epic: E40 — Setup Guide
Story: S5 — Agent Platform CA Auto-Trust

## User Story

As a Platform Admin deploying the Nexus Agent, I want the agent to
automatically install its Device CA certificate into the OS trust store on
first run, so that browsers and tools on the endpoint trust the agent's
TLS-intercepted connections without any manual CA-trust step or user prompt.

---

## 1. Problem

The Nexus Agent generates a per-machine ECDSA P-256 Device CA on first run
(stored in the platform keystore). This CA is used by the agent's MITM relay
to sign per-hostname leaf certificates for inspected connections. Until the
Device CA is added to the OS trust store, every intercepted TLS connection
triggers a certificate error in the browser or tool.

Currently, administrators must distribute the Device CA manually after the
agent starts (export from keystore, import via `security`, `certutil`, or
`update-ca-certificates`). There is no automated install path, which creates
a gap between agent deployment and the first successfully intercepted request.

---

## 2. Goal

Implement an OS-abstracted `InstallCACert` function in the agent's platform
package that:

- Installs the Device CA into the correct OS trust store.
- Is idempotent: a second call when the CA is already trusted performs no
  work and returns nil.
- Is non-fatal: if the installation fails (e.g., insufficient privileges,
  OS error), the function logs a warning and returns nil rather than
  returning an error that could prevent the agent from starting.
- Is called automatically after the TLS Engine initialises and
  `engine.CACertPEM()` is available, from the agent's main entry point.

---

## 3. Non-goals

- Removing or rotating a previously installed CA (addressed in a future
  CA rotation epic).
- Distributing the CA to remote endpoints (handled by E40-S4 MDM profile).
- Installing the compliance proxy Sub-CA on agent machines (the agent uses
  its own Device CA; the compliance proxy has a separate CA distribution
  path via MDM).
- Supporting platforms other than macOS, Windows, and Linux.

---

## 4. Design

### 4.1 Package Structure

The new `catrust` code lives in `packages/agent/core/platform/` alongside
the existing `platform.go`, `darwin.go`, `windows.go`, and `linux.go` files.
Three new build-tag files are added:

```
packages/agent/core/platform/
    catrust.go           -- interface + shared helper (no build tag)
    catrust_darwin.go    -- //go:build darwin
    catrust_windows.go   -- //go:build windows
    catrust_linux.go     -- //go:build linux
```

This mirrors the pattern already used by `darwin.go`, `windows.go`,
`linux.go` in the same package for the `Platform` interface implementations.

### 4.2 Package-level Function Signature

`catrust.go` exports exactly one function:

```go
// InstallCACert installs certPEM into the OS system trust store so that
// connections intercepted by the Nexus Agent TLS engine are trusted by
// browsers and OS tools.
//
// The call is idempotent: if the certificate is already trusted, the function
// returns nil without re-installing.
//
// Installation failure is non-fatal: the function logs a warning via logger
// and returns nil. The agent must not fail to start because of a CA trust
// installation error (FR-4.4).
func InstallCACert(certPEM []byte, logger *slog.Logger) error
```

The per-platform files provide this function with the matching build tag.
The `catrust.go` file must not itself declare `InstallCACert` — it provides
only shared helpers (e.g., writing a PEM to a temp file) that the per-platform
files call. This avoids duplicate symbol errors.

A small shared helper is provided in `catrust.go`:

```go
// writeTempCert writes certPEM to a temporary file and returns its path.
// The caller is responsible for removing the file after use.
func writeTempCert(certPEM []byte) (string, error)
```

### 4.3 macOS Implementation (`catrust_darwin.go`, build tag `darwin`)

**Idempotency check:**

```go
// Import the cert to a temp file, then verify via `security verify-cert`.
// Exit code 0 means already trusted; skip install.
tmpPath, err := writeTempCert(certPEM)
// ...
cmd := exec.Command("security", "verify-cert", "-c", tmpPath)
if err := cmd.Run(); err == nil {
    logger.Debug("catrust: CA already trusted, skipping install")
    return nil
}
```

**Install:**

```go
cmd := exec.Command(
    "security", "add-trusted-cert",
    "-d",                                    // add to admin cert store
    "-r", "trustRoot",                       // always trust as root CA
    "-k", "/Library/Keychains/System.keychain",
    tmpPath,
)
```

If the command exits non-zero, the error message from `cmd.CombinedOutput()`
is logged at `Warn` level and the function returns nil (non-fatal per
FR-4.4).

The agent runs as root via a LaunchDaemon, so the `security` command has the
necessary privileges to write to the System keychain.

### 4.4 Windows Implementation (`catrust_windows.go`, build tag `windows`)

```go
tmpPath, err := writeTempCert(certPEM)
// ...
cmd := exec.Command("certutil", "-addstore", "-f", "Root", tmpPath)
```

The `-f` flag makes `certutil` a no-op when the same certificate (matched by
thumbprint) is already present in the Root store — this is the idempotency
mechanism on Windows.

If the command exits non-zero, the combined output is logged at `Warn` level
and the function returns nil.

The agent runs as `SYSTEM` (Windows service), so `certutil -addstore Root`
has the necessary privileges.

### 4.5 Linux Implementation (`catrust_linux.go`, build tag `linux`)

Linux distros use two incompatible CA bundle mechanisms. The implementation
detects the distro family at runtime by reading `/etc/os-release` and
matching the `ID=` or `ID_LIKE=` field:

**Debian/Ubuntu family** (`ID=ubuntu`, `ID=debian`, or `ID_LIKE` contains
`debian`):

```
Destination: /usr/local/share/ca-certificates/nexus-agent-ca.crt
Command:     update-ca-certificates
```

Idempotency: before copying, check if the destination file already exists
AND its SHA-256 hash matches the incoming cert bytes. If both are true,
skip the copy and the `update-ca-certificates` call.

**RHEL/CentOS/Fedora family** (`ID` is one of `rhel`, `centos`, `fedora`,
`rocky`, `almalinux`, or `ID_LIKE` contains `rhel` or `fedora`):

```
Destination: /etc/pki/ca-trust/source/anchors/nexus-agent-ca.crt
Command:     update-ca-trust
```

Same SHA-256 idempotency check as the Debian path.

**Other/unknown distro:** Log a `Warn`-level message:
```
catrust: unrecognised Linux distro family; CA not installed automatically.
Install manually: copy nexus-agent-ca.crt to your system CA bundle directory and run the appropriate update command.
```
Return nil (non-fatal).

### 4.6 Call Site in `cmd/agent/main.go`

After the TLS Engine is initialised (the existing `engine.Start()` or
equivalent call), add:

```go
if caPEM := engine.CACertPEM(); len(caPEM) > 0 {
    if err := platform.InstallCACert(caPEM, logger); err != nil {
        // InstallCACert is documented to return nil on non-fatal errors
        // (it logs internally). A non-nil error here is unexpected and
        // indicates a programming error in the platform implementation.
        logger.Warn("catrust: unexpected error from InstallCACert", "error", err)
    }
}
```

The call is synchronous and blocks until the OS command completes. Because the
agent daemon starts before user sessions on macOS/Windows, the install
completes before any user initiates a TLS connection to an intercepted host.
If startup latency becomes a concern in a future iteration, the call may be
moved to a goroutine, but that is out of scope for E40.

### 4.7 Error Handling Policy

`InstallCACert` never returns a non-nil error. All errors are logged at
`Warn` level internally and the function returns nil. This means callers need
not handle the error value for correctness, but they should log unexpected
non-nil returns as a defensive measure (see §4.6).

Rationale: FR-4.4 states that CA installation failure is non-fatal. Making
the function signature return an error is consistent with Go conventions and
allows unit-test code to distinguish "ran and succeeded" from "ran and failed
but suppressed the error", but the call site treats all returns as advisory.

---

## 5. Tasks

- T1 — Add `packages/agent/core/platform/catrust.go`:
  - T1.1 — Add package docstring explaining the `InstallCACert` contract.
  - T1.2 — Implement the `writeTempCert(certPEM []byte) (string, error)` helper that writes PEM bytes to a `os.CreateTemp("", "nexus-ca-*.pem")` file and returns the path.
  - T1.3 — Do NOT declare `InstallCACert` in this file; it is declared only in the per-platform build-tag files.

- T2 — Add `packages/agent/core/platform/catrust_darwin.go` (build tag `darwin`):
  - T2.1 — Implement `InstallCACert(certPEM []byte, logger *slog.Logger) error`.
  - T2.2 — Write PEM to temp file via `writeTempCert`; defer `os.Remove(tmpPath)`.
  - T2.3 — Run `security verify-cert -c <tmpPath>`; if exit 0, log debug and return nil.
  - T2.4 — Run `security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain <tmpPath>`.
  - T2.5 — On non-zero exit: log `Warn` with combined output; return nil.

- T3 — Add `packages/agent/core/platform/catrust_windows.go` (build tag `windows`):
  - T3.1 — Implement `InstallCACert(certPEM []byte, logger *slog.Logger) error`.
  - T3.2 — Write PEM to temp file via `writeTempCert` with `.crt` extension (certutil requires a file extension it recognises); defer `os.Remove`.
  - T3.3 — Run `certutil -addstore -f Root <tmpPath>`.
  - T3.4 — On non-zero exit: log `Warn` with combined output; return nil.

- T4 — Add `packages/agent/core/platform/catrust_linux.go` (build tag `linux`):
  - T4.1 — Implement `InstallCACert(certPEM []byte, logger *slog.Logger) error`.
  - T4.2 — Read `/etc/os-release`; parse `ID` and `ID_LIKE` fields.
  - T4.3 — Detect Debian/Ubuntu family; if matched:
    - T4.3.1 — Set `destPath = /usr/local/share/ca-certificates/nexus-agent-ca.crt`.
    - T4.3.2 — Idempotency check: if file exists and `sha256(file) == sha256(certPEM)`, log debug and return nil.
    - T4.3.3 — `os.WriteFile(destPath, certPEM, 0644)`.
    - T4.3.4 — `exec.Command("update-ca-certificates").Run()`; log Warn on error; return nil.
  - T4.4 — Detect RHEL/CentOS/Fedora family; if matched:
    - T4.4.1 — Set `destPath = /etc/pki/ca-trust/source/anchors/nexus-agent-ca.crt`.
    - T4.4.2 — Same idempotency check as T4.3.2.
    - T4.4.3 — `os.WriteFile(destPath, certPEM, 0644)`.
    - T4.4.4 — `exec.Command("update-ca-trust").Run()`; log Warn on error; return nil.
  - T4.5 — Unknown distro: log `Warn` with manual install instructions; return nil.

- T5 — Update `packages/agent/cmd/agent/main.go`:
  - T5.1 — After TLS Engine initialisation, add the `InstallCACert` call site (see §4.6).
  - T5.2 — Ensure `engine.CACertPEM()` is accessible at the call site; add the method to the engine interface if not already present.
  - T5.3 — Confirm the call is after `engine.Start()` or equivalent and before the agent begins accepting intercepted connections.

- T6 — Unit tests (`packages/agent/core/platform/catrust_test.go`):
  - T6.1 — Introduce a test-only `execCommand` variable (function pointer) in each platform file, initialised to `exec.Command`. Tests replace it with a mock that records invocations and returns a configurable exit code.
  - T6.2 — macOS test: verify that when the idempotency `security verify-cert` mock returns exit 0, `add-trusted-cert` is NOT called.
  - T6.3 — macOS test: verify that when `verify-cert` returns exit 1, `add-trusted-cert` IS called with the correct arguments (`-d`, `-r trustRoot`, `-k /Library/Keychains/System.keychain`, `<path>`).
  - T6.4 — macOS test: verify that when `add-trusted-cert` returns exit 1, `InstallCACert` still returns nil (non-fatal).
  - T6.5 — Windows test: verify `certutil -addstore -f Root <path>` is called.
  - T6.6 — Windows test: verify non-zero exit from `certutil` returns nil.
  - T6.7 — Linux test (Debian): mock `/etc/os-release` content; verify `update-ca-certificates` is called and the cert is written to the Debian path.
  - T6.8 — Linux test (RHEL): mock `/etc/os-release` content; verify `update-ca-trust` is called and the cert is written to the RHEL path.
  - T6.9 — Linux test (unknown distro): mock `/etc/os-release` with `ID=archlinux`; verify no exec calls are made and nil is returned.
  - T6.10 — Idempotency test (Linux): write the same cert to the dest path, then call `InstallCACert` again; verify the update command is NOT called a second time.

---

## 6. Acceptance Criteria

- AC1 — On macOS (agent running as root), after the first `InstallCACert`
  call with a valid PEM cert, `security find-certificate -c "Nexus"
  /Library/Keychains/System.keychain` returns a matching entry.
- AC2 — On Windows (agent running as SYSTEM), after the first
  `InstallCACert` call, `certutil -store Root | findstr Nexus` returns a
  matching entry.
- AC3 — On Debian/Ubuntu Linux (agent running as root), after the first
  `InstallCACert` call, `/usr/local/share/ca-certificates/nexus-agent-ca.crt`
  exists and `update-ca-certificates` has run (verifiable via
  `openssl verify -CApath /etc/ssl/certs nexus-agent-ca.crt`).
- AC4 — On RHEL/CentOS/Fedora Linux (agent running as root), after the first
  `InstallCACert` call, the cert is present in
  `/etc/pki/ca-trust/source/anchors/nexus-agent-ca.crt` and
  `update-ca-trust` has run.
- AC5 — A second `InstallCACert` call with the same cert PEM does not invoke
  the install command again (idempotent): no new `add-trusted-cert`,
  `certutil`, or `update-ca-*` process is spawned.
- AC6 — If `InstallCACert` fails (simulated by a non-zero exit code from the
  OS command), the agent process does not exit and traffic interception
  continues normally.
- AC7 — `go build ./packages/agent/...` succeeds on all three platform build
  targets (requires cross-compilation or CI matrix: `GOOS=darwin`,
  `GOOS=windows`, `GOOS=linux`).
- AC8 — `go test -race -count=1 ./packages/agent/core/platform/...`
  passes with all catrust unit tests green.
- AC9 — No `InstallCACert` call is made when `engine.CACertPEM()` returns
  an empty slice (start-up race guard).

---

## 7. Risks

- **R1** — On macOS, `security verify-cert` may return exit 0 for certs that
  are in the System keychain but not set to `trustRoot`, causing the
  idempotency check to skip a re-trust. Mitigation: use
  `security verify-cert -c <tmpPath> -p ssl` (adds SSL policy check) for
  a more precise trust verification; document the behaviour in the function
  comment.
- **R2** — The temp file path passed to `security` / `certutil` may contain
  spaces on Windows (`%TEMP%` often resolves to a path with spaces).
  Mitigation: quote the path in the command argument or use
  `exec.Command(binary, args...)` variadic form (Go passes each argument
  as a separate process argument, avoiding shell quoting issues entirely).
- **R3** — Writing to `/Library/Keychains/System.keychain` or
  `certutil -addstore Root` requires root / SYSTEM privileges. If the
  agent is started as a non-privileged user during development, the
  install will fail silently (non-fatal, but confusing). Mitigation: log
  the effective UID at `Debug` level before calling `InstallCACert`; the
  Warn-level log on failure will surface the error.
- **R4** — The Linux distro-detection logic via `/etc/os-release` does not
  cover all enterprise distros (e.g. SUSE, Alpine). Mitigation: the
  fallback `Warn` path explains the manual install steps, and the
  function remains non-fatal; detection coverage can be extended in a
  follow-up without breaking the interface.
- **R5** — Cross-platform unit tests: the `catrust_darwin.go` file is
  excluded by the build tag when running tests on Linux CI. Mitigation:
  the mock-based unit tests are guarded by the same build tags as their
  implementation files, so each platform's tests run only in the
  matching CI matrix job.
