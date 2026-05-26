# E74-S6 — build-agent pf-only Target + Entitlement Selection

> Story: e74-s6
> Epic: 74
> Status: Planning
> Date: 2026-05-21
> Requirements: `docs/developers/specs/e74-macos-pf-intercept.md` §FR-6 (FR-6.1 – FR-6.6)
> Source decisions: DEC-001 (NE Swift code build-target-gated), DEC-007 (interceptMode default build-stamped via ldflag), DEC-012 (shared domain.Engine must not be broken by build changes)
> Architecture: `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md` §"Build targets" (to be created in the same PR per FR-9 lockstep); `docs/developers/architecture/services/agent/macos-build-signing-architecture.md` §4 (launch constraints, unchanged)
> Blocked by: S1 (pf rules — must compile cleanly under the pf-only target), S2 (loopback listener — same), S5 (interceptMode wiring that exposes `main.defaultInterceptMode` ldflag variable)
> Blocks: S7 (gap-class closure acceptance gate — needs a notarized pf-only .pkg to run against)

---

## 1. User Story

As a **release engineer or developer**, I want to invoke `build-agent --target=pf-only` and receive a notarized `NexusAgent-pf-only-<version>.pkg` that (a) excludes the `com.apple.developer.networking.networkextension` entitlement, (b) carries `defaultInterceptMode=pf` compiled in, and (c) does NOT trigger the "Allow System Software" gate on a clean macOS VM — while the legacy `build-agent --target=legacy` (default) continues to produce exactly today's behaviour — so that operators can choose pf-only distribution without any Apple re-submission and without breaking the existing NE-approved distribution path.

---

## 2. Tasks

### T6.1 — Extend the build-agent skill to accept `--target`

- T6.1.1 Edit `.claude/skills/build-agent/skill.md`: add a **Build path decision table** row for `--target=pf-only` alongside the existing dev / prod rows. Document: what goes in, what comes out, whether NE works, and the key invariant ("no NE entitlement in the extension bundle; no system-extension activation required at install time").
- T6.1.2 Add a **Skill CLI reference** section to `skill.md` (see §4 Interface Contract below). Document all three arguments (`--target`, `--output`, `--version`), their defaults, exit codes, and the report format the skill produces at the end of a successful run.
- T6.1.3 Add a **Target comparison table** that captures the observable differences between `legacy` and `pf-only` builds: entitlements, ldflag-stamped default, .pkg filename, system-extension bundle presence, "Allow System Software" prompt behaviour.
- T6.1.4 The skill's invocation block (the `bash packages/agent/platform/darwin/Scripts/build-prod.sh ...` block) gains a `TARGET` environment variable that the skill injects: `TARGET=pf-only` or `TARGET=legacy` (default). Open question (defer to Code phase): whether `TARGET` is consumed inside `build-prod.sh` itself or via a wrapper that calls a new `build-prod-pf.sh`.

### T6.2 — Entitlement file selection at build time

- T6.2.1 Create `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/NexusAgentExtension-pf.entitlements` — identical to the existing `NexusAgentExtension.entitlements` **minus** the `com.apple.developer.networking.networkextension` key-value block. The resulting file retains:
  - `com.apple.application-identifier` (unchanged)
  - `com.apple.developer.team-identifier` (`39U3X3FFVK`, unchanged)
  - `com.apple.security.app-sandbox` (`false`, unchanged)
  - `com.apple.security.network.client` (`true`, unchanged)
  - `com.apple.security.network.server` (`true`, unchanged)
- T6.2.2 The `legacy` target continues to use `NexusAgentExtension.entitlements` as-is. The NE entitlement (`com.apple.developer.networking.networkextension: [app-proxy-provider-systemextension]`) remains present and effective — per DEC-001 and FR-6.5 (existing approval kept inert but not removed from the legacy target).
- T6.2.3 The `build-prod.sh` script (or the wrapper introduced in T6.1.4) selects the entitlement file for the `codesign` step of the NE extension bundle based on the `TARGET` variable:
  - `TARGET=legacy` → `codesign --entitlements NexusAgentExtension.entitlements`
  - `TARGET=pf-only` → `codesign --entitlements NexusAgentExtension-pf.entitlements`
- T6.2.4 The host app entitlements (`NexusAgent.entitlements`) and daemon entitlements (`NexusAgentDaemon.entitlements`) are **unchanged for both targets** — the NE capability drop is scoped to the extension bundle only, where the entitlement is enforced by the kernel. Open question (defer to Code phase): whether removing the NE entitlement from the extension bundle alone is sufficient, or whether `NexusAgent.entitlements` must also drop `com.apple.developer.networking.networkextension` and `com.apple.developer.system-extension.install`; verify against macOS 14 / 15 / 26 codesign error output during implementation.
- T6.2.5 The `NexusAgentExtension-pf.entitlements` file is committed to the same path as the existing entitlement file so it is signed into source control, readable by the pre-commit doc-lockstep check, and visible to future maintainers.

### T6.3 — ldflag injection for `defaultInterceptMode`

- T6.3.1 In `packages/agent/cmd/agent/main.go` (or the file that owns the agent's `main` function), declare a package-level variable:
  ```go
  // defaultInterceptMode is stamped at build time by the build-agent skill.
  // Values: "ne" (legacy target) | "pf" (pf-only target).
  // Fallback if ldflag is absent: "ne" (safe default — existing behaviour).
  var defaultInterceptMode = "ne"
  ```
- T6.3.2 The `build-prod.sh` / wrapper passes the ldflag to `go build`:
  - `TARGET=legacy`: `-ldflags "-X 'main.defaultInterceptMode=ne' ..."` (existing flags unchanged, this one appended)
  - `TARGET=pf-only`: `-ldflags "-X 'main.defaultInterceptMode=pf' ..."`
- T6.3.3 The S5 story (interceptMode wiring) reads `defaultInterceptMode` when initialising the `interceptMode` Cat B shadow key's fallback value. S6 does not implement the reading logic — it only ensures the ldflag variable exists and is correctly stamped. The dependency is explicit: S5 must declare the variable path before T6.3.1 can reference it; if S5 is implemented after S6, T6.3.1 is the declaration point and S5 reads it.
- T6.3.4 Verify the ldflag injection is effective: `strings dist/macos/NexusAgent.app/Contents/MacOS/nexus-agent | grep interceptMode` should return `pf` in a pf-only build and `ne` in a legacy build. This check is included in the skill's post-build verification block.

### T6.4 — Ensure Swift NE source files remain buildable in the legacy target

- T6.4.1 The Swift package (`packages/agent/platform/darwin/Package.swift`) has no conditional compilation change — both `NexusAgentExtension` and `NexusAgentUI` targets continue to build in full for both `TARGET=legacy` and `TARGET=pf-only`. Per DEC-001: "NE Swift code stays in tree (build-target-gated, not compiled into pf-only `.pkg`)". The "build-target-gated" in DEC-001 means the NE bundle is **excluded from the pf-only `.pkg` payload** (the `pkgbuild` component list), not that the Swift source files are excluded from compilation.
- T6.4.2 Concretely: the Swift build step runs identically for both targets (same `swift build` invocation). The difference is in the `pkgbuild` payload: for `TARGET=pf-only`, the script excludes `Contents/Library/SystemExtensions/com.nexus-gateway.agent.extension.systemextension` from the component tree passed to `pkgbuild`.
- T6.4.3 Verification: a `TARGET=pf-only` build MUST NOT fail at the Swift compilation step. The acceptance test (§3, A3) confirms this by completing both target builds from the same source tree.
- T6.4.4 Open question (defer to Code phase): whether the pf-only `.pkg` should include the NE bundle as a dormant/unsigned payload (for quick flip-back to NE without re-install) or omit it entirely (smaller pkg, cleaner distribution). The Requirements FR-5.5 + FR-5.6 suggest omitting when the default config is `pf`; implementation should verify that omitting the bundle does not break `launchctl` plist loading on machines that previously had the NE bundle.

### T6.5 — Notarization workflow unchanged

- T6.5.1 Both `TARGET=legacy` and `TARGET=pf-only` builds follow the identical notarization path: `xcrun notarytool submit --wait --keychain-profile nexus-notarytool`, then `xcrun stapler staple`. No new Apple-side keys, profiles, or credentials. Per FR-6.3 and DEC-001 (D2 binding: zero new Apple submissions).
- T6.5.2 The notarytool profile name (`nexus-notarytool`) is unchanged. The submitting Apple ID (`app@alphabitcore.com`) and team ID (`39U3X3FFVK`) are unchanged.
- T6.5.3 The pf-only `.pkg` passes Apple's notarization because it contains no new or un-entitlement-reviewed code: the Go daemon binary is unchanged (carries `NexusAgentDaemon.entitlements` as before), the Swift host app is unchanged, and the NE extension bundle is either omitted from the payload or present with reduced entitlements (both of which satisfy Apple's notarization checks — removing an entitlement never triggers rejection).
- T6.5.4 Confirm in the skill's post-build block: `xcrun stapler validate dist/macos/NexusAgent-pf-only-<version>.pkg` must exit 0.

### T6.6 — Build output naming

- T6.6.1 `TARGET=pf-only` produces: `dist/macos/NexusAgent-pf-only-<version>.pkg` (the signed + notarized installer) and `dist/macos/NexusAgent-pf-only.app` (the signed app, used for codesign inspection).
- T6.6.2 `TARGET=legacy` (default) produces the unchanged names: `dist/macos/NexusAgent-<version>.pkg` and `dist/macos/NexusAgent.app`. No rename to existing outputs — this preserves all prod-deploy scripts that reference the current filename.
- T6.6.3 `<version>` is injected via the `--version=<semver>` skill argument (see §4). Default: the output of `git describe --tags --always` at build time. The version string is also passed as an ldflag (`-X 'main.version=<version>'`) alongside `defaultInterceptMode`.
- T6.6.4 The skill's completion message prints a one-line summary: `Built: dist/macos/<pkg-filename> (target=<target>, version=<version>, notarized=yes)`.

### T6.7 — Cross-target entitlement diff test

- T6.7.1 Add a shell-level test script `packages/agent/platform/darwin/Scripts/test-build-targets.sh` that:
  1. Calls `build-prod.sh TARGET=legacy` (using locally available certs; skip notarization with `SKIP_NOTARIZE=1` for speed in dev environments).
  2. Calls `build-prod.sh TARGET=pf-only SKIP_NOTARIZE=1`.
  3. Expands both `.pkg` payloads via `pkgutil --expand` into temp directories.
  4. Runs `codesign -d --entitlements - <expanded>/Applications/NexusAgent.app/Contents/Library/SystemExtensions/com.nexus-gateway.agent.extension.systemextension` for the legacy payload.
  5. Asserts the legacy entitlement dump contains `com.apple.developer.networking.networkextension`.
  6. For the pf-only payload, asserts the NE extension bundle is absent (pkgutil expand contains no `.systemextension` path) OR — if T6.4.4's open question resolves to "include dormant" — asserts the entitlement dump does NOT contain `com.apple.developer.networking.networkextension`.
  7. Asserts `strings <legacy-daemon>  | grep -q 'interceptMode=ne'`.
  8. Asserts `strings <pf-only-daemon> | grep -q 'interceptMode=pf'`.
  9. Prints `PASS` or `FAIL <reason>` and exits with the appropriate code.
- T6.7.2 The test script is documented in the skill's `skill.md` under a "Verification" section so future maintainers know how to validate a new build change locally.
- T6.7.3 `SKIP_NOTARIZE=1` mode requires the script to set `CS_SKIP_NOTARIZE=1` in the environment. The `build-prod.sh` script must honour this escape hatch so CI + developer machines without notarytool credentials can still run the structural part of the test. Open question (defer to Code phase): whether `SKIP_NOTARIZE=1` is already supported by `build-prod.sh` or requires a new branch.

### T6.8 — macOS 26 launch-constraint XML verified unchanged

- T6.8.1 Confirm programmatically (inside `test-build-targets.sh`) that the daemon binary's launch-constraint XML is byte-identical between the two target builds. Method: extract via `codesign -d --xml - <daemon>` and `diff` both outputs.
- T6.8.2 The pf code added in S1 / S2 runs inside the daemon binary, which already carries the correct launch constraint (per `macos-build-signing-architecture.md` §4 and FR-6.4). No new entitlement is required because pf is accessed via `/sbin/pfctl` (a privileged helper, pre-authorised by the LaunchDaemon plist's root execution context) and via `/dev/pf` (ioctl, accessible by root — same execution context as the daemon).
- T6.8.3 Record the verification result in the acceptance run log. If the diff is non-empty, STOP and investigate before shipping — a changed launch constraint can cause macOS 26 error 163 (launchd job spawn failed).

### T6.9 — Document target differences in skill.md

- T6.9.1 `skill.md` gains a **Target reference** section (distinct from the existing "Build path decision table") documenting:
  - Which entitlements file is used per target (T6.2).
  - The ldflag-stamped variable name and its two values (T6.3).
  - The .pkg payload difference (NE bundle present vs absent) (T6.4).
  - The output filename convention (T6.6).
  - The "Allow System Software" behaviour per target (A4 / A5).
  - The notarization flow (identical for both).
  - The recovery procedure for each: legacy uses `systemextensionsctl uninstall` then reinstall; pf-only uses `pfctl -a nexus-agent/transparent -F all`.
- T6.9.2 The existing "Strict install / uninstall sequence" section in `skill.md` is amended to call out that the sequence differs for pf-only installs: the `open /Applications/NexusAgent.app` step that triggers the system-extension activation prompt is skipped (no system extension to activate). Install + daemon-start verification uses `launchctl list | grep com.nexus-gateway.agent` instead of `systemextensionsctl list`.
- T6.9.3 The existing "Post-install: USER must toggle Network Extension" section is scoped to `TARGET=legacy` only. A parallel section "Post-install: pf-only (no toggle required)" notes that no System Settings approval is needed and provides the pf-rule verification command: `sudo pfctl -a nexus-agent/transparent -s rules`.

---

## 3. Acceptance Criteria

- **A1**: `codesign -d --entitlements - <pf-only-pkg-payload>/NexusAgentExtension.systemextension` does NOT contain `com.apple.developer.networking.networkextension`. `codesign -d --entitlements - <legacy-pkg-payload>/NexusAgentExtension.systemextension` DOES contain it.
- **A2**: Both `NexusAgent-pf-only-<version>.pkg` and `NexusAgent-<version>.pkg` notarize successfully (`xcrun stapler validate` exits 0 for both).
- **A3**: Running `build-agent --target=pf-only` and `build-agent --target=legacy` on the same source tree, back to back, both complete without error. No Swift compilation failure in either run (verifies T6.4).
- **A4**: Installing `NexusAgent-pf-only-<version>.pkg` on a clean macOS 26 VM does NOT trigger the "System Software from HORIZON PURPOSE SDN BHD was blocked" notification and does NOT show an entry under System Settings → General → Login Items & Extensions → Network Extensions.
- **A5**: Installing `NexusAgent-<version>.pkg` (legacy target) on the same VM DOES trigger the system-extension approval prompt (proving the NE entitlement is still effective in the legacy build).
- **A6**: `strings <pf-only-daemon> | grep -c interceptMode=pf` returns 1. `strings <legacy-daemon> | grep -c interceptMode=ne` returns 1.
- **A7**: The launch-constraint XML diff between the two daemon binaries is empty (T6.8).
- **A8**: `test-build-targets.sh SKIP_NOTARIZE=1` exits 0 and prints `PASS`.
- **A9**: `skill.md` contains the Target reference section (T6.9) and the amended install / toggle sections are scoped per target.

---

## 4. Interface Contract

### build-agent skill CLI

The skill is invoked by Claude Code per CLAUDE.md's macOS Agent builds binding:

```
Skill('build-agent', '--target=<target> [--output=<dir>] [--version=<semver>]')
```

| Argument | Values | Default | Notes |
|---|---|---|---|
| `--target` | `legacy` \| `pf-only` | `legacy` | Selects entitlement file, ldflag, .pkg filename, and payload contents |
| `--output` | Any writable directory path | `dist/macos/` (relative to repo root) | Destination for .app and .pkg artefacts |
| `--version` | Semver string (e.g. `1.4.2`) | `git describe --tags --always` | Baked into .pkg filename and `main.version` ldflag |

**Exit codes:**

| Code | Meaning |
|---|---|
| 0 | Build, sign, notarize, and staple all succeeded; verification checks passed |
| 1 | Precondition failure (cert missing, wails not on PATH, notarytool profile absent) — nothing was built |
| 2 | Build step failed (Swift compile error, `go build` error, `pkgbuild` error) |
| 3 | Code-signing failure |
| 4 | Notarization failure (Apple-side rejection or network timeout) |
| 5 | Post-build verification failure (`stapler validate` failed or entitlement diff assertion failed) |

**Report format** (printed to stdout on exit 0):

```
=== build-agent report ===
Target:      pf-only
Version:     1.4.2
Output:      dist/macos/NexusAgent-pf-only-1.4.2.pkg
Notarized:   yes
Stapled:     yes
ldflag:      defaultInterceptMode=pf
NE bundle:   excluded
Entitlements: NexusAgentExtension-pf.entitlements
Verification: PASS (codesign + ldflag + launch-constraint checks)
==========================
```

---

## 5. Dependencies

- **S1 (pf rules)**: The pf rule generation package (`pfintercept/pfrules`) must compile cleanly under both targets. If S1 introduces a build tag (`//go:build darwin`) that isolates the pf code, the daemon `go build` invocation for both targets must include `-tags darwin` or equivalent — verify there is no compile gap.
- **S2 (loopback listener)**: The `pfintercept/listener` package must compile cleanly. Same build-tag caveat as S1.
- **S5 (interceptMode wiring)**: S5 sets up the `main.defaultInterceptMode` variable path that T6.3.1 stamps. If S5 is implemented after S6, T6.3.1 is the declaration point; S5 then assigns `defaultInterceptMode` to the Cat B shadow config default during wiring. The two stories are parallel-safe as long as the variable name (`main.defaultInterceptMode`) is agreed before coding begins — record it in DECISIONS.md (Code-phase entry) if changed from this SDD's default.
- **DEC-012 (shared domain.Engine)**: The build change must not break the shared `domain.Engine` import graph. Verification command (from DEC-012): `grep -RIlE 'type\s+Engine\s+struct' packages/ --include='*.go' | grep -v '_test.go'` must still return exactly one file after this story's changes land. Additionally, `grep -RInE 'func.*Evaluate.*pfintercept' packages/agent/ --include='*.go'` must return zero matches — no evaluation logic is allowed inside the `pfintercept` namespace; it calls the shared engine, it does not re-implement it.
- **S7 (gap-class closure acceptance gate)**: S7 runs the FR-7.1 – FR-7.5 closure tests against the notarized pf-only `.pkg`. S6 is therefore on S7's critical path; a dev-only `SKIP_NOTARIZE=1` build can unblock S7 functional testing, but the final acceptance gate requires a fully notarized artefact.

---

## 6. Out of Scope

- **Revoking the Apple NE entitlement on the developer account** — per DEC-001 (D2 binding): the Apple-side approval for `com.apple.developer.networking.networkextension` is deliberately kept inert as insurance for a future flip-back. No Apple Developer Portal action is required or permitted as part of this story.
- **Provisioning profile changes** — the existing `Nexus_Agent_Extension_Developer_ID.provisionprofile` is used for both targets; no new profile is needed. Removing an entitlement from the code signature does not require a matching profile change (profiles grant permission; not using a granted permission is always valid).
- **Windows / Linux build target changes** — E74 is macOS-only. The build skill changes in this story affect only the `build-prod.sh` / wrapper script and the `.entitlements` files under `packages/agent/platform/darwin/`.
- **NE Swift code deletion** — DEC-001 defers NE Swift cleanup to Phase 8 after pf is stable in prod. This story does not delete any Swift source files.
- **CI/CD pipeline wiring for the pf-only target** — building the pf-only `.pkg` in CI requires a macOS runner with the signing certs provisioned. Adding a GitHub Actions workflow or equivalent pipeline job is a follow-up operational task, not part of this story.
- **MDM mobile config for the pf-only build** — the existing `installer/nexus-agent.mobileconfig.template` targets NE approval; a pf-only variant (which would push different plist keys) is out of scope.

---

## 8. Implementation Notes

### 8.1 Why entitlement removal does not require a new provisioning profile

Apple provisioning profiles are permission grants — they declare which entitlements are allowed. A code signature may use a subset of the profile's grants without error. Removing `com.apple.developer.networking.networkextension` from the `.entitlements` file means the signed binary simply does not claim that entitlement; the profile's inclusion of it is irrelevant. Codesign will not fail. The only error codesign raises is when an entitlement in the `.entitlements` file is NOT in the profile — the opposite direction.

Implication: the single existing `Nexus_Agent_Extension_Developer_ID.provisionprofile` covers both targets without modification. No Apple round-trip.

### 8.2 macOS 26 launch-constraint interaction

`macos-build-signing-architecture.md` §4 describes why macOS 26 enforces that every signed entitlement must be authorised by the embedded provisioning profile. The pf-only build is safe specifically because it uses fewer entitlements than the profile grants — macOS 26's error 163 (launchd job spawn failed) fires on over-claiming, not under-claiming. The verification in T6.8 confirms this.

If the implementation at T6.2.4 (open question on host-app entitlements) determines that `NexusAgent.entitlements` must also drop `com.apple.developer.networking.networkextension` and `com.apple.developer.system-extension.install`, the same logic applies: removing claims from the app's signature while the profile still grants them is safe. What is never safe is adding a claim the profile does not include.

### 8.3 The `SKIP_NOTARIZE=1` escape hatch

Full notarization takes 2-5 minutes (Apple's servers + poll loop). Developers running `test-build-targets.sh` locally during iterative work should set `SKIP_NOTARIZE=1` to skip the `xcrun notarytool submit` and `xcrun stapler staple` steps. The codesign + pkgbuild + productsign steps still run, producing a structurally valid but unstapled `.pkg`.

The entitlement-diff assertions (T6.7) and ldflag assertions (T6.3.4) are fully meaningful on unstapled packages — stapling only affects the Gatekeeper revocation check at install time. `SKIP_NOTARIZE=1` must never be used for production or beta distribution.

### 8.4 `build-prod.sh` TARGET variable injection model

The current `build-prod.sh` does not have a target concept. The implementation has two viable approaches:

- **Option A — Single script with TARGET branch**: add a conditional near the top of `build-prod.sh` that sets `ENTITLEMENTS_EXT` and `LDFLAGS_EXTRA` variables, then uses them in the `codesign` and `go build` steps below.
- **Option B — Thin wrapper**: a new `build-prod-pf.sh` that sets `TARGET=pf-only`, overrides the entitlement and ldflag, then sources (or calls) `build-prod.sh`.

Option A is preferred (CLAUDE.md "delete instead of add" — one script, not two). Option B may be chosen if the maintainer judges that the conditionals make `build-prod.sh` harder to read than the two-file split. The decision is deferred to the Code phase per T6.1.4 and should be recorded in DECISIONS.md as a Code-phase entry.

### 8.5 `go build` darwin-only packages

S1 and S2 introduce packages with `//go:build darwin` tags. The daemon `go build` invocation in `build-prod.sh` currently runs on a macOS host where `GOOS=darwin` is implicit. This is correct for both targets. The concern (T6.4, T6.1.4) is whether a future CI run on a Linux host might accidentally compile the darwin agent binary for darwin via cross-compilation — if so, the build must explicitly set `GOOS=darwin CGO_ENABLED=1 CC=<darwin-cross-toolchain>`. This is an existing concern, not introduced by S6; S6 merely documents it so the Code-phase implementer does not regress it.

### 8.6 Relationship to the prod-deploy skill

The `prod-deploy` skill currently uploads and installs `NexusAgent-<VERSION>.pkg` (legacy naming). After S6 ships, a new operator decision is needed each release: ship the legacy `.pkg` or the pf-only `.pkg`. The `prod-deploy` skill should be updated (in a follow-up PR, not in this story) to accept a `--agent-target=<legacy|pf-only>` argument that selects the artefact name. The follow-up is recorded as an explicit follow-up todo — this story does not touch `prod-deploy`.

---

## 7. References

- `docs/developers/specs/e74-macos-pf-intercept.md` §FR-6 (FR-6.1 – FR-6.6) — all build + signing requirements this story implements.
- `docs/developers/specs/e74/DECISIONS.md` DEC-001 — NE Swift code stays in tree, build-target-gated; hybrid mode retired; escape-hatch via `interceptMode="ne"`.
- `docs/developers/specs/e74/DECISIONS.md` DEC-007 — build-stamped `defaultInterceptMode` ldflag; pf-only pkg stamps `pf`; legacy stamps `ne`.
- `docs/developers/specs/e74/DECISIONS.md` DEC-012 — `shared/policy/domain.Engine` must stay shared; build changes must not break its import graph.
- `packages/agent/platform/darwin/NexusAgent/NexusAgentExtension/NexusAgentExtension.entitlements` — the legacy entitlement file; T6.2.1 derives the pf-only variant from this file.
- `.claude/skills/build-agent/skill.md` — the single source of truth for all macOS Agent build invocations; this story extends it per T6.1 and T6.9.
- `docs/developers/architecture/services/agent/macos-build-signing-architecture.md` §4 — launch-constraint XML requirements; T6.8 verifies they are unchanged across targets.
- `docs/developers/specs/e74/e74-s3-libproc-pid-attribution.md` — sibling story; shares the `pfintercept` package namespace and the same `go build` invocation target.
- `packages/agent/platform/darwin/Scripts/build-prod.sh` — the existing production build script; T6.1.4 and T6.2.3 both modify or wrap this file.
- `packages/agent/cmd/agent/main.go` — declares `var defaultInterceptMode` per T6.3.1; entry point for the ldflag injection.
- `packages/agent/platform/darwin/NexusAgent/NexusAgent.entitlements` — host-app entitlement file; T6.2.4 open question covers whether this also requires a pf-only variant.
- `docs/developers/specs/e74/DECISIONS.md` DEC-009 — daemon panic auto-removal via launchd `KeepAlive` + signal handler; relevant context for T6.9.2 (pf-only uninstall recovery procedure).
- `docs/developers/roadmap.md` §E74 — original scoping entry confirming FR-6 ("Build + signing impact") as a Must-priority epic deliverable for the pf-only distribution path.
