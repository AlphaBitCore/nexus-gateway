# NexusAgentUI (macOS)

SwiftUI menu bar app for the Nexus Agent, built as a SwiftPM executable target and packaged into `NexusAgent.app` by `../Scripts/build.sh`.

The UI polls the local Go agent's status socket every 2s (while visible) or 30s (while hidden) and displays a status banner, today's stats, recent events, policy rules, and audit queue info. Quick actions run update check, config sync, and shutdown against the same Go process.

## Source layout

```
Sources/
├── App/
│   ├── NexusAgentApp.swift       # Scene entry + AppDelegate (NSStatusItem)
│   └── AgentViewModel.swift      # ObservableObject driving the UI
├── IPC/
│   └── StatusClient.swift        # Unix-socket client for the Go agent
├── Models/
│   └── AgentStatus.swift         # Codable DTOs shared with Go
├── Resources/
│   └── Localizable.xcstrings     # Xcode String Catalog (en, zh-Hans, es)
└── Views/
    ├── StatusPanelView.swift     # Banner + stats + recent events
    ├── DetailSectionsView.swift  # Policy / queue / agent info sections
    ├── EventHistoryView.swift    # Full audit event log with search/filter
    └── FooterView.swift          # Open Dashboard + version label
```

## Build + run for local iteration

**You must use a universal build, not plain `swift build`.** The package ships `Localizable.xcstrings`, which SwiftPM's native build system does **not** know how to compile — it only copies the raw catalog into the resource bundle and `String(localized:, bundle: .module)` then returns raw keys at runtime. Only the Apple build system (triggered by passing two `--arch` flags) runs `xcstringstool` and produces proper `.lproj/Localizable.strings`.

```bash
cd packages/agent/platform/darwin/NexusAgentUI

# Build — universal arch triggers the Apple build system + xcstringstool
swift build -c release --arch arm64 --arch x86_64

# Run the bare binary (menu bar icon appears; Dock icon also appears because
# LSUIElement is only applied when launched from the .app bundle).
./.build/apple/Products/Release/NexusAgentUI
```

Do **not** use `swift run`, plain `swift build -c release`, or the
`.build/arm64-apple-macosx/release/NexusAgentUI` output — those skip
`xcstringstool` and you will see every UI string rendered as its raw key
(e.g. `status.error` instead of "Error").

For the full release path (universal binary + Go core bundled into `NexusAgent.app`), use `../Scripts/build.sh` from the repo root.

## Architectural notes

### Menu bar — NSStatusItem, not MenuBarExtra

`NexusAgentApp.swift` uses `NSApplicationDelegateAdaptor` + `NSStatusBar.system.statusItem(...)` + `NSPopover`, with a no-op `Settings { EmptyView() }` Scene that keeps the SwiftUI event loop alive.

We originally used SwiftUI's `MenuBarExtra` but it fails to create a visible status item when the executable is launched from an SPM-built bundle (no Xcode `.app` target wired up the way SwiftUI expects). The NSStatusItem fallback works reliably under both SPM iteration and the production `.app` bundle.

If you are tempted to revert to `MenuBarExtra`: don't. Read the comment at the top of `NexusAgentApp.swift`.

### Localization — always `bundle: .module`

Every localized string site in this target must explicitly pass `bundle: .module`:

```swift
Text("section.policyRules", bundle: .module, comment: "Policy Rules")
String(localized: "op.checkUpdate", bundle: .module)
```

`Text(_: LocalizedStringKey)` and `String(localized:)` default to `Bundle.main`, which is empty for this executable target — all compiled string catalogs live under `Bundle.module`. Omitting `bundle: .module` is silent: the UI compiles, runs, and then displays raw keys.

When adding a new UI string:

1. Add the entry to `Sources/Resources/Localizable.xcstrings` (Xcode can edit this, or hand-edit the JSON).
2. In Swift, pass `bundle: .module` at the call site. Grep the existing code if you forget the exact incantation.
3. Rebuild with `--arch arm64 --arch x86_64` (see above) and verify the key renders as a string, not as the key itself.

### Dock icon during dev iteration

The production `.app` bundle sets `LSUIElement=true` in `Info.plist` so only the menu bar icon shows. When you run the bare binary from `.build/apple/.../NexusAgentUI`, macOS does **not** read `NexusAgent/Info.plist`, so a Dock icon also appears. This is a dev-only quirk; the packaged `.app` is correct.

If this becomes annoying, a runtime `NSApp.setActivationPolicy(.accessory)` call inside `applicationDidFinishLaunching` would belt-and-suspender the behavior — but it is not required for the production path.

## Related docs

- `../Scripts/build.sh` — production build pipeline (Go core + Swift UI → `NexusAgent.app`)
- `../NexusAgent/Info.plist` — production bundle Info.plist (carries `LSUIElement`)
- `Package.swift` — SwiftPM target declaration with `.process("Resources")`
