// QUICFallbackBundles.swift — runtime allowlist of bundle IDs whose UDP
// flows the NE proxy must close to force a QUIC → TCP downgrade.
//
// Single source of truth: agent daemon writes
// `/var/run/nexus-agent/quic-bundles.json` whenever the Hub-pushed
// `agent_settings.forceQUICFallbackBundles` shadow value changes (or
// on agent boot). The file format is the literal JSON array of strings:
//   ["com.google.Chrome", "com.microsoft.edgemac", ...]
//
// Why file-bridged instead of IPC-pushed: NE's `handleNewFlow` MUST
// decide synchronously and is called once per flow (high-frequency).
// A blocking IPC roundtrip on every flow would tank performance.
// File read on a 60s refresh is O(1) per flow (cached) and survives
// daemon restarts cleanly — NE keeps last-known list if daemon goes
// away. Hub admin remains the sole authority over the list.
//
// Why NO hardcoded fallback: the list is admin-controlled config. A
// hardcoded fallback would silently override admin intent (e.g. admin
// removes Safari from the list to allow QUIC-based AI clients on Macs
// — our hardcoded list would still kill it). The cost of "no fallback"
// is a brief bootstrap window on first install before Hub sync lands;
// during that window the agent simply doesn't kill any UDP. That is
// the intended fail-safe behavior — it's better to under-enforce for
// a few seconds than to over-enforce against admin policy forever.

import Foundation
import os.log

final class QUICFallbackBundles {
    private let logger = Logger(subsystem: "com.nexus-gateway.agent", category: "QUICFallback")
    private let filePath = "/var/run/nexus-agent/quic-bundles.json"
    private let refreshInterval: TimeInterval = 60.0

    private let lock = NSLock()
    private var cache: Set<String> = []
    private var lastLoad: Date = .distantPast

    /// Returns true iff the given bundleId is on the current force-fallback
    /// allowlist. Empty bundleId (unsigned binaries) always returns false.
    /// Triggers a lazy refresh when the cache is older than refreshInterval.
    ///
    /// #74: matches by exact bundleId OR helper/child prefix
    /// ("<parent>.<anything>") because the bundleId macOS reports for
    /// outbound UDP is the EXECUTING process bundle, which for Chromium
    /// browsers is `com.google.Chrome.helper` (and `.helper.GPU`,
    /// `.helper.Renderer` etc.), NOT the parent `com.google.Chrome`.
    /// Exact-only matching let Chrome's H3-over-UDP slip past every
    /// kill (chatgpt.com bypasses the TCP intercept entirely — observed
    /// 2026-05-24 with `com.google.Chrome` in allowlist + Chrome's
    /// QUIC bundleId being `com.google.Chrome.helper`).
    func shouldForceFallback(bundleId: String) -> Bool {
        if bundleId.isEmpty { return false }
        refreshIfStale()
        lock.lock()
        defer { lock.unlock() }
        if cache.contains(bundleId) { return true }
        for parent in cache {
            if bundleId.hasPrefix(parent + ".") { return true }
        }
        return false
    }

    /// Force a reload from disk now. Called on first access and from
    /// the lazy refresh path; safe to call from any thread.
    func reload() {
        let url = URL(fileURLWithPath: filePath)
        guard let data = try? Data(contentsOf: url) else {
            // File missing or unreadable → empty allowlist (fail-safe:
            // no UDP gets killed, no DNS collateral). Logged at debug
            // because pre-Hub-sync this is the expected steady state.
            lock.lock()
            let was = cache.count
            cache = []
            lastLoad = Date()
            lock.unlock()
            logger.debug("QUICFallbackBundles: \(self.filePath, privacy: .public) absent or unreadable; allowlist now empty (was=\(was))")
            return
        }
        guard let arr = try? JSONDecoder().decode([String].self, from: data) else {
            logger.error("QUICFallbackBundles: failed to decode JSON array from \(self.filePath, privacy: .public) (\(data.count) bytes); leaving previous allowlist in place")
            lock.lock(); lastLoad = Date(); lock.unlock()
            return
        }
        let next = Set(arr)
        lock.lock()
        let prev = cache
        cache = next
        lastLoad = Date()
        lock.unlock()
        if prev != next {
            logger.info("QUICFallbackBundles: allowlist updated count=\(next.count) bundles=\(arr.joined(separator: ","), privacy: .public)")
        }
    }

    private func refreshIfStale() {
        lock.lock()
        let stale = Date().timeIntervalSince(lastLoad) > refreshInterval
        lock.unlock()
        if stale { reload() }
    }
}

/// Tiny one-shot guard used by peekSNIThenRelay's race between the
/// 500 ms timeout dispatch and the actual readData callback. Whoever
/// calls tryFire() first wins; the loser must drop its work.
final class TimeoutGuard {
    private let lock = NSLock()
    private var fired = false

    func tryFire() -> Bool {
        lock.lock()
        defer { lock.unlock() }
        if fired { return false }
        fired = true
        return true
    }
}
