// BypassBundles.swift — runtime exemption list of SOURCE-app bundle IDs
// whose flows the NE proxy passes through to native routing WITHOUT a TLS
// bump (no daemon bridge, no inspection).
//
// Single source of truth: the agent daemon writes
// `/var/run/nexus-agent/bypass-bundles.json` whenever the Hub-pushed
// `agent_settings.bypassBundles` shadow value changes (or on agent boot).
// The file format is the literal JSON array of strings:
//   ["com.anthropic.claude-code", ...]
//
// Why this exists: a trusted developer tool that PINS its TLS (e.g. the
// local claude-code CLI talking to api.anthropic.com) breaks under bump —
// the bump cert is rejected and the tool's connection fails. Exempting it
// by SOURCE bundle keeps it working while still inspecting the SAME host
// reached from any OTHER app, so destination visibility is preserved.
//
// Why file-bridged (not IPC), why a 60s refresh, why NO hardcoded entry:
// identical rationale to QUICFallbackBundles — handleNewFlow decides
// synchronously per-flow (a blocking IPC roundtrip would tank latency),
// and the list is admin-controlled config (a hardcoded entry would
// silently override admin intent forever). Empty / missing file = exempt
// nothing (inspect everything), the safe default: it is better to
// under-exempt for a few boot seconds than to silently stop inspecting an
// app against admin policy.

import Foundation
import os.log

final class BypassBundles {
    private let logger = Logger(subsystem: "com.nexus-gateway.agent", category: "BypassBundles")
    private let filePath = "/var/run/nexus-agent/bypass-bundles.json"
    private let refreshInterval: TimeInterval = 60.0

    private let lock = NSLock()
    private var cache: Set<String> = []
    private var lastLoad: Date = .distantPast

    /// Returns true iff the given source bundleId is on the current
    /// exemption list. Empty bundleId (unsigned binaries) always returns
    /// false — an unsigned binary can never be deliberately exempted, so it
    /// stays on the inspection path. Triggers a lazy refresh when the cache
    /// is older than refreshInterval.
    ///
    /// Matches by exact bundleId OR helper/child prefix ("<parent>.<…>"),
    /// mirroring QUICFallbackBundles: macOS reports the EXECUTING process
    /// bundle, so exempting `com.github.Electron.helper` should also cover
    /// `com.github.Electron.helper.Renderer` etc.
    func shouldBypass(bundleId: String) -> Bool {
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

    /// Force a reload from disk now. Called on first access and from the
    /// lazy refresh path; safe to call from any thread.
    func reload() {
        let url = URL(fileURLWithPath: filePath)
        guard let data = try? Data(contentsOf: url) else {
            // File missing or unreadable → empty exemption list (fail-safe:
            // inspect everything). Debug because pre-Hub-sync this is the
            // expected steady state.
            lock.lock()
            let was = cache.count
            cache = []
            lastLoad = Date()
            lock.unlock()
            logger.debug("BypassBundles: \(self.filePath, privacy: .public) absent or unreadable; exemption list now empty (was=\(was))")
            return
        }
        guard let arr = try? JSONDecoder().decode([String].self, from: data) else {
            logger.error("BypassBundles: failed to decode JSON array from \(self.filePath, privacy: .public) (\(data.count) bytes); leaving previous exemption list in place")
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
            logger.info("BypassBundles: exemption list updated count=\(next.count) bundles=\(arr.joined(separator: ","), privacy: .public)")
        }
    }

    private func refreshIfStale() {
        lock.lock()
        let stale = Date().timeIntervalSince(lastLoad) > refreshInterval
        lock.unlock()
        if stale { reload() }
    }
}
