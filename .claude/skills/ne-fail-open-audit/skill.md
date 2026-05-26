# ne-fail-open-audit

Run the 5-rule safety audit before merging any change to the macOS Network Extension `TransparentProxyProvider`.

⚠ **SAFETY-CRITICAL.** A misbehaving NE provider takes down the entire Mac's network — DNS, DHCP, mDNS, NTP, Apple Push, VPNs. Recovery requires manual `launchctl unload` + plist delete (incident 2026-05-15). This is one of the highest-blast-radius surfaces in the codebase.

Use this skill whenever a PR touches:

- `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/**`
- `packages/agent/cmd/agent/**` (daemon process — `configappliers.go` writes the QUIC-bundle enforcement file)
- `packages/agent/cmd/agent/platformshim/**` (QUIC-fallback platform shim per OS)

The full doc is `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md`.

---

## Audit checklist (all 5 rules, every PR)

### Rule 1 — `handleNewFlow` decides synchronously

```bash
grep -n "handleNewFlow" packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/TransparentProxyProvider.swift
```

Walk every path in `handleNewFlow`. Confirm:

- [ ] Returns `true` only for flows the provider can fully relay.
- [ ] UDP flows for unknown bundles return `false`.
- [ ] Protocol / bundle-ID checks happen BEFORE returning `true`.
- [ ] No `return true; // TODO: actually relay this` patterns.

### Rule 2 — async callbacks have fail-open timeouts

```bash
grep -nE "requestDecision|peekSNIThenRelay" packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/*.swift
```

For every async daemon callback:

- [ ] `requestDecision` falls through to `passthrough` after 2s.
- [ ] `peekSNIThenRelay` falls through to plain relay after 500ms.
- [ ] No callback can hang waiting on an absent daemon.

### Rule 3 — no hardcoded enforcement lists in NE Swift

```bash
# Look for hardcoded bundle lists / domain lists in the .swift files
grep -nE "Bundle\.|com\.apple|com\." packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/*.swift
```

For each enforcement list (QUIC fallback bundles, deny lists, etc.):

- [ ] The list is **NOT** hardcoded in `.swift`.
- [ ] The list comes from a Hub-pushed shadow blob via the daemon.
- [ ] The daemon writes it to a file (e.g., `/var/run/nexus-agent/quic-bundles.json`).
- [ ] NE reads file-only, with **empty-as-fail-safe** (empty → enforcement off).

### Rule 4 — banned `isLikelyXyz = true` patterns

```bash
grep -nE "isLikely.*=.*true|TODO|FIXME" packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/*.swift
```

For every conditional flag:

- [ ] The condition is real (not a `TODO`-flipped boolean).
- [ ] Or the flag is `false` until a real condition lands.

### Rule 5 — system services kill-list guard

```bash
grep -rnE "mdnsresponder|configd|dhcpcd|apsd|nsurlsessiond|kdc|ntpd" packages/agent/cmd/agent/ packages/agent/internal/policy/
```

For every kill-list / deny-list of processes:

- [ ] The list **never** includes any of: `mdnsresponder`, `configd`, `dhcpcd`, `apsd`, `nsurlsessiond`, `kdc`, `ntpd`.
- [ ] The validation is in the daemon, not the NE provider.

## Pre-merge test invariants

- [ ] Boot agent on a fresh macOS; Wi-Fi browsing works (DNS / DHCP / HTTPS).
- [ ] Disable Hub; network still works.
- [ ] Send malformed flows; nothing hangs.
- [ ] QUIC handshake — pass-through OR capture per the file-only list, never both.
- [ ] Run 24h on a dev machine without "did I lose internet?" question.

## Build & sign

Use **`.claude/skills/build-agent`** (binding). NEVER run `wails build` / `codesign` / `xcrun notarytool` manually.

## Output

Emit a one-paragraph audit summary for the PR:

```
NE fail-open audit (5 rules):
- Rule 1 (handleNewFlow sync decision): PASS
- Rule 2 (async callback fail-open timeouts): PASS
- Rule 3 (no hardcoded enforcement in Swift): PASS
- Rule 4 (no isLikelyXyz=true patterns): PASS
- Rule 5 (system services kill-list guard): PASS
- 24h smoke: PASS (or "scheduled; see runbook")
```

If any rule fails, **STOP and fix before merging**. There is no "small NE change". The 2026-05-15 incident was caused by exactly such a "small change".
