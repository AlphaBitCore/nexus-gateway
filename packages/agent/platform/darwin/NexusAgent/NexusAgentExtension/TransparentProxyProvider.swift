// TransparentProxyProvider.swift — NETransparentProxyProvider implementation
// for macOS traffic interception (E17 Phase 3A).
//
// Intercepts TCP flows from managed applications, queries the Go agent for
// interception decisions via Unix socket IPC, and routes flows accordingly.

import NetworkExtension
import os.log

class NexusProxyProvider: NETransparentProxyProvider {

    private let logger = Logger(subsystem: "com.nexus-gateway.agent", category: "ProxyProvider")
    private let ipcClient = AgentIPCClient()
    private let quicBundles = QUICFallbackBundles()
    private let daemonPIDFilter = DaemonPIDFilter()

    /// macOS system network services whose UDP must NEVER be claimed by
    /// our proxy, regardless of destination port. handleNewFlow returns
    /// false early when bundleId matches, before any QUIC kill logic.
    /// Belt-and-suspenders with the rule-level excludedNetworkRules in
    /// startProxy() — the rule layer catches well-known UDP ports, this
    /// bundle list catches the long tail (e.g. mDNSResponder doing
    /// DNS-over-HTTPS to UDP 443, apsd's varying Apple Push ports,
    /// configd's misc discovery probes). Hard-coded because macOS's
    /// system service bundle ids are stable across releases; new
    /// system services would be a CLAUDE.md-binding-level event
    /// requiring explicit update of this list. Adding to this list
    /// requires triple-checking the bundle is genuinely macOS-system
    /// (security risk: a user app on this list is invisible to us).
    private let systemNetworkServiceBundles: Set<String> = [
        "com.apple.mDNSResponder",
        "com.apple.configd",
        "com.apple.dhcpcd",
        "com.apple.apsd",
        "com.apple.nsurlsessiond",
        "com.apple.kdc",
        "com.apple.timed",
        "com.apple.locationd",
        "com.apple.bootpd",
        "com.apple.symptomsd",
        "ntpd",
        "mdnsresponder",
        "launchd",
    ]

    /// Serial dispatch queue used for time-bounded callbacks
    /// (peekSNIThenRelay timeout, future watchdogs). Separate from
    /// AgentIPCClient's queue so an IPC hang can't starve our timeouts.
    private let queue = DispatchQueue(label: "com.nexus-gateway.agent.proxy", qos: .userInitiated)

    /// Active flow tracking for byte counting and duration.
    private var activeFlows: [String: FlowState] = [:]
    private let flowLock = NSLock()

    struct FlowState {
        let flow: NEAppProxyFlow
        let startTime: Date
        var bytesIn: Int64 = 0
        var bytesOut: Int64 = 0
        var bumpStatus: String = ""
        // E50 phase fields populated by URLSessionTaskMetricsHandler when
        // an upstream URLSession task completes. Optional because not
        // every flow goes through URLSession (raw TCP relay does not).
        var upstreamTtfbMs: Int? = nil
        var upstreamTotalMs: Int? = nil
        // interceptMs = elapsed from kernel NEAppProxyFlow.open() to the
        // start of our handler. Captured at flow registration time.
        var interceptMs: Int? = nil
    }

    // MARK: - Lifecycle

    override func startProxy(options: [String: Any]?, completionHandler: @escaping (Error?) -> Void) {
        let t0 = Date()
        let optionsDesc = options.map { String(describing: $0) } ?? "<nil>"
        logger.info("startProxy: ENTERED — pid=\(ProcessInfo.processInfo.processIdentifier) bundleId=\(Bundle.main.bundleIdentifier ?? "<nil>") options=\(optionsDesc)")

        // Connect to the Go agent's IPC socket
        logger.info("startProxy: connecting to daemon IPC socket via AgentIPCClient")
        guard ipcClient.connect() else {
            logger.error("startProxy: AgentIPCClient.connect() returned false — daemon socket /var/run/nexus-agent/ne.sock unreachable. Check: (1) is the LaunchDaemon running? `launchctl print system/com.nexus-gateway.agent`. (2) does the socket exist? `sudo lsof /var/run/nexus-agent/ne.sock`. (3) sandbox/entitlement: extension's app-sandbox is false and network-server is true.")
            completionHandler(NSError(domain: "NexusAgent", code: 1,
                userInfo: [NSLocalizedDescriptionKey: "Cannot connect to agent IPC socket"]))
            return
        }
        logger.info("startProxy: AgentIPCClient.connect() ok in \(Int(Date().timeIntervalSince(t0) * 1000))ms")

        // Configure network settings: intercept ALL outbound flows
        // (TCP + UDP). UDP intercept exists for one purpose: kill QUIC
        // connections (h3 over UDP 443) early so Chrome / Edge fall
        // back to HTTP/2 over TCP, which we can SNI-parse + run through
        // hook pipeline. This mirrors mitmproxy's `.any` rule (their
        // macos-redirector at network-extension/TransparentProxyProvider.swift)
        // — they intercept UDP precisely so their TCP path actually
        // sees real browser AI traffic. Without this, Chrome → ChatGPT
        // is invisible because Cloudflare-fronted services prefer h3.
        let settings = NETransparentProxyNetworkSettings(tunnelRemoteAddress: "127.0.0.1")

        let anyRule = NENetworkRule(
            remoteNetwork: nil,
            remotePrefix: 0,
            localNetwork: nil,
            localPrefix: 0,
            protocol: .any,
            direction: .outbound
        )
        settings.includedNetworkRules = [anyRule]

        // Layer 1 architecture: catch-all .any (above) captures all TCP +
        // all UDP. Layer 2 (below): excludedNetworkRules tells macOS NECP
        // to NEVER route the following critical-system UDP ports through
        // our proxy, even when the catch-all includedRule would otherwise
        // match. Without this exclusion list, mDNSResponder /
        // configd / dhcpcd / apsd / ntpd outbound UDP packets all enter
        // handleNewFlow; our code does `return false` for non-browser
        // bundles, but the CLAUDE.md binding warns this return-false
        // behaviour under `.any` includedRule is NOT a guaranteed
        // fall-back-to-native — macOS may drop the flow instead of
        // routing natively, breaking DNS / DHCP / mDNS / NTP / Apple
        // Push. Live RCA 2026-05-24: user reported "page froze" + curl
        // getaddrinfo timeout because mDNSResponder UDP/53 queries
        // were being silently dropped. excludedNetworkRules is the only
        // OS-level mechanism to guarantee these critical packets never
        // reach our process. Browser h3 (UDP/443) is NOT excluded — it
        // still enters handleNewFlow and gets QUIC-killed there so
        // Chrome / Edge fall back to HTTP/2 over TCP / 443 (captured by
        // the TCP catch-all path). See memory:
        // feedback_macos_mdns_flush_after_ne_state_change.
        let criticalUDPPorts = [
            "53",    // DNS
            "5353",  // mDNS
            "67",    // DHCP server (boot)
            "68",    // DHCP client (boot)
            "123",   // NTP
            "500",   // IKE
            "4500",  // IKE NAT-T
            "1900",  // SSDP / UPnP discovery
            "5355",  // LLMNR (Windows compat, harmless on macOS)
        ]
        var excludedRules: [NENetworkRule] = []
        for port in criticalUDPPorts {
            // Two rules per port covering IPv4 + IPv6 catch-all hosts.
            // Prefix=0 + hostname="0.0.0.0"/"::"  = match any host on
            // that protocol+port. macOS NENetworkRule's port matching
            // for UDP is documented in NetworkExtension headers; it
            // should respect the port field on the remoteNetwork
            // endpoint. If empirical testing shows NECP ignores the
            // port for UDP rules, the handleNewFlow process-bundle
            // check below acts as a Layer-2 belt-and-suspenders.
            excludedRules.append(NENetworkRule(
                remoteNetwork: NWHostEndpoint(hostname: "0.0.0.0", port: port),
                remotePrefix: 0,
                localNetwork: nil,
                localPrefix: 0,
                protocol: .UDP,
                direction: .outbound
            ))
            excludedRules.append(NENetworkRule(
                remoteNetwork: NWHostEndpoint(hostname: "::", port: port),
                remotePrefix: 0,
                localNetwork: nil,
                localPrefix: 0,
                protocol: .UDP,
                direction: .outbound
            ))
        }
        settings.excludedNetworkRules = excludedRules
        logger.info("startProxy: built NETransparentProxyNetworkSettings (tunnelRemoteAddress=127.0.0.1, includedRules=1 .any catch-all, excludedRules=\(excludedRules.count) critical-UDP-port rules: \(criticalUDPPorts.joined(separator: ","))); calling setTunnelNetworkSettings")

        setTunnelNetworkSettings(settings) { [weak self] error in
            let totalMs = Int(Date().timeIntervalSince(t0) * 1000)
            if let error = error {
                let nsErr = error as NSError
                self?.logger.error("startProxy: setTunnelNetworkSettings FAILED in \(totalMs)ms: domain=\(nsErr.domain) code=\(nsErr.code) localized=\(error.localizedDescription)")
                completionHandler(error)
                return
            }
            self?.logger.info("startProxy: setTunnelNetworkSettings ok in \(totalMs)ms — proxy is now ACTIVE; handleNewFlow will fire for every outbound TCP flow from monitored apps")
            completionHandler(nil)
        }
    }

    override func stopProxy(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        // Map reason rawValue to a name. Apple's enum:
        // .none=0, .userInitiated=1, .providerFailed=2, .noNetworkAvailable=3,
        // .unrecoverableNetworkChange=4, .providerDisabled=5, .authenticationCanceled=6,
        // .configurationFailed=7, .idleTimeout=8, .configurationDisabled=9,
        // .configurationRemoved=10, .superseded=11, .userLogout=12, .userSwitch=13,
        // .connectionFailed=14, .sleep=15, .appUpdate=16.
        // .internalError=17 is macOS 14.2+ only; Package.swift pins .v13
        // deployment target, so raw 17 lands in `@unknown default`.
        let reasonName: String
        switch reason {
        case .none: reasonName = "none(0)"
        case .userInitiated: reasonName = "userInitiated(1)"
        case .providerFailed: reasonName = "providerFailed(2)"
        case .noNetworkAvailable: reasonName = "noNetworkAvailable(3)"
        case .unrecoverableNetworkChange: reasonName = "unrecoverableNetworkChange(4)"
        case .providerDisabled: reasonName = "providerDisabled(5)"
        case .authenticationCanceled: reasonName = "authenticationCanceled(6)"
        case .configurationFailed: reasonName = "configurationFailed(7)"
        case .idleTimeout: reasonName = "idleTimeout(8)"
        case .configurationDisabled: reasonName = "configurationDisabled(9)"
        case .configurationRemoved: reasonName = "configurationRemoved(10)"
        case .superceded: reasonName = "superceded(11)" // Apple's typo, preserved in framework
        case .userLogout: reasonName = "userLogout(12)"
        case .userSwitch: reasonName = "userSwitch(13)"
        case .connectionFailed: reasonName = "connectionFailed(14)"
        case .sleep: reasonName = "sleep(15)"
        case .appUpdate: reasonName = "appUpdate(16)"
        // `default` (not `@unknown default`) so the switch tolerates
        // SDK-visible cases that are availability-gated above the
        // deployment target — e.g. `.internalError` (raw 17) is macOS
        // 14.2+ only and Package.swift pins .v13.
        default: reasonName = "unknown(\(reason.rawValue))"
        }
        flowLock.lock()
        let activeCount = activeFlows.count
        let flows = activeFlows
        activeFlows.removeAll()
        flowLock.unlock()

        logger.info("stopProxy: ENTERED — reason=\(reasonName) activeFlows=\(activeCount) (will report flow_closed for each, then disconnect IPC)")

        for (flowId, state) in flows {
            reportFlowClosed(flowId: flowId, state: state)
        }

        ipcClient.disconnect()
        logger.info("stopProxy: complete (ipcClient disconnected)")
        completionHandler()
    }

    // MARK: - Flow Handling

    override func handleNewFlow(_ flow: NEAppProxyFlow) -> Bool {
        // SAFETY: this method MUST return true/false synchronously and
        // MUST NOT throw — any uncaught Swift error here causes macOS
        // to drop the flow without routing natively, which appears as
        // a network outage to the user. The whole body is wrapped so
        // unexpected nil / cast failures fall through to "decline +
        // let macOS route natively" rather than crashing the provider.
        let bundleId = flow.metaData.sourceAppSigningIdentifier

        // E55 self-intercept guard: NE intercepts ALL outbound traffic
        // including the agent daemon's own connections.
        //
        // 2026-05-23: experiment with claim+direct-relay (openAndRelay)
        // failed — NE's createTCPConnection in self-intercept context
        // returns "read: errno 403" during TLS handshake, breaking
        // both daemon's bootstrap to Hub AND tlsbump's upstream
        // forward (browser → chatgpt.com → bridge → daemon HTTP
        // client → claim+relay → upstream fail → 502 to browser).
        // Reverted to `return false` as the least-bad option.
        //
        // Known consequence: with includedNetworkRules.protocol=.any,
        // NECP routes daemon's outbound to the proxy; `return false`
        // drops the flow. Daemon's HTTP client will retry, and an
        // early-boot race window sometimes lets a request through
        // before NE fully claims the daemon PID (this is how the
        // existing WebSocket gets established + reconnects).
        //
        // Real fix (TODO, task #56): use NETransparentProxyNetwork-
        // Settings.excludedNetworkRules to tell NECP not to route
        // daemon-bound traffic to the proxy at the IP level. Needs
        // daemon to DNS-resolve Hub + key upstreams at startup and
        // push the IP list to the extension via ne.sock.
        let sourcePid = extractPID(from: flow)
        if daemonPIDFilter.isDaemon(pid: sourcePid) {
            logger.debug("handleNewFlow: skipping self-intercept from daemon PID \(sourcePid)")
            return false
        }

        // UDP path: only kill UDP flows from known QUIC-capable clients
        // (Chrome / Edge / Safari / Firefox / Arc / Vivaldi / Brave +
        // Electron AI desktop apps). For anything else — DNS, DHCP,
        // mDNS, NTP, WireGuard, FaceTime, games, custom UDP services —
        // we DECLINE the flow so macOS routes it natively and our
        // proxy never touches it. Without this guard, a process hang
        // anywhere in our code path (IPC stuck, peek timeout, panic)
        // takes down the entire host's UDP stack including DNS, which
        // looks like a complete network outage to the user.
        // See incident 2026-05-15: shipping `.any` rule + blanket
        // close-all-UDP killed DNS/DHCP/mDNS and required manual
        // launchctl unload + plist deletion to recover.
        if let udpFlow = flow as? NEAppProxyUDPFlow {
            // Layer 2 defense: explicit fast-decline for known macOS
            // system network services. Even though startProxy() already
            // registered excludedNetworkRules for the standard
            // critical-UDP ports (53/5353/67/68/123/500/4500/1900/5355),
            // a system process can send UDP to non-standard ports too —
            // e.g. mDNSResponder for DNS-over-HTTPS (UDP/443 in some
            // configs), apsd for Apple Push (multiple varying ports
            // across macOS versions). Bundle-id check here ensures the
            // flow is never claimed regardless of port. Belt + suspenders
            // with the rule-level exclusion above.
            if systemNetworkServiceBundles.contains(bundleId) {
                logger.debug("handleNewFlow: decline UDP from system service \(bundleId, privacy: .public) — bundle in systemNetworkServiceBundles fast-decline list")
                return false
            }
            if quicBundles.shouldForceFallback(bundleId: bundleId) {
                logger.info("handleNewFlow: kill UDP from \(bundleId, privacy: .public) → force QUIC→TCP fallback")
                udpFlow.closeReadWithError(NSError(domain: "NexusAgent", code: 1,
                    userInfo: [NSLocalizedDescriptionKey: "QUIC blocked; fall back to TCP"]))
                udpFlow.closeWriteWithError(nil)
                return true
            }
            // Not on the allowlist — bundleId may be empty (unsigned
            // binary), a system daemon, a game, or a custom UDP app.
            // Decline; macOS handles it natively. This is the
            // critical safety branch: never claim UDP we can't relay.
            return false
        }

        guard let tcpFlow = flow as? NEAppProxyTCPFlow else {
            // Neither TCP nor UDP — unknown flow class. Decline so
            // macOS routes natively.
            logger.debug("handleNewFlow: rejected — neither TCP nor UDP; flow class=\(String(describing: type(of: flow)))")
            return false
        }

        guard let endpoint = tcpFlow.remoteEndpoint as? NWHostEndpoint else {
            logger.debug("handleNewFlow: rejected — remoteEndpoint not NWHostEndpoint; type=\(String(describing: type(of: tcpFlow.remoteEndpoint)))")
            return false
        }

        let flowId = UUID().uuidString
        // endpoint.hostname is the literal connect target — for any caller
        // that pre-resolves DNS (curl, browsers, native getaddrinfo) it is
        // an IP STRING, not the original hostname. Apple exposes the
        // pre-resolved hostname via NEAppProxyFlow.remoteHostname. Prefer
        // that so our audit row stores `chatgpt.com` instead of
        // `172.64.155.209`. Fall back to the endpoint when remoteHostname
        // is nil (rare; happens when the caller skipped name resolution).
        let endpointAddr = endpoint.hostname
        let resolvedHost = (tcpFlow.remoteHostname?.isEmpty == false ? tcpFlow.remoteHostname : nil) ?? endpointAddr
        let host = resolvedHost
        let port = Int(endpoint.port) ?? 443

        // Resolve the source process PID from the flow's audit token
        let hasAuditToken = flow.metaData.sourceAppAuditToken != nil
        let pid = extractPID(from: flow)

        // IP captured separately. When remoteHostname differs from
        // endpointAddr (the common case), endpointAddr already holds the
        // dotted-quad / colon-hex IP string and no DNS round-trip is
        // needed. Only fall back to resolveIP() when both fields are
        // hostnames (rare).
        let ip: String
        if isLikelyIPLiteral(endpointAddr) {
            ip = endpointAddr
        } else {
            ip = resolveIP(host: host)
        }

        let localPort = extractLocalPort(from: flow)

        // INFO so every flow_new lands in default-level agent.log. The
        // bundleId / host / port triple is the fastest answer to "did
        // Chrome's traffic to chatgpt.com even reach the NE?". Without
        // this line at INFO, browsers' passthrough flows are completely
        // silent and the diagnostic dies at "I see nothing in agent.log".
        logger.info("handleNewFlow: ENTER flow=\(flowId, privacy: .public) bundleId=\(bundleId, privacy: .public) host=\(host, privacy: .public) ip=\(ip, privacy: .public) port=\(port) pid=\(pid) hasAuditToken=\(hasAuditToken) — opening flow + peeking SNI before requesting decision")

        // Track the flow
        flowLock.lock()
        activeFlows[flowId] = FlowState(flow: tcpFlow, startTime: Date())
        flowLock.unlock()

        // E55 / #79: peek the TLS ClientHello BEFORE asking the daemon
        // for a decision. Pre-E55 we sent `host` (which is the IP
        // literal for callers like Cursor/curl/Claude Desktop that
        // pre-resolve DNS) directly — daemon Engine.Evaluate would
        // never match an interception_domain pattern → passthrough →
        // user sees their inspect rules silently bypassed. Now we
        // peek for SNI first; the daemon receives the real hostname
        // and Engine sees the right target. The 500ms timeout means
        // server-speaks-first protocols (SSH, SMTP, plain HTTP after
        // redirect) fall through with the original (IP / remote-
        // hostname) host, which is the correct behaviour for those.
        peekSNIThenDecide(
            flowId: flowId,
            flow: tcpFlow,
            initialHost: host,
            ip: ip,
            port: port,
            localPort: localPort,
            pid: pid
        )

        return true // we are handling this flow
    }

    /// E55 / #79: peeks the TLS ClientHello of an inbound flow,
    /// extracts the SNI hostname, and only THEN asks the daemon for
    /// the inspect/passthrough/deny decision. Without this two-step
    /// pre-decision peek, callers that pre-resolve DNS (Cursor /
    /// Claude Desktop / curl / browsers) hand the daemon an IP
    /// literal — Engine.Evaluate matches no domain rule → passthrough
    /// → user's inspect rules silently bypassed.
    ///
    /// Lifecycle: open the NE flow → readData first chunk → on TLS
    /// ClientHello extract SNI → requestDecision with the resolved
    /// host → applyDecision drives the bridge / passthrough / deny
    /// path; the peeked bytes are forwarded onwards by the chosen
    /// path so we don't lose any handshake data.
    ///
    /// Fail-safe: 500ms timeout (server-speaks-first protocols never
    /// emit a ClientHello, e.g. SSH/SMTP/IMAP) — on timeout we just
    /// requestDecision with the original host (IP or remoteHostname).
    /// Same outcome as the pre-E55 path for non-TLS flows.
    private func peekSNIThenDecide(flowId: String,
                                   flow: NEAppProxyTCPFlow,
                                   initialHost: String,
                                   ip: String,
                                   port: Int,
                                   localPort: Int,
                                   pid: Int32) {
        flow.open(withLocalEndpoint: nil) { [weak self] err in
            guard let self else { return }
            if let err = err {
                let nsErr = err as NSError
                self.logger.error("peekSNIThenDecide: flow.open FAILED for flow=\(flowId, privacy: .public) host=\(initialHost, privacy: .public):\(port) — domain=\(nsErr.domain) code=\(nsErr.code)")
                // handleNewFlow already returned true; the OS has claimed this
                // flow on our behalf. Without an explicit close the browser
                // waits ~75 s for its SYN to time out — visible as a frozen
                // tab. Close the flow so the browser sees an immediate reset
                // and retries (where macOS routes it natively or the new
                // handleNewFlow call takes the same flow through a healthy
                // path). Reset is the fail-open shape for a claimed-then-
                // unusable flow; the only worse option is leaving it hung.
                flow.closeReadWithError(nsErr)
                flow.closeWriteWithError(nil)
                self.completeFlow(flowId: flowId)
                return
            }
            // Bound the SNI peek by 500ms so non-TLS protocols don't
            // block decision indefinitely.
            let timeoutFired = TimeoutGuard()
            self.queue.asyncAfter(deadline: .now() + .milliseconds(500)) { [weak self] in
                guard let self else { return }
                if timeoutFired.tryFire() {
                    self.logger.debug("peekSNIThenDecide: 500ms timeout for flow=\(flowId, privacy: .public) — falling back to initial host \(initialHost, privacy: .public)")
                    self.dispatchDecision(flowId: flowId, flow: flow, host: initialHost, peeked: nil, ip: ip, port: port, localPort: localPort, pid: pid)
                }
            }
            flow.readData { [weak self] data, readErr in
                guard let self else { return }
                guard timeoutFired.tryFire() else {
                    // Timeout already dispatched the decision; the
                    // peek result is a duplicate. Drop quietly.
                    return
                }
                if let readErr = readErr {
                    self.logger.debug("peekSNIThenDecide: first read error for flow=\(flowId, privacy: .public): \(readErr.localizedDescription) — dispatching with initial host")
                    self.dispatchDecision(flowId: flowId, flow: flow, host: initialHost, peeked: nil, ip: ip, port: port, localPort: localPort, pid: pid)
                    return
                }
                guard let data = data, !data.isEmpty else {
                    self.logger.debug("peekSNIThenDecide: empty first chunk for flow=\(flowId, privacy: .public); app closed before sending — completing")
                    self.completeFlow(flowId: flowId)
                    return
                }
                // SNI is best-effort. Use it when extracted; otherwise
                // keep initialHost.
                var resolvedHost = initialHost
                if let sni = SNIParser.extractSNI(from: data) {
                    self.logger.info("peekSNIThenDecide: SNI peeked sni=\(sni, privacy: .public) initial_host=\(initialHost, privacy: .public) for flow=\(flowId, privacy: .public) — using SNI as decision host")
                    resolvedHost = sni
                    // Also notify the daemon so the audit row's
                    // dst_host column carries the SNI immediately
                    // (decision and audit are independent paths).
                    self.ipcClient.notifyFlowHost(flowId: flowId, hostname: sni)
                }
                self.dispatchDecision(flowId: flowId, flow: flow, host: resolvedHost, peeked: data, ip: ip, port: port, localPort: localPort, pid: pid)
            }
        }
    }

    /// dispatchDecision fires the daemon decision request and routes
    /// the resulting decision into applyDecision. Called by
    /// peekSNIThenDecide after either SNI extraction or 500ms
    /// timeout. The optional peeked bytes are the TLS ClientHello we
    /// already pulled off the flow — applyDecision passes them to the
    /// bridge / passthrough handler so they get forwarded to the
    /// remote without being lost.
    private func dispatchDecision(flowId: String,
                                  flow: NEAppProxyTCPFlow,
                                  host: String,
                                  peeked: Data?,
                                  ip: String,
                                  port: Int,
                                  localPort: Int,
                                  pid: Int32) {
        let t0 = Date()
        ipcClient.requestDecision(flowId: flowId, host: host, ip: ip, port: port, localPort: localPort, pid: pid) { [weak self] response in
            let latencyMs = Int(Date().timeIntervalSince(t0) * 1000)
            self?.logger.info("dispatchDecision: response decision=\(response.decision, privacy: .public) latency_ms=\(latencyMs) flow=\(flowId, privacy: .public) host=\(host, privacy: .public):\(port) — handing off to applyDecisionAfterPeek")
            self?.applyDecisionAfterPeek(flowId: flowId, flow: flow, host: host, port: port, peeked: peeked, decision: response.decision)
        }
    }

    /// applyDecisionAfterPeek is applyDecision's counterpart for the
    /// post-SNI-peek code path. The differences from applyDecision:
    /// (a) the NE flow is already open (peekSNIThenDecide opened it),
    /// (b) the first chunk has already been read off the flow and is
    /// passed in via peeked — the chosen relay path must forward it
    /// to the remote first before resuming the normal read loop, or
    /// the upstream sees a TCP connection that never sent a
    /// ClientHello.
    private func applyDecisionAfterPeek(flowId: String,
                                        flow: NEAppProxyTCPFlow,
                                        host: String,
                                        port: Int,
                                        peeked: Data?,
                                        decision: String) {
        switch decision {
        case "deny":
            logger.info("applyDecisionAfterPeek: DENY flow=\(flowId, privacy: .public) → \(host, privacy: .public):\(port)")
            updateBumpStatus(flowId: flowId, status: "")
            flow.closeReadWithError(NSError(domain: "NexusAgent", code: 403, userInfo: [NSLocalizedDescriptionKey: "Blocked by policy"]))
            flow.closeWriteWithError(nil)
            completeFlow(flowId: flowId)

        case "inspect":
            // Inspect path → bridge. The peeked TLS ClientHello bytes
            // need to reach the bridge (so MITMRelay's TLS terminator
            // sees them). Open bridge connection, write BRIDGE header,
            // immediately replay peeked bytes, then bidir relay.
            // INFO level so operators can audit "what's being intercepted"
            // without enabling debug logs.
            logger.info("applyDecisionAfterPeek: INSPECT flow=\(flowId, privacy: .public) → \(host, privacy: .public):\(port) → bridge (peeked=\(peeked?.count ?? 0)B)")
            updateBumpStatus(flowId: flowId, status: "BUMP_SUCCESS")
            relayInspectViaBridgePostPeek(flowId: flowId, flow: flow, host: host, port: port, peeked: peeked)

        default: // "passthrough"
            // INFO level so the "Chrome went to chatgpt.com but wasn't
            // captured" diagnostic is answerable from agent.log alone —
            // a passthrough decision means the daemon's policy engine
            // returned passthrough (host not on inspect list / pause /
            // exemption). Without this line at INFO, every Chrome flow
            // is invisible at default log level.
            logger.info("applyDecisionAfterPeek: PASSTHROUGH flow=\(flowId, privacy: .public) → \(host, privacy: .public):\(port) (peeked=\(peeked?.count ?? 0)B)")
            updateBumpStatus(flowId: flowId, status: "")
            relayPassthroughPostPeek(flowId: flowId, flow: flow, host: host, port: port, peeked: peeked)
        }
    }

    /// relayInspectViaBridgePostPeek mirrors openAndRelayViaBridge but
    /// does NOT open the flow (peekSNIThenDecide already did) and
    /// replays the peeked ClientHello bytes immediately after writing
    /// the BRIDGE header so the Go-side MITMRelay's TLS terminator
    /// sees the full handshake.
    private func relayInspectViaBridgePostPeek(flowId: String, flow: NEAppProxyTCPFlow, host: String, port: Int, peeked: Data?) {
        let isIPv6 = host.contains(":")
        let hostPart = isIPv6 ? "[\(host)]" : host
        let header = "BRIDGE \(hostPart):\(port) \(flowId)\n"
        guard let headerData = header.data(using: .utf8) else {
            logger.error("relayInspectViaBridgePostPeek: header encode failed for flow=\(flowId, privacy: .public)")
            relayPassthroughPostPeek(flowId: flowId, flow: flow, host: host, port: port, peeked: peeked)
            return
        }
        let bridgeEndpoint = NWHostEndpoint(hostname: "127.0.0.1", port: "9443")
        let bridgeConn = createTCPConnection(to: bridgeEndpoint, enableTLS: false, tlsParameters: nil, delegate: nil)

        // Compose header + peeked bytes into a single write so the
        // bridge sees them as a contiguous stream (parseHeader
        // reads up to the newline, leaves the remainder buffered).
        var combined = headerData
        if let peeked = peeked { combined.append(peeked) }
        bridgeConn.write(combined) { [weak self] writeErr in
            guard let self else { return }
            if let writeErr = writeErr {
                self.logger.error("relayInspectViaBridgePostPeek: header+peek write FAILED for flow=\(flowId, privacy: .public): \(writeErr.localizedDescription) — falling back to direct")
                bridgeConn.cancel()
                self.relayPassthroughPostPeek(flowId: flowId, flow: flow, host: host, port: port, peeked: peeked)
                return
            }
            if let peeked = peeked {
                self.addBytesOut(flowId: flowId, count: Int64(peeked.count))
            }
            // Now run the standard bidirectional relay loops with the
            // bridge as remote. relayFlowToRemote starts reading
            // FRESH bytes from the flow (the first chunk was already
            // sent via combined above).
            self.relayFlowToRemote(flowId: flowId, flow: flow, remote: bridgeConn)
            self.relayRemoteToFlow(flowId: flowId, flow: flow, remote: bridgeConn)
        }
    }

    /// relayPassthroughPostPeek is the post-peek counterpart of the
    /// passthrough path. Connects directly to remote, replays peeked
    /// bytes, then bidir relay.
    private func relayPassthroughPostPeek(flowId: String, flow: NEAppProxyTCPFlow, host: String, port: Int, peeked: Data?) {
        let remoteHost = NWHostEndpoint(hostname: host, port: String(port))
        let remoteConn = createTCPConnection(to: remoteHost, enableTLS: false, tlsParameters: nil, delegate: nil)
        logger.info("relayPassthroughPostPeek: opened direct upstream flow=\(flowId, privacy: .public) → \(host, privacy: .public):\(port) peek_bytes=\(peeked?.count ?? 0)")
        if let peeked = peeked, !peeked.isEmpty {
            addBytesOut(flowId: flowId, count: Int64(peeked.count))
            remoteConn.write(peeked) { [weak self] err in
                guard let self else { return }
                if let err = err {
                    self.logger.debug("relayPassthroughPostPeek: peek write error for flow=\(flowId, privacy: .public): \(err.localizedDescription)")
                    return
                }
                self.relayFlowToRemote(flowId: flowId, flow: flow, remote: remoteConn)
            }
        } else {
            relayFlowToRemote(flowId: flowId, flow: flow, remote: remoteConn)
        }
        relayRemoteToFlow(flowId: flowId, flow: flow, remote: remoteConn)
    }

    // MARK: - Decision Application

    private func applyDecision(flowId: String, flow: NEAppProxyTCPFlow, host: String, port: Int, decision: String) {
        switch decision {
        case "deny":
            logger.info("applyDecision: DENY flow=\(flowId) → \(host):\(port) (closing with HTTP 403 sentinel)")
            updateBumpStatus(flowId: flowId, status: "")
            flow.closeReadWithError(NSError(domain: "NexusAgent", code: 403,
                userInfo: [NSLocalizedDescriptionKey: "Blocked by policy"]))
            flow.closeWriteWithError(nil)
            completeFlow(flowId: flowId)

        case "inspect":
            // E55: redirect inspect flows to the Go MITM bridge on
            // 127.0.0.1:9443. Bridge speaks the existing
            // proxy.MITMRelay pipeline (TLS termination + HTTP parse +
            // hooks + audit stamping). When bridge isn't reachable
            // (early-boot race / port not bound / Linux-style YAML
            // disabled it), fall through to legacy raw-relay so we
            // never lose the user's flow — same fail-open behaviour as
            // BUMP_FAILED_PASSTHROUGH on the linux/windows path.
            logger.debug("applyDecision: INSPECT flow=\(flowId) → \(host):\(port) → 127.0.0.1:9443 (E55 bridge)")
            updateBumpStatus(flowId: flowId, status: "BUMP_SUCCESS")
            openAndRelayViaBridge(flowId: flowId, flow: flow, host: host, port: port)

        default: // "passthrough"
            logger.debug("applyDecision: PASSTHROUGH flow=\(flowId) → \(host):\(port) (decision=\(decision); raw TCP relay)")
            updateBumpStatus(flowId: flowId, status: "")
            openAndRelay(flowId: flowId, flow: flow, host: host, port: port)
        }
    }

    /// E55 inspect path: open the NE flow, connect to the Go bridge
    /// at 127.0.0.1:9443, write the BRIDGE header, then relay bytes
    /// bidirectionally — exactly the same shape as openAndRelay
    /// except the "remote" is loopback. The Go side does TLS
    /// termination + HTTP parse + hook execution + audit stamping
    /// using the bytes we forward.
    ///
    /// Header format (binding contract with bridge.parseHeader):
    ///   `BRIDGE <host>:<port> <flowId>\n`
    ///
    /// Fail-open: if bridge connect fails (port not bound / refused),
    /// we fall back to the legacy openAndRelay direct-to-remote path
    /// + stamp BUMP_FAILED_PASSTHROUGH so the audit row carries the
    /// reason and the user's flow keeps working.
    private func openAndRelayViaBridge(flowId: String, flow: NEAppProxyTCPFlow, host: String, port: Int) {
        flow.open(withLocalEndpoint: nil) { [weak self] error in
            guard let self else { return }
            if let error = error {
                let nsErr = error as NSError
                self.logger.error("openAndRelayViaBridge: flow.open FAILED for flow=\(flowId, privacy: .public) → \(host, privacy: .public):\(port) — domain=\(nsErr.domain) code=\(nsErr.code)")
                self.updateBumpStatus(flowId: flowId, status: "BUMP_FAILED_PASSTHROUGH")
                // Reset the claimed flow so the browser retries immediately
                // instead of hanging for ~75 s on SYN timeout. See companion
                // comment in peekSNIThenDecide.
                flow.closeReadWithError(nsErr)
                flow.closeWriteWithError(nil)
                self.completeFlow(flowId: flowId)
                return
            }

            // IPv6 hosts must be bracketed in the BRIDGE header so the
            // Go-side parser can split host:port unambiguously.
            let isIPv6 = host.contains(":")
            let hostPart = isIPv6 ? "[\(host)]" : host
            let header = "BRIDGE \(hostPart):\(port) \(flowId)\n"
            guard let headerData = header.data(using: .utf8) else {
                self.logger.error("openAndRelayViaBridge: BRIDGE header utf8 encode failed for flow=\(flowId, privacy: .public)")
                self.fallbackToDirect(flowId: flowId, flow: flow, host: host, port: port)
                return
            }

            let bridgeEndpoint = NWHostEndpoint(hostname: "127.0.0.1", port: "9443")
            let bridgeConn = self.createTCPConnection(to: bridgeEndpoint, enableTLS: false, tlsParameters: nil, delegate: nil)

            // Write the BRIDGE header first. On success start the
            // peek-SNI-then-relay loop with bridgeConn as remote;
            // bridge.parseHeader consumes the header line, then hands
            // the remaining bytes (the TLS ClientHello) to MITMRelay.
            bridgeConn.write(headerData) { [weak self] writeErr in
                guard let self else { return }
                if let writeErr = writeErr {
                    self.logger.error("openAndRelayViaBridge: BRIDGE header write FAILED for flow=\(flowId, privacy: .public) — \(writeErr.localizedDescription) — falling back to direct relay")
                    bridgeConn.cancel()
                    self.fallbackToDirect(flowId: flowId, flow: flow, host: host, port: port)
                    return
                }
                self.logger.debug("openAndRelayViaBridge: BRIDGE header sent for flow=\(flowId, privacy: .public); arming bidir relay through 127.0.0.1:9443")
                self.peekSNIThenRelay(flowId: flowId, flow: flow, remote: bridgeConn)
                self.relayRemoteToFlow(flowId: flowId, flow: flow, remote: bridgeConn)
            }
        }
    }

    /// Fail-open fallback when the E55 bridge is unreachable.
    /// Reuses the legacy raw-relay path. Stamps BUMP_FAILED_PASSTHROUGH
    /// so the audit row tells the operator why the user's inspect
    /// flow ended up uninspected.
    private func fallbackToDirect(flowId: String, flow: NEAppProxyTCPFlow, host: String, port: Int) {
        self.updateBumpStatus(flowId: flowId, status: "BUMP_FAILED_PASSTHROUGH")
        let remoteHost = NWHostEndpoint(hostname: host, port: String(port))
        let remoteConn = self.createTCPConnection(to: remoteHost, enableTLS: false, tlsParameters: nil, delegate: nil)
        self.peekSNIThenRelay(flowId: flowId, flow: flow, remote: remoteConn)
        self.relayRemoteToFlow(flowId: flowId, flow: flow, remote: remoteConn)
    }

    /// Open the flow and establish a TCP connection to the remote server, then
    /// relay data bidirectionally: flow (app outbound) → remote, and remote → flow (app inbound).
    /// Used by the passthrough path. The inspect path goes through openAndRelayViaBridge.
    private func openAndRelay(flowId: String, flow: NEAppProxyTCPFlow, host: String, port: Int) {
        // Open the NE flow first (required before reading/writing).
        flow.open(withLocalEndpoint: nil) { [weak self] error in
            if let error = error {
                let nsErr = error as NSError
                self?.logger.error("openAndRelay: flow.open FAILED for flow=\(flowId) → \(host):\(port) — domain=\(nsErr.domain) code=\(nsErr.code) localized=\(error.localizedDescription)")
                self?.updateBumpStatus(flowId: flowId, status: "BUMP_FAILED_PASSTHROUGH")
                // Reset the claimed flow; see companion comment in
                // peekSNIThenDecide. Without this the browser sits on
                // its SYN for ~75 s.
                flow.closeReadWithError(nsErr)
                flow.closeWriteWithError(nil)
                self?.completeFlow(flowId: flowId)
                return
            }

            // Connect to the actual remote server.
            let remoteHost = NWHostEndpoint(hostname: host, port: String(port))
            guard let remoteConn = self?.createTCPConnection(to: remoteHost, enableTLS: false, tlsParameters: nil, delegate: nil) else {
                self?.logger.error("openAndRelay: createTCPConnection returned nil for flow=\(flowId) → \(host):\(port) — provider self may have been deallocated mid-flight")
                // Same fail-open shape: flow is opened but we cannot create
                // an upstream socket — reset so the browser retries.
                flow.closeReadWithError(NSError(domain: "NexusAgent", code: -1, userInfo: [NSLocalizedDescriptionKey: "upstream socket unavailable"]))
                flow.closeWriteWithError(nil)
                self?.completeFlow(flowId: flowId)
                return
            }

            // Direction 1: app → remote, with one-shot SNI peek on
            // the very first chunk before falling into the normal relay
            // loop. SNI lets us recover the real hostname (e.g.
            // "chatgpt.com") for callers that pre-resolved DNS and
            // therefore left NEAppProxyFlow.remoteHostname nil.
            self?.peekSNIThenRelay(flowId: flowId, flow: flow, remote: remoteConn)
            // Direction 2: remote → app (read from remote, write to flow)
            self?.relayRemoteToFlow(flowId: flowId, flow: flow, remote: remoteConn)
        }
    }

    /// One-shot reader: peek the flow's first chunk, try to extract a
    /// TLS SNI hostname, notify the daemon, then forward the same
    /// chunk to remote and re-arm the normal app→remote relay loop.
    /// On non-TLS / empty / parse-fail flows we just forward the
    /// chunk and continue normally — SNI extraction is best-effort.
    ///
    /// SAFETY: bounded by a 500 ms timeout. Non-TLS protocols where the
    /// client speaks first (HTTPS, gRPC) emit ClientHello within
    /// microseconds of TCP handshake. Protocols where the SERVER speaks
    /// first (SSH, SMTP, IMAP, plain HTTP after redirect) never emit a
    /// ClientHello-shaped first chunk — without the timeout the read
    /// hangs forever and the connection appears broken to the app.
    /// On timeout we abandon the SNI peek and fall straight into the
    /// normal app→remote relay; the audit row just won't carry an SNI
    /// hostname for that flow.
    private func peekSNIThenRelay(flowId: String, flow: NEAppProxyTCPFlow, remote: NWTCPConnection) {
        let timeoutFired = TimeoutGuard()
        queue.asyncAfter(deadline: .now() + .milliseconds(500)) { [weak self] in
            guard let self else { return }
            if timeoutFired.tryFire() {
                self.logger.debug("peekSNIThenRelay: 500 ms timeout for flow=\(flowId, privacy: .public) — falling through to plain relay (likely server-speaks-first protocol)")
                self.relayFlowToRemote(flowId: flowId, flow: flow, remote: remote)
            }
        }

        flow.readData(completionHandler: { [weak self] data, error in
            guard let self else { return }
            // If the timeout already won the race, do not double-arm
            // the relay loop or write to remote (the loop is already
            // pulling from the flow and would race with us).
            guard timeoutFired.tryFire() else {
                self.logger.debug("peekSNIThenRelay: read completed AFTER 500 ms timeout for flow=\(flowId, privacy: .public) — discarding")
                return
            }
            if let error = error {
                self.logger.debug("App→Remote first-read done for \(flowId): \(error.localizedDescription)")
                remote.cancel()
                return
            }
            guard let data = data, !data.isEmpty else {
                // App closed before sending anything — send FIN.
                remote.writeClose()
                return
            }

            if let sni = SNIParser.extractSNI(from: data) {
                self.logger.info("SNI extracted for flow=\(flowId): \(sni, privacy: .public)")
                self.ipcClient.notifyFlowHost(flowId: flowId, hostname: sni)
            }

            self.addBytesOut(flowId: flowId, count: Int64(data.count))

            remote.write(data) { [weak self] writeError in
                guard let self else { return }
                if let writeError = writeError {
                    self.logger.debug("App→Remote first-write error for \(flowId): \(writeError.localizedDescription)")
                    return
                }
                // Hand off to the normal relay loop for subsequent chunks.
                self.relayFlowToRemote(flowId: flowId, flow: flow, remote: remote)
            }
        })
    }

    /// Read data from the app (via flow.readData) and forward to the remote server.
    private func relayFlowToRemote(flowId: String, flow: NEAppProxyTCPFlow, remote: NWTCPConnection) {
        flow.readData(completionHandler: { [weak self] data, error in
            if let error = error {
                self?.logger.debug("App→Remote read done for \(flowId): \(error.localizedDescription)")
                remote.cancel()
                return
            }
            guard let data = data, !data.isEmpty else {
                // App finished sending — send FIN to remote.
                remote.writeClose()
                return
            }

            self?.addBytesOut(flowId: flowId, count: Int64(data.count))

            remote.write(data) { writeError in
                if let writeError = writeError {
                    // INFO: a write error to the upstream socket means the
                    // remote dropped while the app was still sending —
                    // visible to the user as "page upload froze" / failed
                    // POST / dropped websocket. Always actionable.
                    self?.logger.info("relayFlowToRemote: WRITE upstream FAILED flow=\(flowId, privacy: .public) bytes=\(data.count) err=\(writeError.localizedDescription, privacy: .public) — remote likely closed; relay tearing down")
                    return
                }
                self?.relayFlowToRemote(flowId: flowId, flow: flow, remote: remote)
            }
        })
    }

    /// Read data from the remote server and forward to the app (via flow.write).
    private func relayRemoteToFlow(flowId: String, flow: NEAppProxyTCPFlow, remote: NWTCPConnection) {
        remote.readMinimumLength(1, maximumLength: 65536) { [weak self] data, error in
            if let error = error {
                self?.logger.debug("Remote→App read done for \(flowId): \(error.localizedDescription)")
                flow.closeReadWithError(nil)
                self?.completeFlow(flowId: flowId)
                return
            }
            guard let data = data, !data.isEmpty else {
                flow.closeReadWithError(nil)
                self?.completeFlow(flowId: flowId)
                return
            }

            self?.addBytesIn(flowId: flowId, count: Int64(data.count))

            flow.write(data) { writeError in
                if let writeError = writeError {
                    // INFO: a write error to the app side means the user's
                    // app dropped the connection (closed tab, killed app,
                    // socket reset) before all upstream bytes arrived.
                    // Visible to the user as "download truncated" /
                    // "page partially loaded then froze". Always actionable.
                    self?.logger.info("relayRemoteToFlow: WRITE app FAILED flow=\(flowId, privacy: .public) bytes=\(data.count) err=\(writeError.localizedDescription, privacy: .public) — app likely closed; relay tearing down")
                    remote.cancel()
                    self?.completeFlow(flowId: flowId)
                    return
                }
                self?.relayRemoteToFlow(flowId: flowId, flow: flow, remote: remote)
            }
        }
    }

    // MARK: - Flow State Management

    private func addBytesIn(flowId: String, count: Int64) {
        flowLock.lock()
        activeFlows[flowId]?.bytesIn += count
        flowLock.unlock()
    }

    private func addBytesOut(flowId: String, count: Int64) {
        flowLock.lock()
        activeFlows[flowId]?.bytesOut += count
        flowLock.unlock()
    }

    private func updateBumpStatus(flowId: String, status: String) {
        flowLock.lock()
        activeFlows[flowId]?.bumpStatus = status
        flowLock.unlock()
    }

    private func completeFlow(flowId: String) {
        flowLock.lock()
        guard let state = activeFlows.removeValue(forKey: flowId) else {
            flowLock.unlock()
            return
        }
        flowLock.unlock()

        // Per-flow terminal summary at INFO so any "this flow froze for
        // the user" investigation can pivot directly from flow_id to the
        // exit reason (bytes seen / duration / bumpStatus). The bumpStatus
        // tells you which path the flow took: empty=passthrough,
        // BUMP_SUCCESS=inspect, BUMP_FAILED_PASSTHROUGH=inspect-attempted-
        // then-fallback. duration_ms near 0 with 0 bytes = flow was
        // reset before any relay (typically flow.open failed and we
        // closeRead'd per the #82 fail-open contract).
        let durationMs = Int(Date().timeIntervalSince(state.startTime) * 1000)
        logger.info("completeFlow: flow=\(flowId, privacy: .public) bytes_in=\(state.bytesIn) bytes_out=\(state.bytesOut) duration_ms=\(durationMs) bump_status=\(state.bumpStatus, privacy: .public)")

        reportFlowClosed(flowId: flowId, state: state)
    }

    private func reportFlowClosed(flowId: String, state: FlowState) {
        let durationMs = Int(Date().timeIntervalSince(state.startTime) * 1000)
        // E50: forward optional phase fields. nil when URLSession metrics
        // weren't captured this flow (passthrough / raw TCP relay path).
        ipcClient.notifyFlowClosed(
            flowId: flowId,
            bytesIn: state.bytesIn,
            bytesOut: state.bytesOut,
            durationMs: durationMs,
            bumpStatus: state.bumpStatus,
            upstreamTtfbMs: state.upstreamTtfbMs,
            upstreamTotalMs: state.upstreamTotalMs,
            interceptMs: state.interceptMs
        )
    }

    /// E50 phase setters — called by the URLSession metrics delegate when
    /// upstream task metrics surface (typically `urlSession(_:task:didFinishCollecting:)`).
    /// `metrics.taskInterval.duration` ≈ upstreamTotalMs and
    /// `responseStartDate - requestStartDate` ≈ upstreamTtfbMs.
    func stampPhaseMetrics(flowId: String,
                           upstreamTtfbMs: Int?,
                           upstreamTotalMs: Int?) {
        flowLock.lock()
        defer { flowLock.unlock() }
        guard var state = activeFlows[flowId] else { return }
        if let v = upstreamTtfbMs { state.upstreamTtfbMs = v }
        if let v = upstreamTotalMs { state.upstreamTotalMs = v }
        activeFlows[flowId] = state
    }

    // MARK: - Process Resolution

    /// Extract PID from the flow's metadata audit token.
    private func extractPID(from flow: NEAppProxyFlow) -> Int32 {
        // audit_token_t is a struct of 8 uint32 values. Index 5 holds the PID
        // per the XNU kernel convention (same field used by SecCodeCopyGuestWithAttributes).
        // This avoids the private audit_token_to_pid() symbol that is not exported
        // from the public SDK and causes linker errors.
        guard let tokenData = flow.metaData.sourceAppAuditToken else { return 0 }
        return tokenData.withUnsafeBytes { buf -> Int32 in
            guard buf.count >= MemoryLayout<audit_token_t>.size else { return 0 }
            let token = buf.load(as: audit_token_t.self)
            // PID is at index 5 of the val[8] array in audit_token_t.
            return Int32(bitPattern: withUnsafeBytes(of: token) { raw in
                raw.load(fromByteOffset: 5 * MemoryLayout<UInt32>.stride, as: UInt32.self)
            })
        }
    }

    /// Cheap test: dotted-quad (a.b.c.d) or colon-hex (xxx:yyy:…) ⇒ IP literal.
    /// Avoids DNS round-trip when endpointAddr is already an IP, which is
    /// the normal case for callers that pre-resolved DNS (curl, browsers).
    private func isLikelyIPLiteral(_ s: String) -> Bool {
        if s.isEmpty { return false }
        return s.contains(":") || (s.unicodeScalars.allSatisfy { c in
            (c >= "0" && c <= "9") || c == "."
        } && s.contains("."))
    }

    private func extractLocalPort(from flow: NEAppProxyFlow) -> Int {
        // NEAppProxyFlow doesn't directly expose local port in all versions.
        // Return 0 as a fallback — the Go agent doesn't require it for decisions.
        return 0
    }

    /// Best-effort hostname → IP resolution.
    private func resolveIP(host: String) -> String {
        let host = CFHostCreateWithName(nil, host as CFString).takeRetainedValue()
        CFHostStartInfoResolution(host, .addresses, nil)
        var success: DarwinBoolean = false
        guard let addresses = CFHostGetAddressing(host, &success)?.takeUnretainedValue() as? [Data],
              let first = addresses.first else {
            return ""
        }
        return first.withUnsafeBytes { buf -> String in
            let sa = buf.load(as: sockaddr.self)
            if sa.sa_family == sa_family_t(AF_INET) {
                let addr4 = buf.load(as: sockaddr_in.self)
                var ip = addr4.sin_addr
                var str = [CChar](repeating: 0, count: Int(INET_ADDRSTRLEN))
                inet_ntop(AF_INET, &ip, &str, socklen_t(INET_ADDRSTRLEN))
                return String(cString: str)
            }
            return ""
        }
    }
}
