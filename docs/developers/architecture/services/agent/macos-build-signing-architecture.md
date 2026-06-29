# macOS agent build & signing

The macOS agent ships as a Developer ID-signed, notarized `.pkg`. Three signed
artifacts have to agree on identity and entitlements for the OS to let the agent
load its NetworkExtension, so the build is a fixed pipeline rather than a set of
ad-hoc commands. This document describes that pipeline's architecture and the
macOS trust and approval model it must satisfy.

The pipeline is owned by the **`build-agent` skill**, which is the single source
of truth for how it is invoked — the concrete signing identities, certificate
and provisioning-profile locations, notarization credentials, the exact command
sequence, and the post-install verification and recovery steps. Never improvise
`codesign` / `pkgbuild` / `productbuild` / `productsign` / `xcrun notarytool` /
`wails build` / `swift build` calls; run the skill. NetworkExtension is the sole
intercept path on macOS (see
[agent-macos-platform-architecture.md](agent-macos-platform-architecture.md)).

## The artifacts

The build produces one app bundle that contains everything and one installer:

- **`NexusAgent.app`** — the Swift menu-bar host app (bundle ID
  `com.nexus-gateway.agent`).
- **The Go daemon** — the `nexus-agent` binary inside the app bundle; a universal
  (arm64 + amd64) build with CGO enabled for Keychain access.
- **The NE system extension** — bundled inside the app at
  `Contents/Library/SystemExtensions/com.nexus-gateway.agent.extension.systemextension`
  (bundle ID `com.nexus-gateway.agent.extension`).
- **`NexusAgent-<version>.pkg`** — the distribution installer end users run.

## The build → sign → notarize → package pipeline

The production script runs these steps in order; each signing step uses
`codesign --options runtime --timestamp` (hardened runtime) with the artifact's
own entitlements:

1. **Go binary** — universal `nexus-agent` (arm64 + amd64 via `lipo`).
2. **Swift build** — the `NexusAgentUI` host app and the `NexusAgentExtension`,
   universal, from the SwiftPM package rooted at `darwin/`.
3. **Bundle the system extension** into the app's `Library/SystemExtensions`.
4. **Sign the extension** with the Developer ID Application identity and the
   extension entitlements (embedding the extension provisioning profile).
5. **Sign the Go daemon** with the Developer ID Application identity and the
   minimal daemon entitlements (no NetworkExtension).
6. **Sign the embedded dashboard** — the nested Wails dashboard app under the
   host's `Contents/Resources` is signed `--deep` with hardened runtime and no
   entitlements (it is a plain WebKit app), because Apple's notary rejects any
   unsigned, un-hardened, or un-timestamped nested binary.
7. **Sign the host app** with the Developer ID Application identity, the host
   entitlements, and the embedded host provisioning profile (innermost-first
   signing order means the host is signed last), then
   `codesign --verify --deep --strict` over the whole bundle.
8. **`pkgbuild`** the component package (non-relocatable).
9. **`productbuild`** the distribution package.
10. **`productsign`** with the Developer ID Installer identity.
11. **Notarize** — `notarytool submit --wait` then `stapler staple` + `validate`.

Notarization is the one optional step: without stored notarytool credentials the
script still produces a correctly codesigned, productsigned `.pkg` (usable on
internal machines), and skips submit/staple. Every other step is mandatory — an
ad-hoc/linker-signed build (what the plain dev build script emits) lacks the
system-extension install entitlement and a team-id binding, so macOS rejects the
extension and the proxy configuration cannot be saved.

## Entitlements and launch constraints

The three artifacts carry three distinct entitlement sets:

| Artifact | Entitlements |
|----------|-------------|
| Host app | `com.apple.developer.networking.networkextension` = `app-proxy-provider-systemextension`, plus `system-extension.install`, `application-identifier`, `team-identifier` |
| Go daemon | network client/server only — no NetworkExtension |
| Extension | `com.apple.developer.networking.networkextension` = `app-proxy-provider-systemextension`, plus `network.client` |

macOS enforces launch constraints that the signing must satisfy: every
entitlement in the code signature must be authorized by the embedded
provisioning profile; `application-identifier` (team + bundle id) and
`team-identifier` must be present explicitly, since signing with `codesign
--entitlements` directly does not auto-inject them; and the host app must not
carry an entitlement absent from its profile. A mismatch surfaces as a launchd
spawn failure. Both the host and the extension entitlements use the
`-systemextension`-suffixed networkextension value; the unsuffixed
`com.apple.networkextension.app-proxy` appears only as the extension-point key
in the extension's `Info.plist` `NEProviderClasses`, not as an entitlement
value.

The extension's `Info.plist` must declare `CFBundlePackageType` `SYSX` and put
its principal class under `NetworkExtension` → `NEProviderClasses`, keyed by the
app-proxy extension-point identifier, with the value in `ModuleName.ClassName`
form (the SwiftPM target name is the module name). The wrong package type or
placement makes the extension fail to register at all.

## The two-stage approval model

Even a perfectly signed install does not start intercepting until the user
grants two separate approvals:

1. **System extension activation** (OS-level: the extension binary is allowed to
   load) — granted by the first-run prompt, or pre-approved on managed devices by
   deploying the bundled configuration-profile template.
2. **Network Extension filter** (per-configuration: this proxy configuration is
   allowed to run) — toggled in System Settings under Login Items & Extensions →
   Network Extensions.

Saving the proxy configuration succeeds even when the filter toggle is off, so
the agent log cannot tell that the toggle is the missing piece; the visible
symptom is the menu bar staying yellow with no extension process attached to the
daemon's `ne.sock`. The install is not complete until the user flips the filter
toggle on.

A signed build binds the system-extension approval slot to its team identifier.
Because System Integrity Protection guards that binding, an ad-hoc or
wrong-team build cannot claim a slot a prior signed build already holds; the
clean recovery is to reinstall a build signed with the same team identifier (the
`build-agent` skill documents the destructive fallbacks).

## Install and uninstall

Registration is **bundle-tied via `SMAppService`** (macOS 13+), the modern
replacement for the deprecated `SMJobBless` / `SMLoginItemSetEnabled`. The daemon
launchd plist ships *inside* the app bundle at
`Contents/Library/LaunchDaemons/com.nexus-gateway.agent.plist` (a `BundleProgram`
relative to the bundle), and the menu app registers it on first launch via
`SMAppService.daemon(plistName:)` — a system-domain boot daemon (root,
`RunAtLoad`, `KeepAlive`), so pre-login enforcement is preserved (it is **not** a
per-user login agent). The menu app registers itself as a Login Item via
`SMAppService.mainApp`. Both
registrations are brokered by `smd`/launchd with a one-time approval — the
unprivileged menu app needs no `sudo` and no `launchctl bootstrap`. On a managed
device the `com.apple.servicemanagement` profile pre-approves both, so
`register()` lands in `.enabled` silently. See `LaunchServiceManager.swift`.

Because the registration is tied to the `.app`, **deleting the app deregisters
the daemon** and schedules the NE extension for removal — no orphaned
`/Library/LaunchDaemons` plist, no launchd spawn-error spam, and no
free-standing plist that could be bootstrapped or get stuck disabled (the
`disabled.plist`-override `Bootstrap failed: 5: Input/output error` class).

Installation still goes through the `.pkg` only — never by copying the `.app`
into `/Applications` — and the post-install script does not bootstrap launchd.
It only:

- sets ownership and permissions on the state directory (`root:wheel`, with
  `agent.yaml` mode `640`), the world-writable flags directory used for
  privilege-free signaling, and the log directory (see
  [agent-paths-abstraction-architecture.md](agent-paths-abstraction-architecture.md));
- runs `nexus-agent install-ca` (device CA → state dir + system trust store;
  idempotent on upgrade) and writes the CLI-trust env vars
  (`NEXUS_DEVICE_CA_PEM` / `NODE_EXTRA_CA_CERTS` / `SSL_CERT_FILE`) into the
  console user's shell rc files;
- removes any classic `/Library/LaunchDaemons` + `/Library/LaunchAgents` plists
  left by a pre-SMAppService build (one-time migration — no parallel legacy
  path), then opens the app once in the console user's session so SMAppService
  registration + NE activation (and, on unmanaged devices, the approval prompts)
  kick off immediately;
- flushes the DNS cache (`mDNSResponder`) after the NE state change.

The uninstall script is idempotent: it stops the running daemon by label
(classic or SMAppService), removes the app (which deregisters the SMAppService
daemon and schedules the NE for removal), cleans any classic `/Library` residue,
wipes state/logs/keychain, and optionally the per-user `~/.nexus` data. Upgrades
do not require a manual uninstall-first: preinstall boots out the running
daemon by label and the post-install app launch re-registers the new bundle.

## References

- `.claude/skills/build-agent/skill.md` — the binding source of truth for invoking the pipeline (identities, credentials, command sequence, verification, recovery)
- `packages/agent/platform/darwin/Scripts/build-prod.sh` — the production build / sign / notarize / package pipeline
- `packages/agent/platform/darwin/Scripts/build.sh` — the dev app-bundle builder (ad-hoc signed; not for deployment)
- `packages/agent/platform/darwin/Scripts/uninstall.sh` — the uninstall sequence
- `packages/agent/platform/darwin/Package.swift` — the SwiftPM root building the host app and the extension
- `packages/agent/platform/darwin/NexusAgent/NexusAgent.entitlements` — host-app entitlements
- `packages/agent/platform/darwin/NexusAgent/NexusAgentDaemon.entitlements` — Go daemon entitlements
- `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/NexusAgentExtension.entitlements` — extension entitlements
- `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/Info.plist` — extension bundle (`SYSX`, `NEProviderClasses`)
- `packages/agent/platform/darwin/NexusAgentUI/Sources/App/LaunchServiceManager.swift` — SMAppService daemon + login-item registration (`register`/`status`/approval deep-link)
- `packages/agent/platform/darwin/installer/postinstall.sh` — state dirs, permissions, `install-ca`, CLI-trust env vars, classic-registration migration, DNS flush, first-launch app open (no launchd bootstrap)
- `packages/agent/platform/darwin/installer/LaunchDaemon.plist` — the SMAppService daemon job (`BundleProgram`), embedded in the app at `Contents/Library/LaunchDaemons/`
- `packages/agent/platform/darwin/installer/Distribution.xml` — the productbuild distribution definition
- `packages/agent/platform/darwin/installer/nexus-agent.mobileconfig.template` — MDM pre-approval profile template
