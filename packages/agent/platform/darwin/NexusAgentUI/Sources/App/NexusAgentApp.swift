import SwiftUI
import AppKit
import Combine
import os.log

/// E40 Phase 1 (post-review): Docker-style menu-bar app.
///
/// Menu construction is lazy. The status item carries a single
/// NSMenu instance whose delegate is this AppDelegate; AppKit calls
/// `menuNeedsUpdate(_:)` immediately before showing the menu, which
/// is where items get populated from the current ViewModel state.
/// That pattern fixes two issues the first pass introduced:
///
///  1. The menu no longer rebuilds while the user is hovering an
///     item. Previously a `viewModel.objectWillChange.sink`
///     reassigned `statusItem.menu = newMenu` on every @Published
///     write, which AppKit treats as "current menu was dismissed"
///     and silently closed the menu mid-interaction.
///  2. Icon updates are decoupled from menu updates. The icon
///     refreshes on every 2 s poll (so the tray reflects the
///     daemon's state within ~2 s even when the menu is closed),
///     while the menu only churns on actual opens.
///
/// SwiftUI's `MenuBarExtra` is intentionally NOT used here: it
/// produces no visible status item when launched from an SPM-built
/// bundle without an Xcode target. `NSApplicationDelegateAdaptor`
/// plus `NSStatusBar.system` works on every macOS version we
/// support and is the standard menu-bar app pattern Docker / 1Password
/// / Tailscale use.
@main
struct NexusAgentApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate

    var body: some Scene {
        Settings { EmptyView() }
    }
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate, NSMenuDelegate {
    private var statusItem: NSStatusItem?
    private let viewModel = AgentViewModel()
    private var cancellables = Set<AnyCancellable>()
    let neLogger = Logger(subsystem: "com.nexus-gateway.agent", category: "AppDelegate")
    private var hasActivatedNE = false

    nonisolated func applicationDidFinishLaunching(_ notification: Notification) {
        MainActor.assumeIsolated {
            // Headless uninstall mode: when launched as `--uninstall` (by the
            // root uninstall.sh via `open -W`), do the SIP-gated app-side
            // teardown the shell cannot — deactivate the NE system extension and
            // unregister the SMAppService daemon + login item — then quit. The
            // shell removes the file residue after we exit. No menu is built.
            if CommandLine.arguments.contains("--uninstall") {
                self.runUninstall()
            } else {
                self.setUp()
            }
        }
    }

    /// Set once the uninstall teardown completes. The `--uninstall` flow itself
    /// lives in NexusAgentApp+Uninstall.swift.
    var uninstallDone = false

    private func setUp() {
        // 0. Re-arm the daemon if a previous Quit left the user-quit
        //    flag behind. The Swift Quit handler wrote a marker file
        //    that the daemon's main() reads on every launchd respawn
        //    and uses to exit immediately — that's how Quit "really
        //    quits" without a sudo prompt. Removing the marker here
        //    means the next launchd respawn (within ThrottleInterval
        //    ~10s) brings the daemon back up. The menu status flips
        //    from yellow to green automatically once that happens.
        QuitFlag.clear()

        // 1. Status item with an empty menu that has this object as
        //    its delegate. menuNeedsUpdate(_:) populates items
        //    lazily, just before AppKit shows the menu.
        let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        let menu = NSMenu()
        menu.autoenablesItems = false
        menu.delegate = self
        item.menu = menu
        applyIcon(item: item)
        self.statusItem = item

        // 2. Icon refresh subscription. Only the four properties that
        //    affect tray-icon appearance — no flicker on benign
        //    @Published writes like transientMessage.
        Publishers.CombineLatest4(
            viewModel.$agentState.removeDuplicates(),
            viewModel.$paused.removeDuplicates(),
            viewModel.$pendingEnrollment.removeDuplicates(),
            viewModel.$updateAvailable.removeDuplicates()
        )
        .receive(on: RunLoop.main)
        .sink { [weak self] _, _, _, _ in
            guard let self, let item = self.statusItem else { return }
            self.applyIcon(item: item)
        }
        .store(in: &cancellables)

        // 3. Network extension activation + launchd-service registration —
        //    fires once when pendingEnrollment is false (the default, so on a
        //    normal launch this is the first runloop tick; on a not-yet-enrolled
        //    device the first poll flips pendingEnrollment true and the guard
        //    holds it off until enrollment clears). hasActivatedNE makes it
        //    one-shot per process. Both the NE approval and the SMAppService
        //    daemon + Login Item approval thus surface together at one moment.
        //    SMAppService registration is idempotent, so re-running it on a
        //    later launch is safe and also refreshes the bundle path after an
        //    update.
        viewModel.$pendingEnrollment
            .receive(on: RunLoop.main)
            .sink { [weak self] pending in
                guard let self, !pending, !self.hasActivatedNE else { return }
                self.hasActivatedNE = true
                LaunchServiceManager.register()
                self.activateNetworkExtension()
            }
            .store(in: &cancellables)
    }

    // MARK: - NSMenuDelegate (lazy menu construction)

    nonisolated func menuNeedsUpdate(_ menu: NSMenu) {
        MainActor.assumeIsolated {
            menu.removeAllItems()
            if self.viewModel.pendingEnrollment {
                self.buildPendingEnrollmentMenu(menu)
            } else {
                self.buildSteadyStateMenu(menu)
            }
        }
    }

    // MARK: - Menu construction

    private func buildSteadyStateMenu(_ menu: NSMenu) {
        // 1. Status indicator row (disabled / non-clickable). When
        //    paused with a finite duration the row also shows the
        //    auto-resume time so the user has a clear answer to
        //    "when will protection come back?".
        //    When state=degraded the daemon's stateReason is appended
        //    so the user sees an actionable hint (e.g. "Network
        //    filter not connected") instead of a vague "Degraded".
        let statusTitle: String
        let statusColor: NSColor
        if viewModel.paused {
            let base = String(localized: "menu.status.paused", bundle: .module)
            if let until = viewModel.pausedUntilDisplay {
                let fmt = String(localized: "menu.status.resumesAt", bundle: .module)
                    .replacingOccurrences(of: "{{time}}", with: until)
                statusTitle = "\(base) — \(fmt)"
            } else {
                statusTitle = base
            }
            statusColor = .systemYellow
        } else {
            switch viewModel.agentState {
            case .active:
                statusTitle = String(localized: "menu.status.active", bundle: .module)
                statusColor = .systemGreen
            case .degraded:
                let base = String(localized: "menu.status.degraded", bundle: .module)
                if !viewModel.stateReason.isEmpty {
                    statusTitle = "\(base) — \(viewModel.stateReason)"
                } else {
                    statusTitle = base
                }
                statusColor = .systemYellow
            case .error:
                let base = String(localized: "menu.status.error", bundle: .module)
                if !viewModel.stateReason.isEmpty {
                    statusTitle = "\(base) — \(viewModel.stateReason)"
                } else {
                    statusTitle = base
                }
                statusColor = .systemRed
            }
        }
        menu.addItem(statusRow(title: statusTitle, dotColor: statusColor))

        // Version row was previously here but was removed as part
        // of the menu IA cleanup. The daemon version is operator
        // trivia for 99% of end users; it now lives on the Settings
        // → About panel (Dashboard) where "did the update land"
        // belongs alongside the auto-updater channel. See
        // [[agent-ui-ia-redesign]] memory note.

        // Update-available banner row. Surfaces only when the daemon
        // has detected a newer build on Hub (Updater's availability
        // callback set status.updateAvailable=true). Click opens the
        // Dashboard so the user can review release notes / trigger
        // the .pkg install path. The row uses the same statusRow()
        // helper for a yellow dot + bold label so it visually
        // resembles the other top-of-menu state indicators.
        if viewModel.updateAvailable {
            let updateItem = makeItem(
                title: String(localized: "menu.updateAvailable", bundle: .module),
                systemImage: "arrow.down.circle.fill",
                action: #selector(handleOpenDashboard)
            )
            // Highlight via attributed title so the affordance reads
            // as "do something" rather than as another disabled
            // status row.
            let attr = NSAttributedString(
                string: String(localized: "menu.updateAvailable", bundle: .module),
                attributes: [
                    .foregroundColor: NSColor.systemYellow,
                    .font: NSFont.menuFont(ofSize: 0),
                ])
            updateItem.attributedTitle = attr
            menu.addItem(updateItem)
        }

        // Daemon-approval affordance. SMAppService registers the root daemon
        // from the app bundle, but on an unmanaged device the user must approve
        // it once in System Settings → Login Items & Extensions — the same pane
        // that holds the Network Extension approval, so a single row covers
        // "finish setup". Surfaces only while approval is pending; on a managed
        // device the com.apple.servicemanagement profile pre-approves it, so
        // daemonNeedsApproval is false and this row never shows (the approval UI
        // is unmanaged-only with no MDM-detection code).
        if LaunchServiceManager.daemonNeedsApproval {
            let approveItem = makeItem(
                title: String(localized: "menu.finishSetup", bundle: .module),
                systemImage: "exclamationmark.shield.fill",
                action: #selector(handleOpenLoginItems)
            )
            let attr = NSAttributedString(
                string: String(localized: "menu.finishSetup", bundle: .module),
                attributes: [
                    .foregroundColor: NSColor.systemOrange,
                    .font: NSFont.menuFont(ofSize: 0),
                ])
            approveItem.attributedTitle = attr
            menu.addItem(approveItem)
        }

        menu.addItem(.separator())

        // 2. Open Dashboard
        menu.addItem(makeItem(
            title: String(localized: "menu.openDashboard", bundle: .module),
            systemImage: "rectangle.on.rectangle",
            action: #selector(handleOpenDashboard)
        ))

        // 3. SSO identity row — now an expandable submenu when an SSO
        //    email is present. The header NSMenuItem itself isn't
        //    actionable (clicking the email does nothing) but its
        //    submenu offers Switch identity / Sign Out. Two affordances
        //    so users on shared / loaner machines can hand the device
        //    off without going hunting in the Dashboard. See
        //    [[agent-ui-ia-redesign]].
        if !viewModel.ssoEmail.isEmpty {
            let identity = NSMenuItem(title: viewModel.ssoEmail, action: nil, keyEquivalent: "")
            let cfg = NSImage.SymbolConfiguration(pointSize: 13, weight: .regular)
            identity.image = NSImage(systemSymbolName: "person.crop.circle.fill", accessibilityDescription: nil)?
                .withSymbolConfiguration(cfg)
            // Switch identity / Sign Out drop the device's enrollment — i.e. they
            // turn protection off — so the submenu is offered only when
            // quitAllowed. On a locked fleet the email row is informational only
            // (the daemon also refuses UNENROLL/sign-out over IPC when locked).
            if viewModel.quitAllowed {
                identity.submenu = buildIdentitySubmenu()
            }
            menu.addItem(identity)
        }

        // 4. Pause / Resume protection. When paused: a single
        //    "Resume Protection" row (with auto-resume time suffix
        //    when known). When active: a submenu offering
        //    15 min / 1 h / 8 h / Indefinite.
        if viewModel.paused {
            // Resume is ALWAYS offered — turning protection back ON is never
            // blocked by the quit/always-on policy, even on a locked fleet.
            let title: String
            if let until = viewModel.pausedUntilDisplay {
                title = String(localized: "menu.resumeProtection", bundle: .module) + " (\(until))"
            } else {
                title = String(localized: "menu.resumeProtection", bundle: .module)
            }
            menu.addItem(makeItem(
                title: title,
                systemImage: "play.fill",
                action: #selector(handleResume)
            ))
        } else if viewModel.quitAllowed {
            // Pause turns protection OFF, so it is hidden on a locked
            // (quitAllowed=false) always-on fleet — same policy + same
            // menu-honesty rule as Quit. The daemon also refuses PAUSE_PROTECTION
            // over IPC when locked; hiding the item avoids a show-then-error.
            let pauseItem = makeItem(
                title: String(localized: "menu.pauseProtection", bundle: .module),
                systemImage: "pause.fill",
                action: nil
            )
            pauseItem.submenu = buildPauseSubmenu()
            menu.addItem(pauseItem)
        }

        menu.addItem(.separator())

        // Settings (the About panel was previously a separate menu
        // item but now lives inside the Settings page on Dashboard —
        // same place users go to change theme / language, so About
        // is one click away without owning a top-level menu slot).
        menu.addItem(makeItem(
            title: String(localized: "menu.settings", bundle: .module),
            systemImage: "gearshape",
            action: #selector(handleSettings),
            keyEquivalent: ",",
            keyModifier: .command
        ))

        // Diagnostics opens the Dashboard's Diagnostics page.
        menu.addItem(makeItem(
            title: String(localized: "menu.diagnostics", bundle: .module),
            systemImage: "stethoscope",
            action: #selector(handleOpenDiagnostics),
            keyEquivalent: "d",
            keyModifier: [.command, .shift]
        ))

        // Reinstall Network Extension — the recovery action for the
        // documented "saveToPreferences ok / startVPNTunnel returns
        // without throw / provider never spawns" stuck state. Calls
        // forceReinstall which removes every NETransparentProxyManager
        // bound to our bundle id then re-saves a fresh one, kicking
        // macOS to retry the provider launch with clean state. This
        // is the only GUI escape from the stuck state when auto-recovery
        // in ensureRunning doesn't fire (auto-recovery only triggers on
        // NEVPNError code 1/2; macOS returning success-then-no-spawn
        // never throws so never auto-recovers).
        menu.addItem(makeItem(
            title: String(localized: "menu.reinstallNetworkExtension", bundle: .module),
            systemImage: "arrow.triangle.2.circlepath",
            action: #selector(handleReinstallNE)
        ))

        menu.addItem(.separator())

        // Quit Nexus Agent is gated on the daemon-reported
        // `quitAllowed` policy: compliance always-on deployments
        // deny the SHUTDOWN IPC, so hiding the button keeps the
        // menu honest.
        if viewModel.quitAllowed {
            menu.addItem(makeItem(
                title: String(localized: "menu.quitAgent", bundle: .module),
                systemImage: "power",
                action: #selector(handleQuit),
                keyEquivalent: "q",
                keyModifier: .command
            ))
        }

        // 7. Transient message footer (cleared automatically after a
        //    few seconds by the view model).
        if let msg = viewModel.transientMessage, !msg.isEmpty {
            menu.addItem(.separator())
            let info = NSMenuItem(title: msg, action: nil, keyEquivalent: "")
            info.isEnabled = false
            menu.addItem(info)
        }
    }

    /// buildIdentitySubmenu returns the SSO email row's submenu with
    /// the two identity-change affordances. Switch identity is an
    /// alias for "Sign Out then Sign In as someone else": it calls
    /// UNENROLL to clear local credentials AND opens the Dashboard so
    /// the user lands directly on the onboarding sign-in screen.
    /// Sign Out is the same UNENROLL call without the auto-open — the
    /// user is done with this device and walks away.
    private func buildIdentitySubmenu() -> NSMenu {
        let sub = NSMenu()
        sub.autoenablesItems = false
        let switchItem = NSMenuItem(
            title: String(localized: "menu.switchIdentity", bundle: .module),
            action: #selector(handleSwitchIdentity),
            keyEquivalent: "")
        switchItem.target = self
        let signOutItem = NSMenuItem(
            title: String(localized: "menu.signOut", bundle: .module),
            action: #selector(handleSignOut),
            keyEquivalent: "")
        signOutItem.target = self
        sub.addItem(switchItem)
        sub.addItem(.separator())
        sub.addItem(signOutItem)
        return sub
    }

    private func buildPauseSubmenu() -> NSMenu {
        let sub = NSMenu()
        sub.autoenablesItems = false
        let durations: [(titleKey: String, seconds: Int)] = [
            ("menu.pauseDuration.15min", 15 * 60),
            ("menu.pauseDuration.1hour", 60 * 60),
            ("menu.pauseDuration.8hours", 8 * 60 * 60),
            ("menu.pauseDuration.indefinite", 0),
        ]
        for (key, seconds) in durations {
            let item = NSMenuItem(
                title: String(localized: String.LocalizationValue(key), bundle: .module),
                action: #selector(handlePauseDuration(_:)),
                keyEquivalent: "")
            item.target = self
            item.representedObject = seconds
            sub.addItem(item)
        }
        return sub
    }

    private func buildPendingEnrollmentMenu(_ menu: NSMenu) {
        menu.addItem(statusRow(
            title: String(localized: "menu.status.pendingEnrollment", bundle: .module),
            dotColor: .systemRed
        ))
        menu.addItem(.separator())
        menu.addItem(makeItem(
            title: String(localized: "menu.openSetup", bundle: .module),
            systemImage: "person.badge.key.fill",
            action: #selector(handleOpenSetup)
        ))
        // Quit is gated on quitAllowed here too, for the same
        // menu-honesty reason as the enrolled menu: the prod build bakes
        // quitAllowed=false, so the daemon reports it even before
        // enrollment completes. Hiding the item keeps a locked device
        // from offering a Quit that quitAgent() would only refuse.
        if viewModel.quitAllowed {
            menu.addItem(.separator())
            menu.addItem(makeItem(
                title: String(localized: "menu.quitAgent", bundle: .module),
                systemImage: "power",
                action: #selector(handleQuit),
                keyEquivalent: "q",
                keyModifier: .command
            ))
        }
    }

    private func statusRow(title: String, dotColor: NSColor) -> NSMenuItem {
        let item = NSMenuItem(title: title, action: nil, keyEquivalent: "")
        item.isEnabled = false
        let cfg = NSImage.SymbolConfiguration(pointSize: 10, weight: .bold)
            .applying(NSImage.SymbolConfiguration(paletteColors: [dotColor]))
        item.image = NSImage(systemSymbolName: "circle.fill", accessibilityDescription: nil)?
            .withSymbolConfiguration(cfg)
        return item
    }

    private func makeItem(
        title: String,
        systemImage: String? = nil,
        action: Selector?,
        keyEquivalent: String = "",
        keyModifier: NSEvent.ModifierFlags? = nil
    ) -> NSMenuItem {
        let item = NSMenuItem(title: title, action: action, keyEquivalent: keyEquivalent)
        if action != nil { item.target = self }
        if let modifier = keyModifier {
            item.keyEquivalentModifierMask = modifier
        }
        if let name = systemImage {
            let cfg = NSImage.SymbolConfiguration(pointSize: 13, weight: .regular)
            item.image = NSImage(systemSymbolName: name, accessibilityDescription: nil)?
                .withSymbolConfiguration(cfg)
        }
        return item
    }

    // MARK: - Menu actions

    @objc private func handleOpenDashboard() { viewModel.openDashboard() }
    @objc private func handleOpenSetup() { viewModel.openSetup() }
    @objc private func handleSettings() { viewModel.openSettings() }
    @objc private func handleAbout() { viewModel.openAbout() }
    @objc private func handleResume() { viewModel.resumeProtection() }
    @objc private func handleQuit() { viewModel.quitAgent() }
    @objc private func handleReinstallNE() { self.reinstallNetworkExtension() }
    @objc private func handleOpenLoginItems() { LaunchServiceManager.openLoginItemsSettings() }
    // For now Diagnostics is just an Open-Dashboard alias; once the
    // Dashboard supports a route deep-link (planned in #68 — same
    // task that fleshes out the Diagnostics page with NE reinstall,
    // restart-daemon, copy-support-bundle, view-log actions) this
    // will pass /diagnostics. The Swift-side selector is wired now
    // so the menu IA cleanup ships first and the page polish ships
    // separately without churning Localizable.xcstrings again.
    @objc private func handleOpenDiagnostics() { viewModel.openDashboard() }

    /// Switch identity = "I want to sign in as someone else": clear
    /// local credentials via UNENROLL (same path as Sign Out) AND
    /// pop the Dashboard so the user lands on the onboarding screen
    /// without having to re-launch the app manually. Confirmation
    /// dialog suppressed — the user clicked from a clearly-labelled
    /// menu so they know what they're doing.
    @objc private func handleSwitchIdentity() {
        viewModel.signOut()
        viewModel.openDashboard()
    }

    /// Sign Out = "I'm done with this device" — clear credentials,
    /// don't pop the Dashboard. The daemon respawns into
    /// pending-enrollment mode; if the user opens the Dashboard
    /// later they'll see the sign-in screen. Confirmation is
    /// handled by the underlying ViewModel method which surfaces
    /// failures via the transient footer.
    @objc private func handleSignOut() {
        viewModel.signOut()
    }
    @objc private func handlePauseDuration(_ sender: NSMenuItem) {
        let seconds = (sender.representedObject as? Int) ?? 0
        viewModel.pauseProtection(seconds: seconds)
    }

    // MARK: - Network extension

    private func activateNetworkExtension() {
        runNetworkExtensionInstall(userTriggered: false)
    }

    /// Re-runs the install/save flow on demand from the menu. Unlike
    /// the first-boot path, this BYPASSES the "already enabled"
    /// short-circuit — the whole point of the menu action is to fix
    /// a stuck state where macOS believes the proxy is enabled but
    /// the NE provider never actually attached. forceReinstall tears
    /// down every existing manager for our bundle id and recreates a
    /// fresh one, which kicks NEAgent into re-evaluating startTunnel
    /// from scratch.
    private func reinstallNetworkExtension() {
        viewModel.showTransientMessage(String(localized: "menu.reinstallNetworkExtension.started", bundle: .module))
        let appVersion = (Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String) ?? ""

        // System-extension activation first (idempotent if already
        // activated). Failures are non-fatal — the existing extension
        // keeps running and the forceReinstall below is what matters.
        SystemExtensionManager.shared.installIfNeeded { [weak self] result in
            guard let self else { return }
            // Hoisted so the forceReinstall closure below (a sibling of this
            // switch) can read it when choosing the "done" vs "reboot" toast.
            var pendingReboot = false
            switch result {
            case .success(let outcome):
                pendingReboot = outcome == .pendingReboot
                if pendingReboot {
                    self.neLogger.error("Reinstall: new system extension staged but NOT active until reboot — the running extension keeps serving")
                } else {
                    self.neLogger.info("Reinstall: system-extension installIfNeeded succeeded")
                }
                Task { @MainActor in
                    await self.viewModel.reportProxyInstall(
                        stage: "system-extension-install",
                        outcome: pendingReboot ? "pending-reboot" : "ok",
                        error: nil,
                        appVersion: appVersion
                    )
                }
            case .failure(let error):
                self.neLogger.error("Reinstall: installIfNeeded failed (continuing): \(error.localizedDescription)")
                Task { @MainActor in
                    await self.viewModel.reportProxyInstall(
                        stage: "system-extension-install",
                        outcome: "error",
                        error: error.localizedDescription,
                        appVersion: appVersion
                    )
                }
            }

            // Always run forceReinstall — destructive recreate of the
            // proxy manager regardless of system-extension outcome.
            TransparentProxyManager.shared.forceReinstall { [weak self] error in
                guard let self else { return }
                if let error {
                    self.neLogger.error("forceReinstall failed: \(error.localizedDescription)")
                    Task { @MainActor in
                        await self.viewModel.reportProxyInstall(
                            stage: "transparent-proxy-save",
                            outcome: "error",
                            error: error.localizedDescription,
                            appVersion: appVersion
                        )
                        self.viewModel.showTransientMessage("Reinstall failed: \(error.localizedDescription)")
                    }
                    return
                }
                self.neLogger.info("forceReinstall succeeded")
                Task { @MainActor in
                    await self.viewModel.reportProxyInstall(
                        stage: "transparent-proxy-save",
                        outcome: "ok",
                        error: nil,
                        appVersion: appVersion
                    )
                    // Tell the user a reboot is needed when macOS deferred the
                    // system-extension swap — don't claim "done" while the new
                    // extension is only staged.
                    self.viewModel.showTransientMessage(String(
                        localized: pendingReboot
                            ? "menu.reinstallNetworkExtension.reboot"
                            : "menu.reinstallNetworkExtension.done",
                        bundle: .module
                    ))
                }
            }
        }
    }

    /// Single source of truth for the NE install/save flow. Reports
    /// the outcome of each stage to the Go daemon over IPC so the
    /// result lands in agent.log instead of being silently swallowed
    /// by os.log. `userTriggered=true` is the menu-bar Reinstall path
    /// and also surfaces a transient toast on completion.
    ///
    /// The flow runs in two sequential stages:
    ///
    ///   1. OSSystemExtensionRequest activation (idempotent — if the
    ///      extension is already in `[activated enabled]` per
    ///      `systemextensionsctl list`, this is a no-op).
    ///   2. NETransparentProxyManager.saveToPreferences — installs
    ///      the proxy config into macOS preferences and presents the
    ///      "Allow proxy configurations" dialog when the config is
    ///      new. This is the step that wires the running extension
    ///      to the daemon's IPC socket.
    ///
    /// IMPORTANT: stage 2 ALWAYS runs, even if stage 1 returns an
    /// error. OSSystemExtensionRequest can reject for reasons that
    /// are perfectly benign at this point — the most common is
    /// "Missing entitlement com.apple.developer.system-extension.install"
    /// on dev / ad-hoc builds where the system extension was
    /// originally activated by the .pkg postinstall script invoking
    /// `systemextensionsctl install`. The extension stays activated
    /// across host upgrades, so retrying the activation from a
    /// less-privileged binary fails — but the existing activation
    /// is still fine, and `saveToPreferences` is what we actually
    /// need next to wire the proxy config. Bailing on stage 1
    /// failure was the latent first-boot bug that produced the
    /// silent "agent enrolled, NE never connects, zero traffic
    /// captured" state.
    private func runNetworkExtensionInstall(userTriggered: Bool) {
        let appVersion = (Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String) ?? ""
        SystemExtensionManager.shared.installIfNeeded { [weak self] result in
            guard let self else { return }
            // Hoisted so the forceReinstall closure below (a sibling of this
            // switch) can read it when choosing the "done" vs "reboot" toast.
            var pendingReboot = false
            switch result {
            case .success(let outcome):
                pendingReboot = outcome == .pendingReboot
                if pendingReboot {
                    self.neLogger.error("system extension staged but NOT active until reboot — proxy-save proceeds against the running extension")
                } else {
                    self.neLogger.info("system-extension installIfNeeded succeeded")
                }
                Task { @MainActor in
                    await self.viewModel.reportProxyInstall(
                        stage: "system-extension-install",
                        outcome: pendingReboot ? "pending-reboot" : "ok",
                        error: nil,
                        appVersion: appVersion
                    )
                }
            case .failure(let error):
                // Soft-fail: log + report, then CONTINUE to the
                // proxy-save step. The existing system extension
                // (if any) keeps working; saveToPreferences may
                // still wire it up successfully.
                let msg = error.localizedDescription
                self.neLogger.error("installIfNeeded failed (continuing to proxy-save): \(msg)")
                Task { @MainActor in
                    await self.viewModel.reportProxyInstall(
                        stage: "system-extension-install",
                        outcome: "error",
                        error: msg,
                        appVersion: appVersion
                    )
                }
            }

            // Stage 2 runs unconditionally.
            self.runTransparentProxySave(userTriggered: userTriggered, appVersion: appVersion)
        }
    }

    /// Stage 2 of the NE install flow. Separated from
    /// runNetworkExtensionInstall so the unconditional fall-through
    /// stays readable.
    private func runTransparentProxySave(userTriggered: Bool, appVersion: String) {
        TransparentProxyManager.shared.enableIfNeeded { [weak self] error in
            guard let self else { return }
            if let error {
                self.neLogger.error("enableIfNeeded failed: \(error.localizedDescription)")
                Task { @MainActor in
                    await self.viewModel.reportProxyInstall(
                        stage: "transparent-proxy-save",
                        outcome: "error",
                        error: error.localizedDescription,
                        appVersion: appVersion
                    )
                    if userTriggered {
                        self.viewModel.showTransientMessage("Reinstall failed: \(error.localizedDescription)")
                    }
                }
                return
            }
            self.neLogger.info("transparent-proxy save succeeded")
            Task { @MainActor in
                await self.viewModel.reportProxyInstall(
                    stage: "transparent-proxy-save",
                    outcome: "ok",
                    error: nil,
                    appVersion: appVersion
                )
                if userTriggered {
                    self.viewModel.showTransientMessage(String(
                        localized: "menu.reinstallNetworkExtension.done",
                        bundle: .module
                    ))
                }
            }
        }
    }

    // MARK: - Icon tinting

    private func applyIcon(item: NSStatusItem) {
        guard let button = item.button else { return }
        let symbolName: String
        let tint: NSColor
        if viewModel.pendingEnrollment {
            symbolName = "exclamationmark.shield.fill"
            tint = .systemRed
        } else if viewModel.paused {
            symbolName = "pause.circle.fill"
            tint = .systemYellow
        } else {
            switch viewModel.agentState {
            case .active:
                symbolName = "checkmark.shield.fill"
                tint = .systemGreen
            case .degraded:
                symbolName = "exclamationmark.shield.fill"
                tint = .systemYellow
            case .error:
                symbolName = "xmark.shield.fill"
                tint = .systemRed
            }
        }
        let baseConfig = NSImage.SymbolConfiguration(pointSize: 14, weight: .semibold)
        let paletteConfig = NSImage.SymbolConfiguration(paletteColors: [tint])
        let config = baseConfig.applying(paletteConfig)
        let image = NSImage(systemSymbolName: symbolName, accessibilityDescription: "Nexus Agent")?
            .withSymbolConfiguration(config)
        image?.isTemplate = false
        button.image = image
        button.contentTintColor = nil
        button.title = image == nil ? "NX" : ""
    }
}
