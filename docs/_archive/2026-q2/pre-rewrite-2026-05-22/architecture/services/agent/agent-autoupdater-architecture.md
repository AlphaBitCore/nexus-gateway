---
doc: agent-autoupdater-architecture
area: service
service: agent
tier: 1
updated: 2026-05-21
---

# Agent Autoupdater Architecture

> **Tier 2 architecture doc.** Read when touching `packages/agent/internal/host/updater/` or `packages/agent/internal/sync/hub/client.go`'s `CheckUpdate` path. Today's macOS .pkg distribution is still manual (memory `reference_agent_pkg_distribution`); the in-binary updater is wired and verifies signatures, but the prod release server side is not yet driving it.

The agent polls Hub on a caller-supplied interval, verifies SHA-256 + Ed25519 against a caller-supplied public key, and atomically swaps the binary in place. Crash-loop detection rolls back to the previous binary.

---

## 1. Real implementation: Hub-driven update check

There is no channel system (no `stable` / `beta` / `canary`), no `manifest.json` URL, no `agent_settings.updateChannel` shadow key, no `pinnedVersion`. The agent learns about available updates by calling Hub:

```
GET /api/internal/things/update-check?currentVersion=<v>&os=<osName>
  → hub.UpdateInfo {
      Available    bool
      Version      string   // semver of the new build
      DownloadURL  string   // signed URL to fetch the binary
      Signature    string   // base64 Ed25519 signature over sha256(binary)
      SHA256       string   // hex sha256 of the binary
      ReleaseNotes string   // optional, surfaced in UI
      ForceUpdate  bool     // tells the agent to skip user dismissal
    }
```

Source: `packages/agent/internal/sync/hub/client.go` — `Client.CheckUpdate` + `UpdateInfo` struct. The agent-side caller is `Updater.CheckAndUpdate` in `packages/agent/internal/host/updater/updater.go`.

## 2. Updater configuration

```go
type Config struct {
    Enabled       bool
    CheckInterval time.Duration
    PublicKey     ed25519.PublicKey  // supplied by caller; no go:embed
}
```

The Ed25519 public key is **not** embedded in the agent binary via `//go:embed` — it is supplied by the caller through `Config.PublicKey` when constructing the `Updater` (`NewUpdater(client, cfg, version, osName, binaryPath)`). In prod today, no public key is provisioned, so `NewUpdater` logs a warn and forcibly sets `cfg.Enabled = false`:

> *"auto-updater disabled: no Ed25519 public key configured — updates will be rejected without signature verification"*

This is the expected steady state today; the warn level avoids spamming admin ERROR dashboards on every agent restart.

`CheckInterval` is also caller-supplied — typically 1h per the `RunWithAvailabilityCallback` comment. There is no 6h default in the package.

## 3. Polling cycle

`Run(ctx)` (or `RunWithAvailabilityCallback(ctx, fn)`) fires one immediate check on start, then ticks at `cfg.CheckInterval`. Each tick:

1. If `cfg.Enabled`: run `CheckAndUpdate` (download + verify + swap). On success the loop returns — launchd / systemd restarts the new binary.
2. If not enabled: still call `checkAvailabilityOnce` so the optional `availableFn(bool)` callback can light up an "Update available — install" banner in the Dashboard. This decouples *availability detection* from *automatic install*, so prod (no embedded key, install disabled) can still surface "click here to download the .pkg".

`checkAvailabilityOnce` logs errors at debug only — a Hub outage does not spam the agent log on every poll.

## 4. Download → verify → swap (CheckAndUpdate)

When `Available=true`:

1. **Download** `info.DownloadURL` to `binaryPath + ".tmp"` via the mTLS-pinned HTTP client (`client.HTTPClient()`).
2. **SHA-256 mandatory** — if `info.SHA256` is empty the update is rejected; otherwise the downloaded bytes are hashed and compared.
3. **Ed25519 signature mandatory** — `info.Signature` must be non-empty AND `cfg.PublicKey` must be set. The signature is verified by `ed25519.Verify(pubKey, sha256(file), sig)` (i.e., the agent signs the file's hash, not the file itself; see `verifySignature`).
4. **Atomic swap** — `atomicSwap` renames `binaryPath → binaryPath + ".rollback"`, then renames the new file → `binaryPath`. There is no `.previous/` directory, no per-platform installer invocation (no `installer -pkg`, no `msiexec`, no `apt-get install`). The OS supervisor (launchd / systemd / SCM) restarts the new binary on next spawn.

Any failure at step 2-4 deletes the tmp file and surfaces an error; the next poll re-attempts.

## 5. Crash-loop rollback

`DetectCrashLoop(binaryPath, statusFile, threshold)` is called at agent boot:

1. The previous run wrote `time.Now().Format(time.RFC3339)` to `statusFile` via `WriteStartStatus`.
2. If `time.Since(lastStart) < threshold` (i.e., the previous run died very fast) AND a `binaryPath + ".rollback"` file exists, the rollback is renamed back over `binaryPath` and `DetectCrashLoop` returns `true`.

This is a single-file rollback: there is no version manifest, no multi-step downgrade. The supervisor restarts the original binary on the next spawn.

## 6. Force-update flag

`UpdateInfo.ForceUpdate=true` is honored by the Updater: it logs `force=true` on the "update available" line so operators see it, and the install path runs identically (no separate "skip user prompt" branch — the agent has no user prompt). Real-user dismiss-and-defer UI lives in the Dashboard and consults the same availability callback.

## 7. Today's reality (PKG distribution)

Per memory `reference_agent_pkg_distribution`, today's prod macOS .pkg distribution is **manual**:

- Engineer builds with the `build-agent` skill (binding).
- Manually `scp` to nginx `/downloads/`.
- Canonical URL: `https://nexus.example.com/downloads/NexusAgent-latest.pkg`.
- Users download + install manually.

The in-binary `Updater` exists and is wired into the agent main loop, but with no public key provisioned in prod it stays in the warn-and-disable state. Lighting up automatic install requires (a) provisioning an Ed25519 keypair, (b) wiring `Config.PublicKey` in agent main, (c) standing up the Hub `update-check` backend with a release-server-driven manifest. Tracked as planned work.

## 8. Sources

- `packages/agent/internal/host/updater/updater.go` — `Config`, `Updater`, `CheckAndUpdate`, `Run` / `RunWithAvailabilityCallback`, `verifySignature`, `atomicSwap`, `DetectCrashLoop`, `WriteStartStatus`, `fileSHA256`.
- `packages/agent/internal/sync/hub/client.go` — `UpdateInfo` wire type + `Client.CheckUpdate` (`GET /api/internal/things/update-check`).
- Memory: `reference_agent_pkg_distribution` — current prod distribution state.

## 9. Cross-references

- `.claude/skills/build-agent/` — binding build procedure.
- `agent-paths-abstraction-architecture.md` — install paths.
- `agent-enrollment-architecture.md` — separate device-cert lifecycle.
- `agent-runtime-invariants.mdc` — runtime bindings the new version must honour.
