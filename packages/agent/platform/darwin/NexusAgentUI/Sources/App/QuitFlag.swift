import Foundation
import os.log

/// QuitFlag is a tiny filesystem-mediated handshake between the
/// user-space menu-bar app and the system-level LaunchDaemon. When the
/// user picks "Quit Nexus Agent" the menu sets the flag; the daemon's
/// main() reads the flag on every launchd respawn and exits immediately
/// while the flag is present, so launchd's KeepAlive=true effectively
/// becomes "respawn-and-die loop" (one cheap fork per ThrottleInterval).
/// Re-launching NexusAgent.app clears the flag → next launchd respawn
/// brings the daemon back up. No sudo, no SMAppService, no architectural
/// rewrite. See [[agent-quit-flag-design]].
///
/// Path: `/Library/Application Support/com.nexus-gateway.agent/flags/user-quit`
/// — the parent `flags` dir is created with mode 0777 by the .pkg
/// postinstall script so this user-space app can write/delete inside it
/// while the dir itself remains daemon-owned. The flag's PRESENCE is
/// the signal; its content is ignored (we still write the wall-clock
/// timestamp + pid for forensic value if anyone greps).
enum QuitFlag {

    /// Path of the flag file. Hardcoded because (a) it's a system-wide
    /// per-host signal, (b) the daemon's Go code resolves the same path
    /// hardcoded, (c) running the menu-bar app from a non-standard
    /// install location wouldn't change where the daemon expects to
    /// read it.
    static let path = "/Library/Application Support/com.nexus-gateway.agent/flags/user-quit"

    private static let logger = Logger(subsystem: "com.nexus-gateway.agent", category: "QuitFlag")

    /// `set` writes the flag with a small JSON body. Best-effort —
    /// failures are logged but never bubble up: even if we can't write
    /// the flag (read-only filesystem? perms reset?) the Quit flow
    /// still terminates the UI, and the daemon will respawn into a
    /// normal session. The user can always re-Quit.
    static func set() {
        let body = "{\"setAt\":\"\(ISO8601DateFormatter().string(from: Date()))\",\"by\":\"NexusAgentUI\"}\n"
        do {
            try body.data(using: .utf8)?.write(to: URL(fileURLWithPath: path), options: .atomic)
            logger.info("user-quit flag set at \(path, privacy: .public)")
        } catch {
            logger.error("user-quit flag set failed: \(error.localizedDescription, privacy: .public)")
        }
    }

    /// `clear` removes the flag if present. Idempotent — no-op on
    /// missing files. Failures (e.g. perms) are logged but non-fatal.
    static func clear() {
        let fm = FileManager.default
        guard fm.fileExists(atPath: path) else { return }
        do {
            try fm.removeItem(atPath: path)
            logger.info("user-quit flag cleared at \(path, privacy: .public)")
        } catch {
            logger.error("user-quit flag clear failed: \(error.localizedDescription, privacy: .public)")
        }
    }
}
