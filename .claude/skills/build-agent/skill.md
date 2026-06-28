# build-agent

Build, sign, notarize, and package the macOS NexusAgent for production.

NetworkExtension (NE) is the **sole** intercept path. The experimental
pf-only alternative (E74) was retired before shipping; every build is
NE-based.

---

## Precondition checks BEFORE running build-prod.sh

```bash
# 1. Wails CLI must be on PATH (build.sh + build-prod.sh both invoke `wails build`)
command -v wails || {
  echo "wails CLI not found; install with: go install github.com/wailsapp/wails/v2/cmd/wails@latest"
  echo "Then ensure \$GOPATH/bin is on PATH (run-time env may differ from interactive shell)"
  exit 1
}

# 2. Signing certs must be in keychain
security find-identity -v -p codesigning | grep -E "Developer ID Application: YOUR ORG NAME" || {
  echo "Developer ID Application cert missing from keychain"; exit 1; }
security find-identity -v | grep -E "Developer ID Installer: YOUR ORG NAME" || {
  echo "Developer ID Installer cert missing"; exit 1; }

# 3. Provisioning profiles must exist
test -f ~/nexus-certs/Nexus_Agent_Developer_ID.provisionprofile || { echo "host profile missing"; exit 1; }
test -f ~/nexus-certs/Nexus_Agent_Extension_Developer_ID.provisionprofile || { echo "ext profile missing"; exit 1; }

# 4. Notarytool credentials stored
xcrun notarytool history --keychain-profile nexus-notarytool 2>&1 | head -1 | grep -q "createdDate" || {
  echo "notarytool profile 'nexus-notarytool' not stored; run the store-credentials step in One-Time Setup"; exit 1; }
```

If ANY of these fail, **stop**. Don't run a partial build — that's how the user's machine gets bricked.

## Why adhoc-signed is broken (never deploy `build.sh` output)

`build.sh` produces an `adhoc, linker-signed` .app with no `com.apple.developer.system-extension.install` entitlement and no team-id binding. macOS's `OSSystemExtensionRequest` rejects it (`"Missing entitlement com.apple.developer.system-extension.install"`) and `NETransparentProxyManager.saveToPreferences` returns `"permission denied"`.

If the user's machine **previously** had a Developer-ID-signed build installed (team `39U3X3FFVK`), the prior approval slot stays bound to that team_id and SIP prevents `systemextensionsctl uninstall` from clearing it. The new adhoc-signed .app cannot claim the slot — and you cannot fix this without one of:

1. Re-installing a properly Developer-ID-signed build with the **same team_id** as the prior install (clean), OR
2. Booting into Recovery mode + `systemextensionsctl reset` (destructive — wipes ALL system-extension approvals on the machine), OR
3. Disabling SIP (don't).

**Use `build.sh` only on a clean dev box that has never installed a signed build. For anything else, use `build-prod.sh`.**

---

## Post-install: USER must toggle Network Extension in System Settings (macOS 26+)

Even on a perfectly-signed install where `systemextensionsctl list` shows
`[activated enabled]` and `agent.log` reports both
`system-extension-install: ok` + `transparent-proxy-save: ok`, the
provider **process will NOT start** until the user flips a per-filter
toggle in System Settings. macOS 26 (Tahoe) separated the two approvals:

| Approval | Path | What it grants |
|---|---|---|
| System Extension activation | First-run prompt OR System Settings | OS-level: extension binary is allowed to load |
| Network Extension filter | System Settings → General → Login Items & Extensions → Network Extensions | Per-config: this specific proxy configuration is allowed to run |

`saveToPreferences` succeeds for both states (filter present but toggled off
ALSO returns success), so the agent log gives no hint that the user toggle
is the missing piece. Symptom: menu bar stays yellow with
"Attention Needed — Network filter not connected", `pgrep` finds no
`com.nexus-gateway.agent.extension` process, and `lsof /var/run/nexus-agent/ne.sock`
shows only the daemon listening (no client connection).

**After every install, instruct the user to:**

1. Open **System Settings**
2. **General** → **Login Items & Extensions**
3. Scroll to **Network Extensions** section
4. Find **Nexus Agent (YOUR ORG NAME)** and **toggle it ON**

Then verify:

```bash
pgrep -fl "com.nexus-gateway.agent.extension"   # should show one process
sudo lsof /var/run/nexus-agent/ne.sock           # should show TWO entries
                                                  # (daemon listen + extension client)
```

Menu bar status flips to green / Active within a few seconds of the toggle.

## Strict install / uninstall sequence

```bash
# Uninstall first (idempotent — safe to run on a never-installed machine).
# Skipping this is fine for a first install, but ESSENTIAL when upgrading
# because launchctl will not pick up a replaced binary unless the daemon
# is stopped+restarted.
sudo bash packages/agent/platform/darwin/Scripts/uninstall.sh
# (the script will prompt "remove user data ~/.nexus? [y/N]"; answer y to
#  fully start fresh; n to preserve session preferences)

# Install via .pkg (this is what end users see; the .pkg postinstall handles
# launchctl bootstrap, install-ca, file perms — DO NOT do `cp -R .app /Applications`).
sudo installer -pkg dist/macos/NexusAgent-<VERSION>.pkg -target /

# Open the menu-bar app to trigger system-extension activation prompt
open /Applications/NexusAgent.app

# Verify install
sudo journalctl --no-pager -u nexus-agent --since '1 min ago' 2>/dev/null || \
  sudo tail -20 /Library/Logs/com.nexus-gateway.agent/agent.log
systemextensionsctl list | grep nexus
```

## Recover when a dev-build corrupted the NE approval slot

Symptom: `agent.log` shows repeated `"proxy install report" outcome:error error:"Missing entitlement com.apple.developer.system-extension.install"` AND `"transparent-proxy-save" error:"permission denied"`, menu bar shows `"Attention Needed — Network filter not connected"`.

Fix path (least to most disruptive):

1. **Best**: rebuild via `build-prod.sh` and reinstall the .pkg. macOS will recognize the same team_id approval and the new signed binary will take over the slot cleanly.
2. **If you don't have the certs to sign**: boot into Recovery mode → Terminal → `csrutil disable` → reboot → `systemextensionsctl reset` (wipes ALL system extensions on the machine) → boot back → `csrutil enable` → reboot → reinstall.
3. **Last resort**: a clean macOS reinstall.

---

## Identity & Certificates

| Item | Value |
|------|-------|
| Apple ID | `apple-id@example.com` |
| Team ID | `39U3X3FFVK` |
| Team name | `YOUR ORG NAME` |
| Developer ID Application | `Developer ID Application: YOUR ORG NAME (39U3X3FFVK)` |
| Developer ID Installer | `Developer ID Installer: YOUR ORG NAME (39U3X3FFVK)` |
| Notarytool profile | `nexus-notarytool` (keychain credential, see setup below) |

All certificates and profiles live in `~/nexus-certs/`:

```
~/nexus-certs/
  DeveloperIDG2CA.cer                              # Apple intermediate CA
  developerID_application.cer                      # Developer ID Application cert
  developerID_installer.cer                        # Developer ID Installer cert
  developer_id_application.p12                     # Application cert + key (imported to keychain)
  developer_id_installer.p12                       # Installer cert + key (imported to keychain)
  Nexus_Agent_Developer_ID.provisionprofile        # Host app: com.nexus-gateway.agent
  Nexus_Agent_Extension_Developer_ID.provisionprofile  # Extension: com.nexus-gateway.agent.extension
  app-specific-password.txt                        # App-specific password for notarytool
```

---

## Bundle IDs

| Component | Bundle ID |
|-----------|-----------|
| Host app (Swift menu bar) | `com.nexus-gateway.agent` |
| NE System Extension | `com.nexus-gateway.agent.extension` |
| Go LaunchDaemon binary | (no bundle ID — signed with minimal entitlements) |
| LaunchDaemon label / plist | `com.nexus-gateway.agent` |
| pkg component identifier | `com.nexus-gateway.agent.pkg` |
| pkg distribution identifier | `com.nexus-gateway.agent.distribution` |

---

## Skill CLI Reference

```
Skill('build-agent', '[--version=<semver>]')
```

| Argument | Values | Default | Notes |
|---|---|---|---|
| `--version` | Semver string (e.g. `1.4.2`) | `git describe --tags --always` | Baked into .pkg filename and `main.version` ldflag |

**Exit codes:**

| Code | Meaning |
|---|---|
| 0 | Build, sign, notarize, and staple all succeeded; verification checks passed |
| 1 | Precondition failure (cert missing, wails not on PATH, notarytool profile absent) — nothing was built |
| 2 | Build step failed (Swift compile error, `go build` error, `pkgbuild` error) |
| 3 | Code-signing failure |
| 4 | Notarization failure (Apple-side rejection or network timeout) |
| 5 | Post-build verification failure (`stapler validate` failed) |

**Report format** (printed to stdout on successful run):

```
=== build-agent report ===
Version:      1.4.2
Output:       dist/macos/NexusAgent-1.4.2.pkg
Notarized:    yes
Entitlements: NexusAgentExtension.entitlements
=========================
```

---

## Production Build Command

```bash
cd ~/workspaces/workspace-nexus/nexus-gateway

sudo rm -rf dist/macos/   # clean previous build artifacts (required: codesign sets root ownership)

DEVELOPER_ID_APPLICATION="Developer ID Application: YOUR ORG NAME (39U3X3FFVK)" \
DEVELOPER_ID_INSTALLER="Developer ID Installer: YOUR ORG NAME (39U3X3FFVK)" \
NOTARYTOOL_PROFILE="nexus-notarytool" \
PROVISION_PROFILE="$HOME/nexus-certs/Nexus_Agent_Developer_ID.provisionprofile" \
PROVISION_PROFILE_EXT="$HOME/nexus-certs/Nexus_Agent_Extension_Developer_ID.provisionprofile" \
bash packages/agent/platform/darwin/Scripts/build-prod.sh
```

Output: `dist/macos/NexusAgent-<VERSION>.pkg`

### Optional: Vectorscan rule-pack engine (cgo, statically linked)

Off by default — the agent ships the pure-Go RE2 content-scan engine. To build
the agent with the Vectorscan accelerator instead, set `NEXUS_AGENT_VECTORSCAN=1`
and point `LIBHS_DIR` at a tree containing the Vectorscan headers and a static
archive per target arch:

```
$LIBHS_DIR/include/hs/hs.h
$LIBHS_DIR/lib/arm64/libhs.a
$LIBHS_DIR/lib/amd64/libhs.a   # required for the universal cross-compile
```

`build.sh` then adds `-tags "vectorscan vsstatic"` and links `libhs.a` (plus
`-lc++`, since Vectorscan is C++) statically, so the universal Mach-O has no
`libhs.dylib` runtime dependency and stays notarizable — the same model already
used for go-sqlcipher. Verified on arm64 (self-contained binary, no dylib dep);
the amd64 archive is the remaining vendoring step for a universal build.

Do NOT enable this for a SHIPPED enforcement build until the save-time rule
linter (rule-pack program task #12) lands: a user rule using `.` could silently
under-match multibyte abuse content under Vectorscan's byte-mode. Tag-on builds
for link verification are fine.

### What the script does (in order)

1. **Go binary** — universal `nexus-agent` (arm64+amd64, CGO for keychain).
2. **Swift build** — `NexusAgentUI` + `NexusAgentExtension` (universal, Package.swift at `darwin/`).
3. **Bundle SystemExtension** — `Contents/Library/SystemExtensions/com.nexus-gateway.agent.extension.systemextension`
4. **Sign extension** — Developer ID Application + `NexusAgentExtension.entitlements` (includes `com.apple.developer.networking.networkextension`).
5. **Sign Go daemon** — Developer ID Application + `NexusAgentDaemon.entitlements` (minimal, no NE).
6. **Sign host app** — Developer ID Application + `NexusAgent.entitlements` + host provisioning profile.
7. **pkgbuild** — component.pkg (BundleIsRelocatable=false)
8. **productbuild** — distribution .pkg (named `NexusAgent-<VERSION>.pkg`)
9. **productsign** — Developer ID Installer
10. **notarytool submit --wait** + **stapler staple + validate** — Apple notarization

Outputs:
```
dist/macos/NexusAgent.app
dist/macos/NexusAgent-<VERSION>.pkg           ← install this
```

---

## One-Time Setup (if starting fresh)

### 1. Generate CSRs (two separate ones)

```bash
mkdir -p ~/nexus-certs && cd ~/nexus-certs

# Developer ID Application
openssl req -new -newkey rsa:2048 -nodes \
  -keyout developer-id-application.key \
  -out developer-id-application.csr \
  -subj "/emailAddress=apple-id@example.com/CN=Nexus Agent Application/O=YOUR ORG NAME/C=MY"

# Developer ID Installer (separate key + CSR)
openssl req -new -newkey rsa:2048 -nodes \
  -keyout developer-id-installer.key \
  -out developer-id-installer.csr \
  -subj "/emailAddress=apple-id@example.com/CN=Nexus Agent Installer/O=YOUR ORG NAME/C=MY"
```

### 2. Create certificates in Apple Developer Portal

[developer.apple.com/account/resources/certificates/list](https://developer.apple.com/account/resources/certificates/list)

- **Developer ID Application** — upload `developer-id-application.csr` → download as `developerID_application.cer`
- **Developer ID Installer** — upload `developer-id-installer.csr` → download as `developerID_installer.cer`

### 3. Import Apple intermediate CA + certificates

```bash
cd ~/nexus-certs

# Apple intermediate CA
curl -O https://www.apple.com/certificateauthority/DeveloperIDG2CA.cer
security import DeveloperIDG2CA.cer -k ~/Library/Keychains/login.keychain-db

# Application cert → p12 → import
openssl x509 -inform DER -in developerID_application.cer -out developer_id_application.pem
openssl pkcs12 -export -legacy \
  -inkey developer-id-application.key -in developer_id_application.pem \
  -out developer_id_application.p12 -passout pass:nexus123
security import developer_id_application.p12 \
  -k ~/Library/Keychains/login.keychain-db -P nexus123 -T /usr/bin/codesign -A

# Installer cert → p12 → import
openssl x509 -inform DER -in developerID_installer.cer -out developer_id_installer.pem
openssl pkcs12 -export -legacy \
  -inkey developer-id-installer.key -in developer_id_installer.pem \
  -out developer_id_installer.p12 -passout pass:nexus123
security import developer_id_installer.p12 \
  -k ~/Library/Keychains/login.keychain-db -P nexus123 -A

# Unlock keychain ACL for codesign (prevents password prompts)
security set-key-partition-list \
  -S apple-tool:,apple:,codesign: -s -k "" \
  ~/Library/Keychains/login.keychain-db
```

Verify (should show 2 valid identities):
```bash
security find-identity -v -p codesigning | grep "Developer ID"
```

### 4. Store notarytool credentials

App-specific password is saved in `~/nexus-certs/app-specific-password.txt`.

```bash
xcrun notarytool store-credentials "nexus-notarytool" \
  --apple-id "apple-id@example.com" \
  --team-id "39U3X3FFVK" \
  --password "$(cat ~/nexus-certs/app-specific-password.txt)"
```

### 5. Register App IDs in Developer Portal

Two App IDs needed, both with **Network Extensions** capability enabled:

| App ID | Description |
|--------|-------------|
| `com.nexus-gateway.agent` | Host app |
| `com.nexus-gateway.agent.extension` | NE System Extension |

### 6. Create provisioning profiles

Developer Portal → Profiles → + → **Developer ID** → select App ID → select Developer ID Application cert → Generate → Download:

- `com.nexus-gateway.agent` → save as `~/nexus-certs/Nexus_Agent_Developer_ID.provisionprofile`
- `com.nexus-gateway.agent.extension` → save as `~/nexus-certs/Nexus_Agent_Extension_Developer_ID.provisionprofile`

---

## Prod URLs the .pkg bakes into agent.yaml

Domains below are placeholders — substitute the deployment's real domain.

| Key | Value | Source service |
|-----|-------|---------------|
| `hubURL` | `wss://hub.<your-domain>/ws` | Hub WebSocket (thingclient) |
| `hubHTTPURL` | `https://hub.<your-domain>` | Hub REST (audit upload, enrollment, /api/internal/things/*) |
| `cpURL` | `https://cp.<your-domain>` | Control Plane (SSO sign-in OAuth callback, agent-bootstrap, IdP) |

The AI Gateway (`api.<your-domain>`) is NOT in agent config — the agent's hooks (content-safety / pii-detector / rulepack-engine / …) all run locally in-process via `packages/shared/policy/hooks`; there is no agent → ai-gateway HTTP callout. The AI Gateway is a destination for END-USER LLM traffic that the agent's transparent proxy intercepts like any other host.

## Post-Install: Enroll Device

After installing the .pkg, enroll the device to connect it to the Hub:

```bash
sudo /Applications/NexusAgent.app/Contents/MacOS/nexus-agent enroll \
  --hub-url https://hub.<your-domain> \
  --token <enrollment-token>

# OR — SSO self-enrollment (preferred, picks up hubHTTPURL from agent.yaml):
sudo /Applications/NexusAgent.app/Contents/MacOS/nexus-agent enroll-sso
```

Get `<enrollment-token>` from the Control Plane admin UI at https://cp.<your-domain> (Nodes → Add Device).

---

## First Launch: Approve System Extension

On first launch after install, macOS will show a notification:
> "System Extension Blocked"

User must go to **System Settings → Privacy & Security** → allow `com.nexus-gateway.agent.extension` from YOUR ORG NAME.

On MDM-managed devices: deploy `installer/nexus-agent.mobileconfig.template` (fill `{{TEAM_ID}}`, `{{ORGANIZATION}}`, `{{PROFILE_UUID}}`) to pre-approve without user prompt.

---

## Uninstall

```bash
sudo bash packages/agent/platform/darwin/Scripts/uninstall.sh
```

---

## Key File Locations

| File | Purpose |
|------|---------|
| `darwin/Package.swift` | SPM root — builds NexusAgentUI + NexusAgentExtension |
| `darwin/NexusAgent/NexusAgent.entitlements` | Host app: app-proxy-provider-systemextension + system-extension.install + application-identifier + team-identifier |
| `darwin/NexusAgent/NexusAgentDaemon.entitlements` | Go binary: network only (no NE) |
| `darwin/NexusAgent/NexusAgentExtension/NexusAgentExtension.entitlements` | Extension: `com.apple.developer.networking.networkextension` = app-proxy-provider-systemextension (+ network.client) |
| `darwin/NexusAgent/NexusAgentExtension/Info.plist` | Extension bundle: EXExtensionPointIdentifier = com.apple.networkextension.app-proxy |
| `darwin/Scripts/build-prod.sh` | Full prod pipeline |
| `darwin/Scripts/build.sh` | .app bundle builder |
| `packages/agent/agent.prod.yaml.example` | Hub URL, log paths, platform bridge address |

---

## System Extension Info.plist Requirements (Critical)

The NE system extension's `Info.plist` must use this exact format — Xcode-generated apps work because Xcode injects these correctly, but a hand-built bundle is easy to get wrong:

```xml
<key>CFBundlePackageType</key>
<string>SYSX</string>          <!-- Must be SYSX, NOT XPC! -->

<key>NSSystemExtensionUsageDescription</key>
<string>Reason shown to user during approval prompt.</string>

<key>NetworkExtension</key>
<dict>
    <key>NEProviderClasses</key>
    <dict>
        <key>com.apple.networkextension.app-proxy</key>
        <string>NexusAgentExtension.NexusProxyProvider</string>  <!-- ModuleName.ClassName -->
    </dict>
</dict>
```

**Symptoms of wrong format:**
- `OSSystemExtensionManager.submitRequest()` fails synchronously with:
  `"Invalid extension configuration in Info.plist and/or entitlements: System extension X does not appear to belong to any extension categories"`
- Extension does NOT appear in `systemextensionsctl list` (not even as "waiting for user")
- No notification, nothing in **System Settings → Login Items & Extensions**

**Required:**
1. `CFBundlePackageType` = `SYSX` (Xcode's `XPC!` is for XPC services, not system extensions on modern macOS)
2. The principal class entry under `NetworkExtension/NEProviderClasses` keyed by the extension point identifier — NOT inside `NSExtension` or `EXAppExtensionAttributes` (those are for app extensions)
3. Principal class string is `ModuleName.ClassName` — for SPM, `ModuleName` = the executable target name (`NexusAgentExtension` here)

**Sanity check after install:**
```bash
systemextensionsctl list   # should show your extension after first launch
```

## macOS 26 (Tahoe) System Extension Approval Path

The location for approving system extensions changed in macOS 26:
- macOS 13–15: **System Settings → Privacy & Security** (scroll to bottom)
- macOS 26+: **System Settings → General → Login Items & Extensions → Network Extensions**

`systemextensionsctl list` always prints the current location at the top of its output.

## macOS 26 (Tahoe) Launch Constraint Requirements

macOS 26 enforces stricter launch constraints for apps with embedded provisioning profiles. Error 163 (`Launchd job spawn failed`) means there is an entitlement mismatch.

**Rules:**
1. Every entitlement in the code signature must be authorized by the embedded provisioning profile.
2. `com.apple.application-identifier` (`TEAMID.BUNDLE_ID`) **must** be present in the code signature — it is not auto-injected when using `codesign --entitlements` directly (unlike Xcode builds).
3. `com.apple.developer.team-identifier` **must** also be present.
4. The `com.apple.developer.networking.networkextension` entitlement value must use the `-systemextension` suffix form (`app-proxy-provider-systemextension`) in BOTH the host app and the system extension — the non-suffixed `app-proxy-provider` is the legacy app-extension (appex) form and is not used by a system extension. (The unsuffixed `com.apple.networkextension.app-proxy` in the extension's `Info.plist` is the `NEProviderClasses` extension-point key — a different field, not an entitlement value.)
5. The host app must **not** carry entitlements that are absent from its provisioning profile. If `system-extension.install` is not in the profile, remove it from the entitlements file (and disable `activateNetworkExtension()` until the profile is regenerated).

**Verify before shipping:**
```bash
# Check signed entitlements match profile
codesign -d --entitlements - /Applications/NexusAgent.app 2>&1

# Decode embedded profile entitlements
security cms -D -i /Applications/NexusAgent.app/Contents/embedded.provisionprofile | grep -A 30 "Entitlements"

# Quick launch test (error 163 = entitlement mismatch; app not shown = success)
open /Applications/NexusAgent.app && sleep 3 && pgrep -l NexusAgent
```
