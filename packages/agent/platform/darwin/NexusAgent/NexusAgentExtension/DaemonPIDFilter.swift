// DaemonPIDFilter.swift — self-intercept guard for the E55 MITM bridge.
//
// Background: the macOS NETransparentProxyProvider intercepts ALL
// outbound TCP — including the agent daemon's own connections to the
// real upstream when MITMRelay forwards an inspect flow's decrypted
// HTTP. Without filtering, those daemon connections re-enter the NE
// → bridge → MITMRelay loop, blowing up as:
//   - source_process=nexus-agent on every audit row
//   - method/path/hooks all empty (the loop never completes)
//   - 12 rapid-fire identical rows then connections stall
//
// Daemon writes its PID to /var/run/nexus-agent/daemon.pid at startup
// (cmd/agent/main.go). NE reads + caches; on every flow, compares the
// flow's source PID against the cached daemon PID. Refresh on cache
// miss handles daemon restart without an extension restart.
//
// File-based delivery (vs IPC push) so the filter is testable in
// isolation and the protocol is a single integer text file. World-
// readable (0644) so the unsandboxed extension can stat + read.
//
// Fail-safe: missing / unreadable PID file disables the filter (no
// extra loop protection, but no false-positive blocking either).
// Same shape as QUICFallbackBundles' fail-safe.

import Foundation
import os.log

final class DaemonPIDFilter {
    private let logger = Logger(subsystem: "com.nexus-gateway.agent", category: "DaemonPIDFilter")
    private let filePath = "/var/run/nexus-agent/daemon.pid"
    /// Re-read the PID file no more often than this. Daemon restart
    /// produces a new PID; we want to pick that up within ~3s without
    /// reading on every flow.
    private let refreshInterval: TimeInterval = 3.0

    private let lock = NSLock()
    private var cachedPID: Int32 = 0
    private var lastLoad: Date = .distantPast

    /// isDaemon returns true iff pid matches the daemon's currently-
    /// known PID. Loads the PID file lazily on first call and refreshes
    /// every refreshInterval seconds. Empty / unreadable file disables
    /// the filter (returns false for everything).
    func isDaemon(pid: Int32) -> Bool {
        if pid <= 0 { return false }
        refreshIfStale()
        lock.lock()
        defer { lock.unlock() }
        return cachedPID > 0 && pid == cachedPID
    }

    /// reload re-reads the daemon PID file. Called from the lazy
    /// refresh path. Safe from any thread.
    func reload() {
        let data = (try? String(contentsOfFile: filePath, encoding: .utf8))
            .map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }
        let parsed = data.flatMap { Int32($0) } ?? 0
        lock.lock()
        let prev = cachedPID
        cachedPID = parsed
        lastLoad = Date()
        lock.unlock()
        if prev != parsed && parsed > 0 {
            logger.info("DaemonPIDFilter: PID updated → \(parsed) (was \(prev))")
        } else if parsed == 0 {
            logger.debug("DaemonPIDFilter: \(self.filePath, privacy: .public) absent or unparseable; filter disabled")
        }
    }

    private func refreshIfStale() {
        lock.lock()
        let stale = Date().timeIntervalSince(lastLoad) > refreshInterval
        lock.unlock()
        if stale { reload() }
    }
}
