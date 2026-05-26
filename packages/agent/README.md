# Agent

Cross-platform desktop agent. Intercepts local network traffic at the
host level (macOS Network Extension, Linux nftables/iptables redirect,
Windows WinDivert), runs the same compliance hook pipeline as the
Compliance Proxy on the device itself, forwards approved traffic
directly to the provider's origin, and uploads `audit_event` rows to
Nexus Hub. The agent is a *peer* of the AI Gateway and the Compliance
Proxy under the Trinity data-plane model — not a layer above either.

## Conceptual layout

Three pillars under `packages/agent/`:

| Pillar | Path | Contents |
|--------|------|----------|
| Core (cross-platform Go) | `internal/` | Hook engine, audit pipeline, config sync, enrollment, killswitch, relay, intercept dispatcher — the pure-Go business logic. |
| UI (desktop dashboard) | `ui/` | Wails app — React frontend (`ui/frontend/`) + Go bindings. The user-visible status / policy / events panel that lives in the dock or system tray. |
| Platform native | `platform/{darwin,windows,linux}/` | Per-OS native code: macOS Network Extension (Swift), Windows WinDivert glue, Linux netfilter. Only built when targeting that OS. |

The CLI binary entry points live at the package root (`cmd/agent/`,
`cmd/agent-tray/`) per Go convention.

## Build

```bash
make agent-build              # Go-only CLI: dist/bin/agent/agent (+ agent-tray on linux/windows)

# Platform-native installers (real users use these):
make agent-build-macos        # builds the Swift NE bundle
make agent-package-macos      # signs + notarises + produces dist/macos/*.pkg
make agent-build-windows      # builds the Windows variant
make agent-package-windows    # signs + produces dist/windows/*.msi
```

The macOS path is the most involved (Swift NE, code signing,
notarisation, system extension activation). Always use the
`build-agent` slash command / skill for macOS releases — the script
encodes signing identity, entitlements, and the install/uninstall
sequence that has bricked the NE in past sessions when improvised.

## Test

```bash
make agent-test               # go test -race -count=1 ./...
```

## Configuration

- `agent.dev.yaml` — local boot defaults.
- `agent.prod.yaml.example` — production template (signed PKG embeds a
  filled-in version per maintainer build).
- Hub URL, enrollment token, intercept allowlist all delivered via the
  Hub shadow once enrolled.

## Architecture references

- `docs/developers/architecture/services/agent/agent-internals-sibling-pairs-architecture.md` —
  the cross-platform internal/ layout.
- `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md` — Swift NE + tray.
- `docs/developers/architecture/services/agent/agent-windows-platform-architecture.md` — WinDivert glue.
- `docs/developers/architecture/services/agent/agent-linux-platform-architecture.md` — netfilter.
- `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md` — the safety-critical
  fail-open binding rules for the macOS NE.
- `docs/developers/architecture/services/agent/macos-build-signing-architecture.md` — `.pkg` build
  pipeline.
