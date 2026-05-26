import SwiftUI
import Combine
import UserNotifications

/// Minimal menu-bar view model (E40 Phase 1 + review fix-ups).
///
/// Owns only the state the seven-item NSMenu actually reads. Every
/// detail view that used to live inside a 320×520 popover moves to
/// the Wails Dashboard in Phase 2. This file is deliberately small.
///
/// Polling is a constant 2 s. The menu itself is built lazily via
/// NSMenuDelegate.menuNeedsUpdate (see AppDelegate), so a poll that
/// finds no UI-relevant change doesn't churn AppKit; the icon
/// subscription only fires when the four icon-relevant properties
/// actually move.
@MainActor
class AgentViewModel: ObservableObject {
    // ─── Menu-bar surface state ───────────────────────────────────────

    @Published var agentState: AgentState = .error
    @Published var stateReason: String = ""
    @Published var ssoEmail: String = ""
    @Published var paused: Bool = false
    @Published var pausedUntil: Date?
    @Published var pendingEnrollment: Bool = false
    @Published var updateAvailable: Bool = false
    @Published var transientMessage: String?
    /// Daemon build identity, surfaced in the menu header so the
    /// user can verify "did the update land" without opening About
    /// or running a CLI. Pulled from GET_STATUS.agent.version on
    /// every poll.
    @Published var daemonVersion: String = ""
    /// Runtime policy on whether the daemon honours SHUTDOWN-style IPC.
    /// Drives whether the menu builder includes the Restart Agent +
    /// Quit Nexus Agent items at all (compliance always-on deployments
    /// have this false and so neither affordance is surfaced). Defaults
    /// to true so dev / test builds keep both items visible until the
    /// first poll arrives — and so older daemons that don't yet emit
    /// the field continue to render the full menu.
    @Published var quitAllowed: Bool = true
    /// Live-traffic indicator (#69). True when the daemon has reported
    /// a provider-tagged audit event within the last 3 seconds, i.e.
    /// the user just made an LLM call. The tray-icon view subscribes
    /// to this to render a brief highlight overlay so the user gets
    /// "agent is doing AI work" feedback without us shouting at
    /// them on every connection.
    @Published var providerTrafficActive: Bool = false
    /// Per-locale shutdown warning text from the daemon's
    /// agent_settings shadow. quitAgent() consumes this to show a
    /// confirm dialog before tearing the daemon down. Empty / nil
    /// means admin hasn't enabled the warning — quit proceeds with
    /// no confirmation.
    @Published var shutdownWarning: [String: String] = [:]

    // ─── Internals ────────────────────────────────────────────────────

    private let client = StatusClient()
    private var pollTimer: Timer?
    private static let pollInterval: TimeInterval = 2
    /// Monotonic epoch for transientMessage so a fast sequence of
    /// operations never collapses into a single flash.
    private var transientMessageEpoch: Int = 0

    var trayIconName: String {
        // Live-traffic pulse: when an LLM call landed in the last
        // ~3s, swap the icon to its "fill" variant for a brief
        // moment so the user gets visual confirmation. SF Symbols
        // names follow the *.fill convention; agentState.systemImage
        // returns "shield" for active → "shield.fill" reads as a
        // bolder weight without changing colour or shape.
        if providerTrafficActive {
            return "\(agentState.systemImage).fill"
        }
        return agentState.systemImage
    }

    var trayIconColor: Color {
        if pendingEnrollment { return .red }
        if paused { return .yellow }
        switch agentState {
        case .active: return .green
        case .degraded: return .yellow
        case .error: return .red
        }
    }

    /// Pretty-prints the auto-resume time when one is scheduled.
    /// Returns nil for indefinite pauses or when not paused. The
    /// menu inserts this into the status row and the Resume item.
    var pausedUntilDisplay: String? {
        guard let d = pausedUntil else { return nil }
        let f = DateFormatter()
        f.dateStyle = .none
        f.timeStyle = .short
        return f.string(from: d)
    }

    init() {
        startPolling()
    }

    func startPolling() {
        fetchStatus()
        pollTimer?.invalidate()
        pollTimer = Timer.scheduledTimer(withTimeInterval: Self.pollInterval, repeats: true) { [weak self] _ in
            Task { @MainActor [weak self] in
                self?.fetchStatus()
            }
        }
    }

    deinit {
        pollTimer?.invalidate()
    }

    private var previousState: AgentState?

    func fetchStatus() {
        Task {
            do {
                let snap = try await client.getStatus()
                let newState = AgentState(rawValue: snap.state) ?? .error
                // Notify on degraded/error transitions so the user
                // sees a system banner even when the menu is closed.
                if let prev = previousState, prev == .active, newState != .active {
                    sendNotification(
                        title: "Nexus Agent",
                        body: snap.stateReason.isEmpty ? "Agent state: \(snap.state)" : snap.stateReason
                    )
                }
                self.previousState = newState
                self.agentState = newState
                self.stateReason = snap.stateReason
                self.pendingEnrollment = snap.agent.deviceID.isEmpty
                // Reflect both user-initiated and admin (Hub-shadow)
                // pauses; the menu shouldn't lag the daemon.
                self.paused = snap.paused ?? false
                self.pausedUntil = Self.parseRFC3339(snap.pausedUntil)
                self.ssoEmail = snap.agent.ssoEmail ?? ""
                self.daemonVersion = snap.agent.version ?? ""
                self.quitAllowed = snap.agent.quitAllowed ?? true
                // Update-available banner: the daemon's updater polls
                // Hub on a schedule (UpdaterCheckSec) and forwards the
                // boolean via StatusCollector; menu builder reads
                // viewModel.updateAvailable to decide whether to
                // surface the yellow "Update available — install" row.
                self.updateAvailable = snap.agent.updateAvailable ?? false
                self.shutdownWarning = snap.shutdownWarning ?? [:]
                // Live-traffic pulse: active when the daemon saw a
                // provider call within the last 3 seconds. The 3-s
                // window is tuned to the menu's 2-s poll cadence —
                // wide enough that a call between two polls still
                // lights the indicator at least once, narrow enough
                // that it fades quickly after activity stops.
                if let ts = snap.agent.lastProviderTrafficAt,
                   let t = Self.parseRFC3339(ts) {
                    self.providerTrafficActive = Date().timeIntervalSince(t) < 3.0
                } else {
                    self.providerTrafficActive = false
                }
            } catch {
                self.agentState = .error
                self.stateReason = ""
            }
        }
    }

    /// Post the result of a NE install attempt (system-extension
    /// activation or NETransparentProxyManager save) to the daemon
    /// so it lands in agent.log. Wrapped in showTransient so a slow
    /// IPC doesn't visibly stall the menu interaction.
    func reportProxyInstall(stage: String, outcome: String, error: String?, appVersion: String) async {
        let report = ProxyInstallReport(stage: stage, outcome: outcome, error: error, appVersion: appVersion)
        do {
            _ = try await client.reportProxyInstall(report)
        } catch {
            // IPC failure here is non-fatal — the os.log path still
            // captured the same data on the Swift side.
        }
    }

    /// Public entry point for the AppDelegate to surface a brief
    /// notice in the menu footer. Wraps the existing private
    /// transient mechanism so callers outside this file can reach it
    /// (the AppDelegate uses it for Reinstall NE / Restart App).
    func showTransientMessage(_ message: String) {
        showTransient(message)
    }

    // ─── Actions wired to NSMenu items ───────────────────────────────

    /// Launches the Dashboard window (Phase 2's Wails app). Falls back
    /// to a "not installed" alert if the bundled .app isn't on disk
    /// — covers users still on a Phase-1-era installation while the
    /// Phase-2 .pkg rolls out.
    func openDashboard() {
        if launchDashboardApp() { return }
        showComingSoonAlert(
            title: "Dashboard not installed",
            body: """
            The Nexus Agent Dashboard ships as a separate application \
            that pairs with this menu bar. Reinstall the latest \
            Nexus Agent .pkg and try again.

            CLI fallback:
                nexus-agent enroll-sso --hub-url <YOUR_HUB_URL>
            """
        )
    }

    /// Same surface as openDashboard — the Dashboard self-decides
    /// whether to render the Onboarding page based on the daemon's
    /// pending-enrollment state, so there's no separate route to
    /// drive from here.
    func openSetup() {
        if launchDashboardApp() { return }
        showComingSoonAlert(
            title: "Dashboard not installed",
            body: """
            Browser-based enrollment lives in the Nexus Agent Dashboard, \
            which ships with the .pkg. Reinstall to get it.

            CLI fallbacks:
                nexus-agent enroll-sso --hub-url <YOUR_HUB_URL>
                nexus-agent enroll      --hub-url <YOUR_HUB_URL> --token <T>
            """
        )
    }

    /// Tries to launch "Nexus Agent Dashboard.app". Search order:
    ///   1. <this menu-bar bundle>/Contents/Resources/Nexus Agent Dashboard.app
    ///                                                                    (canonical — embedded by build.sh)
    ///   2. /Applications/NexusAgent.app/Contents/Resources/Nexus Agent Dashboard.app
    ///                                                                    (explicit fallback when /Applications/NexusAgent.app/ doesn't match Bundle.main, e.g. during dev runs from dist/)
    ///   3. /Applications/Nexus Agent Dashboard.app                       (legacy standalone install)
    ///   4. ~/Applications/Nexus Agent Dashboard.app                      (per-user install)
    /// Returns true when a bundle was found and launched.
    ///
    /// NOTE: the deb/rpm-style installer drops the host app at
    /// `/Applications/NexusAgent.app/` (NO space) per pkgbuild's bundle
    /// component inference — `Bundle.main.bundleURL` resolves to that
    /// path on a normal install. Earlier drafts of this list used
    /// `/Applications/Nexus Agent.app/` (WITH space) which never
    /// matches the installed location and surfaced the "Dashboard
    /// not installed" alert on every Open Dashboard click.
    private func launchDashboardApp() -> Bool {
        var candidates: [String] = []

        // Canonical: the dashboard is embedded inside THIS bundle.
        let embedded = Bundle.main.bundleURL
            .appendingPathComponent("Contents/Resources/Nexus Agent Dashboard.app")
            .path
        candidates.append(embedded)

        // Explicit Applications-dir fallbacks (no-space + space variants
        // cover both pkgbuild-inferred and human-friendly naming).
        candidates.append("/Applications/NexusAgent.app/Contents/Resources/Nexus Agent Dashboard.app")
        candidates.append("/Applications/Nexus Agent.app/Contents/Resources/Nexus Agent Dashboard.app")

        // Standalone Dashboard installs (legacy path; not produced by
        // current build but kept so a manual drag-to-Applications
        // still works).
        candidates.append("/Applications/Nexus Agent Dashboard.app")
        candidates.append((NSHomeDirectory() as NSString).appendingPathComponent("Applications/Nexus Agent Dashboard.app"))

        for path in candidates where FileManager.default.fileExists(atPath: path) {
            NSWorkspace.shared.open(URL(fileURLWithPath: path))
            return true
        }
        return false
    }

    /// Settings clicks open the Dashboard window. The Dashboard's left
    /// sidebar carries the "Settings" nav item which the user taps to
    /// land on the real settings panel (theme, language, pause, sign
    /// out). A direct deep-link route would need a URL-scheme handler
    /// on the Wails side; not worth the build-config complexity until
    /// users complain that the extra click matters.
    func openSettings() {
        if launchDashboardApp() { return }
        showComingSoonAlert(
            title: "Dashboard not installed",
            body: """
            Settings live inside the Nexus Agent Dashboard, which ships \
            with the .pkg. Reinstall to get it.
            """
        )
    }

    func openAbout() {
        let alert = NSAlert()
        alert.messageText = "Nexus Agent"
        alert.informativeText = "Nexus Gateway desktop agent.\n© Nexus Gateway."
        alert.alertStyle = .informational
        alert.runModal()
    }

    func pauseProtection(seconds: Int = 0) {
        Task {
            do {
                let resp = try await client.pauseProtection(seconds: seconds)
                self.paused = resp.paused
                self.pausedUntil = Self.parseRFC3339(resp.resumesAt)
                if let err = resp.error {
                    self.showTransient("Pause failed: \(err)")
                }
            } catch {
                self.showTransient("Pause failed: \(error.localizedDescription)")
            }
        }
    }

    func resumeProtection() {
        Task {
            do {
                let resp = try await client.resumeProtection()
                self.paused = resp.paused
                self.pausedUntil = nil
                if let err = resp.error {
                    self.showTransient("Resume failed: \(err)")
                }
            } catch {
                self.showTransient("Resume failed: \(error.localizedDescription)")
            }
        }
    }

    /// Sign out / Switch identity: call UNENROLL via IPC. Daemon
    /// clears device cert + token and respawns into pending-
    /// enrollment mode; the Dashboard's next launch shows the
    /// onboarding sign-in screen. Best-effort — failures surface
    /// via the transient footer instead of throwing into the
    /// menu's hot path. Called from the SSO row's submenu (both
    /// Switch identity and Sign Out items go through here; the
    /// difference is whether NexusAgentApp.handleSwitchIdentity
    /// auto-opens the Dashboard afterwards).
    func signOut() {
        Task {
            do {
                let resp = try await client.signOut()
                if !resp.acknowledged {
                    self.showTransient(resp.error?.isEmpty == false
                        ? resp.error!
                        : "Sign out blocked")
                }
            } catch {
                self.showTransient("Sign out failed: \(error.localizedDescription)")
            }
        }
    }

    /// Restart the daemon via SHUTDOWN IPC; launchd respawns it.
    /// Surfaces the daemon's policy refusal (operator set
    /// `quitAllowed: false`) instead of silently swallowing it.
    func restartAgent() {
        Task {
            do {
                let resp = try await client.shutdown()
                if !resp.acknowledged {
                    self.showTransient(resp.error?.isEmpty == false
                        ? resp.error!
                        : "Restart blocked by policy")
                }
            } catch {
                self.showTransient("Restart failed: \(error.localizedDescription)")
            }
        }
    }

    func quitAgent() {
        // Quit ALWAYS confirms — it's a destructive operation that stops
        // the enterprise security daemon entirely. Two text sources, in
        // priority order:
        //   1. Admin-configured shutdownWarning from the agent_settings
        //      shadow (compliance teams want exact wording for their
        //      org's policy reminder).
        //   2. Built-in localized fallback when admin hasn't enabled
        //      a custom warning. Previously the dialog was suppressed
        //      entirely in this case, which let users one-click stop
        //      the daemon without any "are you sure?" — wrong default
        //      for an enterprise security agent.
        let warningText = resolveShutdownWarning(self.shutdownWarning)
            ?? String(localized: "quit.fallbackWarning", bundle: .module)
        let alert = NSAlert()
        alert.messageText = String(localized: "quit.confirmTitle", bundle: .module)
        alert.informativeText = warningText
        alert.alertStyle = .warning
        alert.addButton(withTitle: String(localized: "quit.confirmAction", bundle: .module))
        alert.addButton(withTitle: String(localized: "quit.cancelAction", bundle: .module))
        NSApp.activate(ignoringOtherApps: true)
        let resp = alert.runModal()
        // .alertFirstButtonReturn = Quit; .alertSecondButtonReturn = Cancel
        guard resp == .alertFirstButtonReturn else {
            return
        }

        // User-controlled daemon lifecycle (see [[agent-quit-flag-design]]):
        //   1. Write the user-quit flag file. This is the load-bearing
        //      step — once it's there, every launchd respawn of the
        //      daemon sees the flag at startup and self-exits. The
        //      daemon also runs a 2 s-tick flag-watcher goroutine, so
        //      even an already-running daemon notices and exits when
        //      the flag appears. Re-launching NexusAgent.app removes
        //      the flag (AppDelegate.setUp) and the next launchd
        //      respawn (~10 s, capped by ThrottleInterval) brings the
        //      daemon back.
        //   2. Synchronously call SHUTDOWN IPC with a tight timeout.
        //      When the daemon is healthy this races the watcher and
        //      wins (sub-100 ms vs 2 s), so the user gets an immediate
        //      "really gone" feel. When the daemon is dead or the
        //      socket is missing, the call errors out fast and the
        //      watcher path still picks it up within 2 s. The IPC
        //      lets the daemon flush its audit queue + close the
        //      WebSocket cleanly instead of getting killed mid-write
        //      by the watcher's bare ctx-cancel.
        //   3. Terminate the Dashboard + menu-bar app processes. The
        //      menu-bar and the nested Wails Dashboard are separate
        //      NSApplication processes; quitting one without the
        //      other leaves an orphaned window with no tray.
        //
        // No sudo needed at any step — the flag dir at
        // /Library/Application Support/com.nexus-gateway.agent/flags
        // is chmod 0777 by the .pkg postinstall script. The daemon
        // owns the dir, user-space writes inside it.
        QuitFlag.set()

        // Synchronous IPC send: blocks the menu-bar's main thread
        // briefly so the daemon acks BEFORE we terminate. Without
        // this, the older fire-and-forget Task would race
        // NSApp.terminate(nil) and lose — the menu-bar process would
        // exit before the async Task ever connected to the socket,
        // leaving the running daemon untouched. The 1.5 s ceiling
        // bounds the worst-case "menu Quit feels frozen" pause; on a
        // healthy daemon the round-trip is sub-100 ms. DispatchSemaphore
        // on the main thread is acceptable here exclusively because
        // we are mid-shutdown and about to call NSApp.terminate
        // anyway — no UI work is happening.
        let semaphore = DispatchSemaphore(value: 0)
        Task.detached { [client] in
            _ = try? await client.shutdown()
            semaphore.signal()
        }
        _ = semaphore.wait(timeout: .now() + 1.5)

        // Remove the NETransparentProxyManager config so macOS NECP
        // no longer routes flows through our extension. Without this,
        // the OS keeps the filter configured (even though our daemon
        // is dead via QuitFlag) and tries to dispatch new flows to a
        // non-existent provider, causing user-visible network breakage.
        // 3 s ceiling: removeFromPreferences round-trips to NEAgent
        // and typically completes in 100-300 ms; cap higher than that
        // so a slow NEAgent still completes within the Quit budget.
        // User binding (#83): "Quit means the network goes back to
        // native routing fully and immediately." This is what gives
        // the user a true "get me back to normal" button.
        let removeSemaphore = DispatchSemaphore(value: 0)
        TransparentProxyManager.shared.removeAllConfigs {
            removeSemaphore.signal()
        }
        _ = removeSemaphore.wait(timeout: .now() + 3.0)

        let dashboardBundleIDs = [
            "com.nexus-gateway.agent.dashboard",
            "com.wails.Nexus Agent Dashboard",
        ]
        var apps: [NSRunningApplication] = []
        for id in dashboardBundleIDs {
            apps.append(contentsOf: NSRunningApplication.runningApplications(withBundleIdentifier: id))
        }
        for app in apps {
            // terminate() asks politely; if the Dashboard is mid-IPC
            // it'll finish the current request via Wails onShutdown
            // (which already sends AUTHENTICATE CANCEL with a 500 ms
            // budget — see ui/bridge.go). Force-quit is intentionally
            // NOT used here so an in-flight SSO flow doesn't get
            // half-completed on the Hub side.
            app.terminate()
        }
        NSApp.terminate(nil)
    }

    func checkUpdate() {
        Task {
            do {
                let resp = try await client.checkUpdate()
                self.updateAvailable = resp.available
                self.showTransient(resp.available
                    ? "Update available: \(resp.version ?? "")"
                    : "Up to date")
            } catch {
                self.showTransient("Update check failed")
            }
        }
    }

    // ─── Helpers ─────────────────────────────────────────────────────

    private func showTransient(_ message: String) {
        transientMessageEpoch += 1
        let epoch = transientMessageEpoch
        transientMessage = message
        Task { @MainActor [weak self] in
            try? await Task.sleep(for: .seconds(3))
            guard let self else { return }
            if self.transientMessageEpoch == epoch {
                self.transientMessage = nil
            }
        }
    }

    private func sendNotification(title: String, body: String) {
        let center = UNUserNotificationCenter.current()
        center.requestAuthorization(options: [.alert, .sound]) { granted, _ in
            guard granted else { return }
            let content = UNMutableNotificationContent()
            content.title = title
            content.body = body
            content.sound = .default
            let request = UNNotificationRequest(identifier: UUID().uuidString, content: content, trigger: nil)
            center.add(request)
        }
    }

    private func showComingSoonAlert(title: String, body: String) {
        NSApp.activate(ignoringOtherApps: true)
        let alert = NSAlert()
        alert.messageText = title
        alert.informativeText = body
        alert.alertStyle = .informational
        alert.runModal()
    }

    private static let rfc3339Formatter: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f
    }()

    private static func parseRFC3339(_ s: String?) -> Date? {
        guard let s, !s.isEmpty else { return nil }
        return rfc3339Formatter.date(from: s)
    }
}

/// resolveShutdownWarning picks the locale-matching string from the
/// admin-configured warning map and returns it. Lookup order:
///   1. Exact match on the OS's preferred language (e.g. "zh-Hans").
///   2. Two-letter prefix (e.g. "zh" matches when the map has "zh"
///      even if the OS reports "zh-Hans-CN").
///   3. The "en" key as the fleet-default fallback.
///   4. Any first value present (last-resort, deterministic order
///      by sorted key so multiple invocations agree).
/// Returns nil when the map is empty — caller skips the dialog.
private func resolveShutdownWarning(_ warnings: [String: String]) -> String? {
    if warnings.isEmpty {
        return nil
    }
    let preferred = Locale.preferredLanguages.first ?? "en"
    if let exact = warnings[preferred], !exact.isEmpty {
        return exact
    }
    if let dash = preferred.firstIndex(of: "-") {
        let prefix = String(preferred[preferred.startIndex..<dash])
        if let pref = warnings[prefix], !pref.isEmpty {
            return pref
        }
    }
    if let en = warnings["en"], !en.isEmpty {
        return en
    }
    return warnings.keys.sorted().first.flatMap { warnings[$0] }
}
