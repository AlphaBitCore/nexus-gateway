import NetworkExtension
import os.log

// Enables the NETransparentProxyManager configuration that activates the
// com.nexus-gateway.agent.extension system extension as a transparent proxy.
// Must be called after the system extension is installed and approved.
final class TransparentProxyManager {

    static let shared = TransparentProxyManager()

    private let extensionID = "com.nexus-gateway.agent.extension"
    private let logger = Logger(subsystem: "com.nexus-gateway.agent", category: "NEProxy")

    /// Guards the auto-recovery path so a forceReinstall whose own
    /// applyConfig → ensureRunning chain ALSO throws code=1 doesn't
    /// recurse forever. Set true the moment we decide to recover;
    /// cleared in the recovery completion handler. Single-threaded
    /// access via the NETransparentProxyManager callback queue, so
    /// no extra lock is needed.
    private var recoveryInFlight = false

    /// Idempotent installer — called on every host launch. Reuses an
    /// existing manager when one is already configured + enabled for
    /// our extension; otherwise creates and saves a fresh one. The
    /// short-circuit is important here because saveToPreferences
    /// presents the macOS "Allow proxy configurations" dialog when
    /// the user has not approved it before — we do NOT want that
    /// dialog re-popping on every launch.
    func enableIfNeeded(completion: @escaping (Error?) -> Void) {
        let t0 = Date()
        logger.info("enableIfNeeded: start (extensionID=\(self.extensionID))")
        NETransparentProxyManager.loadAllFromPreferences { [weak self] managers, error in
            guard let self else { return }
            if let error {
                self.logger.error("enableIfNeeded: loadAllFromPreferences failed: \(error.localizedDescription)")
                completion(error)
                return
            }
            let total = managers?.count ?? 0
            self.logger.info("enableIfNeeded: loadAllFromPreferences ok in \(Int(Date().timeIntervalSince(t0) * 1000))ms; \(total) total NETransparentProxyManager(s) on disk")
            for (i, m) in (managers ?? []).enumerated() {
                let bid = (m.protocolConfiguration as? NETunnelProviderProtocol)?.providerBundleIdentifier ?? "<nil>"
                self.logger.info("enableIfNeeded: existing[\(i)] bundleId=\(bid) isEnabled=\(m.isEnabled) status=\(self.statusName(m.connection.status)) desc=\(m.localizedDescription ?? "<nil>")")
            }

            // Reuse an existing manager if it is already configured for our extension.
            let existing = managers?.first(where: {
                ($0.protocolConfiguration as? NETunnelProviderProtocol)?
                    .providerBundleIdentifier == self.extensionID
            })
            let manager: NETransparentProxyManager
            if let existing {
                manager = existing
                self.logger.info("enableIfNeeded: reusing existing manager (isEnabled=\(existing.isEnabled), status=\(self.statusName(existing.connection.status)))")
            } else {
                manager = NETransparentProxyManager()
                self.logger.info("enableIfNeeded: no existing manager for our bundleId — creating fresh one")
            }

            if manager.isEnabled {
                // Config is persisted + flagged enabled, but that
                // does NOT mean the provider process is running.
                // NETransparentProxyManager.saveToPreferences only
                // PERSISTS the config; macOS will not spawn the
                // system extension's NETransparentProxyProvider until
                // someone calls connection.startVPNTunnel. Skipping
                // ensureRunning here is exactly what produced the
                // long-running "Network filter not connected" stuck
                // state across reinstalls and reboots.
                self.logger.info("Manager already enabled; ensuring session is started")
                self.ensureRunning(manager, completion: completion)
                return
            }

            self.applyConfig(to: manager, completion: completion)
        }
    }

    /// Force-reinstall path — invoked by the menu-bar's "Reinstall
    /// Network Extension" button. Bypasses the `isEnabled` short-
    /// circuit because the whole point of the menu action is to fix
    /// a stuck state where macOS believes the proxy is enabled but
    /// the NE provider never actually attached to the daemon (e.g.
    /// after an ad-hoc-signed build saved a config that the kernel
    /// silently dropped, or after a prior install was interrupted).
    ///
    /// Strategy: tear every existing manager for our bundle id down
    /// via removeFromPreferences, then create a fresh one and save.
    /// macOS NEAgent treats a removeFromPreferences-then-save as a
    /// clean reinstall and re-evaluates the system extension's
    /// startTunnel path from scratch.
    func forceReinstall(completion: @escaping (Error?) -> Void) {
        let t0 = Date()
        logger.info("forceReinstall: start (extensionID=\(self.extensionID))")
        NETransparentProxyManager.loadAllFromPreferences { [weak self] managers, error in
            guard let self else { return }
            if let error {
                self.logger.error("forceReinstall: loadAllFromPreferences failed: \(error.localizedDescription)")
                completion(error)
                return
            }
            self.logger.info("forceReinstall: loadAllFromPreferences ok in \(Int(Date().timeIntervalSince(t0) * 1000))ms; total=\(managers?.count ?? 0)")

            let ours = (managers ?? []).filter {
                ($0.protocolConfiguration as? NETunnelProviderProtocol)?
                    .providerBundleIdentifier == self.extensionID
            }
            self.logger.info("forceReinstall: \(ours.count) existing manager(s) match our bundleId — will remove sequentially then create fresh")

            // Remove all existing managers in sequence, then create
            // and save a fresh one. Errors removing a manager are
            // logged but don't abort — saving fresh should still
            // overwrite stale state.
            self.removeNext(remaining: ours) { [weak self] in
                guard let self else { return }
                let manager = NETransparentProxyManager()
                self.applyConfig(to: manager, completion: completion)
            }
        }
    }

    /// Public Quit-path helper: load every NETransparentProxyManager
    /// matching our bundle id and removeFromPreferences each one. After
    /// this returns, macOS NECP no longer routes flows to our extension
    /// — flows go back to native routing immediately, exactly as if
    /// the agent had never been installed. Paired with the daemon's
    /// shutdown + mDNSResponder flush, this gives the user a Quit
    /// button that ACTUALLY frees the network even if the extension
    /// is misbehaving.
    ///
    /// Idempotent: completion fires with `nil` when there were no
    /// managers to remove. Errors per-manager are logged but never
    /// propagated — a half-removed state is strictly better than
    /// "Quit failed; network still blocked." The user's mental model
    /// is "Quit gets me back to normal"; we must honour it.
    func removeAllConfigs(completion: @escaping () -> Void) {
        let t0 = Date()
        logger.info("removeAllConfigs: start (extensionID=\(self.extensionID))")
        NETransparentProxyManager.loadAllFromPreferences { [weak self] managers, error in
            guard let self else { completion(); return }
            if let error {
                self.logger.error("removeAllConfigs: loadAllFromPreferences failed (continuing with whatever we found): \(error.localizedDescription)")
            }
            let ours = (managers ?? []).filter {
                ($0.protocolConfiguration as? NETunnelProviderProtocol)?
                    .providerBundleIdentifier == self.extensionID
            }
            self.logger.info("removeAllConfigs: loadAllFromPreferences ok in \(Int(Date().timeIntervalSince(t0) * 1000))ms; \(ours.count) manager(s) for our bundleId — removing all")
            if ours.isEmpty {
                completion()
                return
            }
            self.removeNext(remaining: ours) {
                self.logger.info("removeAllConfigs: all managers removed; NE filter is gone — flows now route natively")
                completion()
            }
        }
    }

    /// Sequential removeFromPreferences for an array of managers.
    /// Continues even if individual removes error so a missing /
    /// half-saved manager doesn't block the fresh save.
    private func removeNext(remaining: [NETransparentProxyManager], done: @escaping () -> Void) {
        guard let head = remaining.first else {
            logger.info("removeNext: drained — proceeding to fresh save")
            done()
            return
        }
        let bid = (head.protocolConfiguration as? NETunnelProviderProtocol)?.providerBundleIdentifier ?? "<nil>"
        logger.info("removeNext: removing manager (bundleId=\(bid), isEnabled=\(head.isEnabled), status=\(self.statusName(head.connection.status))); \(remaining.count - 1) remaining after")
        let tail = Array(remaining.dropFirst())
        head.removeFromPreferences { [weak self] err in
            if let err {
                let nsErr = err as NSError
                self?.logger.error("removeNext: removeFromPreferences FAILED (continuing): domain=\(nsErr.domain) code=\(nsErr.code) localized=\(err.localizedDescription)")
            } else {
                self?.logger.info("removeNext: removed ok")
            }
            self?.removeNext(remaining: tail, done: done)
        }
    }

    /// Shared "stamp configuration + save" used by both enableIfNeeded
    /// and forceReinstall.
    private func applyConfig(to manager: NETransparentProxyManager,
                             completion: @escaping (Error?) -> Void) {
        let t0 = Date()
        let priorBundleId = (manager.protocolConfiguration as? NETunnelProviderProtocol)?.providerBundleIdentifier ?? "<nil>"
        let priorEnabled = manager.isEnabled
        let priorStatus = manager.connection.status
        logger.info("applyConfig: start (priorBundleId=\(priorBundleId), priorEnabled=\(priorEnabled), priorStatus=\(self.statusName(priorStatus)))")

        let proto = NETunnelProviderProtocol()
        proto.providerBundleIdentifier = self.extensionID
        proto.serverAddress = "127.0.0.1"

        manager.protocolConfiguration = proto
        manager.localizedDescription = "Nexus Agent"
        manager.isEnabled = true
        logger.info("applyConfig: stamped protocolConfiguration (providerBundleIdentifier=\(self.extensionID), serverAddress=127.0.0.1) and isEnabled=true; calling saveToPreferences")

        manager.saveToPreferences { [weak self] error in
            guard let self else { return }
            let elapsed = Int(Date().timeIntervalSince(t0) * 1000)
            if let error {
                let nsErr = error as NSError
                self.logger.error("applyConfig: saveToPreferences FAILED in \(elapsed)ms: domain=\(nsErr.domain) code=\(nsErr.code) localized=\(error.localizedDescription)")
                completion(error)
                return
            }
            self.logger.info("applyConfig: saveToPreferences ok in \(elapsed)ms; handing off to ensureRunning")
            self.ensureRunning(manager, completion: completion)
        }
    }

    /// Issue startVPNTunnel when the proxy session is not already
    /// running. NETransparentProxyManager.saveToPreferences ONLY
    /// persists the configuration — macOS does NOT auto-launch the
    /// NETransparentProxyProvider extension until a client explicitly
    /// asks for a session via connection.startVPNTunnel. Without this
    /// call the saved config sits in `[Primary Tunnel:Nexus Agent:…]
    /// stopped status disconnected` indefinitely (visible in the
    /// `nesessionmanager` log) and the menu bar permanently shows
    /// "Attention Needed — Network filter not connected".
    ///
    /// The loadFromPreferences round-trip is required: startVPNTunnel
    /// against a manager that has not been refreshed since a
    /// saveToPreferences may throw NEVPNErrorConfigurationStale.
    private func ensureRunning(_ manager: NETransparentProxyManager,
                               completion: @escaping (Error?) -> Void) {
        let t0 = Date()
        let preLoadStatus = manager.connection.status
        logger.info("ensureRunning: start (pre-load status=\(self.statusName(preLoadStatus))); calling loadFromPreferences")
        manager.loadFromPreferences { [weak self] error in
            guard let self else { return }
            let loadMs = Int(Date().timeIntervalSince(t0) * 1000)
            if let error {
                let nsErr = error as NSError
                self.logger.error("ensureRunning: loadFromPreferences FAILED in \(loadMs)ms: domain=\(nsErr.domain) code=\(nsErr.code) localized=\(error.localizedDescription)")
                completion(error)
                return
            }
            let status = manager.connection.status
            self.logger.info("ensureRunning: loadFromPreferences ok in \(loadMs)ms; status now=\(self.statusName(status))")
            switch status {
            case .connected, .connecting, .reasserting:
                self.logger.info("ensureRunning: session already \(self.statusName(status)) — no-op (provider should be running; check pgrep com.nexus-gateway.agent.extension)")
                completion(nil)
            default:
                do {
                    self.logger.info("ensureRunning: calling connection.startVPNTunnel(options: nil) [prior status=\(self.statusName(status))]")
                    try manager.connection.startVPNTunnel(options: nil)
                    self.logger.info("ensureRunning: startVPNTunnel returned without throw — macOS should now spawn the provider extension within ~1s; new connection.status=\(self.statusName(manager.connection.status))")
                    completion(nil)
                } catch {
                    let nsErr = error as NSError
                    self.logger.error("ensureRunning: startVPNTunnel THREW: domain=\(nsErr.domain) code=\(nsErr.code) localized=\(error.localizedDescription) — common causes: NEVPNErrorConfigurationDisabled (isEnabled=false), NEVPNErrorConfigurationStale (need re-load), NEVPNErrorConfigurationInvalid (bad protocol), NEVPNErrorConfigurationReadWriteFailed")

                    // Auto-recovery (build #16+): code=1
                    // (NEVPNErrorConfigurationInvalid) and code=2
                    // (NEVPNErrorConfigurationStale) almost always
                    // mean the on-disk manager config was written by
                    // a previous build whose protocol shape macOS no
                    // longer accepts. The prior recovery story was
                    // "user clicks Reinstall NE in the menu", but
                    // the menu has since been slimmed — leaving
                    // upgrade-broken users with no GUI escape.
                    // forceReinstall removes every manager bound to
                    // our bundle id and re-saves a fresh one, which
                    // unsticks this state. recoveryInFlight is the
                    // anti-loop guard: if forceReinstall's own
                    // applyConfig→ensureRunning chain ALSO throws
                    // code=1 we surface the original error rather
                    // than recursing forever.
                    if !self.recoveryInFlight,
                       nsErr.domain == NEVPNErrorDomain,
                       nsErr.code == 1 || nsErr.code == 2 {
                        self.logger.info("ensureRunning: auto-recovering via forceReinstall (NEVPNErrorDomain code=\(nsErr.code) — stale/invalid config from a prior build)")
                        self.recoveryInFlight = true
                        self.forceReinstall { [weak self] err in
                            self?.recoveryInFlight = false
                            if let err {
                                self?.logger.error("ensureRunning: auto-recovery forceReinstall FAILED: \(err.localizedDescription)")
                            } else {
                                self?.logger.info("ensureRunning: auto-recovery forceReinstall ok")
                            }
                            completion(err)
                        }
                        return
                    }

                    completion(error)
                }
            }
        }
    }

    /// Map NEVPNStatus rawValue to a human name so logs are searchable
    /// without needing to know the enum encoding (Apple's enum is
    /// `NEVPNStatus.invalid = 0, .disconnected = 1, .connecting = 2,
    /// .connected = 3, .reasserting = 4, .disconnecting = 5`).
    private func statusName(_ s: NEVPNStatus) -> String {
        switch s {
        case .invalid: return "invalid(0)"
        case .disconnected: return "disconnected(1)"
        case .connecting: return "connecting(2)"
        case .connected: return "connected(3)"
        case .reasserting: return "reasserting(4)"
        case .disconnecting: return "disconnecting(5)"
        @unknown default: return "unknown(\(s.rawValue))"
        }
    }

    func disable(completion: @escaping (Error?) -> Void) {
        logger.info("disable: start (extensionID=\(self.extensionID))")
        NETransparentProxyManager.loadAllFromPreferences { [weak self] managers, error in
            guard let self else { return }
            if let error {
                self.logger.error("disable: loadAllFromPreferences failed: \(error.localizedDescription)")
                completion(error)
                return
            }
            guard let manager = managers?.first(where: {
                ($0.protocolConfiguration as? NETunnelProviderProtocol)?
                    .providerBundleIdentifier == self.extensionID
            }) else {
                self.logger.info("disable: no manager found for our bundleId — nothing to disable")
                completion(nil)
                return
            }
            self.logger.info("disable: found manager (isEnabled=\(manager.isEnabled), status=\(self.statusName(manager.connection.status))) — setting isEnabled=false and saving")
            manager.isEnabled = false
            manager.saveToPreferences { [weak self] err in
                if let err {
                    let nsErr = err as NSError
                    self?.logger.error("disable: saveToPreferences FAILED: domain=\(nsErr.domain) code=\(nsErr.code) localized=\(err.localizedDescription)")
                } else {
                    self?.logger.info("disable: saveToPreferences ok — proxy session should disconnect within ~1s")
                }
                completion(err)
            }
        }
    }
}
