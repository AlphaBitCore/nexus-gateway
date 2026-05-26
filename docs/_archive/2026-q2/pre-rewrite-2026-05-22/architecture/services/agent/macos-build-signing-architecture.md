---
doc: macos-build-signing-architecture
area: service
service: agent
tier: 1
updated: 2026-05-20
---

# macOS Build & Signing Architecture

> **Tier 2 architecture doc.** Read when touching the macOS build pipeline for the agent. **The procedural binding is the `build-agent` skill** (`.claude/skills/build-agent/skill.md` — lowercase `skill.md`) — never run `wails build` / `codesign` / `productbuild` / `xcrun notarytool` manually. This doc is the design-side companion explaining what those steps do and why.

The macOS agent is the highest-blast-radius binary in the system (cross-ref `agent-ne-fail-open-architecture.md`). The build pipeline is correspondingly strict: signed by a Developer ID, notarised by Apple, packaged with launch-constraints, distributed via a signed `.pkg`.

---

## 1. The artefact chain

```
Go source                  →  daemon binary (signed)         ──┐
Swift source (NE provider) →  .appex bundle (signed)         ──┤
Wails frontend (React)     →  .app bundle (signed)           ──┼─→  combined .app
                              + launch-constraints embedded     │
                                                                ▼
                                                              .pkg (signed by Developer ID Installer)
                                                                │
                                                                ▼
                                                              notarytool (Apple notarisation)
                                                                │
                                                                ▼
                                                              stapled .pkg (ready for distribution)
```

## 2. Signing identities

| Identity | Used for |
|---|---|
| **Developer ID Application** | `.app`, `.appex` (daemon, NE provider, UI) |
| **Developer ID Installer** | `.pkg` |
| **Provisioning Profile (Network Extension)** | NE entitlement |

Identities live in the keychain on the build host. The CI build host has its own keychain isolated from developer machines.

## 3. Entitlements

The agent uses several entitlements:

- `com.apple.security.network.client` — outbound network (always on).
- `com.apple.developer.networking.networkextension` (value `app-proxy-provider-systemextension`) — NE registration. Per the `build-agent` skill identity table this is the suffixed form; the non-suffixed `app-proxy-provider` is only valid INSIDE the extension bundle (macOS 26 launch-constraint requirement).
- `com.apple.developer.system-extension.install` — system extension install.

The entitlement plist + provisioning profile must match the bundle ID. Mismatches cause silent NE refusal at runtime.

## 4. macOS 26 launch constraints

Apple introduced launch constraints on macOS 26 that require system extensions to be launched only by signed parent processes. The agent installer / daemon embeds launch-constraint blobs in its bundle to declare:

- Allowed parent process: `com.nexus-gateway.agent` (the daemon).
- Required signature: the same Developer ID.

Mis-configured constraints make the NE refuse to launch with a cryptic error. The `build-agent` skill carries the canonical constraint XML.

## 5. Notarisation

After signing, the `.pkg` is submitted to Apple's notarytool service:

```
xcrun notarytool submit <pkg> --keychain-profile <profile-name> --wait
```

`keychain-profile` is a notarytool credential stored in the keychain — never as an env var (apple keychain handle is the safest place).

Apple scans the package, returns a result. On success, staple the ticket to the `.pkg`:

```
xcrun stapler staple <pkg>
```

The stapled pkg can install offline (the ticket is embedded; macOS gatekeeper doesn't need to phone home).

## 6. Why `build-agent` skill is binding

Improvising around the build steps can brick the Network Extension. Even with the right `codesign` command, mis-ordering steps (e.g., signing the inner .appex after the outer .app) corrupts the signature chain in ways that pass `codesign --verify` locally but fail Gatekeeper on a clean machine.

The `build-agent` skill is the **single source of truth** for the exact command sequence, env vars, and post-install verification. CLAUDE.md "Use the `build-agent` skill" binding rule reinforces this.

Adjustments allowed under the skill's discretion: prefixing PATH so the non-interactive shell finds Wails CLI. Everything else: verbatim.

## 7. The adhoc vs prod fork

The `build-agent` skill has two paths:

- `build.sh` — adhoc-signed `.app`. **Borderline-banned.** Use only on a dev machine that has NEVER had a Developer-ID-signed prod build installed (the same machine cannot host both — the system extension subsystem will brick on the next NE activation when the signatures collide). NE silently fails to connect; menu bar + UI work; useful for tearing through Go-only changes without the signing dance.
- `build-prod.sh` — signed + notarised. Use for testing on a user's machine and for shipping.

The decision table in the skill's first section is emphatic: the brick-risk on a mixed host is real and recovery can require `sudo systemextensionsctl reset` plus reinstall. When in doubt, default to `build-prod.sh`. The wrong path wastes time per attempt and can require host-level recovery.

## 8. Build host setup

Required:

- macOS (Apple Silicon or Intel — both work; CI uses arm64).
- Xcode (full install, not just Command Line Tools — Swift compiler needs the SDK).
- Go 1.25+.
- Wails CLI 2.x (`go install github.com/wailsapp/wails/v2/cmd/wails@latest`).
- The signing identities in the keychain.
- The notarytool keychain profile registered.

Setup is one-time per host. Documented in the `build-agent` skill's preconditions.

## 9. Distribution

Today: manual scp + nginx `/downloads/` (memory `reference_agent_pkg_distribution`). Canonical URL: `https://nexus.taskforce10x.com/downloads/NexusAgent-latest.pkg`.

Future: signed-release server (cross-ref `agent-autoupdater-architecture.md` §2 manifest). The signing flow doesn't change; only the distribution layer.

## 10. Install / uninstall sequencing

Install:

1. Run `.pkg` (requires admin password).
2. Installer drops daemon + UI + LaunchDaemon plist.
3. Daemon starts, prompts user to allow the Network Extension in System Settings.
4. User approves → NE active.

Uninstall:

```bash
sudo bash packages/agent/platform/darwin/Scripts/uninstall.sh
```

This is the canonical path. The script unloads the LaunchDaemon (`com.nexus-gateway.agent`), removes the system extension, and deletes:

- `/Library/LaunchDaemons/com.nexus-gateway.agent.plist`
- `/Library/Application Support/com.nexus-gateway.agent/`
- `/Library/Logs/com.nexus-gateway.agent/`

For an unrecoverable host, the nuclear fallback is `sudo systemextensionsctl reset` (clears ALL system extensions on the machine).

Never improvise raw `launchctl unload` + `rm` recipes — they skip the system-extension teardown and leave the host in a partial-uninstall state that breaks the next install.

<!-- 💡 harvest: the "verbatim follow build-agent skill" rule is already in CLAUDE.md as a binding. Could be a Cursor rule scoped to packages/agent/platform/darwin/** but binding-rules-quick-reference already points to it. Skipping additional rule. -->

## 11. Sources

- `.claude/skills/build-agent/skill.md` — the procedural binding (single source of truth for commands; filename is lowercase, case-sensitive on Linux build hosts).
- `packages/agent/platform/darwin/NexusAgent/` — Xcode project + signing config.
- `dist/macos/` — build output directory (gitignored).
- Memory: `reference_agent_pkg_distribution` — current distribution channel.

## 12. Cross-references

- `agent-ne-fail-open-architecture.md` — what the signed NE provider does at runtime.
- `agent-autoupdater-architecture.md` — future autoupdate channel using these signed pkgs.
- `.claude/skills/build-agent/` — canonical build procedure.
