// IPCProtocol.swift — JSON-over-Unix-socket protocol for NE ↔ Go Agent IPC.
//
// The Network Extension sends flow metadata to the Go agent via a Unix domain
// socket. The agent responds with interception decisions.
//
// IMPORTANT — IPC transport choice: this client uses Apple's Network framework
// (`NWConnection(to: .unix(path:), using: .tcp)`) instead of POSIX
// `socket()` / `connect()`. macOS system extensions run inside a sandbox that
// silently denies the POSIX BSD socket syscalls even when
// `com.apple.security.app-sandbox = false` is set in the entitlements; the
// `connect()` call returns -1 with no error reaching the extension's logs and
// no audit event in /var/log. NWConnection is the supported high-level API
// inside the system extension container and is allowed through. Reference:
// mitmproxy/mitmproxy_rs `mitmproxy-macos/redirector/network-extension/
// TransparentProxyProvider.swift` uses the same pattern. This entire
// AgentIPCClient was rewritten 2026-05-15 after weeks of "Network filter not
// connected" stuck state caused by silently failing POSIX connect().

import Foundation
import Network
import os.log

// MARK: - Wire Types

/// Message sent from the NE to the Go agent when a new flow is intercepted.
struct NEFlowNewMessage: Encodable {
    let flowId: String
    let remoteHost: String
    let remoteIp: String
    let remotePort: Int
    let localPort: Int
    let pid: Int32
    let transportProtocol: String

    enum CodingKeys: String, CodingKey {
        case type_ = "type"
        case flowId, remoteHost, remoteIp, remotePort, localPort, pid
        case transportProtocol = "protocol"
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode("flow_new", forKey: .type_)
        try container.encode(flowId, forKey: .flowId)
        try container.encode(remoteHost, forKey: .remoteHost)
        try container.encode(remoteIp, forKey: .remoteIp)
        try container.encode(remotePort, forKey: .remotePort)
        try container.encode(localPort, forKey: .localPort)
        try container.encode(pid, forKey: .pid)
        try container.encode(transportProtocol, forKey: .transportProtocol)
    }
}

/// Message sent from the NE when a flow closes.
///
/// E50 latency phase fields (`upstreamTtfbMs`, `upstreamTotalMs`,
/// `interceptMs`) are optional; the Swift NE proxy populates them when
/// the upstream call surfaces `URLSessionTaskMetrics` data (most flows
/// do — Apple's NEAppProxyTCPFlow + URLSession path emits the metric
/// pairs around request send / first response byte / last byte). When
/// absent the field is omitted from the JSON envelope and the Go side
/// leaves the corresponding `traffic_event` column NULL. See
/// `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md` for the cross-language contract.
struct NEFlowClosedMessage: Encodable {
    let flowId: String
    let bytesIn: Int64
    let bytesOut: Int64
    let durationMs: Int
    let bumpStatus: String

    // E50 phase fields — nil means "not measured this flow". URLSession
    // metrics are best-effort: passthrough flows that go directly via
    // raw TCP relay (no URLSessionTask wrapping) report nil for all 3.
    let upstreamTtfbMs: Int?
    let upstreamTotalMs: Int?
    let interceptMs: Int?

    enum CodingKeys: String, CodingKey {
        case type_ = "type"
        case flowId, bytesIn, bytesOut, durationMs, bumpStatus
        case upstreamTtfbMs, upstreamTotalMs, interceptMs
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode("flow_closed", forKey: .type_)
        try container.encode(flowId, forKey: .flowId)
        try container.encode(bytesIn, forKey: .bytesIn)
        try container.encode(bytesOut, forKey: .bytesOut)
        try container.encode(durationMs, forKey: .durationMs)
        try container.encode(bumpStatus, forKey: .bumpStatus)
        // Omit nil phase fields — Go side treats absent + null identically.
        if let v = upstreamTtfbMs { try container.encode(v, forKey: .upstreamTtfbMs) }
        if let v = upstreamTotalMs { try container.encode(v, forKey: .upstreamTotalMs) }
        if let v = interceptMs { try container.encode(v, forKey: .interceptMs) }
    }
}

/// Decision response from the Go agent.
struct NEDecisionResponse: Codable {
    let flowId: String
    let decision: String // "inspect" | "passthrough" | "deny"
}

/// One-shot upgrade message sent after we extract the SNI hostname
/// from the flow's first TLS ClientHello frame. The daemon updates
/// the in-flight flowState's destination host so the audit row
/// (written on flow_closed) carries the real hostname instead of
/// the IP literal that NEAppProxyFlow.remoteHostname surfaced
/// (typically nil for browsers / Electron / anything that pre-
/// resolves DNS itself).
struct NEFlowHostUpdateMessage: Encodable {
    let flowId: String
    let hostname: String

    enum CodingKeys: String, CodingKey {
        case type_ = "type"
        case flowId, hostname
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode("flow_update_host", forKey: .type_)
        try container.encode(flowId, forKey: .flowId)
        try container.encode(hostname, forKey: .hostname)
    }
}

// MARK: - IPC Client

/// Communicates with the Go agent over a Unix domain socket via Apple
/// Network framework's `NWConnection` (NOT POSIX socket — see file header
/// comment for why).
class AgentIPCClient {
    private var connection: NWConnection?
    private let socketPath: String
    private let queue = DispatchQueue(label: "com.nexus-gateway.agent.ipc", qos: .userInitiated)
    private var responseBuffer = Data()
    private let bufferLock = NSLock()
    private let pendingLock = NSLock()
    private let logger = Logger(subsystem: "com.nexus-gateway.agent", category: "IPC")

    /// Pending decision callbacks keyed by flowId.
    private var pending: [String: (NEDecisionResponse) -> Void] = [:]

    init() {
        let home = FileManager.default.homeDirectoryForCurrentUser.path
        if getuid() == 0 {
            socketPath = "/var/run/nexus-agent/ne.sock"
        } else {
            socketPath = "\(home)/.nexus/ne.sock"
        }
    }

    /// Connect to the Go agent's IPC socket. Blocks the caller until
    /// either the NWConnection reaches `.ready` or a 25 s total budget
    /// is exhausted. Each individual attempt is capped at 3 s; failed
    /// attempts cancel + recreate the NWConnection and sleep 1 s before
    /// retrying.
    ///
    /// Why retry: when macOS spawns the system extension the daemon may
    /// not yet have bound /var/run/nexus-agent/ne.sock — we observed a
    /// ~2.5 s race in production where the provider started BEFORE the
    /// daemon. NWConnection's built-in retry-on-`.waiting` behaviour
    /// does NOT reliably re-attempt for unix-socket ENOENT (only for
    /// network-path changes), so we must manually cancel and re-issue
    /// the connection in a retry loop until the socket appears.
    func connect() -> Bool {
        // E55-T19: 5-minute total budget with exponential backoff.
        //
        // Was 25 s in the original implementation, which assumed the
        // daemon was already up when macOS spawned the extension. In
        // practice runPendingEnrollment defers IPC socket creation
        // until the user finishes SSO (browser flow + 2FA + IdP
        // approval), commonly 30-180 s. The previous 25 s budget
        // exhausted before the daemon socket existed; the extension
        // gave up and the menu bar stuck on "Network filter not
        // connected" forever (until manual toggle off / on).
        //
        // T18 (daemon side) now stands up the IPC socket immediately
        // in pending-enrollment mode, so the typical case is a
        // sub-second connect. The extended budget here is defence in
        // depth: covers a slow LaunchDaemon respawn, a socket churn
        // during install/upgrade, or a Hub-side enrollment delay
        // that holds runPendingEnrollment past the old 25 s mark.
        //
        // Backoff: 50ms → 100ms → 200ms → 400ms → 800ms → 1.6s →
        // capped at 1s. Tight initial spacing keeps menu-bar latency
        // sub-second on the happy path; the cap stops a stuck daemon
        // from burning CPU on a 10 ms-poll loop for 5 minutes.
        let totalDeadline = Date().addingTimeInterval(300.0)
        var attempt = 0
        var backoffMs: Int = 50
        while Date() < totalDeadline {
            attempt += 1
            let remaining = totalDeadline.timeIntervalSince(Date())
            let attemptTimeout = min(3.0, remaining)
            // Lower initial verbosity so 5-min budget doesn't drown the log
            // on a slow SSO flow; INFO every 10 attempts + on the very
            // first / very last attempt is enough for ops to follow.
            if attempt == 1 || attempt % 10 == 0 {
                logger.info("connect: attempt #\(attempt) (remaining budget=\(String(format: "%.1f", remaining))s, attempt cap=\(String(format: "%.1f", attemptTimeout))s) — opening NWConnection to unix socket path=\(self.socketPath, privacy: .public)")
            }
            if attemptConnect(timeout: attemptTimeout) {
                logger.info("connect: succeeded on attempt #\(attempt) after \(Int(300.0 - remaining))s")
                return true
            }
            // Sleep before next attempt; daemon may still be coming up.
            // Skip the sleep on the final iteration to avoid wasted time.
            let sleepSec = Double(backoffMs) / 1000.0
            if Date().addingTimeInterval(sleepSec) < totalDeadline {
                Thread.sleep(forTimeInterval: sleepSec)
            }
            // Exponential backoff capped at 1 s.
            if backoffMs < 1000 {
                backoffMs = min(backoffMs * 2, 1000)
            }
        }
        logger.error("connect: GAVE UP after \(attempt) attempts spanning ~300 s. The daemon socket at \(self.socketPath, privacy: .public) never became reachable. Most likely the LaunchDaemon failed to start — check `launchctl print system/com.nexus-gateway.agent` and `sudo tail /Library/Logs/com.nexus-gateway.agent/agent.log`.")
        return false
    }

    /// Single connection attempt. Caller is responsible for retry / total-budget logic.
    private func attemptConnect(timeout: TimeInterval) -> Bool {
        let t0 = Date()

        let conn = NWConnection(
            to: .unix(path: socketPath),
            using: .tcp
        )
        self.connection = conn

        let semaphore = DispatchSemaphore(value: 0)
        var settled = false
        var connected = false

        conn.stateUpdateHandler = { [weak self] state in
            guard let self else { return }
            self.logger.info("attemptConnect: NWConnection state → \(String(describing: state), privacy: .public)")
            switch state {
            case .ready:
                if !settled {
                    settled = true
                    connected = true
                    semaphore.signal()
                }
            case .failed(let err):
                self.logger.error("attemptConnect: NWConnection FAILED — \(String(describing: err), privacy: .public). Common causes: socket path mismatch, sandbox denial.")
                if !settled {
                    settled = true
                    semaphore.signal()
                }
            case .cancelled:
                if !settled {
                    settled = true
                    semaphore.signal()
                }
            case .waiting(let err):
                // .waiting on a unix socket usually means ENOENT (file
                // does not exist). NWConnection does not reliably
                // re-attempt for unix-socket ENOENT, so we treat
                // .waiting as a failed attempt and let the outer
                // connect() loop tear down + re-create.
                self.logger.error("attemptConnect: NWConnection waiting — \(String(describing: err), privacy: .public). Treating as failed attempt; outer loop will retry.")
                if !settled {
                    settled = true
                    semaphore.signal()
                }
            default:
                break
            }
        }

        conn.start(queue: queue)

        let waitResult = semaphore.wait(timeout: .now() + timeout)
        if waitResult == .timedOut {
            logger.error("attemptConnect: timed out after \(String(format: "%.1f", timeout))s (final state=\(String(describing: conn.state), privacy: .public)); cancelling")
            conn.cancel()
            self.connection = nil
            return false
        }
        if !connected {
            // We already cancelled implicitly via the .failed/.waiting/.cancelled paths above.
            conn.cancel()
            self.connection = nil
            return false
        }

        logger.info("attemptConnect: NWConnection ready in \(Int(Date().timeIntervalSince(t0) * 1000))ms; arming receive loop")
        receiveNext()
        return true
    }

    /// Disconnect from the agent. Pending decision callbacks are fired
    /// with a synthetic **`passthrough`** response — NOT `deny`. This is
    /// the fail-open contract (CLAUDE.md binding: "macOS NE proxy must
    /// fail-open, never fail-closed"). Returning `deny` here causes
    /// every in-flight user TCP flow to be rejected by the extension
    /// the moment daemon IPC drops, manifesting as a wholesale network
    /// outage where only flows that already entered the inspect-bridge
    /// path keep working. Symptom in the wild: "I started the agent
    /// and only interception_domain hosts are reachable; everything
    /// else is broken." With `passthrough`, the flow proceeds without
    /// inspection — exactly the same outcome as the 2 s requestDecision
    /// timeout fallback below.
    func disconnect() {
        logger.info("disconnect: cancelling NWConnection")
        connection?.cancel()
        connection = nil

        // Drain pending callbacks with a fail-open passthrough so any
        // in-flight user flow continues uninspected rather than being
        // rejected.
        pendingLock.lock()
        let stale = pending
        pending.removeAll()
        pendingLock.unlock()

        if !stale.isEmpty {
            logger.info("disconnect: draining \(stale.count) pending decision callback(s) with synthetic passthrough (fail-open)")
        }
        for (flowId, callback) in stale {
            callback(NEDecisionResponse(flowId: flowId, decision: "passthrough"))
        }
    }

    /// Request a decision for a new flow (async callback).
    ///
    /// SAFETY: bounded by a 2 s timeout. If the daemon does not respond
    /// in time, we fire a synthetic `passthrough` decision so the flow
    /// proceeds without inspection. The alternative — letting the flow
    /// hang indefinitely waiting on a dead daemon — manifests to the
    /// user as a frozen application (curl spins, browser shows "waiting
    /// for ..."). We choose to fail-open: silent passthrough is better
    /// than appearing to break the network. Audit row will not be
    /// recorded for the timed-out flow because flow_closed never
    /// reaches the daemon either; that is the correct semantics — if
    /// the daemon is not reachable, we cannot persist anyway.
    func requestDecision(flowId: String, host: String, ip: String, port: Int, localPort: Int, pid: Int32, completion: @escaping (NEDecisionResponse) -> Void) {
        pendingLock.lock()
        pending[flowId] = completion
        pendingLock.unlock()

        let msg = NEFlowNewMessage(
            flowId: flowId,
            remoteHost: host,
            remoteIp: ip,
            remotePort: port,
            localPort: localPort,
            pid: pid,
            transportProtocol: "tcp"
        )
        sendJSON(msg)

        queue.asyncAfter(deadline: .now() + 2.0) { [weak self] in
            guard let self else { return }
            self.pendingLock.lock()
            let stillPending = self.pending.removeValue(forKey: flowId)
            self.pendingLock.unlock()
            if let cb = stillPending {
                self.logger.error("requestDecision: 2 s TIMEOUT for flow=\(flowId, privacy: .public) host=\(host, privacy: .public):\(port) — daemon did not respond, defaulting to passthrough (fail-open). Check daemon health: `sudo tail /Library/Logs/com.nexus-gateway.agent/agent.log` and `sudo lsof /var/run/nexus-agent/ne.sock`.")
                cb(NEDecisionResponse(flowId: flowId, decision: "passthrough"))
            }
        }
    }

    /// Tell the daemon a real hostname for an in-flight flow (parsed
    /// from TLS SNI on first packet). Best-effort — drops silently
    /// when the connection is down rather than blocking the relay.
    func notifyFlowHost(flowId: String, hostname: String) {
        let trimmed = hostname.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return }
        sendJSON(NEFlowHostUpdateMessage(flowId: flowId, hostname: trimmed))
    }

    /// Notify the agent that a flow has closed. E50 phase fields are
    /// optional — pass nil when URLSessionTaskMetrics weren't available
    /// (passthrough flows that bypass URLSession entirely).
    func notifyFlowClosed(
        flowId: String,
        bytesIn: Int64,
        bytesOut: Int64,
        durationMs: Int,
        bumpStatus: String,
        upstreamTtfbMs: Int? = nil,
        upstreamTotalMs: Int? = nil,
        interceptMs: Int? = nil
    ) {
        let msg = NEFlowClosedMessage(
            flowId: flowId,
            bytesIn: bytesIn,
            bytesOut: bytesOut,
            durationMs: durationMs,
            bumpStatus: bumpStatus,
            upstreamTtfbMs: upstreamTtfbMs,
            upstreamTotalMs: upstreamTotalMs,
            interceptMs: interceptMs
        )
        sendJSON(msg)
    }

    // MARK: - Internal

    private func sendJSON<T: Encodable>(_ value: T) {
        guard let conn = connection else {
            logger.error("sendJSON: no connection (was disconnect() called?)")
            return
        }
        do {
            var data = try JSONEncoder().encode(value)
            data.append(0x0A) // newline delimiter
            conn.send(content: data, completion: .contentProcessed { [weak self] err in
                if let err = err {
                    self?.logger.error("sendJSON: send FAILED — \(String(describing: err), privacy: .public). Frame size=\(data.count) bytes")
                }
            })
        } catch {
            logger.error("sendJSON: JSONEncoder encode error: \(String(describing: error), privacy: .public)")
        }
    }

    /// Pull bytes from NWConnection in a continuous loop, dispatching
    /// newline-delimited JSON frames to pending callbacks. Re-arms itself
    /// after each chunk by recursively calling NWConnection.receive.
    /// Stops on error / peer-close / cancellation.
    private func receiveNext() {
        guard let conn = connection else { return }
        conn.receive(minimumIncompleteLength: 1, maximumLength: 65536) { [weak self] data, _, isComplete, err in
            guard let self else { return }
            if let err = err {
                self.logger.error("receiveNext: receive error — \(String(describing: err), privacy: .public). Tearing down connection.")
                self.disconnect()
                return
            }
            if let data = data, !data.isEmpty {
                self.bufferLock.lock()
                self.responseBuffer.append(data)
                self.bufferLock.unlock()
                self.processBuffer()
            }
            if isComplete {
                self.logger.info("receiveNext: peer closed (isComplete=true). Tearing down.")
                self.disconnect()
                return
            }
            self.receiveNext()
        }
    }

    private func processBuffer() {
        while true {
            bufferLock.lock()
            guard let newlineIndex = responseBuffer.firstIndex(of: 0x0A) else {
                bufferLock.unlock()
                break
            }
            let lineData = Data(responseBuffer[responseBuffer.startIndex..<newlineIndex])
            responseBuffer.removeSubrange(responseBuffer.startIndex...newlineIndex)
            bufferLock.unlock()

            guard let resp = try? JSONDecoder().decode(NEDecisionResponse.self, from: lineData) else {
                logger.error("processBuffer: failed to decode NEDecisionResponse from \(lineData.count) bytes — dropping frame")
                continue
            }

            pendingLock.lock()
            let callback = pending.removeValue(forKey: resp.flowId)
            pendingLock.unlock()

            if callback == nil {
                logger.debug("processBuffer: decision for unknown flowId=\(resp.flowId, privacy: .public) — likely race with disconnect or duplicate response")
            }
            callback?(resp)
        }
    }
}
