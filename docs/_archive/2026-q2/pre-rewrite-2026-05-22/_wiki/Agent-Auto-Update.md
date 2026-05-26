# Agent Auto Update

*Audience: operators managing agent fleet versions; security reviewers auditing the update signing chain.*

The agent includes an in-binary autoupdater that polls Hub for available updates, verifies SHA-256 + Ed25519 signatures before applying, and performs an atomic binary swap with crash-loop rollback. The update mechanism is fully wired in code but automatic installs are disabled in the current production configuration — no Ed25519 public key is provisioned, so the updater runs in availability-detection-only mode. Operators currently distribute new versions by downloading from the canonical distribution URL and installing manually.

---

## Hub-driven update check

The agent learns about available updates by querying Hub:

```
GET /api/internal/things/update-check?currentVersion=<v>&os=<osName>
```

Hub responds with:

```json
{
  "Available":    true,
  "Version":      "1.2.3",
  "DownloadURL":  "<signed URL to the binary>",
  "Signature":    "<base64 Ed25519 sig over sha256(binary)>",
  "SHA256":       "<hex sha256 of the binary>",
  "ReleaseNotes": "...",
  "ForceUpdate":  false
}
```

Source: `packages/agent/internal/sync/hub/client.go` — `Client.CheckUpdate` and `UpdateInfo` struct. The agent-side caller is `Updater.CheckAndUpdate` in `packages/agent/internal/host/updater/updater.go`.

When `Available=true` and automatic installs are enabled, the updater:

1. Downloads the binary from `DownloadURL` to `binaryPath + ".tmp"` via the mTLS-pinned HTTP client.
2. Verifies the SHA-256 hash (mandatory — empty `SHA256` rejects the update).
3. Verifies the Ed25519 signature: `ed25519.Verify(pubKey, sha256(file), sig)`. The public key is supplied at construction time via `Config.PublicKey`; both the public key and the signature must be non-empty.
4. Atomically swaps the binary: renames `binaryPath → binaryPath + ".rollback"`, then renames the new file → `binaryPath`.

The OS supervisor (launchd / systemd / Windows SCM) restarts the new binary on the next spawn. Any failure at steps 2-4 deletes the temp file; the next poll re-attempts.

## Updater configuration

```go
type Config struct {
    Enabled       bool
    CheckInterval time.Duration
    PublicKey     ed25519.PublicKey  // supplied by caller; no embedded key
}
```

The Ed25519 public key is not embedded in the agent binary via `//go:embed`. It is supplied by the caller via `Config.PublicKey` when constructing the `Updater`. In the current production configuration no public key is provisioned, so `NewUpdater` logs a warning and sets `cfg.Enabled = false`:

> "auto-updater disabled: no Ed25519 public key configured — updates will be rejected without signature verification"

This is the expected steady state. The updater still calls `checkAvailabilityOnce` so the Dashboard's "About" page can show an "Update available — install" banner pointing the user to the download URL.

Enabling automatic install requires provisioning an Ed25519 keypair, wiring `Config.PublicKey` in the agent main boot, and standing up the Hub `update-check` backend with a release-server-driven manifest.

## Crash-loop rollback

`DetectCrashLoop(binaryPath, statusFile, threshold)` runs at agent boot:

1. On a clean start, the agent writes the current timestamp to `statusFile` via `WriteStartStatus`.
2. On the next boot, if `time.Since(lastStart) < threshold` (the previous run died very quickly) AND a `.rollback` file exists, the rollback is renamed back over the live binary and `DetectCrashLoop` returns `true`.
3. The supervisor restarts the agent using the rolled-back binary.

This is a single-file rollback. There is no version manifest, no multi-step downgrade. The threshold is caller-configured; a typical value is 60s (a run that dies within 60s of starting is treated as a crash).

## Rollout rings

Operators can control which devices receive updates first via per-device-group config cascades:

1. Create DeviceGroups: `canary-ring`, `beta-ring`, `stable-ring`.
2. Attach `device_group_config` rows keyed on `agent_settings.autoUpdateChannel` with priority overrides (canary → 300, beta → 200, stable → 100).
3. Hub's config cascade resolves: per-device override > highest-priority group > fleet template.

Devices move between rings by changing their group membership or tags. Smart-group predicates can automate ring membership — for example, devices tagged `canary` enter the canary ring automatically.

To verify a device resolved the correct channel:

```bash
# Query the thing's desired state directly in the DB:
psql -U nexus -d nexus_gateway \
  -c "SELECT desired->'agent_settings'->>'autoUpdateChannel' AS channel
        FROM thing WHERE id = '<device-id>';"
```

The `source` field in the Hub config pull response (`group:canary-ring`, `fleet:default`, etc.) confirms which tier won the cascade.

For per-platform pinning (Windows on stable, macOS on beta), combine smart-group predicates — for example, `{"all": [{"field": "os", "op": "eq", "value": "windows"}]}` — with per-ring channel assignments.

## Current distribution (manual)

The canonical macOS distribution URL is `https://nexus.taskforce10x.com/downloads/NexusAgent-latest.pkg`. Linux and Windows follow the same manual pattern. The in-binary updater exists and runs the availability check on each poll, but does not install automatically until a public key is provisioned.

---

## Canonical docs

- [`agent-autoupdater-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-autoupdater-architecture.md) — Hub-driven update check API; download/verify/swap flow; crash-loop rollback; current production state
- [`r-version-pinning-rollout-rings.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/r-version-pinning-rollout-rings.md) — Rollout ring setup via per-group config cascade; promotion workflow; verification commands

**Adjacent wiki pages**: [Agent Overview](Agent-Overview) · [Agent macOS NE Architecture](Agent-macOS-NE-Architecture) · [Agent Enrollment Attestation](Agent-Enrollment-Attestation) · [Thing Model And Config Sync](Thing-Model-And-Config-Sync) · [Security Supply Chain](Security-Supply-Chain)
