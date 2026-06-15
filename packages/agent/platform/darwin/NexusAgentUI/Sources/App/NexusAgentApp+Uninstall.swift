import AppKit
import Foundation

// App-side uninstall teardown (the `--uninstall` headless mode). Removes the NE
// config, deactivates the system extension (only the app can, under SIP), and
// unregisters the SMAppService daemon + login item, then terminates. A hard
// timeout guarantees the process exits even if a macOS request never calls back,
// so the root uninstall.sh's `open -W` never hangs. Extracted from
// NexusAgentApp.swift to keep the AppDelegate file under the size ratchet.
extension AppDelegate {

    func runUninstall() {
        // Hard fail-safe FIRST: `--uninstall` is driven by uninstall.sh via
        // `open -W`, so this process must never hang the caller — even if the
        // status query or the teardown below stalls on an unresponsive daemon.
        // finishUninstall is idempotent, so this backstop is harmless when the
        // normal path already terminated.
        DispatchQueue.main.asyncAfter(deadline: .now() + 35.0) { [weak self] in
            self?.neLogger.error("uninstall: fail-safe timeout reached — terminating")
            self?.finishUninstall()
        }
        // Policy gate, parallel to quitAgent(): a quitAllowed=false (locked)
        // device must refuse uninstall too — otherwise `sudo uninstall.sh` would
        // tear down enforcement on a device whose whole point is that it can't be.
        // Refuse only on an explicit false (an unreachable/absent daemon means
        // this isn't a healthy locked fleet, so a broken/unmanaged install can
        // still be removed — matches the `?? true` back-compat used elsewhere).
        Task { @MainActor in
            if let snap = try? await StatusClient().getStatus(), snap.agent.quitAllowed == false {
                neLogger.error("uninstall refused: quitAllowed=false (locked device); terminating without teardown")
                NSApp.terminate(nil)
                return
            }
            performUninstallTeardown()
        }
    }

    func performUninstallTeardown() {
        neLogger.info("uninstall mode: NE deactivate + SMAppService unregister")
        let group = DispatchGroup()
        group.enter()
        TransparentProxyManager.shared.removeAllConfigs { group.leave() }
        group.enter()
        SystemExtensionManager.shared.deactivate { result in
            if case .failure(let e) = result {
                self.neLogger.error("uninstall: NE deactivate failed (continuing): \(e.localizedDescription)")
            }
            group.leave()
        }
        group.notify(queue: .main) { [weak self] in self?.finishUninstall() }
        // Fail-safe: never let uninstall hang the caller. 30 s covers a slow
        // NEAgent/sysext round-trip; past that we unregister + quit regardless.
        DispatchQueue.main.asyncAfter(deadline: .now() + 30.0) { [weak self] in self?.finishUninstall() }
    }

    /// Idempotent final step of the uninstall teardown (runs from whichever of
    /// the completion or the fail-safe timeout fires first).
    func finishUninstall() {
        guard !uninstallDone else { return }
        uninstallDone = true
        LaunchServiceManager.unregister()
        neLogger.info("uninstall: app-side teardown complete; terminating")
        NSApp.terminate(nil)
    }
}
